package metrics

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTooManySubscribers is returned when the configured subscriber ceiling is
// reached. It is a policy refusal, not a resource error.
var ErrTooManySubscribers = errors.New("too many metrics subscribers")

// Scope selects which workspaces a subscriber sees. OwnerID scopes a
// service-wide stream to one principal's workspaces so the shared sampler never
// leaks another owner's data; WorkspaceID pins a stream to exactly one
// workspace. An owner-scoped subscriber sees every workspace the principal owns;
// a workspace-scoped subscriber sees only its one workspace (and only while the
// principal still owns it).
type Scope struct {
	OwnerID     string
	WorkspaceID string
}

// matches reports whether a workspace is in scope.
func (s Scope) matches(info WorkspaceInfo) bool {
	if s.WorkspaceID != "" {
		return info.ID == s.WorkspaceID
	}
	return s.OwnerID == "" || info.OwnerID == s.OwnerID
}

// matchesSnapshot reports whether an already-sampled workspace is in scope.
func (s Scope) matchesSnapshot(w WorkspaceSnapshot) bool {
	if s.WorkspaceID != "" {
		return w.WorkspaceID == s.WorkspaceID
	}
	return s.OwnerID == "" || w.ownerID == s.OwnerID
}

// counters holds the sampler's atomic work counters. They are exposed so the
// cost claims (idle elision, fan-out, coalescing, lazy start) are observable.
type counters struct {
	ticks       atomic.Int64
	cgroupReads atomic.Int64
	statfsReads atomic.Int64
	subscribers atomic.Int64
	dropped     atomic.Int64
	candidates  atomic.Int64
}

// subscriber is one connected stream. It owns a single-slot channel: the
// sampler delivers at most ONE pending frame and drops (never queues) when the
// client has not read the previous one.
type subscriber struct {
	id      uint64
	scope   Scope
	ch      chan []byte
	dropped atomic.Int64
	closeMu sync.Once
}

// Frames returns the single-slot channel of encoded SSE frames. The caller
// reads and writes them to its client; a frame that is never read is dropped by
// the sampler, never buffered.
func (s *subscriber) Frames() <-chan []byte { return s.ch }

// Drops returns how many frames were coalesced away for this subscriber.
func (s *subscriber) Drops() int64 { return s.dropped.Load() }

// Sampler is the ONE service-wide resource sampler. It starts its goroutine on
// the first subscriber and stops on the last, so an unwatched service costs
// exactly zero. It samples once per tick and fans the same frame out to every
// subscriber, so cost is independent of the subscriber count.
type Sampler struct {
	cfg      Config
	provider Provider
	reader   Reader

	interval        atomic.Int64 // nanoseconds (effective, may be overridden)
	defaultInterval time.Duration

	mu      sync.Mutex
	subs    map[uint64]*subscriber
	nextID  uint64
	running bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	wakeCh  chan struct{}
	stopped bool

	// pinned keeps the sample loop alive with zero subscribers. It is used by
	// the write-rate watchdog, which rides this sampler's tick instead of
	// starting a second timer. Unpinned (the default) the sampler keeps its
	// lazy start/stop contract: it runs only while a subscriber is connected.
	pinned bool
	// observer, when set, is called once per sample PASS with the snapshot the
	// tick just built (so an observer costs no extra reads). It is invoked
	// without s.mu held, so an observer may take its own locks. It is NOT
	// called for a one-shot SampleScoped (a one-shot is not a tick).
	observer func(Snapshot)

	c counters
}

// New builds a sampler with the configured (clamped) interval. It does not
// start the goroutine; that happens lazily on the first Subscribe.
func New(cfg Config, provider Provider, reader Reader) *Sampler {
	interval := ClampInterval(cfg.Interval, cfg.MinInterval, cfg.MaxInterval)
	s := &Sampler{
		cfg:      cfg,
		provider: provider,
		reader:   reader,
		subs:     make(map[uint64]*subscriber),
	}
	s.defaultInterval = interval
	s.interval.Store(int64(interval))
	return s
}

// IntervalSeconds returns the effective (clamped) cadence in seconds.
func (s *Sampler) IntervalSeconds() float64 {
	return time.Duration(s.interval.Load()).Seconds()
}

