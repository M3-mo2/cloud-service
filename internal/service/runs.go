package service

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/storage"
)

// StartRunSpec describes an asynchronous run request.
type StartRunSpec struct {
	Kind    model.RunKind
	File    string
	Args    []string
	Argv    []string
	Env     map[string]string
	Timeout int
}

// StartRun creates and launches an asynchronous run.
func (s *Service) StartRun(w *model.Workspace, spec StartRunSpec) (*model.Run, error) {
	if err := s.ensureActive(w); err != nil {
		return nil, err
	}
	timeout := spec.Timeout
	if timeout <= 0 || timeout > w.Limits.MaxWallTimeSec {
		timeout = w.Limits.MaxWallTimeSec
	}
	argv := spec.Argv
	if spec.Kind == model.RunKindFile {
		// Validate the target file resolves inside the workspace and exists
		// before accepting the run, rather than deferring the failure to
		// execution time.
		fs, err := s.WorkspaceFs(w)
		if err != nil {
			return nil, err
		}
		resolved, err := fs.Resolve(spec.File)
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(resolved); err != nil {
			if os.IsNotExist(err) {
				return nil, model.ErrNotFound
			}
			return nil, err
		} else if fi.IsDir() {
			return nil, model.New(422, model.CodeUnprocessable, "file run target is a directory")
		}
		// A file run executes the default runtime (python3) against the file,
		// whose path is relative to the workspace root (bind-mounted at
		// /workspace, which is the sandbox CWD).
		file := strings.TrimPrefix(spec.File, "/")
		argv = append([]string{"python3", file}, spec.Args...)
	}

	// Prune old runs before starting a new one (retention policy).
	if _, err := s.store.PruneRuns(w.ID, storage.Retention-1); err != nil {
		s.logger.Warn("prune runs failed", "workspace_id", w.ID, "error", err)
	}

	now := time.Now().UTC()
	run := &model.Run{
		ID:          model.NewRunID(),
		WorkspaceID: w.ID,
		Kind:        spec.Kind,
		Argv:        argv,
		Status:      model.RunQueued,
		StartedAt:   now,
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
		Cwd:           "/",
		Env:           spec.Env,
		Timeout:       time.Duration(timeout) * time.Second,
		Context:       ctx,
		RunID:         run.ID,
		StdoutPath:    stdoutPath,
		StderrPath:    stderrPath,
		StdoutCap:     caps.RunLogBytes(),
		StderrCap:     caps.RunLogBytes(),
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
