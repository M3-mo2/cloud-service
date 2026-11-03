package service

import (
	"fmt"
	"time"

	"sandboxd/internal/model"
	"sandboxd/internal/subscription"
	"sandboxd/internal/workspace"
)

// Subscription terms are enforced by a single periodic reaper, started from
// main beside the disk watchdog, whose decision is a PURE function of
// (now, subscription, config) in internal/subscription. This file owns the
// clock-injected loop and the effects (lock, delete, extend, grant) and
// deliberately holds NO in-memory term state: every value is recomputed from the
// absolute instants stored on the workspace, so a restart is trivially correct.

// ReaperMarker is an opaque build marker embedded in the binary so an acceptance
// harness can `strings` it and prove the tested artifact is a fresh build of the
// intended commit (a positive control against a stale binary). It names no
// engine internals and is only ever written to the operator's log.
const ReaperMarker = "zcp-subscription-reaper-v0.0.19"

// SubscriptionRequest is the parsed create/extend body: exactly one of duration
// or expires_at. A pointer distinguishes an absent field from an empty one.
type SubscriptionRequest struct {
	Duration  *string
	ExpiresAt *time.Time
}

// Empty reports whether the request names no term at all.
func (r SubscriptionRequest) Empty() bool { return r.Duration == nil && r.ExpiresAt == nil }

// Now returns the service clock reading (UTC). It is overridable in tests so a
// whole lifecycle can be asserted without sleeping.
func (s *Service) Now() time.Time { return s.now().UTC() }

// grantSubscription builds the Subscription a workspace gets at create time.
// It returns (nil, nil) when no term is wanted (no request and no configured
// default), which is the unchanged behaviour for a workspace that does not ask.
func (s *Service) grantSubscription(now time.Time, req SubscriptionRequest) (*model.Subscription, error) {
	if req.Empty() {
		d, ok, err := s.cfg.Subscription.DefaultDurationValue()
		if err != nil {
			return nil, model.Wrap(422, model.CodeSubscriptionInvalid,
				"server default_duration is not parseable: "+err.Error(), map[string]string{"config": "subscription.default_duration"})
		}
		if !ok {
			return nil, nil
		}
		max, _ := s.cfg.Subscription.MaxDurationValue()
		if d > max {
			d = max
		}
		return s.newSubscription(now, now.Add(d))
	}
	exp, err := s.computeExpiry(now, nil, req)
	if err != nil {
		return nil, err
	}
	return s.newSubscription(now, exp)
}

// newSubscription stamps the derived fields on a freshly granted term.
func (s *Service) newSubscription(now, expiresAt time.Time) (*model.Subscription, error) {
	grace, err := s.cfg.Subscription.GracePeriodValue()
	if err != nil {
		return nil, model.Wrap(500, model.CodeInternal, "server grace_period is not parseable", nil)
	}
	return &model.Subscription{
		GrantedAt:   now,
		ExpiresAt:   expiresAt,
		GraceEndsAt: expiresAt.Add(grace),
	}, nil
}

// computeExpiry resolves the absolute expiry for a create (current == nil) or an
// extension (current is the live/lapsed term). It enforces the documented rules:
//
//   - duration and expires_at are mutually exclusive;
//   - a duration must parse and be positive; expires_at must be in the future;
//   - while the term is LIVE a duration ADDS to the existing expiry (additive
//     renewal) and an expires_at earlier than the current one is refused
//     (never shorten a paid term);
//   - after it lapsed an extension starts from NOW;
//   - the result may not land further than max_duration from now.
func (s *Service) computeExpiry(now time.Time, current *model.Subscription, req SubscriptionRequest) (time.Time, error) {
	if req.Duration != nil && req.ExpiresAt != nil {
		return time.Time{}, model.Wrap(422, model.CodeSubscriptionInvalid,
			"provide either duration or expires_at, not both", nil)
	}
	max, err := s.cfg.Subscription.MaxDurationValue()
	if err != nil {
		max = 365 * 24 * time.Hour
	}
	live := current.HasExpiry() && current.LockedAt.IsZero() && now.Before(current.ExpiresAt)

	if req.ExpiresAt != nil {
		exp := *req.ExpiresAt
		if !exp.After(now) {
			return time.Time{}, model.Wrap(422, model.CodeSubscriptionInvalid,
				"expires_at must be in the future",
				map[string]string{"field": "expires_at", "expires_at": exp.UTC().Format(time.RFC3339)})
		}
		if exp.Sub(now) > max {
			return time.Time{}, maxDurationExceeded(max)
		}
		if live && exp.Before(current.ExpiresAt) {
			return time.Time{}, model.Wrap(422, model.CodeSubscriptionWouldShorten,
				"expires_at is earlier than the current expiry; a live term is never shortened",
				map[string]string{
					"field":           "expires_at",
					"current_expires": current.ExpiresAt.UTC().Format(time.RFC3339),
					"requested":       exp.UTC().Format(time.RFC3339),
				})
		}
		return exp, nil
	}

	if req.Duration == nil {
		return time.Time{}, model.Wrap(422, model.CodeSubscriptionInvalid,
			"a duration or an expires_at is required", nil)
	}
	d, perr := subscription.ParseDuration(*req.Duration)
	if perr != nil {
		return time.Time{}, model.Wrap(422, model.CodeSubscriptionInvalid,
			"invalid duration: "+perr.Error(), map[string]string{"field": "duration"})
	}
	base := now
	if live {
		// Additive renewal: an early renewer keeps the whole paid term.
		base = current.ExpiresAt
	}
	exp := base.Add(d)
	if exp.Sub(now) > max {
		return time.Time{}, maxDurationExceeded(max)
	}
	return exp, nil
}

