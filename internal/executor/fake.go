package executor

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// FakeRunner is an in-memory Runner for tests. It does not invoke nsjail.
type FakeRunner struct {
	AvailableFlag bool
	CgroupsFlag   bool
	Stdout        string
	Stderr        string
	ExitCode      int
	TimedOut      bool
	Err           error
	Delay         time.Duration

	// Sessions: when LiveOutput is set, Start writes it to the session's
	// StdoutPath and stays alive until Kill/Close (or the lifetime delay).
	StartErr   error
	LiveErr    error
	LiveOutput string
	LiveDelay  time.Duration

	mu       sync.Mutex
	killed   map[string]bool
	lastReq  *Request
	live     map[int]*fakeLive
	lastLive *Request
	nextPID  int

	// Processes, when set, is returned by ListProcesses so the API/service layer
	// can be tested without a real nsjail. killedPids tracks KillProcessTree;
	// KilledProcesses records the pids in order.
	Processes       []ProcessInfo
	KilledProcesses []int
	killedPids      map[int]bool
	reaped          int
}

// fakeLive is an in-memory LiveProcess for tests.
type fakeLive struct {
	owner  *FakeRunner
	pid    int
	stdin  *fakeStdin
	done   chan struct{}
	res    Result
	err    error
	closed sync.Once
	killed bool
}

type fakeStdin struct {
	mu     sync.Mutex
	buf    []byte
	owner  *fakeLive
	closed bool
}

func (s *fakeStdin) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, errors.New("stdin closed")
	}
	s.buf = append(s.buf, p...)
	s.mu.Unlock()
	// Model the helper: a structured {"type":"close"} ends the process.
	if strings.Contains(string(p), `"type":"close"`) {
		if s.owner != nil {
			s.owner.finish(Result{ExitCode: 0, Streamed: true})
		}
	}
	return len(p), nil
}

func (s *fakeStdin) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Written returns the bytes written to the fake session's stdin so far.
func (f *FakeRunner) Written(pid int) []byte {
	f.mu.Lock()
	lp := f.live[pid]
	f.mu.Unlock()
	if lp == nil {
		return nil
	}
	lp.stdin.mu.Lock()
	defer lp.stdin.mu.Unlock()
	return append([]byte(nil), lp.stdin.buf...)
}

// LastLiveRequest returns the most recent Start request.
func (f *FakeRunner) LastLiveRequest() *Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastLive
}

// LastLivePID returns the pid of the most recent Start.
func (f *FakeRunner) LastLivePID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastLive == nil {
		return 0
	}
	return f.nextPID
}

// LiveCount returns the number of fake interactive processes still alive.
func (f *FakeRunner) LiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

// FakeRunnerInterface guards against accidental signature drift.
var _ Runner = (*FakeRunner)(nil)

// Available reports the configured availability.
func (f *FakeRunner) Available() bool { return f.AvailableFlag }

// CgroupsUsable reports the configured cgroup availability.
func (f *FakeRunner) CgroupsUsable() bool { return f.CgroupsFlag }

// LastRequest returns the most recent request.
func (f *FakeRunner) LastRequest() *Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

// Run returns the configured canned result.
func (f *FakeRunner) Run(req Request) (Result, error) {
	f.mu.Lock()
	cp := req
	f.lastReq = &cp
	if f.killed == nil {
		f.killed = map[string]bool{}
	}
	f.mu.Unlock()

	if f.Delay > 0 {
		time.Sleep(f.Delay)
	}
	if req.RunID != "" {
		// Simulate streaming to disk for async runs.
		stdout := f.Stdout
		stderr := f.Stderr
		f.mu.Lock()
		killed := f.killed[req.RunID]
		f.mu.Unlock()
		if killed {
			return Result{ExitCode: -1, Streamed: true, Killed: true}, nil
		}
		if req.StdoutPath != "" {
			_ = appendFile(req.StdoutPath, stdout)
		}
		if req.StderrPath != "" {
			_ = appendFile(req.StderrPath, stderr)
		}
		res := Result{ExitCode: f.ExitCode, Streamed: true, TimedOut: f.TimedOut}
		if f.TimedOut {
			res.ExitCode = 124
		}
		return res, f.Err
	}
	truncated := false
	if req.StdoutCap > 0 && int64(len(f.Stdout)) > req.StdoutCap {
		f.Stdout = f.Stdout[:req.StdoutCap]
		truncated = true
	}
	if req.StderrCap > 0 && int64(len(f.Stderr)) > req.StderrCap {
		f.Stderr = f.Stderr[:req.StderrCap]
		truncated = true
	}
	res := Result{
		ExitCode:  f.ExitCode,
		Stdout:    []byte(f.Stdout),
		Stderr:    []byte(f.Stderr),
		Truncated: truncated,
		TimedOut:  f.TimedOut,
	}
	if f.TimedOut {
		res.ExitCode = 124
	}
	return res, f.Err
}

