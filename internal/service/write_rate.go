package service

import (
	"time"

	"sandboxd/internal/metrics"
	"sandboxd/internal/model"
)

// The per-workspace write-RATE ceiling (docs/DECISIONS.md ADR 95).
//
// WHY NOT A KERNEL LIMIT. `io.max` cannot limit write speed on this host
// (measured: a writer started with CLONE_INTO_CGROUP writing 64 MB with
// oflag=direct,conv=fsync ran at 3.7 GB/s with no cap and 3.6 GB/s with
// `io.max wbps=5242880`, and the cgroup's `io.stat` stayed at 0 bytes in both
// runs; the image and `/` are on ZFS and the bio is generated outside our
// cgroup). So the ceiling is SAMPLED, exactly like the disk counter watchdog.
//
// WHY IT IS NOT A TOKEN BUCKET IN OUR WRITE PATH. The engine's Go code writes
// API uploads and archive extractions, but a tenant's OWN process (dd, git
// clone, a build) writes straight through FUSE and never touches our code. A
// bucket in Fs.Write would throttle API uploads and nothing else.
//
// WHAT IT IS. The workspace's real disk growth (one statfs, already sampled by
// the metrics sampler every tick) covers writes from ANY source. The watchdog
// therefore rides that sampler's tick (it starts no timer of its own), computes
// bytes/second from the growth, and — after the configured number of
// CONSECUTIVE over-limit samples — stops the offending run(s) with the distinct
// `rate_limited` outcome. The engine's OWN write path (an API upload, an
// archive extraction) has no run to stop, so it consults the same watchdog's
// flag and refuses to keep writing while the workspace is over its ceiling.
// This is honest: the ceiling is the sampled growth; the two enforcement paths
// (cancel a run; refuse an engine write) are how each source is stopped.
//
// The overshoot is bounded by (write_rate x sample_interval) and is STATED AS A
// NUMBER for the shipped defaults in docs/DECISIONS.md; it is NEVER presented as
// equivalent to a kernel boundary.

// rateState is the per-workspace sampled-rate state. It is held in memory and
// is never persisted: after a restart the first tick simply has no baseline.
type rateState struct {
	hasBaseline bool
	prevUsed    int64
	// prevMono carries a monotonic reading (time.Now() includes one), so the
	// elapsed time is measured monotonically and a wall-clock jump cannot
	// fabricate a spike.
	prevMono time.Time
	// over counts CONSECUTIVE over-limit samples.
	over int
	// limitedUntil is when the engine's own write path stops refusing this
	// workspace's writes. It is advanced every over-limit tick.
	limitedUntil time.Time
}

// rateSample is the pure input to the rate arithmetic: the current used bytes,
// the monotonically measured elapsed time since the previous sample, the
// ceiling in bytes/second (<=0 = no ceiling) and the kill threshold N.
type rateSample struct {
	used      int64
	dt        time.Duration
	limitBps  float64
	threshold int
}

// advanceRate is the PURE write-rate arithmetic. Given the previous state and
// the current sample it returns the updated state and whether the rate has been
// over the ceiling for `threshold` consecutive samples (the kill condition). It
// has no clock, no I/O and no locks, so it is unit-tested in isolation.
//
// Rules, each a requirement (see the task spec):
//   - a limit of 0 (or negative) means no ceiling: the state resets and the
//     function never reports over;
//   - the first sample has no baseline: the rate is unknown, so a fresh
//     workspace is never killed on its first tick;
//   - usage moving DOWN (a delete) resets both the baseline and the counter:
//     falling usage is never a negative rate and never a violation;
//   - a non-positive elapsed interval cannot yield a rate: the baseline moves
//     (so the next sample is measured from here) but nothing is counted;
//   - a run already at its disk ceiling (used >= diskLimit) has its state reset
//     by the caller, so a full workspace does not read as a violation.
func advanceRate(prev rateState, s rateSample) (rateState, bool) {
	next := prev
	next.prevUsed = s.used
	next.hasBaseline = true

	if s.limitBps <= 0 {
		next.over = 0
		return next, false
	}
	if !prev.hasBaseline {
		next.over = 0
		return next, false
	}
	if s.used < prev.prevUsed {
		// Usage fell (a delete, or the workspace shrank): reset the baseline,
		// never emit a negative rate and never count a violation.
		next.over = 0
		return next, false
	}
	if s.dt <= 0 {
		next.over = 0
		return next, false
	}
	rate := float64(s.used-prev.prevUsed) / s.dt.Seconds()
	if rate > s.limitBps {
		next.over = prev.over + 1
	} else {
		next.over = 0
	}
	thr := s.threshold
	if thr <= 0 {
		thr = 1
	}
	return next, next.over >= thr
}

