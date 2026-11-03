package subscription

import (
	"testing"
	"time"

	"sandboxd/internal/model"
)

func TestParseDurationValid(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1s", time.Second},
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"12h", 12 * time.Hour},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1mo", 30 * 24 * time.Hour},
		{"1y", 365 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"1w2d3h4m5s", 7*24*time.Hour + 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second},
		{"12h30m", 12*time.Hour + 30*time.Minute},
		{" 3s ", 3 * time.Second},
		{"100d", 100 * 24 * time.Hour},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Fatalf("ParseDuration(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationInvalid(t *testing.T) {
	// Every rejected input names a real mistake: empty, zero, negative,
	// fractional, no unit, unknown unit, a bare number (ambiguous), or garbage.
	for _, in := range []string{
		"", "  ", "0", "0s", "0d", "-1d", "-1s", "1.5h", "5", "30",
		"d", "d1", "1x", "1mo2", "1dd", "abc", "1s-", "1 s", "٥د", "1h1",
	} {
		if got, err := ParseDuration(in); err == nil {
			t.Fatalf("ParseDuration(%q) = %v, want error", in, got)
		}
	}
}

// TestParseDurationZeroSumIsRejected covers "0d0h": every pair parses but the
// sum is zero, which must be rejected as a non-positive term.
func TestParseDurationZeroSumIsRejected(t *testing.T) {
	for _, in := range []string{"0d0h", "0s0m", "0y"} {
		if got, err := ParseDuration(in); err == nil {
			t.Fatalf("ParseDuration(%q) = %v, want error", in, got)
		}
	}
}

// TestParseDurationOverflow guards the ceiling: an absurd term is refused
// rather than overflowing time.Duration.
func TestParseDurationOverflow(t *testing.T) {
	if _, err := ParseDuration("999999999999y"); err == nil {
		t.Fatal("expected overflow/too-large error")
	}
	if _, err := ParseDuration("101y"); err == nil {
		t.Fatal("101y exceeds the 100y ceiling and must be refused")
	}
	if d, err := ParseDuration("100y"); err != nil || d != 100*YearApprox {
		t.Fatalf("100y should be accepted: %v %v", d, err)
	}
}

func sub(expiresAt time.Time, grace time.Duration) *model.Subscription {
	return &model.Subscription{
		GrantedAt:   expiresAt.Add(-24 * time.Hour),
		ExpiresAt:   expiresAt,
		GraceEndsAt: expiresAt.Add(grace),
	}
}

func TestDecideBoundaries(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	grace := 168 * time.Hour
	s := sub(base, grace)

	// No subscription -> never act.
	if d := Decide(base.Add(1000*time.Hour), nil, grace); d.Action != ActionNone {
		t.Fatalf("nil subscription: %+v", d)
	}
	// Zero expiry -> unlimited, never act.
	zero := &model.Subscription{}
	if d := Decide(base.Add(1000*time.Hour), zero, grace); d.Action != ActionNone {
		t.Fatalf("zero expiry: %+v", d)
	}
	// One nanosecond before expiry -> live.
	if d := Decide(base.Add(-time.Nanosecond), s, grace); d.Action != ActionNone {
		t.Fatalf("just before expiry: %+v", d)
	}
	// EXACTLY at expiry -> locked (now >= expires_at is expired).
	if d := Decide(base, s, grace); d.Action != ActionLock {
		t.Fatalf("exactly at expiry: %+v", d)
	}
	// One nanosecond before grace ends -> locked.
	if d := Decide(base.Add(grace-time.Nanosecond), s, grace); d.Action != ActionLock {
		t.Fatalf("just before grace end: %+v", d)
	}
	// EXACTLY at grace end -> delete.
	if d := Decide(base.Add(grace), s, grace); d.Action != ActionDelete {
		t.Fatalf("exactly at grace end: %+v", d)
	}
	// Well past grace -> delete.
	if d := Decide(base.Add(grace+1000*time.Hour), s, grace); d.Action != ActionDelete {
		t.Fatalf("well past grace: %+v", d)
	}
}

// TestDecideBackwardsClockNeverDeletes is the explicit backwards-clock rule:
// after a workspace was locked, a wall clock reading BEFORE the recorded lock
// instant must not delete it (the workspace stays locked and is deleted on a
// later monotonic tick).
func TestDecideBackwardsClockNeverDeletes(t *testing.T) {
	expires := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	grace := time.Hour
	lockedAt := expires.Add(2 * grace) // locked long after grace ended
	s := &model.Subscription{
		GrantedAt:   expires.Add(-time.Hour),
		ExpiresAt:   expires,
		GraceEndsAt: expires.Add(grace),
		LockedAt:    lockedAt,
	}
	// now is before LockedAt (a backwards jump): never delete.
	if d := Decide(expires.Add(grace), s, grace); d.Action != ActionLock {
		t.Fatalf("backwards clock: got %+v, want lock", d)
	}
	// The lock instant is the boundary: at exactly LockedAt a delete is allowed
	// again (the reading is monotonic).
	if d := Decide(lockedAt, s, grace); d.Action != ActionDelete {
		t.Fatalf("at lock instant: got %+v, want delete", d)
	}
}

func TestDecideGraceFromStoredGraceEndsAt(t *testing.T) {
	expires := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// GraceEndsAt stored as 168h, but caller passes a different grace; the
	// stored instant wins when present.
	s := &model.Subscription{
		GrantedAt:   expires.Add(-time.Hour),
		ExpiresAt:   expires,
		GraceEndsAt: expires.Add(168 * time.Hour),
	}
	if d := Decide(expires.Add(200*time.Hour), s, time.Minute); d.Action != ActionDelete {
		t.Fatalf("stored grace past: %+v", d)
	}
}

