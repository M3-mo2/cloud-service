package service

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/storage"
	"sandboxd/internal/workspace"
)

// StartRunSpec describes an asynchronous run request.
type StartRunSpec struct {
	Kind    model.RunKind
	File    string
	Args    []string
	Argv    []string
	Env     map[string]string
	Timeout int
	// Cwd is the working directory, relative to the workspace root. Empty means
	// the workspace root. It is validated to exist and be a directory before the
	// run is accepted.
	Cwd string
	// Interpreter overrides the extension-derived interpreter for a kind=file
	// run. It must be in the allowlist (model.Interpreters).
	Interpreter string
	// NoTimeout requests a wall-clock-unbounded run. It is distinct from
	// Timeout<=0 (which means "use the workspace default") and must be permitted
	// at both the server and the workspace level.
	NoTimeout bool
}

// workspaceFilePath renders a workspace-relative file as the absolute sandbox
// path (/workspace/<rel>). The workspace is bind-mounted at /workspace inside
// the sandbox, so this path is valid regardless of the run's cwd. Passing the
// file as an absolute sandbox path (rather than a cwd-relative one) is what
// keeps `cwd` purely a working directory rather than a relative-path prefix.
func workspaceFilePath(file string) string {
	p := strings.ReplaceAll(file, "\\", "/")
	parts := make([]string, 0, 4)
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, part)
		}
	}
	return "/workspace/" + strings.Join(parts, "/")
}

// resolveFileInterpreter chooses the interpreter for a kind=file run: an
// explicit, allowlisted override wins; otherwise the file extension maps to
// its interpreter. An unsupported extension with no override is 422 naming the
// supported set.
func resolveFileInterpreter(file, explicit string) (string, error) {
	if explicit != "" {
		if !model.IsAllowedInterpreter(explicit) {
			return "", model.Wrap(422, model.CodeUnprocessable,
				"interpreter is not in the supported set",
				map[string]string{
					"interpreter": explicit,
					"supported":   strings.Join(model.SupportedInterpreters(), ","),
				})
		}
		return explicit, nil
	}
	if in := model.InterpreterForFile(file); in != "" {
		return in, nil
	}
	return "", model.Wrap(422, model.CodeUnprocessable,
		"no interpreter is registered for this file extension",
		map[string]string{
			"file":                 file,
			"supported_extensions": strings.Join(model.SupportedExtensions(), ","),
		})
}

// resolveRunCwd validates a run working directory against the workspace root
// and returns its normalized workspace-rooted path (leading slash; "/" is the
// root). A path that escapes the root, does not exist, or is not a directory is
// rejected with 422 -- never deferred to execution, where a missing cwd would
// otherwise surface as a misleading 503 sandbox-unavailable error.
func (s *Service) resolveRunCwd(fs *workspace.Fs, cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return "/", nil
	}
	abs, err := fs.Resolve(cwd)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", model.Wrap(422, model.CodeUnprocessable, "cwd does not exist",
				map[string]string{"cwd": cwd})
		}
		return "", err
	}
	if !fi.IsDir() {
		return "", model.Wrap(422, model.CodeUnprocessable, "cwd is not a directory",
			map[string]string{"cwd": cwd})
	}
	rel, err := filepath.Rel(fs.Root(), abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "/", nil
	}
	return "/" + filepath.ToSlash(rel), nil
}

