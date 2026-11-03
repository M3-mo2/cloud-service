package metrics

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReader is a deterministic Reader. It counts reads so the tests can assert
// the idle-elision and fan-out properties directly.
type fakeReader struct {
	mu       sync.Mutex
	current  map[string]int64
	max      map[string]int64
	usage    map[string]int64
	pids     map[string]int64
	diskUsed map[string]int64
	diskOK   map[string]bool

	memReads   atomic.Int64
	statfsRead int64
	cpuReads   int64
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		current:  map[string]int64{},
		max:      map[string]int64{},
		usage:    map[string]int64{},
		pids:     map[string]int64{},
		diskUsed: map[string]int64{},
		diskOK:   map[string]bool{},
	}
}

func (f *fakeReader) MemoryCurrent(dir string) (int64, error) {
	f.memReads.Add(1)
	return f.current[dir], nil
}
func (f *fakeReader) MemoryMax(dir string) (int64, error) { return f.max[dir], nil }
func (f *fakeReader) CPUUsageUsec(dir string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cpuReads++
	return f.usage[dir], nil
}
func (f *fakeReader) PidsCurrent(dir string) (int64, error) { return f.pids[dir], nil }
func (f *fakeReader) DiskUsed(root string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statfsRead++
	return f.diskUsed[root], f.diskOK[root]
}

// fakeProvider returns a fixed candidate set.
type fakeProvider struct {
	mu   sync.Mutex
	info []WorkspaceInfo
}

func (p *fakeProvider) Candidates() []WorkspaceInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]WorkspaceInfo(nil), p.info...)
}
func (p *fakeProvider) set(info []WorkspaceInfo) {
	p.mu.Lock()
	p.info = info
	p.mu.Unlock()
}

func TestClampInterval(t *testing.T) {
	cases := []struct {
		in, min, max, want time.Duration
	}{
		{3 * time.Second, time.Second, 300 * time.Second, 3 * time.Second},
		{10 * time.Millisecond, time.Second, 300 * time.Second, time.Second},
		{time.Hour, time.Second, 300 * time.Second, 300 * time.Second},
		{0, time.Second, 300 * time.Second, time.Second},
		{-5 * time.Second, time.Second, 300 * time.Second, time.Second},
		{2 * time.Second, 0, 0, 2 * time.Second},
	}
	for _, c := range cases {
		if got := ClampInterval(c.in, c.min, c.max); got != c.want {
			t.Errorf("ClampInterval(%v,%v,%v)=%v want %v", c.in, c.min, c.max, got, c.want)
		}
	}
}

func TestPercentZeroLimit(t *testing.T) {
	if got := Percent(100, 0); got != 0 {
		t.Errorf("Percent with zero limit = %v, want 0", got)
	}
	if got := Percent(100, -5); got != 0 {
		t.Errorf("Percent with negative limit = %v, want 0", got)
	}
	if got := Percent(50, 100); got != 50 {
		t.Errorf("Percent(50,100)=%v want 50", got)
	}
	if got := Percent(-1, 100); got != 0 {
		t.Errorf("Percent negative used = %v, want 0", got)
	}
}

func TestParseHelpers(t *testing.T) {
	if v, err := parseBytes([]byte("167772160\n")); err != nil || v != 167772160 {
		t.Fatalf("parseBytes = %d, %v", v, err)
	}
	b := []byte("usage_usec 12345\nsystem_usec 9\n")
	if v, err := parseStatUsec(b); err != nil || v != 12345 {
		t.Fatalf("parseStatUsec = %d, %v", v, err)
	}
	// A cpu.stat without usage_usec yields 0, not an error.
	if v, err := parseStatUsec([]byte("nr_periods 1\n")); err != nil || v != 0 {
		t.Fatalf("parseStatUsec missing field = %d, %v", v, err)
	}
}

