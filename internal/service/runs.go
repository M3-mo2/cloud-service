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
	timeout := spec.Timeout
	if timeout <= 0 || timeout > w.Limits.MaxWallTimeSec {
		timeout = w.Limits.MaxWallTimeSec
	}
	argv := spec.Argv
	if spec.Kind == model.RunKindFile {
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

	stdoutPath := s.store.RunLogPath(w.ID, run.ID, model.StreamStdout)
	stderrPath := s.store.RunLogPath(w.ID, run.ID, model.StreamStderr)
	caps := s.cfg.Caps

	req := executor.Request{
		Workspace:     w,
		WorkspaceRoot: s.store.WorkspaceRoot(w.ID),
		Argv:          argv,
		Cwd:           "/",
		Env:           spec.Env,
		Timeout:       time.Duration(timeout) * time.Second,
		RunID:         run.ID,
		StdoutPath:    stdoutPath,
		StderrPath:    stderrPath,
		StdoutCap:     caps.RunLogBytes(),
		StderrCap:     caps.RunLogBytes(),
	}

	ctx, cancel := context.WithCancel(context.Background())
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
	return s.runner.Kill(runID), run.Status, nil
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

// GetRun returns a run for a workspace.
func (s *Service) GetRun(w *model.Workspace, runID string) (*model.Run, error) {
	return s.store.GetRun(w.ID, runID)
}

// ListRuns lists runs for a workspace.
func (s *Service) ListRuns(w *model.Workspace) ([]*model.Run, error) {
	return s.store.ListRuns(w.ID)
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
