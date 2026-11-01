package service

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"sandboxd/internal/executor"
	"sandboxd/internal/model"
)

// StartSessionSpec describes a terminal-session create request. Empty fields
// take the configured defaults; Cols/Rows are validated by the caller.
type StartSessionSpec struct {
	Shell string
	Cwd   string
	Cols  int
	Rows  int
	Env   map[string]string
	// TimeoutSec optionally requests a shorter absolute lifetime than the
	// configured maximum; it is clamped to sessions.lifetime_sec.
	TimeoutSec int
}

// liveSession is the in-memory handle for a running terminal session. The
// session goroutine (runSession) is the single writer of the terminal state;
// the API goroutines only append to stdin and nudge activity, so metadata
// races are avoided.
type liveSession struct {
	id       string
	wsID     string
	proc     executor.LiveProcess
	done     chan struct{}
	activity chan struct{}

	writeMu sync.Mutex // serializes control-protocol writes to stdin

	mu      sync.Mutex
	session *model.Session
	lastAct time.Time
	// lifetime is this session's absolute wall-clock ceiling; it is the
	// configured maximum unless the create request asked for a shorter one.
	lifetime time.Duration
	// pendingReason is set by an external end request (DELETE, timeout,
	// shutdown, suspend) and consumed by runSession once the process exits.
	pendingReason string
	ended         bool
}

// SessionCapError is returned when a workspace already has the maximum number
// of concurrent sessions.
func SessionCapError(max int) error {
	return model.Wrap(409, model.CodeTooManySessions, "workspace session limit reached",
		map[string]string{"max": itoaInt(max)})
}

// SessionEndedError is returned when input/signal/resize addresses a session
// whose record is retained but which has already reached a terminal state.
func SessionEndedError(status model.SessionStatus) error {
	return model.Wrap(409, model.CodeSessionEnded, "session has ended",
		map[string]string{"status": string(status)})
}

func itoaInt(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// StartSession creates and launches a terminal session in the workspace
// sandbox. The session runs a PTY relay helper under nsjail with the same
// identity, mounts and seccomp policy as any workload.
func (s *Service) StartSession(w *model.Workspace, spec StartSessionSpec) (*model.Session, error) {
	if err := s.ensureActive(w); err != nil {
		return nil, err
	}
	if s.countLiveSessions(w.ID) >= s.cfg.Sessions.MaxPerWorkspace {
		return nil, SessionCapError(s.cfg.Sessions.MaxPerWorkspace)
	}

	shell := spec.Shell
	if shell == "" {
		shell = s.cfg.Sessions.Shell
	}
	if !strings.HasPrefix(shell, "/") {
		return nil, model.Wrap(422, model.CodeUnprocessable, "shell must be an absolute path",
			map[string]string{"shell": shell})
	}

	// Resolve cwd with the workspace path-safety helpers. An escape is unsafe
	// (422); a path that does not exist or is not a directory is 422 too.
	cwd := spec.Cwd
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return nil, err
	}
	resolvedCwd := fs.Root()
	if strings.TrimSpace(cwd) != "" {
		resolvedCwd, err = fs.Resolve(cwd)
		if err != nil {
			return nil, err
		}
	}
	if fi, err := os.Stat(resolvedCwd); err != nil {
		if os.IsNotExist(err) {
			return nil, model.Wrap(422, model.CodeUnprocessable, "cwd does not exist",
				map[string]string{"cwd": cwd})
		}
		return nil, err
	} else if !fi.IsDir() {
		return nil, model.Wrap(422, model.CodeUnprocessable, "cwd is not a directory",
			map[string]string{"cwd": cwd})
	}

	sharedHost, privateHost, err := s.packageDirs(w)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	sess := &model.Session{
		ID:             model.NewSessionID(),
		WorkspaceID:    w.ID,
		Shell:          shell,
		Cwd:            cwd,
		Cols:           spec.Cols,
		Rows:           spec.Rows,
		Status:         model.SessionRunning,
		CreatedAt:      now,
		LastActivityAt: now,
		StartedAt:      now,
	}
	if err := s.store.CreateSession(w.ID, sess); err != nil {
		return nil, err
	}
	s.touchActivity(w.ID)

	req := executor.Request{
		Workspace:          w,
		WorkspaceRoot:      s.store.WorkspaceRoot(w.ID),
		Cwd:                cwd,
		Env:                spec.Env,
		PTY:                true,
		Shell:              shell,
		Cols:               spec.Cols,
		Rows:               spec.Rows,
		StdoutPath:         s.store.SessionOutputPath(w.ID, sess.ID),
		SessionOutputCap:   s.cfg.Sessions.OutputBytes(),
		SharedPackagesDir:  sharedHost,
		PrivatePackagesDir: privateHost,
	}

	lp, err := s.runner.Start(req)
	if err != nil {
		// No process was launched; leave no misleading record behind.
		_ = s.store.DeleteSession(w.ID, sess.ID)
		return nil, err
	}

	lifetime := s.cfg.Sessions.Lifetime()
	if spec.TimeoutSec > 0 && time.Duration(spec.TimeoutSec)*time.Second < lifetime {
		lifetime = time.Duration(spec.TimeoutSec) * time.Second
	}

	ls := &liveSession{
		id:       sess.ID,
		wsID:     w.ID,
		proc:     lp,
		done:     make(chan struct{}),
		activity: make(chan struct{}, 1),
		session:  sess,
		lastAct:  now,
		lifetime: lifetime,
	}
	s.mu.Lock()
	s.sessions[sess.ID] = ls
	s.mu.Unlock()

	go s.runSession(ls, lp)

	return cloneSession(sess), nil
}

