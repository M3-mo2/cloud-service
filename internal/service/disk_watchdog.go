package service

import (
	"time"
)

// Disk counter watchdog (the SAMPLED fallback).
//
// When the FUSE backend is active the disk allowance is enforced by the
// filesystem boundary itself and no watchdog is needed. When it is NOT (no
// /dev/fuse, missing tools, or disk.backend=counter), the engine's used-bytes
// counter is promoted to an authority over RUNNING processes: this watchdog
// periodically rechecks every active run's workspace usage and stops the run's
// process tree once the workspace directory exceeds its allowance.
//
// This is REAL but SAMPLED. Unlike the kernel boundary it does not refuse the
// write that crosses the allowance; it notices afterwards and terminates the
// offender. The overshoot is bounded by how much a run can write between two
// polls. The measured overshoot is recorded in docs/DECISIONS.md.
//
// The watchdog never runs for a FUSE-backed workspace (the kernel already
// refuses the write), so it adds no cost to the default configuration.

// startDiskWatchdog launches the counter watchdog once. It is a no-op when the
// effective mechanism is not the counter.
func (s *Service) startDiskWatchdog() {
	if s.diskWatchdogOnce {
		return
	}
	s.diskWatchdogOnce = true
	if !s.counterMode() {
		return
	}
	poll := time.Duration(s.cfg.Disk.CounterPollMS) * time.Millisecond
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	go func() {
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-s.diskDone:
				return
			case <-t.C:
				s.checkActiveRunQuotas()
			}
		}
	}()
}

// counterMode reports whether the disk allowance is enforced by the counter
// watchdog (rather than a FUSE filesystem).
func (s *Service) counterMode() bool {
	enabled, mechanism, _ := s.DiskStatus()
	return enabled && mechanism == "counter"
}

// checkActiveRunQuotas audits every active run's workspace usage and stops any
// run whose workspace has exceeded its allowance. A workspace can host several
// runs; each is checked against the same limit, and the first over-limit run
// stops only itself (the workload that wrote the bytes).
func (s *Service) checkActiveRunQuotas() {
	s.mu.Lock()
	active := make([]*runningRun, 0, len(s.runs))
	for _, rr := range s.runs {
		active = append(active, rr)
	}
	s.mu.Unlock()
	// Cache per-workspace usage so N runs in one workspace trigger one walk.
	usage := map[string]int64{}
	limit := map[string]int64{}
	for _, rr := range active {
		if _, seen := usage[rr.wsID]; !seen {
			used, lim := s.workspaceDiskUsage(rr.wsID)
			usage[rr.wsID] = used
			limit[rr.wsID] = lim
		}
		if limit[rr.wsID] > 0 && usage[rr.wsID] > limit[rr.wsID] {
			s.logger.Warn("workspace disk allowance exceeded; stopping run",
				"workspace_id", rr.wsID, "run_id", rr.runID,
				"used_bytes", usage[rr.wsID], "limit_bytes", limit[rr.wsID])
			if rr.cancel != nil {
				rr.cancel()
			}
		}
	}
}

// workspaceDiskUsage returns the workspace's tracked-and-measured usage and its
// allowance in bytes. It measures from the filesystem so the watchdog sees
// bytes written by a bypassing workload (dd, a direct write), which the API-only
// counter would not.
func (s *Service) workspaceDiskUsage(wsID string) (used, limit int64) {
	w, err := s.store.GetWorkspace(wsID)
	if err != nil {
		return 0, 0
	}
	limit = int64(w.Limits.DiskMB) * 1024 * 1024
	fs, err := s.WorkspaceFs(w)
	if err != nil {
		return 0, limit
	}
	if size, complete, err := fs.DiskUsageBounded(usageWalkEntries); err == nil && complete {
		return size, limit
	}
	return fs.Used(), limit
}

// stopDiskWatchdog stops the watchdog goroutine (idempotent).
func (s *Service) stopDiskWatchdog() {
	if s.diskDone == nil {
		return
	}
	select {
	case <-s.diskDone:
	default:
		close(s.diskDone)
	}
}