func TestDerivationsUnlimited(t *testing.T) {
	now := time.Now()
	state, exp, rem, graceEnds, locked, ext := Derivations(now, nil)
	if state != StateUnlimited || !exp.IsZero() || rem != 0 || !graceEnds.IsZero() || !locked.IsZero() || ext != 0 {
		t.Fatalf("unlimited derivations: %s %v %d %v %v %d", state, exp, rem, graceEnds, locked, ext)
	}
}

func TestDerivationsStates(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// A 10-day term; expiring-soon window is 24h (10% = 24h, capped at 24h).
	term := 10 * 24 * time.Hour
	s := &model.Subscription{GrantedAt: now, ExpiresAt: now.Add(term), GraceEndsAt: now.Add(term).Add(168 * time.Hour)}

	// Mid-term, active, remaining > window.
	state, _, rem, _, _, _ := Derivations(now.Add(24*time.Hour), s)
	if state != StateActive {
		t.Fatalf("state = %s, want active", state)
	}
	if rem <= 0 {
		t.Fatalf("remaining = %d, want > 0", rem)
	}

	// One second inside the window (now = expires - 24h + 1s).
	at := now.Add(term - 24*time.Hour + time.Second)
	state, _, rem, _, _, _ = Derivations(at, s)
	if state != StateExpiringSoon {
		t.Fatalf("at window edge state = %s, want expiring_soon", state)
	}
	if rem != int64(24*time.Hour-time.Second)/int64(time.Second) {
		t.Fatalf("remaining = %d", rem)
	}

	// EXACTLY at expiry: locked, remaining clamped to 0 (never negative).
	state, _, rem, _, _, _ = Derivations(s.ExpiresAt, s)
	if state != StateLocked || rem != 0 {
		t.Fatalf("at expiry: state=%s rem=%d, want locked/0", state, rem)
	}

	// Past expiry: locked, remaining still 0 (never negative).
	state, _, rem, _, _, _ = Derivations(s.ExpiresAt.Add(time.Hour), s)
	if state != StateLocked || rem != 0 {
		t.Fatalf("past expiry: state=%s rem=%d, want locked/0", state, rem)
	}

	// A recorded LockedAt reports locked even before expiry (e.g. after a
	// manual lock) and grace end is reported.
	s2 := &model.Subscription{GrantedAt: now, ExpiresAt: now.Add(term), GraceEndsAt: now.Add(term).Add(168 * time.Hour), LockedAt: now.Add(time.Hour)}
	state, _, rem, grace, locked, _ := Derivations(now.Add(2*time.Hour), s2)
	if state != StateLocked || rem != 0 || locked.IsZero() || grace.IsZero() {
		t.Fatalf("recorded lock: state=%s rem=%d grace=%v locked=%v", state, rem, grace, locked)
	}
}

// TestRemainingZeroImpliesLocked pins the hard invariant: a live term NEVER
// reports remaining_sec 0, so "remaining_sec == 0" always means locked.
func TestRemainingZeroImpliesLocked(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s := &model.Subscription{GrantedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	// A live term in its final HALF second must round up to 1, not floor to 0.
	state, _, rem, _, _, _ := Derivations(s.ExpiresAt.Add(-500*time.Millisecond), s)
	if state == StateLocked {
		t.Fatalf("live term reported locked before expiry")
	}
	if rem != 1 {
		t.Fatalf("remaining = %d, want 1 (never 0 while live)", rem)
	}
	// Exactly at expiry -> locked, 0.
	state, _, rem, _, _, _ = Derivations(s.ExpiresAt, s)
	if state != StateLocked || rem != 0 {
		t.Fatalf("at expiry state=%s rem=%d, want locked/0", state, rem)
	}
}

// TestDerivationsShortTermNotAlwaysExpiringSoon checks the window has no floor:
// a 10s term is not "expiring_soon" for its whole life.
func TestDerivationsShortTermNotAlwaysExpiringSoon(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s := &model.Subscription{GrantedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	// Halfway through, active (window is 1s).
	state, _, rem, _, _, _ := Derivations(now.Add(5*time.Second), s)
	if state != StateActive || rem != 5 {
		t.Fatalf("short term mid: state=%s rem=%d, want active/5", state, rem)
	}
	// Inside the last second: expiring_soon.
	state, _, _, _, _, _ = Derivations(now.Add(10*time.Second-500*time.Millisecond), s)
	if state != StateExpiringSoon {
		t.Fatalf("short term near end: state=%s, want expiring_soon", state)
	}
}

func TestExpiringSoonWindow(t *testing.T) {
	if got := expiringSoonWindow(10 * time.Hour); got != time.Hour {
		t.Fatalf("10%% of 10h = %v, want 1h", got)
	}
	if got := expiringSoonWindow(10 * 24 * time.Hour); got != 24*time.Hour {
		t.Fatalf("10%% of 10d capped = %v, want 24h", got)
	}
	if got := expiringSoonWindow(time.Minute); got != 6*time.Second {
		t.Fatalf("10%% of 1m = %v, want 6s", got)
	}
}

// TestApproximationConstants pins mo/y to the documented fixed-length values.
func TestApproximationConstants(t *testing.T) {
	if MonthApprox != 30*24*time.Hour {
		t.Fatalf("MonthApprox = %v", MonthApprox)
	}
	if YearApprox != 365*24*time.Hour {
		t.Fatalf("YearApprox = %v", YearApprox)
	}
}