// TestBuildSnapshotSumsCgroups proves the RAM figure is the SUM over the
// workspace's per-run and per-session cgroups, with the per-run memory.max sum
// exposed separately. This is the assertion that catches "read one cgroup and
// call it the workspace".
func TestBuildSnapshotSumsCgroups(t *testing.T) {
	info := WorkspaceInfo{ID: "ws_1", MemoryLimitBytes: 160 * 1024 * 1024, DiskLimitBytes: 64 * 1024 * 1024}
	// Two runs: 61 MB and 56 MB (the measured two-run case).
	var used int64 = 61546496 + 56934400
	var max int64 = 167772160 + 167772160
	snap := BuildWorkspaceSnapshot(info, 90112, info.DiskLimitBytes, true, used, max, true, 2, 2, 0, 0)
	if snap.RAMUsedBytes != used {
		t.Fatalf("RAMUsedBytes = %d, want sum %d", snap.RAMUsedBytes, used)
	}
	if snap.RAMCgroupLimitBytes != max {
		t.Fatalf("RAMCgroupLimitBytes = %d, want %d", snap.RAMCgroupLimitBytes, max)
	}
	if snap.RAMCgroupLimitBytes <= info.MemoryLimitBytes {
		t.Fatalf("per-run ceiling sum (%d) should exceed the workspace allowance (%d)", snap.RAMCgroupLimitBytes, info.MemoryLimitBytes)
	}
	if snap.LiveRuns != 2 || snap.LiveCgroups != 2 {
		t.Fatalf("live runs/cgroups = %d/%d", snap.LiveRuns, snap.LiveCgroups)
	}
}

// TestIdleWorkspaceZeroCgroupReads proves a workspace with no live cgroup costs
// ZERO cgroup file reads (only one statfs) and reports RAM 0.
func TestIdleWorkspaceZeroCgroupReads(t *testing.T) {
	r := newFakeReader()
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{
		ID: "ws_idle", OwnerID: "u1", DiskRoot: "/none",
		DiskLimitBytes: 64 << 20, MemoryLimitBytes: 512 << 20,
	}})
	r.diskOK["/none"] = true
	s := New(DefaultConfig(), p, r)
	snap := s.Sample()
	if len(snap.Workspaces) != 1 {
		t.Fatalf("workspaces = %d", len(snap.Workspaces))
	}
	w := snap.Workspaces[0]
	if w.RAMUsedBytes != 0 {
		t.Fatalf("idle RAM = %d, want 0", w.RAMUsedBytes)
	}
	if w.LiveCgroups != 0 {
		t.Fatalf("idle live cgroups = %d, want 0", w.LiveCgroups)
	}
	if got := r.memReads.Load(); got != 0 {
		t.Fatalf("idle workspace caused %d cgroup reads, want 0", got)
	}
	if snap.Sampler.CgroupReads != 0 {
		t.Fatalf("sampler cgroup counter = %d, want 0", snap.Sampler.CgroupReads)
	}
	if snap.Sampler.StatfsReads != 1 {
		t.Fatalf("statfs reads = %d, want 1", snap.Sampler.StatfsReads)
	}
}

// TestLiveCgroupReadsOnlyMemoryFiles proves a live cgroup reads exactly two
// files by default (memory.current + memory.max), not the expensive extras.
func TestLiveCgroupReadsOnlyMemoryFiles(t *testing.T) {
	r := newFakeReader()
	r.current["/cg/a"] = 100
	r.max["/cg/a"] = 1000
	r.current["/cg/b"] = 200
	r.max["/cg/b"] = 1000
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{
		ID: "ws_live", OwnerID: "u1", DiskRoot: "/none",
		DiskLimitBytes: 64 << 20, MemoryLimitBytes: 1000,
		RunCgroups: []string{"/cg/a"}, SessionCgroups: []string{"/cg/b"},
	}})
	r.diskOK["/none"] = true
	s := New(DefaultConfig(), p, r)
	snap := s.Sample()
	w := snap.Workspaces[0]
	if w.RAMUsedBytes != 300 {
		t.Fatalf("summed RAM = %d, want 300", w.RAMUsedBytes)
	}
	if w.RAMCgroupLimitBytes != 2000 {
		t.Fatalf("cgroup max sum = %d, want 2000", w.RAMCgroupLimitBytes)
	}
	if snap.Sampler.CgroupReads != 4 { // 2 cgroups x 2 files
		t.Fatalf("cgroup reads = %d, want 4", snap.Sampler.CgroupReads)
	}
	if r.cpuReads != 0 {
		t.Fatalf("cpu.stat read with include_cpu off (%d)", r.cpuReads)
	}
}

