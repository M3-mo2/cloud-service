package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"time"

	"sandboxd/internal/model"
)

// Process is one live process belonging to a workspace, as reported by the
// process inventory. A process started inside a session (or any process that
// left the run/session process group) is still attributed by the sandbox tag
// inherited from its origin; a process inside a workspace's sandbox cgroup that
// carries no source tag is reported honestly as kind "detached".
type Process struct {
	// ID is a stable, opaque identifier for the process. It is a hash of
	// (workspace, host pid, kernel start time), so it distinguishes two
	// processes that reuse the same pid across time and stays stable for the
	// life of the process. It is what the single-process stop endpoint accepts.
	ID string
	// PID is the host-side process id.
	PID int
	// PPID is the host-side parent pid.
	PPID int
	// Argv is the command line.
	Argv []string
	// Kind is "run", "session" or "detached".
	Kind string
	// SourceID is the run id / session id the process belongs to, and is empty
	// for a detached process.
	SourceID string
	// StartedAt is when the process appeared (derived from its kernel start
	// time).
	StartedAt time.Time
}

// ProcessID derives the stable id for (workspace, pid, startTicks). The id is
// content-addressed so a restarted daemon with recycled pids does not alias two
// different processes.
func ProcessID(workspaceID string, pid int, startTicks uint64) string {
	h := sha256.New()
	h.Write([]byte(workspaceID))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(pid)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatUint(startTicks, 10)))
	return "proc_" + hex.EncodeToString(h.Sum(nil))[:24]
}

// ReapOrphans kills and removes every orphaned sandbox cgroup left by a hard
// (SIGKILL) daemon death, returning how many it reaped. Runs it at startup,
// alongside run/session recovery, because no sandbox process survives a restart
// and an orphan would otherwise keep running forever (or for its time_limit).
func (s *Service) ReapOrphans() int {
	return s.runner.ReapOrphans()
}

// ListProcesses returns every live process belonging to a workspace, oldest
// first. Attribution uses the per-jail cgroup plus the tag the executor injects
// into every sandbox (workspace, kind, source). A process that is inside a
// workspace sandbox but carries no source tag is reported as "detached" — it is
// never hidden. Isolation holds: a workspace's inventory lists only processes
// tagged for that workspace; an empty inventory is an honest empty list.
func (s *Service) ListProcesses(w *model.Workspace) ([]Process, error) {
	all := s.runner.ListProcesses()
	out := make([]Process, 0, len(all))
	for _, p := range all {
		if p.WorkspaceID != w.ID {
			continue
		}
		kind := p.Kind
		if kind == "" {
			kind = "detached"
		}
		out = append(out, Process{
			ID:        ProcessID(w.ID, p.PID, p.StartTicks),
			PID:       p.PID,
			PPID:      p.PPID,
			Argv:      argvOrEmpty(p.Argv),
			Kind:      kind,
			SourceID:  p.SourceID,
			StartedAt: p.StartedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].PID < out[j].PID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

func argvOrEmpty(argv []string) []string {
	if argv == nil {
		return []string{}
	}
	return argv
}

// StopProcessResult is the outcome of a single-process stop.
type StopProcessResult struct {
	ID      string
	Found   bool
	Stopped bool
}

// StopProcess stops the process tree of one process in the workspace, addressed
// by its stable id. It is idempotent: a process that has already exited is
// reported as found-but-already-gone (Stopped false) rather than an error. An
// id that does not name a process in this workspace is not found (the caller
// maps that to 404). Because the pid's sandbox cgroup is used, the whole tree is
// stopped, including a process that escaped its process group with setsid().
func (s *Service) StopProcess(w *model.Workspace, processID string) (StopProcessResult, error) {
	procs, err := s.ListProcesses(w)
	if err != nil {
		return StopProcessResult{}, err
	}
	for _, p := range procs {
		if p.ID != processID {
			continue
		}
		stopped := s.runner.KillProcessTree(p.PID)
		return StopProcessResult{ID: p.ID, Found: true, Stopped: stopped}, nil
	}
	return StopProcessResult{}, nil
}

// StopDetachedProcesses stops every process in the workspace whose kind is
// "detached" (a process with no run/session attribution), returning the stopped
// process ids. It is used by the workspace "stop everything" control so a
// detached bot/worker the user started in a session is actually stopped. Runs
// and sessions are stopped through their own paths, so this only handles the
// processes that would otherwise be unreachable.
func (s *Service) StopDetachedProcesses(w *model.Workspace) []string {
	procs, err := s.ListProcesses(w)
	if err != nil {
		return nil
	}
	stopped := make([]string, 0, len(procs))
	for _, p := range procs {
		if p.Kind != "detached" {
			continue
		}
		if s.runner.KillProcessTree(p.PID) {
			stopped = append(stopped, p.ID)
		}
	}
	return stopped
}