// StartRun creates and launches an asynchronous run.
func (s *Service) StartRun(w *model.Workspace, spec StartRunSpec) (*model.Run, error) {
	if err := s.ensureActive(w); err != nil {
		return nil, err
	}
	// raw argv and kind=file are mutually exclusive; the precedence is explicit
	// and documented (docs/API.md). Rejecting the combination is safer than
	// silently picking one.
	if spec.Kind == model.RunKindFile && len(spec.Argv) > 0 {
		return nil, model.Wrap(422, model.CodeUnprocessable,
			"argv cannot be combined with kind=file; use args, or kind=command",
			map[string]string{"field": "argv"})
	}

	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return nil, err
	}

	// cwd applies to both kinds and is a working directory, not a relative-path
	// prefix for file.
	cwd, err := s.resolveRunCwd(fs, spec.Cwd)
	if err != nil {
		return nil, err
	}

	argv := spec.Argv
	interpreter := ""
	if spec.Kind == model.RunKindFile {
		// Path safety is checked first so a traversal/escape is reported as
		// unsafe (422) regardless of the file's extension.
		resolved, err := fs.Resolve(spec.File)
		if err != nil {
			return nil, err
		}
		// Resolve the interpreter next (a pure request-level check) so an
		// unsupported extension or a non-allowlisted override is reported as 422
		// even when the file does not exist.
		interpreter, err = resolveFileInterpreter(spec.File, spec.Interpreter)
		if err != nil {
			return nil, err
		}
		// Validate the target file exists before accepting the run, rather than
		// deferring the failure to execution time.
		if fi, err := os.Stat(resolved); err != nil {
			if os.IsNotExist(err) {
				return nil, model.ErrNotFound
			}
			return nil, err
		} else if fi.IsDir() {
			return nil, model.New(422, model.CodeUnprocessable, "file run target is a directory")
		}
		// A file run executes the chosen interpreter against the file. The file
		// is resolved relative to the workspace root and passed as an absolute
		// sandbox path (/workspace is the fixed mount point), so it is found
		// regardless of the requested working directory: cwd is a working
		// directory, NOT a relative-path prefix for file.
		file := workspaceFilePath(spec.File)
		argv = append([]string{interpreter, file}, spec.Args...)
	}

	// Wall-clock policy. An explicit unbounded request must be permitted at both
	// the server and the workspace level; a finite request is clamped to the
	// workspace maximum (Timeout <= 0 means "use the workspace default").
	unlimited := spec.NoTimeout
	timeout := 0
	if unlimited {
		if err := s.UnlimitedAllowed(w); err != nil {
			return nil, err
		}
	} else {
		timeout = spec.Timeout
		if timeout <= 0 || timeout > w.Limits.MaxWallTimeSec {
			timeout = w.Limits.MaxWallTimeSec
		}
	}

	// Prune old runs before starting a new one (retention policy).
	if _, err := s.store.PruneRuns(w.ID, storage.Retention-1); err != nil {
		s.logger.Warn("prune runs failed", "workspace_id", w.ID, "error", err)
	}

	// Prepare the workspace's isolated network before recording the run, so a
	// networking failure refuses the request instead of leaving a queued run
	// that can never get a sandbox.
	if err := s.EnsureWorkspaceNetwork(w); err != nil {
		return nil, err
	}

	// Mount the workspace's per-workspace disk before the run is recorded, so
	// the sandbox binds a bounded filesystem with the workspace's own size.
	if err := s.ensureWorkspaceDisk(w); err != nil {
		return nil, model.New(503, model.CodeUnavailable, "workspace disk unavailable: "+err.Error())
	}

	// Create (idempotently) the workspace's parent cgroup and write its enforced
	// limits before the run is recorded. Every run's leaf cgroup is created
	// inside this parent, so the SUM across concurrent runs is bounded by the
	// advertised memory_mb. A provisioning failure refuses the run rather than
	// run without the ceiling the workspace was promised.
	if err := s.ensureWorkspaceCgroup(w); err != nil {
		return nil, model.New(503, model.CodeUnavailable, "workspace cgroup unavailable: "+err.Error())
	}

	// Resolve the two package tiers before the run starts. A failure to
	// prepare the private tier is fatal; a missing shared tier is not.
	sharedHost, privateHost, err := s.packageDirs(w)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	run := &model.Run{
		ID:          model.NewRunID(),
		WorkspaceID: w.ID,
		Kind:        spec.Kind,
		Argv:        argv,
		Status:      model.RunQueued,
		StartedAt:   now,
		Cwd:         cwd,
		Unlimited:   unlimited,
		Interpreter: interpreter,
	}
	if !unlimited {
		run.SetTimeoutSec(timeout)
	}
	if err := s.store.CreateRun(w.ID, run); err != nil {
		return nil, err
	}
	s.touchActivity(w.ID)

	stdoutPath := s.store.RunLogPath(w.ID, run.ID, model.StreamStdout)
	stderrPath := s.store.RunLogPath(w.ID, run.ID, model.StreamStderr)
	caps := s.cfg.Caps

	ctx, cancel := context.WithCancel(context.Background())

	req := executor.Request{
		Workspace:     w,
		WorkspaceRoot: s.store.WorkspaceRoot(w.ID),
		Argv:          argv,
		Cwd:           cwd,
		Env:           spec.Env,
		Timeout:       time.Duration(timeout) * time.Second,
		NoTimeout:     unlimited,
		Context:       ctx,
		RunID:         run.ID,
		StdoutPath:    stdoutPath,
		StderrPath:    stderrPath,
		StdoutCap:     caps.RunLogBytes(),
		StderrCap:     caps.RunLogBytes(),

		// Tag the sandbox so the process inventory can attribute every process
		// (including a descendant that left the process group) to this run.
		ProcessKind:   executor.KindRun,
		ProcessSource: run.ID,

		// The workspace's parent cgroup: nsjail creates this run's leaf inside
		// it, and the executor reads its oom_kill counter to tell an OOM at the
		// ceiling apart from a clean exit.
		WorkspaceCgroup: workspaceCgroupPath(w.ID),

		SharedPackagesDir:  sharedHost,
		PrivatePackagesDir: privateHost,
	}
	// Attach the workspace's own isolated network to the executor request.
	req, err = s.ApplyNetwork(w, req)
	if err != nil {
		cancel()
		_ = s.store.DeleteRun(w.ID, run.ID)
		return nil, err
	}

	rr := &runningRun{runID: run.ID, wsID: w.ID, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.runs[run.ID] = rr
	s.mu.Unlock()

	// The goroutine owns its own copy of the run so the caller's snapshot
	// (returned below for immediate rendering) is never mutated concurrently.
	asyncRun := cloneRun(run)
	go s.executeRun(ctx, w, asyncRun, req, rr)

	return run, nil
}