// runSession owns the session's lifecycle: it waits for the process, records
// the terminal transition exactly once, and unregisters the session.
func (s *Service) runSession(ls *liveSession, lp executor.LiveProcess) {
	defer close(ls.done)
	defer func() {
		s.mu.Lock()
		delete(s.sessions, ls.id)
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchSession(ctx, ls)

	res, err := lp.Wait()

	// Capture the persisted output size before deciding the state.
	outputBytes, truncated := s.sessionOutputStats(ls.wsID, ls.id)

	status := model.SessionExited
	reason := model.SessionReasonShellExited
	var exitCode *int
	switch {
	case err != nil:
		// The session never produced a shell (e.g. nsjail could not set up the
		// sandbox). Record it as killed with the sandbox reason, never as a
		// clean shell exit.
		status = model.SessionKilled
		reason = model.SessionReasonSandboxUnavailable
	case ls.pendingEnded():
		p := ls.pending()
		switch p {
		case model.SessionReasonIdleTimeout, model.SessionReasonLifetime:
			status = model.SessionTimedOut
		default:
			status = model.SessionKilled
		}
		reason = p
	case res.Killed:
		status = model.SessionKilled
		reason = model.SessionReasonUserClosed
	default:
		status = model.SessionExited
		reason = model.SessionReasonShellExited
		code := res.ExitCode
		exitCode = &code
	}

	s.finishSession(ls, status, reason, exitCode, outputBytes, truncated)
}

// watchSession enforces the idle timeout and the absolute lifetime. "Activity"
// is a client input/signal/resize or growth of the PTY output log, so a shell
// producing output (a long build, `tail -f`) is not reaped as idle. The
// absolute lifetime is independent of activity. It ends the session by killing
// the process and recording the pending reason, which runSession then converts
// into the terminal state.
func (s *Service) watchSession(ctx context.Context, ls *liveSession) {
	lifetime := ls.lifetime
	idle := s.cfg.Sessions.IdleTimeout()
	start := time.Now()
	lastSize := s.sessionOutputSize(ls.wsID, ls.id)
	for {
		ls.mu.Lock()
		last := ls.lastAct
		ls.mu.Unlock()

		// Output growth counts as activity: fold it into last activity before
		// deciding the idle state.
		if size := s.sessionOutputSize(ls.wsID, ls.id); size > lastSize {
			lastSize = size
			now := time.Now()
			ls.mu.Lock()
			ls.lastAct = now
			ls.mu.Unlock()
			last = now
		}

		idleRemain := idle - time.Since(last)
		lifeRemain := lifetime - time.Since(start)
		var d time.Duration
		switch {
		case lifeRemain <= 0:
			s.endSession(ls, model.SessionReasonLifetime)
			return
		case idleRemain <= 0:
			s.endSession(ls, model.SessionReasonIdleTimeout)
			return
		case idleRemain < lifeRemain:
			d = idleRemain
		default:
			d = lifeRemain
		}
		// Poll at least a few times per idle window so output growth is noticed
		// promptly, but never sleep longer than half a second on a short idle
		// window.
		if d > 500*time.Millisecond {
			d = 500 * time.Millisecond
		}

		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-ls.done:
			timer.Stop()
			return
		case <-ls.activity:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// sessionOutputSize returns the current persisted output size in bytes.
func (s *Service) sessionOutputSize(wsID, sessionID string) int64 {
	fi, err := os.Stat(s.store.SessionOutputPath(wsID, sessionID))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// endSession requests that a session end with the given reason and kills its
// process group. The terminal state is recorded by runSession once the process
// has actually exited. Idempotent: the first reason wins.
func (s *Service) endSession(ls *liveSession, reason string) {
	ls.mu.Lock()
	if ls.ended {
		ls.mu.Unlock()
		return
	}
	if ls.pendingReason == "" {
		ls.pendingReason = reason
	}
	ls.mu.Unlock()
	ls.proc.Kill()
}

// finishSession records the terminal transition once and persists it. It holds
// the session lock across the save so a concurrent activity update (input,
// signal, resize) can never persist a stale running status after the terminal
// transition: markActivity re-checks ls.ended under the same lock.
func (s *Service) finishSession(ls *liveSession, status model.SessionStatus, reason string, exitCode *int, outputBytes int64, truncated bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.ended {
		return
	}
	ls.ended = true
	sess := ls.session
	sess.Status = status
	sess.Reason = reason
	sess.EndedAt = time.Now().UTC()
	sess.OutputBytes = outputBytes
	sess.OutputTruncated = truncated || sess.OutputTruncated
	if exitCode != nil {
		sess.SetExitCode(*exitCode)
	}
	if err := s.store.SaveSession(ls.wsID, sess); err != nil {
		s.logger.Warn("save session finish failed", "session_id", ls.id, "error", err)
	}
}

func (ls *liveSession) pending() string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.pendingReason
}

func (ls *liveSession) pendingEnded() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.pendingReason != ""
}

// markActivity resets the idle timer and refreshes last_activity_at. It holds
// the session lock across the save so it cannot clobber a terminal transition
// recorded by finishSession (which holds the same lock).
func (s *Service) markActivity(ls *liveSession) {
	now := time.Now().UTC()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.lastAct = now
	ls.session.LastActivityAt = now
	if ls.ended {
		return
	}
	select {
	case ls.activity <- struct{}{}:
	default:
	}
	if err := s.store.SaveSession(ls.wsID, ls.session); err != nil {
		s.logger.Warn("save session activity failed", "session_id", ls.id, "error", err)
	}
}

// sessionOutputStats returns the persisted output size and whether the cap was
// hit.
func (s *Service) sessionOutputStats(wsID, sessionID string) (int64, bool) {
	fi, err := os.Stat(s.store.SessionOutputPath(wsID, sessionID))
	if err != nil {
		return 0, false
	}
	capped := s.cfg.Sessions.OutputBytes()
	return fi.Size(), capped > 0 && fi.Size() >= capped
}

// sessionFor loads an owned session record and checks it is live in memory.
func (s *Service) liveSessionFor(wsID, sessionID string) (*liveSession, *model.Session, error) {
	sess, err := s.store.GetSession(wsID, sessionID)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	ls := s.sessions[sessionID]
	s.mu.Unlock()
	return ls, sess, nil
}

// GetSession returns a session record.
func (s *Service) GetSession(w *model.Workspace, sessionID string) (*model.Session, error) {
	return s.store.GetSession(w.ID, sessionID)
}

// Input appends data to the session's PTY.
func (s *Service) Input(w *model.Workspace, sessionID, data string) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	limit := s.cfg.Sessions.MaxInputBytes()
	if limit > 0 && int64(len(data)) > limit {
		return model.Wrap(413, model.CodePayloadTooLarge, "input exceeds maximum allowed size",
			map[string]string{"limit_bytes": itoaInt(int(limit))})
	}
	ls, sess, err := s.liveSessionFor(w.ID, sessionID)
	if err != nil {
		return err
	}
	if ls == nil {
		return SessionEndedError(sess.Status)
	}
	if err := s.sendControl(ls, map[string]any{"type": "input", "data": data}); err != nil {
		return model.New(409, model.CodeSessionEnded, "session input stream is closed")
	}
	s.markActivity(ls)
	return nil
}

// Signal delivers a signal to the session's foreground process group.
func (s *Service) Signal(w *model.Workspace, sessionID, sig string) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	ls, sess, err := s.liveSessionFor(w.ID, sessionID)
	if err != nil {
		return err
	}
	if ls == nil {
		return SessionEndedError(sess.Status)
	}
	if err := s.sendControl(ls, map[string]any{"type": "signal", "signal": sig}); err != nil {
		return model.New(409, model.CodeSessionEnded, "session signal stream is closed")
	}
	s.markActivity(ls)
	return nil
}

