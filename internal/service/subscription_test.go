package service

import (
	"testing"
	"time"

	"sandboxd/internal/config"
	"sandboxd/internal/executor"
	"sandboxd/internal/model"
)

// subTestService builds a service with an injected, mutable clock and the given
// subscription configuration, so a whole lifecycle is asserted in-memory with no
// sleeping and no daemon.
func subTestService(t *testing.T, mutate func(*config.Config)) (*Service, *executor.FakeRunner, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc, fake := newTestService(t)
	if mutate != nil {
		mutate(&svc.cfg)
	}
	svc.now = func() time.Time { return clock }
	return svc, fake, &clock
}

func mustCreateSub(t *testing.T, svc *Service, req SubscriptionRequest) *model.Workspace {
	t.Helper()
	w, err := svc.CreateWorkspaceWithSubscription("u", "w", "", nil, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return w
}

func strptr(s string) *string { return &s }

func TestCreateWithDurationSetsExpiry(t *testing.T) {
	svc, _, clock := subTestService(t, nil)
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("30d")})
	if w.Subscription == nil {
		t.Fatal("expected a subscription")
	}
	want := clock.Add(30 * 24 * time.Hour)
	if !w.Subscription.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", w.Subscription.ExpiresAt, want)
	}
	if !w.Subscription.GrantedAt.Equal(*clock) {
		t.Fatalf("granted_at = %v", w.Subscription.GrantedAt)
	}
	if w.Subscription.GraceEndsAt.Sub(w.Subscription.ExpiresAt) != 168*time.Hour {
		t.Fatalf("grace = %v", w.Subscription.GraceEndsAt.Sub(w.Subscription.ExpiresAt))
	}
	if w.Subscription.Extensions != 0 {
		t.Fatalf("extensions = %d", w.Subscription.Extensions)
	}
}

func TestCreateWithNoSubscriptionIsUnlimited(t *testing.T) {
	svc, _, _ := subTestService(t, nil)
	w := mustCreateSub(t, svc, SubscriptionRequest{})
	if w.Subscription != nil {
		t.Fatalf("expected nil subscription, got %+v", w.Subscription)
	}
	// The reaper never touches a workspace with no term.
	svc.ReapSubscriptions(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	got, err := svc.Store().GetWorkspace(w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.WorkspaceActive || got.Subscription != nil {
		t.Fatalf("unlimited workspace was touched: %+v", got)
	}
}

func TestCreateDefaultDurationConfig(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.DefaultDuration = "7d" })
	w := mustCreateSub(t, svc, SubscriptionRequest{})
	if w.Subscription == nil || !w.Subscription.ExpiresAt.Equal(clock.Add(7*24*time.Hour)) {
		t.Fatalf("default duration not applied: %+v", w.Subscription)
	}
}

func TestCreateWithAbsoluteExpiresAt(t *testing.T) {
	svc, _, clock := subTestService(t, nil)
	exp := clock.Add(48 * time.Hour)
	w := mustCreateSub(t, svc, SubscriptionRequest{ExpiresAt: &exp})
	if w.Subscription == nil || !w.Subscription.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at = %+v", w.Subscription)
	}
}

func TestCreateSubscriptionValidation(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.MaxDuration = "365d" })
	cases := []struct {
		name string
		req  SubscriptionRequest
		code string
	}{
		{"zero duration", SubscriptionRequest{Duration: strptr("0d")}, model.CodeSubscriptionInvalid},
		{"negative duration", SubscriptionRequest{Duration: strptr("-1d")}, model.CodeSubscriptionInvalid},
		{"unparseable duration", SubscriptionRequest{Duration: strptr("banana")}, model.CodeSubscriptionInvalid},
		{"bare number", SubscriptionRequest{Duration: strptr("30")}, model.CodeSubscriptionInvalid},
		{"both fields", SubscriptionRequest{Duration: strptr("1d"), ExpiresAt: timePtr(clock.Add(time.Hour))}, model.CodeSubscriptionInvalid},
		{"past expires_at", SubscriptionRequest{ExpiresAt: timePtr(clock.Add(-time.Hour))}, model.CodeSubscriptionInvalid},
		{"now expires_at", SubscriptionRequest{ExpiresAt: timePtr(*clock)}, model.CodeSubscriptionInvalid},
		{"over max", SubscriptionRequest{Duration: strptr("400d")}, model.CodeMaxDurationExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.CreateWorkspaceWithSubscription("u", "w", "", nil, c.req)
			if err == nil {
				t.Fatalf("expected error")
			}
			if got := model.AsError(err).Code; got != c.code {
				t.Fatalf("code = %q, want %q", got, c.code)
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestExtendWhileLiveAddsForward(t *testing.T) {
	svc, _, clock := subTestService(t, nil)
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("30d")})
	original := w.Subscription.ExpiresAt
	// Move a little way into the term and renew early.
	*clock = clock.Add(10 * 24 * time.Hour)

	updated, err := svc.ExtendSubscription(w.ID, SubscriptionRequest{Duration: strptr("30d")})
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	want := original.Add(30 * 24 * time.Hour)
	if !updated.Subscription.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v (must ADD, never move backwards)", updated.Subscription.ExpiresAt, want)
	}
	if !updated.Subscription.ExpiresAt.After(original) {
		t.Fatal("expiry must move FORWARD")
	}
	if updated.Subscription.Extensions != 1 {
		t.Fatalf("extensions = %d, want 1", updated.Subscription.Extensions)
	}
	if !updated.Subscription.RenewedAt.Equal(*clock) {
		t.Fatalf("renewed_at = %v", updated.Subscription.RenewedAt)
	}
}