// cloneRun returns a deep copy of a run, including its argv slice.
func cloneRun(r *model.Run) *model.Run {
	cp := *r
	cp.Argv = append([]string(nil), r.Argv...)
	if r.ExitCode != nil {
		code := *r.ExitCode
		cp.ExitCode = &code
	}
	if r.TimeoutSec != nil {
		t := *r.TimeoutSec
		cp.TimeoutSec = &t
	}
	return &cp
}

func (s *Service) executeRun(ctx context.Context, w *model.Workspace, run *model.Run, req executor.Request, rr *runningRun) {
	defer close(rr.done)
	defer func() {
		s.mu.Lock()
		delete(s.runs, run.ID)
		s.mu.Unlock()
	}()

	run.Status = model.RunRunning
	run.StartedAt = time.Now().UTC()
	if err := s.store.SaveRun(w.ID, run); err != nil {
		s.logger.Error("save run start failed", "run_id", run.ID, "error", err)
	}

	res, err := s.runner.Run(req)
	run.EndedAt = time.Now().UTC()
	if err != nil {
		run.Status = model.RunFailed
		s.logger.Error("run failed", "run_id", run.ID, "error", err)
	} else if res.TimedOut {
		run.Status = model.RunTimeout
		run.SetExitCode(res.ExitCode)
	} else if res.OOM {
		// Killed by the kernel at the workspace's memory ceiling. This is NOT a
		// clean exit: it reads as `oom`, an explicit terminal state, so a client
		// can tell "the workspace ran out of memory" from "the program finished".
		run.Status = model.RunOOM
		run.SetExitCode(res.ExitCode)
	} else if res.Killed {
		run.Status = model.RunKilled
		run.SetExitCode(res.ExitCode)
	} else {
		run.Status = model.RunExited
		run.SetExitCode(res.ExitCode)
	}

	if fi, err := os.Stat(s.store.RunLogPath(w.ID, run.ID, model.StreamStdout)); err == nil {
		run.Logs.StdoutBytes = fi.Size()
	}
	if fi, err := os.Stat(s.store.RunLogPath(w.ID, run.ID, model.StreamStderr)); err == nil {
		run.Logs.StderrBytes = fi.Size()
	}
	run.Logs.Truncated = res.Truncated

	if err := s.store.SaveRun(w.ID, run); err != nil {
		s.logger.Error("save run finish failed", "run_id", run.ID, "error", err)
	}
}