// Resize changes the PTY window size.
func (s *Service) Resize(w *model.Workspace, sessionID string, cols, rows int) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	ls, sess, err := s.liveSessionFor(w.ID, sessionID)
	if err != nil {
		return err
	}
	if ls == nil {
		return SessionEndedError(sess.Status)
	}
	if err := s.sendControl(ls, map[string]any{"type": "resize", "cols": cols, "rows": rows}); err != nil {
		return model.New(409, model.CodeSessionEnded, "session control stream is closed")
	}
	s.markActivity(ls)
	return nil
}

// sendControl writes one line-delimited JSON control message to the helper.
func (s *Service) sendControl(ls *liveSession, msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	ls.writeMu.Lock()
	defer ls.writeMu.Unlock()
	return ls.proc.Write(b)
}

// CloseSession ends a live session gracefully (SIGHUP the shell, then SIGKILL
// after the grace period) and waits briefly for the terminal state to be
// recorded. It is idempotent: closing an ended session reports changed=false
// and leaves the record untouched. The record and output log are retained so
// the session stays inspectable; they are removed with the workspace.
func (s *Service) CloseSession(w *model.Workspace, sessionID string) (*model.Session, bool, error) {
	sess, err := s.store.GetSession(w.ID, sessionID)
	if err != nil {
		return nil, false, err
	}
	if sess.Status.Terminal() {
		return sess, false, nil
	}
	s.mu.Lock()
	ls := s.sessions[sessionID]
	s.mu.Unlock()
	if ls == nil {
		// Recorded running but no live process (e.g. a prior crash): mark it
		// honestly instead of pretending it is still running.
		sess.Status = model.SessionKilled
		sess.Reason = model.SessionReasonInterrupted
		sess.EndedAt = time.Now().UTC()
		if err := s.store.SaveSession(w.ID, sess); err != nil {
			return nil, false, err
		}
		return sess, true, nil
	}

	// Graceful close: record the pending reason, then ask the helper to SIGHUP
	// the shell and exit. If the shell ignores SIGHUP or a stray child holds
	// the PTY, force-kill the process group after the grace period.
	ls.mu.Lock()
	if ls.pendingReason == "" {
		ls.pendingReason = model.SessionReasonUserClosed
	}
	ls.mu.Unlock()
	_ = s.sendControl(ls, map[string]any{"type": "close"})
	select {
	case <-ls.done:
	case <-time.After(s.cfg.Sessions.KillGrace() + time.Second):
		ls.proc.Kill()
		select {
		case <-ls.done:
		case <-time.After(2 * time.Second):
		}
	}
	updated, err := s.store.GetSession(w.ID, sessionID)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// SessionLogChunk is a polled slice of session output.
type SessionLogChunk struct {
	Data      []byte
	Offset    int64
	Next      int64
	EOF       bool
	Truncated bool
}

// ReadSessionOutput reads up to limit bytes of a session's PTY output starting
// at offset, with the same byte-offset semantics as the run-log endpoint. The
// output stays readable after a natural exit or an idle/lifetime timeout, so a
// client can still capture the final bytes. A session ended by DELETE (status
// killed, reason user_closed) is a tombstone: its output reports the documented
// session_ended error, because the session was explicitly removed.
func (s *Service) ReadSessionOutput(w *model.Workspace, sessionID string, offset, limit int64) (SessionLogChunk, error) {
	sess, err := s.store.GetSession(w.ID, sessionID)
	if err != nil {
		return SessionLogChunk{}, err
	}
	if sess.Status == model.SessionKilled && sess.Reason == model.SessionReasonUserClosed {
		return SessionLogChunk{}, SessionEndedError(sess.Status)
	}
	path := s.store.SessionOutputPath(w.ID, sessionID)
	fh, err := os.Open(path)
	if err != nil {
		return SessionLogChunk{}, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return SessionLogChunk{}, err
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
			return SessionLogChunk{}, err
		}
	}
	next := offset + toRead
	return SessionLogChunk{
		Data:      buf,
		Offset:    offset,
		Next:      next,
		EOF:       next >= size,
		Truncated: sess.OutputTruncated,
	}, nil
}

