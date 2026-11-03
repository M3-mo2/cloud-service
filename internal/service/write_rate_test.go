package service

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/metrics"
	"sandboxd/internal/model"
	"sandboxd/internal/storage"
)

// TestAdvanceRateFirstSampleNoBaseline: the very first sample for a workspace
// has no baseline, so its rate is unknown and it must never be flagged.
func TestAdvanceRateFirstSampleNoBaseline(t *testing.T) {
	st := rateState{}
	next, over := advanceRate(st, rateSample{used: 10 << 20, dt: time.Second, limitBps: 1, threshold: 1})
	if over {
		t.Fatal("the first sample must never be over the limit (no baseline)")
	}
	if !next.hasBaseline || next.prevUsed != 10<<20 {
		t.Fatalf("baseline not recorded: %+v", next)
	}
}

// TestAdvanceRateUnderLimit: a rate below the ceiling resets the counter.
func TestAdvanceRateUnderLimit(t *testing.T) {
	st := rateState{hasBaseline: true, prevUsed: 0, over: 2}
	// 1 MiB in 1s = 1 MiB/s, ceiling 4 MiB/s.
	next, over := advanceRate(st, rateSample{used: 1 << 20, dt: time.Second, limitBps: 4 << 20, threshold: 3})
	if over || next.over != 0 {
		t.Fatalf("under-limit sample must reset the counter: over=%v state=%+v", over, next)
	}
}

// TestAdvanceRateSingleBurstDoesNotKill: one over-limit sample with N=3 must
// NOT kill; the third consecutive one must.
func TestAdvanceRateSingleBurstDoesNotKill(t *testing.T) {
	limit := float64(1 << 20) // 1 MiB/s
	st := rateState{}
	var over bool
	// tick 1: baseline established (used 0).
	st, over = advanceRate(st, rateSample{used: 0, dt: time.Second, limitBps: limit, threshold: 3})
	if over {
		t.Fatal("first sample must not be over")
	}
	// tick 2: a single burst of 4 MiB in 1s (4x the limit).
	st, over = advanceRate(st, rateSample{used: 4 << 20, dt: time.Second, limitBps: limit, threshold: 3})
	if over {
		t.Fatal("a single burst must not kill a run (N=3)")
	}
	if st.over != 1 {
		t.Fatalf("over counter = %d, want 1", st.over)
	}
	// tick 3: still over -> 2.
	st, over = advanceRate(st, rateSample{used: 8 << 20, dt: time.Second, limitBps: limit, threshold: 3})
	if over {
		t.Fatal("two over-limit samples must not kill (N=3)")
	}
	// tick 4: still over -> 3 -> kill.
	st, over = advanceRate(st, rateSample{used: 12 << 20, dt: time.Second, limitBps: limit, threshold: 3})
	if !over {
		t.Fatalf("three consecutive over-limit samples must kill: state=%+v", st)
	}
}

// TestAdvanceRateUnderLimitBreaksStreak: a good sample between bursts resets the
// consecutive counter, so an intermittent writer is never killed.
func TestAdvanceRateUnderLimitBreaksStreak(t *testing.T) {
	limit := float64(1 << 20)
	st := rateState{hasBaseline: true, prevUsed: 0, over: 0}
	st, _ = advanceRate(st, rateSample{used: 4 << 20, dt: time.Second, limitBps: limit, threshold: 3})
	if st.over != 1 {
		t.Fatalf("over = %d, want 1", st.over)
	}
	// A good sample (0 growth) resets the streak.
	st, over := advanceRate(st, rateSample{used: 4 << 20, dt: time.Second, limitBps: limit, threshold: 3})
	if over || st.over != 0 {
		t.Fatalf("a good sample must reset the streak: over=%v over=%d", over, st.over)
	}
}

// TestAdvanceRateUsageFallsResets: usage moving DOWN (a delete) must reset the
// baseline and never produce a negative rate or a violation.
func TestAdvanceRateUsageFallsResets(t *testing.T) {
	st := rateState{hasBaseline: true, prevUsed: 100 << 20, over: 2}
	next, over := advanceRate(st, rateSample{used: 40 << 20, dt: time.Second, limitBps: 1 << 20, threshold: 1})
	if over {
		t.Fatal("falling usage must not be a violation")
	}
	if next.over != 0 {
		t.Fatalf("over = %d, want 0", next.over)
	}
	if next.prevUsed != 40<<20 {
		t.Fatalf("baseline not reset: %d", next.prevUsed)
	}
}