// StopRun stops a running run by ID. Returns true if a running process was
// signalled. Idempotent: stopping an unknown/finished run returns false, nil.
// The returned status reflects the state driven by the stop: a live run is
// reported as killed; an already-finished run reports its stored status.
func (s *Service) StopRun(wsID, runID string) (bool, model.RunStatus, error) {
	run, err := s.store.GetRun(wsID, runID)
	if err != nil {
		return false, "", err
	}
	s.mu.Lock()
	rr, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		return false, run.Status, nil
	}
	rr.cancel()
	killed := s.runner.Kill(runID)
	if killed {
		return true, model.RunKilled, nil
	}
	return false, run.Status, nil
}

// StopAllRuns cancels every running run for a workspace and waits briefly.
func (s *Service) StopAllRuns(wsID string) {
	s.mu.Lock()
	var victims []*runningRun
	for _, rr := range s.runs {
		if rr.wsID == wsID {
			victims = append(victims, rr)
		}
	}
	s.mu.Unlock()
	for _, rr := range victims {
		rr.cancel()
		s.runner.Kill(rr.runID)
	}
	deadline := time.After(5 * time.Second)
	for _, rr := range victims {
		select {
		case <-rr.done:
		case <-deadline:
			return
		}
	}
}

// WaitRuns blocks until no run for the workspace is executing. It is primarily
// useful for orderly shutdown and tests.
func (s *Service) WaitRuns(wsID string) {
	s.mu.Lock()
	var pending []chan struct{}
	for _, rr := range s.runs {
		if rr.wsID == wsID {
			pending = append(pending, rr.done)
		}
	}
	s.mu.Unlock()
	for _, done := range pending {
		<-done
	}
}

// WaitAll blocks until every tracked run across all workspaces has finished.
func (s *Service) WaitAll() {
	s.mu.Lock()
	var pending []chan struct{}
	for _, rr := range s.runs {
		pending = append(pending, rr.done)
	}
	s.mu.Unlock()
	for _, done := range pending {
		<-done
	}
}

// RecoverRuns reconciles run state after a restart. Any run still recorded as
// queued or running has no surviving process, so it is moved to the terminal
// interrupted state. Returns the number of runs recovered.
func (s *Service) RecoverRuns() (int, error) {
	workspaces, err := s.store.ListAllWorkspaces()
	if err != nil {
		return 0, err
	}
	recovered := 0
	now := time.Now().UTC()
	for _, w := range workspaces {
		runs, err := s.store.ListRuns(w.ID)
		if err != nil {
			s.logger.Warn("recover runs: list failed", "workspace_id", w.ID, "error", err)
			continue
		}
		for _, run := range runs {
			if run.Status != model.RunQueued && run.Status != model.RunRunning {
				continue
			}
			run.Status = model.RunInterrupted
			if run.EndedAt.IsZero() {
				run.EndedAt = now
			}
			if err := s.store.SaveRun(w.ID, run); err != nil {
				s.logger.Warn("recover run: save failed", "run_id", run.ID, "error", err)
				continue
			}
			recovered++
		}
	}
	return recovered, nil
}

// Shutdown stops every running run and waits for them to drain, bounded by
// deadline. It returns false if the deadline elapsed with runs still active.
func (s *Service) Shutdown(deadline time.Duration) bool {
	// Stop the background loops first: neither must act during drain.
	s.stopDiskWatchdog()
	s.stopReaper()
	s.ShutdownMetrics()
	s.mu.Lock()
	ids := make([]string, 0, len(s.runs))
	for id, rr := range s.runs {
		ids = append(ids, id)
		rr.cancel()
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.runner.Kill(id)
	}
	timer := time.After(deadline)
	s.mu.Lock()
	pending := make([]chan struct{}, 0, len(s.runs))
	for _, rr := range s.runs {
		pending = append(pending, rr.done)
	}
	s.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-timer:
			return false
		}
	}
	return true
}