func maxDurationExceeded(max time.Duration) error {
	return model.Wrap(422, model.CodeMaxDurationExceeded,
		fmt.Sprintf("subscription would exceed the server maximum of %s", max),
		map[string]string{"field": "expires_at", "max": max.String()})
}

// ExtendSubscription applies a renewal to a workspace. It is the most important
// path: while LIVE it ADDS to the expiry (never shortens); after the term LAPSED
// it starts from now AND unlocks the workspace, restoring service. It fails on a
// deleted workspace (whose record is gone) and never resolves one.
func (s *Service) ExtendSubscription(id string, req SubscriptionRequest) (*model.Workspace, error) {
	w, err := s.store.GetWorkspace(id)
	if err != nil {
		return nil, err
	}
	if req.Empty() {
		return nil, model.Wrap(422, model.CodeSubscriptionInvalid,
			"a duration or an expires_at is required", nil)
	}
	now := s.now().UTC()
	exp, err := s.computeExpiry(now, w.Subscription, req)
	if err != nil {
		return nil, err
	}
	grace, err := s.cfg.Subscription.GracePeriodValue()
	if err != nil {
		return nil, model.Wrap(500, model.CodeInternal, "server grace_period is not parseable", nil)
	}
	updated, err := s.store.UpdateWorkspace(id, func(ws *model.Workspace) error {
		if ws.Subscription == nil {
			ws.Subscription = &model.Subscription{GrantedAt: now}
		}
		if ws.Subscription.GrantedAt.IsZero() {
			ws.Subscription.GrantedAt = now
		}
		ws.Subscription.ExpiresAt = exp
		ws.Subscription.GraceEndsAt = exp.Add(grace)
		ws.Subscription.RenewedAt = now
		ws.Subscription.Extensions++
		// A renewal restores service: clear the lock and un-lock the status.
		ws.Subscription.LockedAt = time.Time{}
		if ws.Status == model.WorkspaceLocked {
			ws.Status = model.WorkspaceActive
		}
		ws.LastActivityAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// LockWorkspaceForSubscription actively stops the workspace using resources and
// marks it locked. It does NOT merely label: it stops runs, kills sessions and
// detached processes, deletes tunnels, and UNMOUNTS the per-workspace disk image
// to release the fuse2fs process, keeping every byte of user data. It is
// idempotent and safe to re-run after a crash (an already-unmounted workspace is
// a no-op). The status becomes `locked` (distinct from `suspended`).
func (s *Service) LockWorkspaceForSubscription(w *model.Workspace, now time.Time) error {
	if w == nil {
		return nil
	}
	// Stop compute first (runs, sessions, detached processes), then release
	// tunnels, then unmount. Order matters: unmounting while a workload still
	// holds the filesystem would force a lazy unmount.
	s.StopAllRuns(w.ID)
	s.StopWorkspaceSessions(w.ID, model.SessionReasonWorkspaceLocked)
	s.StopDetachedProcesses(w)
	s.ReleaseWorkspaceTunnels(w)
	s.releaseWorkspaceDisk(w.ID)

	_, err := s.store.UpdateWorkspace(w.ID, func(ws *model.Workspace) error {
		if ws.Status != model.WorkspaceSuspended && ws.Status != model.WorkspaceDeleted {
			ws.Status = model.WorkspaceLocked
		}
		if ws.Subscription != nil && ws.Subscription.LockedAt.IsZero() {
			ws.Subscription.LockedAt = now
		}
		return nil
	})
	return err
}

// deleteWorkspaceFully tears a workspace down in the right order and purges it:
// stop compute, unmount (and delete disk.img), then remove the metadata and the
// network. It is idempotent: a workspace whose disk.img or tree is already gone
// is handled without error. It is used by both the manual DELETE and the reaper.
func (s *Service) deleteWorkspaceFully(w *model.Workspace) error {
	if w == nil {
		return nil
	}
	s.StopAllRuns(w.ID)
	s.StopWorkspaceSessions(w.ID, model.SessionReasonWorkspaceDeleted)
	s.StopDetachedProcesses(w)
	s.ReleaseWorkspaceTunnels(w)
	// Unmount first, then delete the image, then the tree; the network last.
	s.removeWorkspaceDisk(w.ID)
	if err := s.store.DeleteWorkspace(w.ID); err != nil {
		return err
	}
	s.ReleaseWorkspaceNetwork(w.ID)
	return nil
}

// ReapSubscriptions runs ONE reaper tick at the injected instant. It is the
// exported entry point the loop calls and the acceptance harness drives
// directly, so the whole lifecycle can be asserted deterministically. Each
// workspace is decided independently; one failure is logged and never stops the
// others (or the loop).
func (s *Service) ReapSubscriptions(now time.Time) {
	workspaces, err := s.store.ListAllWorkspaces()
	if err != nil {
		s.logger.Error("subscription reaper: list workspaces failed", "error", err)
		return
	}
	grace, _ := s.cfg.Subscription.GracePeriodValue()
	for _, w := range workspaces {
		if w.Status == model.WorkspaceDeleted || w.Status == model.WorkspaceSuspended {
			// A suspended workspace is a manual operator action; the reaper
			// leaves it alone (documented). Deleted records are gone already.
			continue
		}
		decision := subscription.Decide(now, w.Subscription, grace)
		switch decision.Action {
		case subscription.ActionLock:
			if err := s.LockWorkspaceForSubscription(w, now); err != nil {
				s.logger.Error("subscription reaper: lock failed",
					"workspace_id", w.ID, "error", err)
			} else {
				s.logger.Info("subscription: workspace locked", "workspace_id", w.ID, "reason", decision.Reason)
			}
		case subscription.ActionDelete:
			if err := s.deleteWorkspaceFully(w); err != nil {
				s.logger.Error("subscription reaper: delete failed",
					"workspace_id", w.ID, "error", err)
			} else {
				s.logger.Info("subscription: workspace deleted", "workspace_id", w.ID, "reason", decision.Reason)
			}
		}
	}
}

// StartReaper launches the single periodic subscription loop. It copies the
// disk watchdog's shape (one goroutine, one ticker, one done channel) and is a
// no-op when subscription.enabled is false, in which case nothing is ever locked
// or deleted automatically.
func (s *Service) StartReaper() {
	if s.reaperOnce {
		return
	}
	s.reaperOnce = true
	if !s.cfg.Subscription.Enabled {
		return
	}
	interval, err := s.cfg.Subscription.ReapIntervalValue()
	if err != nil || interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.reaperDone:
				return
			case <-t.C:
				s.ReapSubscriptions(s.now().UTC())
			}
		}
	}()
}