func TestExtendLiveWithEarlierExpiresAtRefused(t *testing.T) {
	svc, _, clock := subTestService(t, nil)
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("30d")})
	*clock = clock.Add(time.Hour)
	// An absolute expiry earlier than the current one: refuse, never shorten.
	earlier := w.Subscription.ExpiresAt.Add(-time.Hour)
	_, err := svc.ExtendSubscription(w.ID, SubscriptionRequest{ExpiresAt: &earlier})
	if err == nil || model.AsError(err).Code != model.CodeSubscriptionWouldShorten {
		t.Fatalf("err = %v, want would_shorten", err)
	}
}

func TestExtendLiveWithLaterExpiresAtAccepted(t *testing.T) {
	svc, _, clock := subTestService(t, nil)
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("30d")})
	*clock = clock.Add(time.Hour)
	later := w.Subscription.ExpiresAt.Add(10 * 24 * time.Hour)
	updated, err := svc.ExtendSubscription(w.ID, SubscriptionRequest{ExpiresAt: &later})
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if !updated.Subscription.ExpiresAt.Equal(later) {
		t.Fatalf("expiry = %v, want %v", updated.Subscription.ExpiresAt, later)
	}
}

func TestReaperLocksAtExpiry(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.GracePeriod = "3s" })
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("3s")})

	// Not yet expired: nothing happens.
	svc.ReapSubscriptions(*clock)
	if got, _ := svc.Store().GetWorkspace(w.ID); got.Status != model.WorkspaceActive {
		t.Fatalf("locked too early: %s", got.Status)
	}

	// Exactly at expiry: lock.
	*clock = w.Subscription.ExpiresAt
	svc.ReapSubscriptions(*clock)
	got, _ := svc.Store().GetWorkspace(w.ID)
	if got.Status != model.WorkspaceLocked {
		t.Fatalf("status = %s, want locked", got.Status)
	}
	if got.Subscription.LockedAt.IsZero() {
		t.Fatal("locked_at not recorded")
	}
	// A locked workspace refuses compute with the stable code.
	if _, err := svc.Exec(got, ExecSpec{Cmd: []string{"x"}}); model.AsError(err).Code != model.CodeSubscriptionExpired {
		t.Fatalf("exec on locked: %v", err)
	}
}

func TestReaperDeleteAfterGrace(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.GracePeriod = "3s" })
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("3s")})
	*clock = w.Subscription.ExpiresAt
	svc.ReapSubscriptions(*clock)
	// One nanosecond before grace end: still locked, not deleted.
	*clock = w.Subscription.GraceEndsAt.Add(-time.Nanosecond)
	svc.ReapSubscriptions(*clock)
	if _, err := svc.Store().GetWorkspace(w.ID); err != nil {
		t.Fatalf("deleted before grace end: %v", err)
	}
	// Exactly at grace end: deleted.
	*clock = w.Subscription.GraceEndsAt
	svc.ReapSubscriptions(*clock)
	if _, err := svc.Store().GetWorkspace(w.ID); err == nil {
		t.Fatal("workspace should be deleted at grace end")
	}
}

func TestExtendWhileLockedUnlocks(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.GracePeriod = "168h" })
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("3s")})
	*clock = w.Subscription.ExpiresAt
	svc.ReapSubscriptions(*clock)
	locked, _ := svc.Store().GetWorkspace(w.ID)
	if locked.Status != model.WorkspaceLocked {
		t.Fatalf("not locked: %s", locked.Status)
	}

	// The renewal flow: extend while locked starts from NOW and unlocks.
	updated, err := svc.ExtendSubscription(w.ID, SubscriptionRequest{Duration: strptr("30d")})
	if err != nil {
		t.Fatalf("extend while locked: %v", err)
	}
	if updated.Status != model.WorkspaceActive {
		t.Fatalf("status = %s, want active", updated.Status)
	}
	if !updated.Subscription.LockedAt.IsZero() {
		t.Fatal("locked_at must be cleared on renewal")
	}
	if !updated.Subscription.ExpiresAt.Equal(clock.Add(30 * 24 * time.Hour)) {
		t.Fatalf("expiry = %v, want now+30d", updated.Subscription.ExpiresAt)
	}
	// Service is restored.
	if _, err := svc.Exec(updated, ExecSpec{Cmd: []string{"x"}}); err != nil {
		t.Fatalf("exec after renewal: %v", err)
	}
}