// Kill marks a run as killed and reports whether it was known.
func (f *FakeRunner) Kill(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.killed == nil {
		f.killed = map[string]bool{}
	}
	already := f.killed[runID]
	f.killed[runID] = true
	return !already
}

// ListProcesses returns the configured processes, minus any whose pid was
// stopped through KillProcessTree. It models the real inventory without a jail.
func (f *FakeRunner) ListProcesses() []ProcessInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ProcessInfo, 0, len(f.Processes))
	for _, p := range f.Processes {
		if f.killedPids[p.PID] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// KillProcessTree records the pid as stopped and returns true (it models a
// successful cgroup/process-group kill).
func (f *FakeRunner) KillProcessTree(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.killedPids == nil {
		f.killedPids = map[int]bool{}
	}
	f.killedPids[pid] = true
	f.KilledProcesses = append(f.KilledProcesses, pid)
	return true
}

// ReapOrphans reports and clears the configured orphan count.
func (f *FakeRunner) ReapOrphans() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.reaped
	f.reaped = 0
	return n
}

// SetOrphans configures how many orphans ReapOrphans reports.
func (f *FakeRunner) SetOrphans(n int) {
	f.mu.Lock()
	f.reaped = n
	f.mu.Unlock()
}

// SetProcesses replaces the inventory the fake returns.
func (f *FakeRunner) SetProcesses(procs []ProcessInfo) {
	f.mu.Lock()
	f.Processes = procs
	f.killedPids = map[int]bool{}
	f.KilledProcesses = nil
	f.mu.Unlock()
}

// Start launches an in-memory interactive process. It writes the configured
// LiveOutput to the request's StdoutPath and blocks until the process is killed,
// its stdin is closed, or LiveDelay elapses -- modelling a long-lived session
// without nsjail.
func (f *FakeRunner) Start(req Request) (LiveProcess, error) {
	f.mu.Lock()
	f.lastLive = &req
	if f.StartErr != nil {
		err := f.StartErr
		f.mu.Unlock()
		return nil, err
	}
	if f.live == nil {
		f.live = map[int]*fakeLive{}
	}
	f.nextPID++
	pid := f.nextPID
	lp := &fakeLive{
		owner: f,
		pid:   pid,
		stdin: &fakeStdin{},
		done:  make(chan struct{}),
		res:   Result{ExitCode: f.ExitCode, Streamed: true},
		err:   f.LiveErr,
	}
	lp.stdin.owner = lp
	f.live[pid] = lp
	out := f.LiveOutput
	delay := f.LiveDelay
	liveErr := f.LiveErr
	f.mu.Unlock()

	if req.StdoutPath != "" {
		_ = appendFile(req.StdoutPath, out)
	}
	go func() {
		if liveErr != nil {
			// Model a relay that dies at startup (e.g. no PTY): it ends at once
			// with its error, so the service can record a failed session.
			lp.finish(Result{ExitCode: f.ExitCode, Streamed: true})
			return
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
				lp.finish(Result{ExitCode: f.ExitCode, Streamed: true})
			case <-lp.done:
			}
			return
		}
		<-lp.done
	}()
	return lp, nil
}

// finish closes the fake process exactly once.
func (l *fakeLive) finish(res Result) {
	l.owner.mu.Lock()
	delete(l.owner.live, l.pid)
	l.owner.mu.Unlock()
	l.closed.Do(func() {
		l.res = res
		close(l.done)
	})
}

// Write appends to the fake process's stdin.
func (l *fakeLive) Write(p []byte) error {
	_, err := l.stdin.Write(p)
	return err
}

// Close ends the fake process like a session leader losing its terminal.
func (l *fakeLive) Close() error {
	_ = l.stdin.Close()
	l.finish(Result{ExitCode: 0, Streamed: true})
	return nil
}

// Pid returns the fake process id.
func (l *fakeLive) Pid() int { return l.pid }

// Kill ends the fake process as killed.
func (l *fakeLive) Kill() bool {
	l.owner.mu.Lock()
	known := l.owner.live[l.pid] != nil
	l.owner.mu.Unlock()
	if !known {
		return false
	}
	l.finish(Result{ExitCode: 137, Killed: true, Streamed: true})
	return true
}

// Wait blocks until the fake process finishes.
func (l *fakeLive) Wait() (Result, error) {
	<-l.done
	return l.res, l.err
}

func appendFile(path, data string) error {
	if data == "" {
		return nil
	}
	return writeAppend(path, strings.NewReader(data))
}