// SetInterval changes the service-wide cadence. The value is clamped to
// [MinInterval, MaxInterval]; the running goroutine picks it up at once.
func (s *Sampler) SetInterval(d time.Duration) time.Duration {
	v := ClampInterval(d, s.cfg.MinInterval, s.cfg.MaxInterval)
	s.interval.Store(int64(v))
	s.mu.Lock()
	if s.running {
		select {
		case s.wakeCh <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	return v
}

// MinInterval returns the configured floor a caller may not undercut.
func (s *Sampler) MinInterval() time.Duration { return s.cfg.MinInterval }

// MaxInterval returns the configured ceiling.
func (s *Sampler) MaxInterval() time.Duration { return s.cfg.MaxInterval }

// MaxSubscribers returns the configured subscriber ceiling (0 = unlimited).
func (s *Sampler) MaxSubscribers() int { return s.cfg.MaxSubscribers }

// IncludeCPU reports whether the opt-in cpu.stat/pids.current extras are read.
func (s *Sampler) IncludeCPU() bool { return s.cfg.IncludeCPU }

// Counters returns a point-in-time copy of the sampler's work counters.
func (s *Sampler) Counters() Counters {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	var run int64
	if running {
		run = 1
	}
	return Counters{
		Ticks:         s.c.ticks.Load(),
		CgroupReads:   s.c.cgroupReads.Load(),
		StatfsReads:   s.c.statfsReads.Load(),
		Subscribers:   s.c.subscribers.Load(),
		Running:       run,
		DroppedFrames: s.c.dropped.Load(),
		Candidates:    int(s.c.candidates.Load()),
	}
}

// SetObserver installs a per-tick observer. It is called once per sample pass
// with the snapshot that pass already built, so the observer adds no reads of
// its own. It is not called for a one-shot SampleScoped.
func (s *Sampler) SetObserver(fn func(Snapshot)) {
	s.mu.Lock()
	s.observer = fn
	s.mu.Unlock()
}

// Pin keeps the sample loop alive with zero subscribers and starts it at once if
// needed. The write-rate watchdog pins the sampler so its tick continues with no
// watcher connected; Unpin restores the lazy stop with the last subscriber.
func (s *Sampler) Pin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pinned = true
	if s.stopped || s.running {
		return
	}
	s.startLocked()
}

// Unpin releases a Pin. The loop then stops if no subscriber remains.
func (s *Sampler) Unpin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pinned = false
	if s.running && len(s.subs) == 0 {
		s.stopLocked()
	}
}

// notify invokes the per-tick observer, if any, outside s.mu.
func (s *Sampler) notify(snap Snapshot) {
	s.mu.Lock()
	fn := s.observer
	s.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
}

// Subscribe registers a scoped stream and lazily starts the sampler. An
// optional per-request interval override adjusts the ONE service-wide cadence
// (there is a single ticker and a single cadence for every subscriber); the
// value is clamped to [MinInterval, MaxInterval], so a client can never drive
// the engine below the configured floor. The returned subscriber must be Closed
// by the caller on every exit path (the handler defers it), which is what stops
// the sampler when the last one leaves.
func (s *Sampler) Subscribe(scope Scope, override time.Duration) (*subscriber, error) {
	if override > 0 {
		s.SetInterval(override)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, errors.New("metrics sampler is shut down")
	}
	if s.cfg.MaxSubscribers > 0 && len(s.subs) >= s.cfg.MaxSubscribers {
		return nil, ErrTooManySubscribers
	}
	s.nextID++
	sub := &subscriber{id: s.nextID, scope: scope, ch: make(chan []byte, 1)}
	s.subs[sub.id] = sub
	s.c.subscribers.Store(int64(len(s.subs)))
	if !s.running {
		s.startLocked()
	}
	return sub, nil
}

// closeSub removes a subscriber from the registry and stops the sampler when it
// was the last one. It is idempotent. The channel close happens under s.mu, the
// same lock publish holds for its entire fan-out, so a send can never race a
// close (the send-on-closed-channel bug).
func (s *Sampler) closeSub(sub *subscriber) {
	s.mu.Lock()
	if _, ok := s.subs[sub.id]; ok {
		delete(s.subs, sub.id)
		s.c.subscribers.Store(int64(len(s.subs)))
		sub.closeMu.Do(func() { close(sub.ch) })
		if len(s.subs) == 0 && s.running && !s.pinned {
			s.stopLocked()
		}
	} else {
		sub.closeMu.Do(func() { close(sub.ch) })
	}
	s.mu.Unlock()
}

// Close unregisters a subscriber. It is safe to call more than once.
func (s *Sampler) Close(sub *subscriber) { s.closeSub(sub) }

// startLocked launches the sample loop. Caller holds s.mu.
func (s *Sampler) startLocked() {
	s.running = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.wakeCh = make(chan struct{}, 1)
	go s.run(s.stopCh, s.doneCh, s.wakeCh)
}

// stopLocked signals the loop to stop and restores the configured default
// cadence, so a per-request override never lingers for the next session (a
// stopped sampler has no cadence to carry). Caller holds s.mu.
func (s *Sampler) stopLocked() {
	s.running = false
	s.interval.Store(int64(s.defaultInterval))
	close(s.stopCh)
}