// SessionFilter selects a subset of a workspace's sessions.
type SessionFilter struct {
	Statuses []model.SessionStatus
	Limit    int
	Offset   int
}

// ListSessions returns sessions matching the filter, newest first, plus the
// total matching before pagination (mirrors the runs list).
func (s *Service) ListSessions(w *model.Workspace, f SessionFilter) ([]*model.Session, int, error) {
	all, err := s.store.ListSessions(w.ID)
	if err != nil {
		return nil, 0, err
	}
	matched := make([]*model.Session, 0, len(all))
	for _, sess := range all {
		if len(f.Statuses) > 0 && !containsSessionStatus(f.Statuses, sess.Status) {
			continue
		}
		matched = append(matched, sess)
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

func containsSessionStatus(statuses []model.SessionStatus, st model.SessionStatus) bool {
	for _, s := range statuses {
		if s == st {
			return true
		}
	}
	return false
}

// countLiveSessions returns the number of running sessions tracked for a
// workspace.
func (s *Service) countLiveSessions(wsID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ls := range s.sessions {
		if ls.wsID == wsID {
			n++
		}
	}
	return n
}

// StopWorkspaceSessions ends every live session of a workspace with the given
// reason and waits (bounded) for the processes to die. It returns the IDs of
// the sessions signalled.
func (s *Service) StopWorkspaceSessions(wsID, reason string) []string {
	s.mu.Lock()
	var victims []*liveSession
	for _, ls := range s.sessions {
		if ls.wsID == wsID {
			victims = append(victims, ls)
		}
	}
	s.mu.Unlock()
	for _, ls := range victims {
		s.endSession(ls, reason)
	}
	deadline := time.After(s.cfg.Sessions.KillGrace() + 2*time.Second)
	for _, ls := range victims {
		select {
		case <-ls.done:
		case <-deadline:
			break
		}
	}
	ids := make([]string, 0, len(victims))
	for _, ls := range victims {
		ids = append(ids, ls.id)
	}
	return ids
}

// WaitSessions blocks until no session is executing. Used by tests and shutdown.
func (s *Service) WaitSessions() {
	s.mu.Lock()
	var pending []chan struct{}
	for _, ls := range s.sessions {
		pending = append(pending, ls.done)
	}
	s.mu.Unlock()
	for _, done := range pending {
		<-done
	}
}

// RecoverSessions reconciles session state after a restart. A session persisted
// running has no surviving process (no sandbox process outlives the service),
// so it is moved to killed with reason interrupted rather than being left to
// look live. Returns the number recovered.
func (s *Service) RecoverSessions() (int, error) {
	workspaces, err := s.store.ListAllWorkspaces()
	if err != nil {
		return 0, err
	}
	recovered := 0
	now := time.Now().UTC()
	for _, w := range workspaces {
		sessions, err := s.store.ListSessions(w.ID)
		if err != nil {
			s.logger.Warn("recover sessions: list failed", "workspace_id", w.ID, "error", err)
			continue
		}
		for _, sess := range sessions {
			if sess.Status.Terminal() {
				continue
			}
			sess.Status = model.SessionKilled
			sess.Reason = model.SessionReasonInterrupted
			if sess.EndedAt.IsZero() {
				sess.EndedAt = now
			}
			if err := s.store.SaveSession(w.ID, sess); err != nil {
				s.logger.Warn("recover session: save failed", "session_id", sess.ID, "error", err)
				continue
			}
			recovered++
		}
	}
	return recovered, nil
}

// ShutdownSessions terminates every live session and waits for them to drain,
// bounded by deadline. Returns false if the deadline elapsed with sessions
// still active.
func (s *Service) ShutdownSessions(deadline time.Duration) bool {
	s.mu.Lock()
	var pending []*liveSession
	for _, ls := range s.sessions {
		pending = append(pending, ls)
	}
	s.mu.Unlock()
	for _, ls := range pending {
		s.endSession(ls, model.SessionReasonServiceShutdown)
	}
	timer := time.After(deadline)
	for _, ls := range pending {
		select {
		case <-ls.done:
		case <-timer:
			return false
		}
	}
	return true
}

// cloneSession returns a deep copy so a caller's snapshot is never mutated by
// the session goroutine.
func cloneSession(sess *model.Session) *model.Session {
	cp := *sess
	if sess.ExitCode != nil {
		code := *sess.ExitCode
		cp.ExitCode = &code
	}
	return &cp
}