// WriteRateEnabled reports whether the server-level write-rate watchdog is on.
func (s *Service) WriteRateEnabled() bool {
	return s.cfg.Disk.WriteRateEnabled && s.rateWatchdogOn
}

// WriteRateSamples returns N, the number of consecutive over-limit samples
// required before a run is stopped. It defaults to 3 when unset.
func (s *Service) WriteRateSamples() int {
	n := s.cfg.Disk.WriteRateSamples
	if n <= 0 {
		return 3
	}
	return n
}

// WriteRateOvershootMB returns the documented worst-case overshoot in MiB for a
// workspace at its own ceiling: the bytes a workspace can write above the
// ceiling line during the N-sample detection window before the watchdog stops
// it. It is N x interval_seconds x ceiling_mb_s. It is 0 when the feature is
// off or the workspace has no rate ceiling; it is the honest number the ADR
// states and is never presented as a kernel boundary (see ADR 95).
func (s *Service) WriteRateOvershootMB(ceilingMBs int) float64 {
	if !s.WriteRateEnabled() || ceilingMBs <= 0 {
		return 0
	}
	intervalSec := s.cfg.Metrics.MetricsInterval().Seconds()
	if intervalSec <= 0 {
		intervalSec = 3
	}
	return float64(s.WriteRateSamples()) * intervalSec * float64(ceilingMBs)
}

// startWriteRateWatchdog wires the watchdog onto the metrics sampler's tick. It
// is a no-op unless the feature is enabled AND the sampler exists (the watchdog
// deliberately has no timer of its own). Pinning the sampler keeps its tick
// alive with no subscriber connected.
func (s *Service) startWriteRateWatchdog() {
	if s.rateWatchdogOnce {
		return
	}
	s.rateWatchdogOnce = true
	if !s.cfg.Disk.WriteRateEnabled {
		return
	}
	if s.metrics == nil {
		// The watchdog deliberately has no timer of its own; it rides the
		// metrics sampler's tick. Without the sampler it cannot run, so say so
		// loudly rather than silently doing nothing.
		s.logger.Warn("disk.write_rate_enabled is set but the metrics sampler is disabled; the write-rate ceiling cannot be enforced",
			"hint", "set metrics.enabled: true")
		return
	}
	s.rateWatchdogOn = true
	s.metrics.SetObserver(s.observeWriteRates)
	s.metrics.Pin()
}

// rateLimited reports whether the engine's own write path must refuse a write
// to the workspace because it is over its write-rate ceiling. It is always
// false when the watchdog is off.
func (s *Service) rateLimited(wsID string) bool {
	if !s.rateWatchdogOn {
		return false
	}
	now := s.now()
	s.rateMu.Lock()
	st := s.rateState[wsID]
	limited := st != nil && now.Before(st.limitedUntil)
	s.rateMu.Unlock()
	return limited
}