// Shutdown stops the sampler and drops every subscriber. Used at server
// shutdown so no goroutine is left behind. The channel closes happen under s.mu
// (the same lock publish holds for its fan-out), so a shutdown can never race
// an in-flight send.
func (s *Sampler) Shutdown() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	for _, sub := range s.subs {
		sub.closeMu.Do(func() { close(sub.ch) })
	}
	s.subs = map[uint64]*subscriber{}
	s.c.subscribers.Store(0)
	if s.running {
		s.stopLocked()
	}
	s.mu.Unlock()
}

// Wait blocks until the sample loop has stopped, bounded by d. It is used by a
// test to prove the last unsubscribe really stops the goroutine.
func (s *Sampler) Wait(d time.Duration) bool {
	s.mu.Lock()
	done := s.doneCh
	s.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// run is the single ticker loop. It publishes an initial frame immediately (so
// a client sees data at once), then one frame per interval.
func (s *Sampler) run(stop, done, wake chan struct{}) {
	defer close(done)
	s.publish()
	timer := time.NewTimer(time.Duration(s.interval.Load()))
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Duration(s.interval.Load()))
		case <-timer.C:
			s.publish()
			timer.Reset(time.Duration(s.interval.Load()))
		}
	}
}

// publish builds ONE snapshot (one statfs + the cgroup reads for live
// workspaces) for the whole tick, then fans a scope-filtered copy to every
// subscriber. The expensive work runs exactly once per tick regardless of how
// many subscribers are connected -- the property the gate proves.
//
// The fan-out (including the chunked channel sends) runs under s.mu, and
// closeSub/Shutdown close a subscriber's channel under the same lock, so a send
// can never race a close. The sends are non-blocking (select/default), so
// holding the lock is brief.
func (s *Sampler) publish() {
	s.c.ticks.Add(1)
	snap := s.sampleAll()
	s.fanOut(snap)
	s.notify(snap)
}

// fanOut delivers one snapshot to every subscriber, coalescing (never queueing)
// per subscriber. It holds s.mu, the same lock closeSub/Shutdown hold when they
// close a subscriber's channel, so a send can never race a close.
func (s *Sampler) fanOut(snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Cache the encoded frame per scope: several subscribers of the same owner
	// (or the same workspace) share one marshal, so the per-subscriber work is
	// just a channel send.
	cache := make(map[Scope][]byte, len(s.subs))
	for _, sub := range s.subs {
		frame, ok := cache[sub.scope]
		if !ok {
			frame = sseDataFrame(marshalScoped(snap, sub.scope))
			cache[sub.scope] = frame
		}
		select {
		case sub.ch <- frame:
		default:
			sub.dropped.Add(1)
			s.c.dropped.Add(1)
		}
	}
}

// marshalScoped renders the portion of a snapshot visible to a scope.
func marshalScoped(snap Snapshot, scope Scope) []byte {
	if scope == (Scope{}) {
		b, _ := json.Marshal(snap)
		return b
	}
	out := Snapshot{
		Timestamp:       snap.Timestamp,
		IntervalSeconds: snap.IntervalSeconds,
		Sampler:         snap.Sampler,
	}
	for _, w := range snap.Workspaces {
		if scope.matchesSnapshot(w) {
			out.Workspaces = append(out.Workspaces, w)
		}
	}
	if out.Workspaces == nil {
		out.Workspaces = []WorkspaceSnapshot{}
	}
	b, _ := json.Marshal(out)
	return b
}

// Sample builds one snapshot across every candidate workspace and advances the
// tick counter. It is the SAMPLER-LOOP entry point: only a running sampler tick
// advances Ticks, so a service with no subscriber has a frozen tick counter.
// It never walks a directory tree: disk is one statfs, RAM is one
// memory.current + one memory.max per live cgroup.
func (s *Sampler) Sample() Snapshot {
	s.c.ticks.Add(1)
	snap := s.sampleAll()
	s.notify(snap)
	return snap
}

// SampleScoped builds one snapshot restricted to a scope for a ONE-SHOT request.
// It does NOT advance the tick counter (a one-shot is not a sampler tick), so
// the lazy-start guarantee ("with zero subscribers the tick counter is frozen")
// holds even while one-shot clients poll. It uses the exact same sample pass as
// the stream, so the two can never disagree.
func (s *Sampler) SampleScoped(scope Scope) Snapshot {
	full := s.sampleAll()
	if scope == (Scope{}) {
		return full
	}
	out := Snapshot{
		Timestamp:       full.Timestamp,
		IntervalSeconds: full.IntervalSeconds,
		Sampler:         full.Sampler,
	}
	for _, w := range full.Workspaces {
		if scope.matchesSnapshot(w) {
			out.Workspaces = append(out.Workspaces, w)
		}
	}
	if out.Workspaces == nil {
		out.Workspaces = []WorkspaceSnapshot{}
	}
	return out
}