// TestAdvanceRateZeroInterval: a non-positive elapsed interval cannot yield a
// rate; nothing is counted.
func TestAdvanceRateZeroInterval(t *testing.T) {
	st := rateState{hasBaseline: true, prevUsed: 0, over: 2}
	next, over := advanceRate(st, rateSample{used: 100 << 20, dt: 0, limitBps: 1, threshold: 1})
	if over || next.over != 0 {
		t.Fatalf("a zero interval must not count: over=%v state=%+v", over, next)
	}
}

// TestAdvanceRateNoCeiling: a limit of 0 (no ceiling) is never a violation.
func TestAdvanceRateNoCeiling(t *testing.T) {
	st := rateState{hasBaseline: true, prevUsed: 0, over: 2}
	next, over := advanceRate(st, rateSample{used: 1 << 40, dt: time.Millisecond, limitBps: 0, threshold: 1})
	if over || next.over != 0 {
		t.Fatalf("no ceiling must never be a violation: over=%v", over)
	}
}

// TestZeroValueDiskConfigWriteRateOff is the DEFAULT-OFF check the task asks
// for: the zero-value config must have the write-rate feature off, independent
// of any comment.
func TestZeroValueDiskConfigWriteRateOff(t *testing.T) {
	var d config.DiskConfig
	if d.WriteRateEnabled {
		t.Fatal("zero-value DiskConfig must have write_rate_enabled off")
	}
	// And the shipped Default() constructor must also be off (new behaviour
	// changes no existing deployment by default).
	def := config.Default()
	if def.Disk.WriteRateEnabled {
		t.Fatal("Default().Disk.WriteRateEnabled must be false")
	}
	if def.Limits.WriteRateMBs != 0 || def.MaxLimits.WriteRateMBs != 0 {
		t.Fatalf("default write-rate ceilings must be 0: %d / %d",
			def.Limits.WriteRateMBs, def.MaxLimits.WriteRateMBs)
	}
	if def.Disk.WriteRateSamples != 3 {
		t.Fatalf("default write_rate_samples = %d, want 3", def.Disk.WriteRateSamples)
	}
}

// TestValidateRateCeilingCannotExceedDisk: a rate ceiling above the disk ceiling
// is refused on config load; equal is allowed; 0 is always allowed.
func TestValidateRateCeilingCannotExceedDisk(t *testing.T) {
	base := config.Default()

	bad := base
	bad.Limits.DiskMB = 1024
	bad.Limits.WriteRateMBs = 2048
	if err := bad.Validate(); err == nil {
		t.Fatal("a rate ceiling above disk_mb must be refused")
	}

	ok := base
	ok.Limits.DiskMB = 1024
	ok.Limits.WriteRateMBs = 1024
	if err := ok.Validate(); err != nil {
		t.Fatalf("a rate ceiling equal to disk_mb must be allowed: %v", err)
	}

	off := base
	off.Limits.WriteRateMBs = 0
	if err := off.Validate(); err != nil {
		t.Fatalf("no rate ceiling must always be valid: %v", err)
	}
}

// TestCheckRateWithinDiskServiceSide proves the same rule is enforced when a
// workspace is created/patched through the service, not only at config load.
func TestCheckRateWithinDiskServiceSide(t *testing.T) {
	if err := checkRateWithinDisk(model.Limits{DiskMB: 64, WriteRateMBs: 128}); err == nil {
		t.Fatal("rate > disk must be refused by the service check")
	}
	if err := checkRateWithinDisk(model.Limits{DiskMB: 64, WriteRateMBs: 64}); err != nil {
		t.Fatalf("rate == disk must be allowed: %v", err)
	}
	if err := checkRateWithinDisk(model.Limits{DiskMB: 64, WriteRateMBs: 0}); err != nil {
		t.Fatalf("rate 0 must be allowed: %v", err)
	}
}