func TestExtendDeletedFails(t *testing.T) {
	svc, _, _ := subTestService(t, nil)
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("1d")})
	if err := svc.DeleteWorkspace(w.ID, w.OwnerID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := svc.ExtendSubscription(w.ID, SubscriptionRequest{Duration: strptr("1d")})
	if err == nil {
		t.Fatal("extending a deleted workspace must fail")
	}
	if _, gerr := svc.Store().GetWorkspace(w.ID); gerr == nil {
		t.Fatal("extension must never resurrect a deleted workspace")
	}
}

func TestReaperIdempotent(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.GracePeriod = "3s" })
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("3s")})
	*clock = w.Subscription.ExpiresAt
	// Run the lock tick twice: the second is a no-op and must not error or
	// change the recorded lock instant.
	svc.ReapSubscriptions(*clock)
	first, _ := svc.Store().GetWorkspace(w.ID)
	svc.ReapSubscriptions(*clock)
	second, _ := svc.Store().GetWorkspace(w.ID)
	if !first.Subscription.LockedAt.Equal(second.Subscription.LockedAt) {
		t.Fatalf("lock not idempotent: %v vs %v", first.Subscription.LockedAt, second.Subscription.LockedAt)
	}
	// Delete tick twice: the second is a no-op (already gone) and must not panic.
	*clock = w.Subscription.GraceEndsAt
	svc.ReapSubscriptions(*clock)
	svc.ReapSubscriptions(*clock)
	if _, err := svc.Store().GetWorkspace(w.ID); err == nil {
		t.Fatal("workspace should be gone")
	}
}

func TestReaperBackwardsClockDoesNotDelete(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.GracePeriod = "3s" })
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("3s")})
	// Force a locked workspace whose recorded LockedAt is in the FUTURE relative
	// to the moment we then evaluate at: this is exactly a wall clock that went
	// backwards after the lock. The reaper must NOT delete on that reading.
	lockedInstant := w.Subscription.ExpiresAt.Add(time.Hour)
	if _, err := svc.Store().UpdateWorkspace(w.ID, func(ws *model.Workspace) error {
		ws.Status = model.WorkspaceLocked
		ws.Subscription.LockedAt = lockedInstant
		return nil
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// now is past the grace end (a delete would normally fire) but BEFORE the
	// recorded lock instant -> must be refused.
	*clock = w.Subscription.ExpiresAt.Add(10 * time.Second)
	if !clock.Before(lockedInstant) {
		t.Fatal("test setup: clock should be before locked instant")
	}
	svc.ReapSubscriptions(*clock)
	if _, err := svc.Store().GetWorkspace(w.ID); err != nil {
		t.Fatalf("deleted on a backwards clock reading: %v", err)
	}
}

func TestReaperSkipsSuspended(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.GracePeriod = "3s" })
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("3s")})
	if _, _, err := svc.SetSuspended(w.ID, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	*clock = w.Subscription.GraceEndsAt.Add(time.Hour)
	svc.ReapSubscriptions(*clock)
	got, err := svc.Store().GetWorkspace(w.ID)
	if err != nil {
		t.Fatalf("suspended workspace was deleted: %v", err)
	}
	if got.Status != model.WorkspaceSuspended {
		t.Fatalf("status = %s, want suspended", got.Status)
	}
}

func TestSummaryDerivationAgreesWithLock(t *testing.T) {
	svc, _, clock := subTestService(t, func(c *config.Config) { c.Subscription.GracePeriod = "3s" })
	w := mustCreateSub(t, svc, SubscriptionRequest{Duration: strptr("3s")})
	// Before expiry: active/expiring_soon, remaining > 0, not locked.
	sum := svc.Summary(w, *clock)
	if sum.State == "locked" || sum.Status == model.WorkspaceLocked {
		t.Fatalf("early summary says locked: %+v", sum)
	}
	// Exactly at expiry: the summary reports locked AND remaining 0, and the
	// workspace must actually be refusing compute (the numbers agree with
	// enforcement: the summary and the gate read the same clock).
	*clock = w.Subscription.ExpiresAt
	sum = svc.Summary(w, *clock)
	if sum.State != "locked" || sum.Subscription.RemainingSec != 0 || sum.Status != model.WorkspaceLocked {
		t.Fatalf("at expiry summary = %+v", sum)
	}
	_, err := svc.Exec(w, ExecSpec{Cmd: []string{"x"}})
	if err == nil || model.AsError(err).Code != model.CodeSubscriptionExpired {
		t.Fatalf("summary says locked but exec not refused: %v", err)
	}
}
