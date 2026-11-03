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
// workspace's per-run and per-session cgroups, and that the reported ceiling is
// the workspace's ENFORCED parent limit (passed in), not a per-run sum. This is
// the assertion that catches "read one cgroup and call it the workspace" and
// "publish a per-run ceiling that can exceed the workspace's allowance".
func TestBuildSnapshotSumsCgroups(t *testing.T) {
	// The workspace's enforced ceiling (the parent memory.max): 160 MB.
	var limit int64 = 160 * 1024 * 1024
	info := WorkspaceInfo{ID: "ws_1", MemoryLimitBytes: limit, DiskLimitBytes: 64 * 1024 * 1024}
	// Two runs: 61 MB and 56 MB (the measured two-run case).
	var used int64 = 61546496 + 56934400
	snap := BuildWorkspaceSnapshot(info, 90112, info.DiskLimitBytes, true, used, limit, true, 2, 2, 0, 0)
	if snap.RAMUsedBytes != used {
		t.Fatalf("RAMUsedBytes = %d, want sum %d", snap.RAMUsedBytes, used)
	}
	if snap.RAMLimitBytes != limit {
		t.Fatalf("RAMLimitBytes = %d, want the enforced ceiling %d", snap.RAMLimitBytes, limit)
	}
	// The contract the tenant sees: usage never exceeds the advertised ceiling,
	// because the kernel enforces the SUM at the parent.
	if snap.RAMUsedBytes > snap.RAMLimitBytes {
		t.Fatalf("ram_used (%d) must never exceed ram_limit (%d)", snap.RAMUsedBytes, snap.RAMLimitBytes)
	}
	if snap.RAMPercent > 100 {
		t.Fatalf("ram_percent = %v, must never exceed 100", snap.RAMPercent)
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

// TestLiveCgroupReadsOnlyMemoryFiles proves a live workspace reads only the
// cheap memory files by default: one memory.current per leaf cgroup, plus one
// read of the workspace parent cgroup's memory.max for the ENFORCED ceiling. The
// expensive extras (cpu.stat, pids.current) are NOT read.
func TestLiveCgroupReadsOnlyMemoryFiles(t *testing.T) {
	r := newFakeReader()
	r.current["/cg/a"] = 100
	r.current["/cg/b"] = 200
	r.max["/ws/parent"] = 3000
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{
		ID: "ws_live", OwnerID: "u1", DiskRoot: "/none",
		DiskLimitBytes: 64 << 20, MemoryLimitBytes: 3000,
		ParentCgroup: "/ws/parent",
		RunCgroups:   []string{"/cg/a"}, SessionCgroups: []string{"/cg/b"},
	}})
	r.diskOK["/none"] = true
	s := New(DefaultConfig(), p, r)
	snap := s.Sample()
	w := snap.Workspaces[0]
	if w.RAMUsedBytes != 300 {
		t.Fatalf("summed RAM = %d, want 300", w.RAMUsedBytes)
	}
	if w.RAMLimitBytes != 3000 {
		t.Fatalf("RAMLimitBytes = %d, want the parent memory.max 3000", w.RAMLimitBytes)
	}
	if snap.Sampler.CgroupReads != 3 { // 2 leaf memory.current + 1 parent memory.max
		t.Fatalf("cgroup reads = %d, want 3", snap.Sampler.CgroupReads)
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
	sub, err := s.Subscribe(Scope{OwnerID: "u1"}, 0)
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
	r.max["/ws/parent"] = 1000
	p := &fakeProvider{}
	p.set([]WorkspaceInfo{{
		ID: "ws", OwnerID: "u1", DiskRoot: "/none",
		DiskLimitBytes: 1, MemoryLimitBytes: 1000,
		ParentCgroup: "/ws/parent", RunCgroups: []string{"/cg/a"},
	}})
	r.diskOK["/none"] = true
	cfg := DefaultConfig()
	cfg.Interval = time.Hour // only the explicit publish() calls below tick
	s := New(cfg, p, r)

	var subs []*subscriber
	for i := 0; i < 8; i++ {
		sub, err := s.Subscribe(Scope{OwnerID: "u1"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}
	// Wait for the one initial publish to settle (the interval is 1h, so no
	// further automatic sample pass will race the measurement below).
	last := int64(-1)
	for i := 0; i < 100; i++ {
		cur := s.Counters().CgroupReads
		if cur == last {
			break
		}
		last = cur
		time.Sleep(5 * time.Millisecond)
	}
	// Drain the initial publish's frame from each subscriber.
	for _, sub := range subs {
		select {
		case <-sub.Frames():
		default:
		}
	}
	before := s.Counters().CgroupReads
	// ONE publish fans out to all 8 subscribers and performs exactly one sample
	// pass (2 cgroup reads here: one leaf memory.current + the parent
	// memory.max), so cost is independent of subscriber count.
	s.publish()
	after := s.Counters().CgroupReads
	if after-before != 2 {
		t.Fatalf("one publish read %d cgroups, want 2 (independent of 8 subscribers)", after-before)
	}
	// Every subscriber received the frame (fan-out, not per-subscriber drop).
	for i, sub := range subs {
		select {
		case <-sub.Frames():
		default:
			t.Fatalf("subscriber %d did not receive the fanned-out frame", i)
		}
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
	sub, err := s.Subscribe(Scope{OwnerID: "u1"}, 0)
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
		sub, err := s.Subscribe(Scope{OwnerID: "u1"}, 0)
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
	cfg := DefaultConfig()
	cfg.Interval = time.Hour // only the initial publish and the explicit calls below
	s := New(cfg, p, r)
	sub, _ := s.Subscribe(Scope{OwnerID: "u1"}, 0)
	// Read the initial frame the sampler goroutine publishes on start, so the
	// manual publishes below are the only writers from here on (no concurrent
	// publish can interleave and reorder timestamps).
	<-sub.Frames()
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
		if !last.IsZero() && !snap.Timestamp.After(last) {
			t.Fatalf("timestamp not strictly increasing: %v <= %v", snap.Timestamp, last)
		}
		last = snap.Timestamp
	}
	s.Close(sub)
}