// stopReaper stops the reaper goroutine (idempotent).
func (s *Service) stopReaper() {
	if s.reaperDone == nil {
		return
	}
	select {
	case <-s.reaperDone:
	default:
		close(s.reaperDone)
	}
}

// WorkspaceSummary is the cheap, derived view of a workspace: computed from
// stored fields and cheap syscalls (a statvfs of an already-mounted filesystem,
// a runs-directory read), never a directory-tree walk.
type WorkspaceSummary struct {
	ID             string
	Status         string
	State          string
	Profile        string
	CreatedAt      time.Time
	AgeSec         int64
	LastActivityAt time.Time
	RunsTotal      int
	RunsActive     int
	DiskUsedBytes  int64
	DiskLimitBytes int64
	Subscription   *SubscriptionSummary
}

// SubscriptionSummary is the derived term view. remaining_sec is clamped at 0
// and NEVER negative; State disambiguates a 0.
type SubscriptionSummary struct {
	State        string
	ExpiresAt    time.Time
	RemainingSec int64
	GraceEndsAt  time.Time
	Extensions   int
	LockedAt     time.Time
}

// Summary computes the derived view for a workspace at now, cheaply. It never
// walks the file tree and never mounts: disk used is read with statvfs only when
// the per-workspace filesystem is already mounted (else it is reported 0).
func (s *Service) Summary(w *model.Workspace, now time.Time) WorkspaceSummary {
	sum := WorkspaceSummary{
		ID:             w.ID,
		Status:         w.EffectiveStatus(now),
		Profile:        w.Profile,
		CreatedAt:      w.CreatedAt,
		LastActivityAt: w.LastActivityAt,
		DiskLimitBytes: int64(w.Limits.DiskMB) * 1024 * 1024,
	}
	if !w.CreatedAt.IsZero() {
		age := now.Sub(w.CreatedAt)
		if age < 0 {
			age = 0
		}
		sum.AgeSec = int64(age / time.Second)
	}
	state, exp, remaining, graceEnds, lockedAt, ext := subscription.Derivations(now, w.Subscription)
	sum.State = state
	if w.Subscription.HasExpiry() {
		sum.Subscription = &SubscriptionSummary{
			State:        state,
			ExpiresAt:    exp,
			RemainingSec: remaining,
			GraceEndsAt:  graceEnds,
			Extensions:   ext,
			LockedAt:     lockedAt,
		}
	}
	if runs, err := s.store.ListRuns(w.ID); err == nil {
		sum.RunsTotal = len(runs)
		for _, r := range runs {
			if r.Status == model.RunQueued || r.Status == model.RunRunning {
				sum.RunsActive++
			}
		}
	}
	if used, ok := workspace.MountedDiskUsed(s.store.WorkspaceRoot(w.ID)); ok {
		sum.DiskUsedBytes = used
	}
	return sum
}