// TestIncludeCPUReadsExtras proves the opt-in extras are read when enabled.
func TestIncludeCPUReadsExtras(t *testing.T) {
	r := newFakeReader()
	r.current["/cg/a"] = 10
	r.max["/cg/a"] = 20
	r.usage["/cg/a"] = 777
	r.pids["/cg/a"] = 3
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{
		ID: "ws", OwnerID: "u1", DiskRoot: "/none",
		DiskLimitBytes: 1, MemoryLimitBytes: 1, RunCgroups: []string{"/cg/a"},
	}})
	r.diskOK["/none"] = true
	cfg := DefaultConfig()
	cfg.IncludeCPU = true
	s := New(cfg, p, r)
	w := s.Sample().Workspaces[0]
	if w.CPUUsageUsec != 777 || w.PidsCurrent != 3 {
		t.Fatalf("extras = %d/%d, want 777/3", w.CPUUsageUsec, w.PidsCurrent)
	}
}

// TestLazyStartAndStop proves the sampler goroutine starts on the first
// subscriber and stops on the last, and that with zero subscribers the tick
// counter is frozen.
func TestLazyStartAndStop(t *testing.T) {
	r := newFakeReader()
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{ID: "ws", OwnerID: "u1", DiskRoot: "/none", DiskLimitBytes: 1, MemoryLimitBytes: 1}})
	r.diskOK["/none"] = true
	cfg := DefaultConfig()
	cfg.Interval = 20 * time.Millisecond
	s := New(cfg, p, r)

	// No subscriber: no ticks.
	time.Sleep(60 * time.Millisecond)
	if got := s.Counters().Ticks; got != 0 {
		t.Fatalf("ticks with no subscriber = %d, want 0", got)
	}
	sub, err := s.Subscribe(Scope{OwnerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	// The first tick is immediate.
	select {
	case <-sub.Frames():
	case <-time.After(time.Second):
		t.Fatal("no initial frame")
	}
	time.Sleep(70 * time.Millisecond)
	if s.Counters().Ticks == 0 {
		t.Fatal("ticks did not advance after subscribe")
	}
	s.Close(sub)
	if !s.Wait(time.Second) {
		t.Fatal("sampler did not stop after the last subscriber left")
	}
	frozen := s.Counters().Ticks
	time.Sleep(60 * time.Millisecond)
	if s.Counters().Ticks != frozen {
		t.Fatal("ticks advanced after the last subscriber left")
	}
}

// TestFanOutReadsIndependentOfSubscribers proves the per-tick cgroup read count
// does NOT grow with the number of subscribers: ONE sample pass is fanned out.
// The subscriber count is deliberately small and the phases are torn down
// between them (this host must not be hammered).
func TestFanOutReadsIndependentOfSubscribers(t *testing.T) {
	r := newFakeReader()
	r.current["/cg/a"] = 100
	r.max["/cg/a"] = 1000
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{
		ID: "ws", OwnerID: "u1", DiskRoot: "/none",
		DiskLimitBytes: 1, MemoryLimitBytes: 1000, RunCgroups: []string{"/cg/a"},
	}})
	r.diskOK["/none"] = true
	cfg := DefaultConfig()
	cfg.Interval = time.Hour // we drive ticks manually by sampling
	s := New(cfg, p, r)

	// Baseline: one sample pass with one subscriber's worth of work.
	base := s.Sample().Sampler.CgroupReads
	// Now eight subscribers and several explicit sample passes.
	var subs []*subscriber
	for i := 0; i < 8; i++ {
		sub, err := s.Subscribe(Scope{OwnerID: "u1"})
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}
	after := s.Sample().Sampler.CgroupReads
	if after-base != 2 { // one pass = 2 reads, regardless of 8 subscribers
		t.Fatalf("one sample pass read %d cgroups, want 2 (independent of 8 subscribers)", after-base)
	}
	for _, sub := range subs {
		s.Close(sub)
	}
	if !s.Wait(time.Second) {
		t.Fatal("sampler did not stop")
	}
}