// observeWriteRates is the per-tick observer. It rides the snapshot the sampler
// already built, so it costs no extra reads: one pass of arithmetic per
// candidate workspace. It stops runs whose workspace has sustained a rate over
// its ceiling and flags the workspace so the engine's own writes are refused
// too.
func (s *Service) observeWriteRates(snap metrics.Snapshot) {
	if !s.rateWatchdogOn {
		return
	}
	threshold := s.WriteRateSamples()
	interval := time.Duration(snap.IntervalSeconds * float64(time.Second))
	if interval <= 0 {
		interval = time.Second
	}
	// The cooldown keeps the engine's write path refusing for two ticks after
	// the last over-limit sample, so a client sees the refusal rather than a
	// one-tick race.
	cooldown := 2 * interval

	now := s.now()
	type kill struct {
		wsID     string
		rateBps  float64
		limitBps float64
	}
	var kills []kill
	for _, w := range snap.Workspaces {
		limitBps := float64(w.WriteRateLimitMBs) * 1024 * 1024
		if w.WriteRateLimitMBs <= 0 {
			s.resetRateState(w.WorkspaceID)
			continue
		}
		// Only a workspace whose disk is a live mount yields a trustworthy
		// growth figure; otherwise degrade to the existing total-ceiling path
		// rather than compute a rate from a zero reading.
		if !w.DiskComplete {
			s.resetRateState(w.WorkspaceID)
			continue
		}
		// A workspace already at its disk ceiling stops growing (the kernel
		// refuses the writes), so it must not read as a rate violation every
		// tick: reset and skip.
		if w.DiskLimitBytes > 0 && w.DiskUsedBytes >= w.DiskLimitBytes {
			s.resetRateState(w.WorkspaceID)
			continue
		}

		s.rateMu.Lock()
		st := s.rateState[w.WorkspaceID]
		if st == nil {
			st = &rateState{}
			s.rateState[w.WorkspaceID] = st
		}
		dt := time.Duration(0)
		prevUsed := st.prevUsed
		if st.hasBaseline {
			dt = now.Sub(st.prevMono)
		}
		next, over := advanceRate(*st, rateSample{
			used:      w.DiskUsedBytes,
			dt:        dt,
			limitBps:  limitBps,
			threshold: threshold,
		})
		next.prevMono = now
		if over {
			next.limitedUntil = now.Add(cooldown)
		}
		s.rateState[w.WorkspaceID] = &next
		rateBps := float64(0)
		if st.hasBaseline && dt > 0 && w.DiskUsedBytes >= prevUsed {
			rateBps = float64(w.DiskUsedBytes-prevUsed) / dt.Seconds()
		}
		s.rateMu.Unlock()

		if over {
			kills = append(kills, kill{wsID: w.WorkspaceID, rateBps: rateBps, limitBps: limitBps})
		}
	}
	for _, k := range kills {
		s.stopWorkspaceRunsForRate(k.wsID, k.rateBps, k.limitBps)
	}
}

// resetRateState clears a workspace's rate state and any active write refusal.
func (s *Service) resetRateState(wsID string) {
	s.rateMu.Lock()
	if st := s.rateState[wsID]; st != nil {
		st.hasBaseline = false
		st.over = 0
		st.limitedUntil = time.Time{}
	}
	s.rateMu.Unlock()
}

// stopWorkspaceRunsForRate stops every run of a workspace that has sustained a
// write rate over its ceiling. The runs are marked rate-limited BEFORE they are
// cancelled, so executeRun classifies them with the distinct outcome.
func (s *Service) stopWorkspaceRunsForRate(wsID string, rateBps, limitBps float64) {
	s.mu.Lock()
	victims := make([]*runningRun, 0, 2)
	for _, rr := range s.runs {
		if rr.wsID == wsID {
			rr.rateLimited = true
			victims = append(victims, rr)
		}
	}
	s.mu.Unlock()
	if len(victims) == 0 {
		// No run to stop (e.g. a sustained API upload): leave the watchdog flag
		// set so the engine's own write path refuses further writes.
		s.logger.Warn("workspace write rate exceeded the ceiling; no live run to stop",
			"workspace_id", wsID, "rate_bytes_per_s", rateBps, "limit_bytes_per_s", limitBps)
		return
	}
	s.logger.Warn("workspace write rate exceeded the ceiling; stopping run",
		"workspace_id", wsID, "rate_bytes_per_s", rateBps, "limit_bytes_per_s", limitBps,
		"runs", len(victims))
	for _, rr := range victims {
		rr.cancel()
		s.runner.Kill(rr.runID)
	}
}

// writeRateLimitedError is returned by the engine's own write path when a
// workspace has sustained a write rate over its ceiling. It is DISTINCT from a
// quota error (the disk is not full) and from a validation error (nothing about
// the request is wrong): the workspace is simply writing too fast right now.
func writeRateLimitedError(wsID string, limitMBs int) error {
	return model.Wrap(429, model.CodeWriteRateExceeded,
		"workspace write rate exceeds its write_rate_mb_s ceiling",
		map[string]string{
			"workspace_id":    wsID,
			"write_rate_mb_s": itoaInt(limitMBs),
			"enforcement":     "sampled disk growth; sustained over-limit writes are stopped",
		})
}

// ensureEngineWriteAllowed refuses an engine-side write (an API file write or
// upload, an archive extraction) while the workspace has sustained a write rate
// over its ceiling. It is a no-op when the watchdog is off or no ceiling is
// configured, so it never changes a default deployment.
func (s *Service) ensureEngineWriteAllowed(w *model.Workspace) error {
	if w == nil || w.Limits.WriteRateMBs <= 0 || !s.rateWatchdogOn {
		return nil
	}
	if s.rateLimited(w.ID) {
		return writeRateLimitedError(w.ID, w.Limits.WriteRateMBs)
	}
	return nil
}