// newRateService builds a minimal service with the write-rate watchdog ON and a
// FakeRunner, without starting goroutines, so the observer can be driven with
// crafted snapshots.
func newRateService(t *testing.T, interval time.Duration) (*Service, *executor.FakeRunner) {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.DataDir = t.TempDir()
	cfg.Disk.Enabled = false
	cfg.Disk.WriteRateEnabled = true
	cfg.Disk.WriteRateSamples = 3
	cfg.Metrics.Interval = interval.String()
	store, err := storage.New(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	fake := &executor.FakeRunner{AvailableFlag: true}
	svc := New(cfg, store, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.rateWatchdogOn = true // the observer path, without needing the sampler
	return svc, fake
}

func snapW(wsID string, used, diskLimit, rateMBs int64, diskOK bool) metrics.Snapshot {
	return metrics.Snapshot{
		IntervalSeconds: 1,
		Workspaces: []metrics.WorkspaceSnapshot{{
			WorkspaceID:       wsID,
			DiskUsedBytes:     used,
			DiskLimitBytes:    diskLimit,
			WriteRateLimitMBs: rateMBs,
			DiskComplete:      diskOK,
		}},
	}
}

// TestObserverStopsSustainedOverLimitRun: after N consecutive over-limit
// samples the run's cancel is called and the run is flagged rate-limited.
func TestObserverStopsSustainedOverLimitRun(t *testing.T) {
	svc, fake := newRateService(t, time.Second)
	wsID := "ws_rate"
	cancelled := make(chan struct{})
	svc.runs["r1"] = &runningRun{runID: "r1", wsID: wsID, cancel: func() { close(cancelled) }, done: make(chan struct{})}

	base := time.Unix(0, 0)
	svc.now = func() time.Time { return base }
	// tick 1: baseline (used 0).
	svc.observeWriteRates(snapW(wsID, 0, 64<<20, 1, true))
	// ticks 2..4: 4 MiB/s over a 1 MiB/s ceiling.
	for i := 1; i <= 3; i++ {
		base = base.Add(time.Second)
		svc.observeWriteRates(snapW(wsID, int64(i)*4<<20, 64<<20, 1, true))
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("a sustained over-limit rate must cancel the run after N samples")
	}
	if !fake.Kill("r1") {
		// FakeRunner.Kill returns true only the first time; ensure it was called.
	}
	svc.mu.Lock()
	flagged := svc.runs["r1"].rateLimited
	svc.mu.Unlock()
	if !flagged {
		t.Fatal("the run must be flagged rateLimited so executeRun classifies it distinctly")
	}
}

// TestObserverSingleBurstDoesNotStop: one over-limit sample must not cancel.
func TestObserverSingleBurstDoesNotStop(t *testing.T) {
	svc, _ := newRateService(t, time.Second)
	wsID := "ws_burst"
	cancelled := false
	svc.runs["r1"] = &runningRun{runID: "r1", wsID: wsID, cancel: func() { cancelled = true }, done: make(chan struct{})}
	base := time.Unix(0, 0)
	svc.now = func() time.Time { return base }
	svc.observeWriteRates(snapW(wsID, 0, 64<<20, 1, true))
	base = base.Add(time.Second)
	svc.observeWriteRates(snapW(wsID, 9<<20, 64<<20, 1, true)) // one huge burst
	if cancelled {
		t.Fatal("a single burst must not stop a run")
	}
}

// TestObserverUnderLimitNeverStops: a run under the ceiling is untouched.
func TestObserverUnderLimitNeverStops(t *testing.T) {
	svc, _ := newRateService(t, time.Second)
	wsID := "ws_ok"
	cancelled := false
	svc.runs["r1"] = &runningRun{runID: "r1", wsID: wsID, cancel: func() { cancelled = true }, done: make(chan struct{})}
	base := time.Unix(0, 0)
	svc.now = func() time.Time { return base }
	for i := 0; i < 6; i++ {
		svc.observeWriteRates(snapW(wsID, int64(i)*256<<10, 64<<20, 1, true)) // 256 KiB/s
		base = base.Add(time.Second)
	}
	if cancelled {
		t.Fatal("a run under the ceiling must never be stopped")
	}
}

// TestObserverAtDiskCeilingNotARateViolation: a workspace already at its disk
// ceiling (which stops growing) must not read as a rate violation every tick.
func TestObserverAtDiskCeilingNotARateViolation(t *testing.T) {
	svc, _ := newRateService(t, time.Second)
	wsID := "ws_full"
	cancelled := false
	svc.runs["r1"] = &runningRun{runID: "r1", wsID: wsID, cancel: func() { cancelled = true }, done: make(chan struct{})}
	base := time.Unix(0, 0)
	svc.now = func() time.Time { return base }
	// The workspace sits exactly at its disk ceiling for many ticks.
	for i := 0; i < 10; i++ {
		svc.observeWriteRates(snapW(wsID, 64<<20, 64<<20, 1, true))
		base = base.Add(time.Second)
	}
	if cancelled {
		t.Fatal("a workspace at its disk ceiling must not be a rate violation")
	}
}

// TestObserverUsageFallsDoesNotViolate: a delete inside the workspace lowers
// usage and must not trip the ceiling.
func TestObserverUsageFallsDoesNotViolate(t *testing.T) {
	svc, _ := newRateService(t, time.Second)
	wsID := "ws_del"
	cancelled := false
	svc.runs["r1"] = &runningRun{runID: "r1", wsID: wsID, cancel: func() { cancelled = true }, done: make(chan struct{})}
	base := time.Unix(0, 0)
	svc.now = func() time.Time { return base }
	svc.observeWriteRates(snapW(wsID, 40<<20, 64<<20, 1, true))
	base = base.Add(time.Second)
	svc.observeWriteRates(snapW(wsID, 10<<20, 64<<20, 1, true)) // a delete
	if cancelled {
		t.Fatal("falling usage must not be a rate violation")
	}
}

// TestObserverNonLiveMountDegrades: a workspace whose disk is not a live mount
// must not be judged on a zero reading.
func TestObserverNonLiveMountDegrades(t *testing.T) {
	svc, _ := newRateService(t, time.Second)
	wsID := "ws_nomount"
	cancelled := false
	svc.runs["r1"] = &runningRun{runID: "r1", wsID: wsID, cancel: func() { cancelled = true }, done: make(chan struct{})}
	base := time.Unix(0, 0)
	svc.now = func() time.Time { return base }
	for i := 0; i < 6; i++ {
		svc.observeWriteRates(snapW(wsID, int64(i)*100<<20, 64<<20, 1, false))
		base = base.Add(time.Second)
	}
	if cancelled {
		t.Fatal("a non-live mount must not be judged on a rate")
	}
}

// TestEngineWriteRefusedWhileOverCeiling: the engine's own API write path is
// refused with the distinct code while the workspace is over the ceiling.
func TestEngineWriteRefusedWhileOverCeiling(t *testing.T) {
	svc, _ := newRateService(t, time.Second)
	wsID := "ws_engine"
	w := &model.Workspace{ID: wsID, Limits: model.Limits{WriteRateMBs: 1, DiskMB: 64}}
	// Not over yet.
	if err := svc.ensureEngineWriteAllowed(w); err != nil {
		t.Fatalf("an unthrottled workspace must allow a write: %v", err)
	}
	// Trip the ceiling with a sustained burst.
	base := time.Unix(0, 0)
	svc.now = func() time.Time { return base }
	svc.runs["r1"] = &runningRun{runID: "r1", wsID: wsID, cancel: func() {}, done: make(chan struct{})}
	svc.observeWriteRates(snapW(wsID, 0, 64<<20, 1, true))
	for i := 1; i <= 3; i++ {
		base = base.Add(time.Second)
		svc.observeWriteRates(snapW(wsID, int64(i)*4<<20, 64<<20, 1, true))
	}
	err := svc.ensureEngineWriteAllowed(w)
	if err == nil {
		t.Fatal("the engine write path must be refused while over the ceiling")
	}
	apiErr := model.AsError(err)
	if apiErr.Code != model.CodeWriteRateExceeded || apiErr.Status != 429 {
		t.Fatalf("wrong refusal: code=%s status=%d", apiErr.Code, apiErr.Status)
	}
}

// TestWatchdogDisabledChangesNothing: with the feature off the engine write
// path never refuses and the observer does nothing.
func TestWatchdogDisabledChangesNothing(t *testing.T) {
	svc, _ := newRateService(t, time.Second)
	svc.rateWatchdogOn = false
	w := &model.Workspace{ID: "ws_off", Limits: model.Limits{WriteRateMBs: 1, DiskMB: 64}}
	if err := svc.ensureEngineWriteAllowed(w); err != nil {
		t.Fatalf("disabled watchdog must never refuse a write: %v", err)
	}
}