// TestCoalescingDropsFrames proves a subscriber that never reads holds at most
// ONE pending frame and the rest are dropped (coalesce, never queue), and the
// drop counter moves.
func TestCoalescingDropsFrames(t *testing.T) {
	r := newFakeReader()
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{ID: "ws", OwnerID: "u1", DiskRoot: "/none", DiskLimitBytes: 1, MemoryLimitBytes: 1}})
	r.diskOK["/none"] = true
	cfg := DefaultConfig()
	s := New(cfg, p, r)
	sub, err := s.Subscribe(Scope{OwnerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	// Publish several times without ever reading from sub.Frames().
	for i := 0; i < 5; i++ {
		s.publish()
	}
	if sub.Drops() == 0 {
		t.Fatal("no drops recorded for a stalled subscriber")
	}
	// The single slot holds exactly one frame.
	if got := len(sub.Frames()); got != 1 {
		t.Fatalf("pending frames = %d, want exactly 1 (no queue)", got)
	}
	if s.Counters().DroppedFrames == 0 {
		t.Fatal("global drop counter did not move")
	}
	s.Close(sub)
}

// TestRepeatOpenCloseNoLeak proves repeated subscribe/close cycles do not grow
// the subscriber count or leak the sampler. The loop is BOUNDED and torn down.
func TestRepeatOpenCloseNoLeak(t *testing.T) {
	r := newFakeReader()
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{ID: "ws", OwnerID: "u1", DiskRoot: "/none", DiskLimitBytes: 1, MemoryLimitBytes: 1}})
	r.diskOK["/none"] = true
	cfg := DefaultConfig()
	s := New(cfg, p, r)
	for i := 0; i < 10; i++ {
		sub, err := s.Subscribe(Scope{OwnerID: "u1"})
		if err != nil {
			t.Fatal(err)
		}
		s.Close(sub)
	}
	if got := s.Counters().Subscribers; got != 0 {
		t.Fatalf("subscribers after cycles = %d, want 0", got)
	}
	if !s.Wait(time.Second) {
		t.Fatal("sampler did not stop after cycles")
	}
}

// TestScopeHidesOtherOwners proves a scoped sample never carries another
// principal's workspace.
func TestScopeHidesOtherOwners(t *testing.T) {
	r := newFakeReader()
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{
		{ID: "ws_a", OwnerID: "alice", DiskRoot: "/a", DiskLimitBytes: 1, MemoryLimitBytes: 1},
		{ID: "ws_b", OwnerID: "bob", DiskRoot: "/b", DiskLimitBytes: 1, MemoryLimitBytes: 1},
	})
	r.diskOK["/a"] = true
	r.diskOK["/b"] = true
	s := New(DefaultConfig(), p, r)
	got := s.SampleScoped(Scope{OwnerID: "alice"})
	if len(got.Workspaces) != 1 || got.Workspaces[0].WorkspaceID != "ws_a" {
		t.Fatalf("alice scope leaked: %+v", got.Workspaces)
	}
}

// TestFormatOverride proves the floor is enforced: below the floor is refused,
// above the ceiling is clamped, and the default is used when absent.
func TestFormatOverride(t *testing.T) {
	s := New(DefaultConfig(), &fakeProvider{}, newFakeReader())
	if _, err := s.FormatOverride(""); err != nil {
		t.Fatalf("empty override should be accepted: %v", err)
	}
	if _, err := s.FormatOverride("10ms"); err == nil {
		t.Fatal("10ms should be refused (below the 1s floor)")
	}
	if d, err := s.FormatOverride("1h"); err != nil || d != 300*time.Second {
		t.Fatalf("1h should clamp to the ceiling; got %v, %v", d, err)
	}
	if d, err := s.FormatOverride("5s"); err != nil || d != 5*time.Second {
		t.Fatalf("5s should be accepted; got %v, %v", d, err)
	}
}

// TestFrameIsParseableJSON proves each frame is a JSON object with the expected
// shape and monotonically increasing timestamps across frames.
func TestFrameIsParseableJSON(t *testing.T) {
	r := newFakeReader()
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{ID: "ws", OwnerID: "u1", DiskRoot: "/none", DiskLimitBytes: 64, MemoryLimitBytes: 64}})
	r.diskOK["/none"] = true
	s := New(DefaultConfig(), p, r)
	sub, _ := s.Subscribe(Scope{OwnerID: "u1"})
	var last time.Time
	for i := 0; i < 3; i++ {
		s.publish()
		frame := <-sub.Frames()
		// Strip the SSE "data: " prefix and trailing blank line.
		body := frame[len("data: ") : len(frame)-2]
		var snap Snapshot
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatalf("frame %d not JSON: %v", i, err)
		}
		if snap.Timestamp.Before(last) {
			t.Fatalf("timestamp went backwards: %v < %v", snap.Timestamp, last)
		}
		last = snap.Timestamp
	}
	s.Close(sub)
}
