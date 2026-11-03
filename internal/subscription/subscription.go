// Package subscription holds the pure, clock-injected parts of the workspace
// subscription feature: the duration grammar parser, the reaper's decision
// function, and the derived summary. Nothing here touches the filesystem, the
// network or a running daemon, so every rule is unit-testable without sleeping
// for days and cannot drift from what the reaper enforces.
//
// Architecture: the engine owns the CLOCK and the ENFORCEMENT and stores an
// ABSOLUTE expiry instant; the client (a separate billing product) owns WHY a
// term is what it is. A renewal is the client sending a new/extended expiry.
package subscription

import (
	"fmt"
	"strings"
	"time"

	"sandboxd/internal/model"
)

// Fixed-length unit approximations. s/m/h are exact; d and w are exact
// multiples of 24h; mo and y are FIXED-LENGTH APPROXIMATIONS (30 and 365 days)
// and are NEVER calendar-exact. This is documented in docs/DECISIONS.md
// (ADR 88): a client that needs calendar months must send an absolute
// expires_at instead.
const (
	Second = time.Second
	Minute = time.Minute
	Hour   = time.Hour
	Day    = 24 * time.Hour
	Week   = 7 * Day
	// MonthApprox is 30 days, NOT a calendar month.
	MonthApprox = 30 * Day
	// YearApprox is 365 days, NOT a calendar year.
	YearApprox = 365 * Day
)

// maxDuration bounds a parsed duration so a typo cannot overflow time.Duration
// (about 292 years) or produce an absurd term. 100 years is generous.
const maxDuration = 100 * YearApprox

// units are matched longest-suffix-first so "mo" is a month and never "m"+"o".
// A trailing "o" is never a valid unit on its own.
var units = []struct {
	suffix string
	dur    time.Duration
}{
	{"mo", MonthApprox},
	{"s", Second},
	{"m", Minute},
	{"h", Hour},
	{"d", Day},
	{"w", Week},
	{"y", YearApprox},
}

// ParseDuration parses the duration grammar: one or more "<number><unit>" pairs
// summed together, e.g. "30d", "1w", "12h30m", "1d12h". Units are
// s, m (minute), h, d (24h), w (7d), mo (30d, approximate) and y (365d,
// approximate). A bare integer is NOT accepted: "30" is ambiguous (seconds?
// minutes?) and every operator example names a unit, so a unit suffix is
// REQUIRED (documented in ADR 88). Zero, negative, fractional and unparseable
// durations are rejected. The result is always > 0 and <= maxDuration.
func ParseDuration(s string) (time.Duration, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, fmt.Errorf("duration is empty")
	}
	var total time.Duration
	i := 0
	pairs := 0
	for i < len(in) {
		// A number is one or more ASCII digits. A leading sign, a decimal point
		// or any other character is rejected here (zero/negative included).
		start := i
		for i < len(in) && in[i] >= '0' && in[i] <= '9' {
			i++
		}
		if i == start {
			return 0, fmt.Errorf("duration %q: expected a number at position %d", s, start)
		}
		numStr := in[start:i]
		// Match the longest known unit suffix at i.
		var dur time.Duration
		matched := false
		for _, u := range units {
			if strings.HasPrefix(in[i:], u.suffix) {
				dur = u.dur
				i += len(u.suffix)
				matched = true
				break
			}
		}
		if !matched {
			return 0, fmt.Errorf("duration %q: unknown unit after %q (use s, m, h, d, w, mo or y)", s, numStr)
		}
		var n int64
		for _, c := range numStr {
			n = n*10 + int64(c-'0')
			if n > int64(maxDuration/time.Second) {
				return 0, fmt.Errorf("duration %q: too large", s)
			}
		}
		part := time.Duration(n) * dur
		if part < 0 { // overflow guard
			return 0, fmt.Errorf("duration %q: too large", s)
		}
		total += part
		pairs++
	}
	if pairs == 0 || total <= 0 {
		return 0, fmt.Errorf("duration %q: must be positive", s)
	}
	if total > maxDuration {
		return 0, fmt.Errorf("duration %q: exceeds the maximum of 100 years", s)
	}
	return total, nil
}

// Action is what the reaper must do for one workspace on a tick.
type Action int

