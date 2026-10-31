package executor

import (
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

	mu      sync.Mutex
	killed  map[string]bool
	lastReq *Request
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

func appendFile(path, data string) error {
	if data == "" {
		return nil
	}
	return writeAppend(path, strings.NewReader(data))
}