// GetRun returns a run for a workspace.
func (s *Service) GetRun(w *model.Workspace, runID string) (*model.Run, error) {
	return s.store.GetRun(w.ID, runID)
}

// RunFilter selects a subset of a workspace's runs. A zero-valued filter
// matches every run. Limit <= 0 means "all remaining".
type RunFilter struct {
	Statuses []model.RunStatus
	Kind     model.RunKind
	Limit    int
	Offset   int
}

// ListRuns returns runs matching filter, newest first, plus the total number of
// runs matching the filter before pagination.
func (s *Service) ListRuns(w *model.Workspace, f RunFilter) ([]*model.Run, int, error) {
	all, err := s.store.ListRuns(w.ID)
	if err != nil {
		return nil, 0, err
	}
	matched := make([]*model.Run, 0, len(all))
	for _, r := range all {
		if len(f.Statuses) > 0 && !containsStatus(f.Statuses, r.Status) {
			continue
		}
		if f.Kind != "" && r.Kind != f.Kind {
			continue
		}
		matched = append(matched, r)
	}
	total := len(matched)
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	matched = matched[offset:]
	if f.Limit > 0 && len(matched) > f.Limit {
		matched = matched[:f.Limit]
	}
	return matched, total, nil
}

func containsStatus(statuses []model.RunStatus, s model.RunStatus) bool {
	for _, st := range statuses {
		if st == s {
			return true
		}
	}
	return false
}

// PurgeRuns deletes the finished run history for a workspace and returns the
// number deleted. A queued/running run makes the whole purge refuse with 409
// and nothing is deleted.
func (s *Service) PurgeRuns(w *model.Workspace) (int, error) {
	runs, err := s.store.ListRuns(w.ID)
	if err != nil {
		return 0, err
	}
	for _, r := range runs {
		if r.Status == model.RunQueued || r.Status == model.RunRunning {
			return 0, model.Wrap(409, model.CodeConflict,
				"cannot purge run history while a run is running",
				map[string]string{"run_id": r.ID})
		}
	}
	deleted := 0
	for _, r := range runs {
		if err := s.store.DeleteRun(w.ID, r.ID); err != nil {
			return deleted, err
		}
		deleted++
	}
	s.touchActivity(w.ID)
	return deleted, nil
}

// PurgeRun deletes a single finished run. It refuses with 409 while the run is
// queued or running.
func (s *Service) PurgeRun(w *model.Workspace, runID string) error {
	run, err := s.store.GetRun(w.ID, runID)
	if err != nil {
		return err
	}
	if run.Status == model.RunQueued || run.Status == model.RunRunning {
		return model.Wrap(409, model.CodeConflict, "cannot delete a running run",
			map[string]string{"run_id": run.ID})
	}
	if err := s.store.DeleteRun(w.ID, runID); err != nil {
		return err
	}
	s.touchActivity(w.ID)
	return nil
}

// LogChunk is a polled slice of a run log.
type LogChunk struct {
	Data      []byte
	Offset    int64
	Next      int64
	EOF       bool
	Truncated bool
}

// ReadLog reads up to limit bytes of a run log stream starting at offset.
func (s *Service) ReadLog(w *model.Workspace, runID, stream string, offset, limit int64) (LogChunk, error) {
	if _, err := s.store.GetRun(w.ID, runID); err != nil {
		return LogChunk{}, err
	}
	path := s.store.RunLogPath(w.ID, runID, stream)
	fh, err := os.Open(path)
	if err != nil {
		return LogChunk{}, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return LogChunk{}, err
	}
	size := fi.Size()
	if offset < 0 {
		offset = 0
	}
	if offset > size {
		offset = size
	}
	if limit <= 0 {
		limit = 65536
	}
	remaining := size - offset
	toRead := limit
	if toRead > remaining {
		toRead = remaining
	}
	buf := make([]byte, toRead)
	if toRead > 0 {
		if _, err := fh.ReadAt(buf, offset); err != nil && err != io.EOF {
			return LogChunk{}, err
		}
	}
	next := offset + toRead
	return LogChunk{
		Data:      buf,
		Offset:    offset,
		Next:      next,
		EOF:       next >= size,
		Truncated: false,
	}, nil
}