// sampleAll performs the actual reading (one statfs per candidate, the cgroup
// files for live workspaces). It advances the read counters but never Ticks.
func (s *Sampler) sampleAll() Snapshot {
	all := s.provider.Candidates()
	s.c.candidates.Store(int64(len(all)))
	out := make([]WorkspaceSnapshot, 0, len(all))
	for _, info := range all {
		out = append(out, s.sampleWorkspace(info))
	}
	return Snapshot{
		Timestamp:       time.Now().UTC(),
		IntervalSeconds: s.IntervalSeconds(),
		Workspaces:      out,
		Sampler:         s.Counters(),
	}
}

// sampleWorkspace reads the minimum for one workspace: one statfs for disk, and
// one memory.current per live cgroup PLUS (when a run or session is live) one
// read of the workspace parent cgroup's memory.max for the enforced ceiling. A
// workspace with no live cgroup does ZERO cgroup reads and reports RAM 0.
func (s *Sampler) sampleWorkspace(info WorkspaceInfo) WorkspaceSnapshot {
	used, ok := s.reader.DiskUsed(info.DiskRoot)
	s.c.statfsReads.Add(1)

	cgroups := make([]string, 0, len(info.RunCgroups)+len(info.SessionCgroups))
	cgroups = append(cgroups, info.RunCgroups...)
	cgroups = append(cgroups, info.SessionCgroups...)

	var ramUsed, cpuUsec, pids int64
	ramOK := true
	live := 0
	for _, dir := range cgroups {
		cur, err := s.reader.MemoryCurrent(dir)
		s.c.cgroupReads.Add(1)
		if err != nil {
			ramOK = false
			continue
		}
		ramUsed += cur
		live++
		if s.cfg.IncludeCPU {
			if u, err := s.reader.CPUUsageUsec(dir); err == nil {
				cpuUsec += u
			}
			s.c.cgroupReads.Add(1)
			if p, err := s.reader.PidsCurrent(dir); err == nil {
				pids += p
			}
			s.c.cgroupReads.Add(1)
		}
	}
	// The enforced ceiling is the workspace parent cgroup's memory.max. It is
	// read only while a run or session is live (an idle workspace must cost zero
	// cgroup reads); when there is no live cgroup, or no parent is known, the
	// configured memory_mb is reported -- the exact number the engine writes to
	// the parent, so the advertised and enforced figures are the same value.
	ramLimit := info.MemoryLimitBytes
	if live > 0 && info.ParentCgroup != "" {
		if m, err := s.reader.MemoryMax(info.ParentCgroup); err == nil && m > 0 {
			ramLimit = m
		}
		s.c.cgroupReads.Add(1)
	}
	return BuildWorkspaceSnapshot(info, used, info.DiskLimitBytes, ok,
		ramUsed, ramLimit, ramOK, len(info.RunCgroups), live, cpuUsec, pids, info.WriteRateLimitMBs)
}

// sseDataFrame encodes one SSE event carrying a JSON payload.
func sseDataFrame(payload []byte) []byte {
	buf := make([]byte, 0, len(payload)+8)
	buf = append(buf, "data: "...)
	buf = append(buf, payload...)
	buf = append(buf, '\n', '\n')
	return buf
}

// SSEDataFrame is the exported encoder used by the HTTP layer for one frame.
func SSEDataFrame(payload []byte) []byte { return sseDataFrame(payload) }

// SSEInitialComment is the comment line a stream writes first so an SSE client
// sees bytes immediately while the first tick is still pending.
const SSEInitialComment = ": connected\n\n"

// FormatOverride validates a per-request interval override against the
// configured floor. A value below the floor is refused rather than silently
// raised: a client asking for 10ms is told, not ignored. A value above the
// ceiling is clamped down (cheap is the promise).
func (s *Sampler) FormatOverride(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("invalid interval: " + raw)
	}
	if d <= 0 {
		return 0, errors.New("interval must be positive")
	}
	if d < s.cfg.MinInterval {
		return 0, errors.New("interval below the configured floor (" + s.cfg.MinInterval.String() + "): " + raw)
	}
	return ClampInterval(d, s.cfg.MinInterval, s.cfg.MaxInterval), nil
}

// ParseCounters reads a counter by name (used by the gate and tests).
func ParseCounters(c Counters, name string) int64 {
	switch name {
	case "ticks":
		return c.Ticks
	case "cgroup_reads":
		return c.CgroupReads
	case "statfs_reads":
		return c.StatfsReads
	case "subscribers":
		return c.Subscribers
	case "running":
		return c.Running
	case "dropped_frames":
		return c.DroppedFrames
	case "candidates":
		return int64(c.Candidates)
	}
	return -1
}