const (
	// ActionNone: the term is live or there is no term. Do nothing.
	ActionNone Action = iota
	// ActionLock: the term has lapsed but the grace period has not ended. Lock
	// (idempotent) so the workspace stops consuming resources, keeping data.
	ActionLock
	// ActionDelete: the grace period has ended. Delete the workspace.
	ActionDelete
)

// String renders an action for logs and tests.
func (a Action) String() string {
	switch a {
	case ActionLock:
		return "lock"
	case ActionDelete:
		return "delete"
	default:
		return "none"
	}
}

// Decision is the reaper's verdict for one workspace.
type Decision struct {
	Action Action
	Reason string
}

// Decide is the PURE reaper decision: a function of (now, subscription, grace)
// and nothing else. It is the single source of truth the loop applies, so it can
// be exhaustively unit-tested with injected clocks.
//
// Rules:
//   - No expiry (nil or zero ExpiresAt): never act ("unlimited").
//   - now < ExpiresAt: term live, never act.
//   - ExpiresAt <= now < ExpiresAt+grace: Lock.
//   - now >= ExpiresAt+grace: Delete.
//
// Backwards wall-clock rule: a backwards jump can only DELAY an action, never
// cause a delete, because every comparison is against a stored absolute instant
// and never against an in-memory countdown. To make that explicit and robust, a
// delete is REFUSED while now is before the recorded LockedAt (a non-monotonic
// reading): the workspace stays locked (service is already stopped) and is
// deleted on a later, monotonic tick. grace <= 0 means "delete immediately at
// expiry" and is only reachable when an operator configures it that way.
func Decide(now time.Time, sub *model.Subscription, grace time.Duration) Decision {
	if !sub.HasExpiry() {
		return Decision{ActionNone, "no expiry"}
	}
	expires := sub.ExpiresAt
	if now.Before(expires) {
		return Decision{ActionNone, "term live"}
	}
	graceEnds := sub.GraceEndsAt
	if graceEnds.IsZero() {
		graceEnds = expires.Add(grace)
	}
	if now.Before(graceEnds) {
		return Decision{ActionLock, "term lapsed"}
	}
	if !sub.LockedAt.IsZero() && now.Before(sub.LockedAt) {
		// Non-monotonic reading: the wall clock is behind the instant this
		// workspace was already locked. Never delete on that basis.
		return Decision{ActionLock, "backwards clock; delete deferred"}
	}
	return Decision{ActionDelete, "grace ended"}
}

// State is the derived summary state of a workspace's term.
const (
	StateUnlimited    = "unlimited"
	StateActive       = "active"
	StateExpiringSoon = "expiring_soon"
	StateLocked       = "locked"
)

// expiringSoonWindow returns the warning window: 10% of the granted term,
// capped at 24h. It never has a floor, so a short term is not reported as
// expiring for its whole life. Exposed for the unit test and the docs.
func expiringSoonWindow(term time.Duration) time.Duration {
	w := term / 10
	if w > 24*time.Hour {
		w = 24 * time.Hour
	}
	return w
}

// Derivations returns the cheap, derived numbers for a workspace's term at now.
// It performs no I/O: every value comes from the stored subscription and now.
// remaining_sec is clamped at 0 and NEVER negative; state disambiguates a 0 that
// means "no limit" (unlimited) from a 0 that means "expired" (locked) from a 0
// that merely means "the last instant" (active with < 1s left, reported 0).
func Derivations(now time.Time, sub *model.Subscription) (state string, expiresAt time.Time, remainingSec int64, graceEndsAt, lockedAt time.Time, extensions int) {
	if !sub.HasExpiry() {
		return StateUnlimited, time.Time{}, 0, time.Time{}, time.Time{}, 0
	}
	// term drives the expiring-soon window. GraceEndsAt is persisted by the
	// engine at grant/extension; a record without it is only possible for a
	// hand-edited file, and the zero value is reported as-is.
	term := sub.ExpiresAt.Sub(sub.GrantedAt)
	graceEnds := sub.GraceEndsAt
	locked := !sub.LockedAt.IsZero() || !now.Before(sub.ExpiresAt)
	if locked {
		return StateLocked, sub.ExpiresAt, 0, graceEnds, sub.LockedAt, sub.Extensions
	}
	remaining := sub.ExpiresAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	st := StateActive
	if term > 0 && remaining <= expiringSoonWindow(term) {
		st = StateExpiringSoon
	}
	return st, sub.ExpiresAt, int64(remaining / time.Second), graceEnds, sub.LockedAt, sub.Extensions
}
