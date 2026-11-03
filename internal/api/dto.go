package api

import (
	"net/http"
	"time"

	"github.com/go-playground/validator/v10"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

var validate = validator.New(validator.WithRequiredStructEnabled())

// workspaceJSON is the API representation of a workspace.
type workspaceJSON struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Name    string `json:"name"`
	// Profile is the named tool profile the workspace was created with. It is
	// immutable: it decides which tools exist in the workspace.
	Profile        string     `json:"profile"`
	CreatedAt      string     `json:"created_at"`
	Status         string     `json:"status"`
	Limits         limitsJSON `json:"limits"`
	LastActivityAt string     `json:"last_activity_at,omitempty"`
	// Subscription is the stored term, or absent for a workspace with no expiry.
	Subscription *subscriptionJSON `json:"subscription,omitempty"`
	// Summary is the CHEAP derived view (Feature 2): age, remaining time, runs
	// and disk. It is present on the get AND on every list item, so a dashboard
	// renders a list in one request.
	Summary *summaryJSON `json:"summary,omitempty"`
}

// subscriptionJSON is the full stored term view.
type subscriptionJSON struct {
	GrantedAt   string `json:"granted_at"`
	ExpiresAt   string `json:"expires_at"`
	RenewedAt   string `json:"renewed_at,omitempty"`
	LockedAt    string `json:"locked_at,omitempty"`
	GraceEndsAt string `json:"grace_ends_at,omitempty"`
	Extensions  int    `json:"extensions"`
}

// summaryJSON is the derived workspace view. remaining_sec is clamped at 0 and
// is never negative; state disambiguates a 0 (unlimited vs locked vs the last
// second of a live term).
type summaryJSON struct {
	State          string            `json:"state"`
	AgeSec         int64             `json:"age_sec"`
	CreatedAt      string            `json:"created_at"`
	LastActivityAt string            `json:"last_activity_at,omitempty"`
	Runs           runsSummaryJSON   `json:"runs"`
	Disk           diskSummaryJSON   `json:"disk"`
	Profile        string            `json:"profile"`
	Subscription   *subscriptionView `json:"subscription,omitempty"`
}

type runsSummaryJSON struct {
	Total  int `json:"total"`
	Active int `json:"active"`
}

type diskSummaryJSON struct {
	UsedBytes  int64 `json:"used_bytes"`
	LimitBytes int64 `json:"limit_bytes"`
}

// subscriptionView is the derived term view nested in the summary.
type subscriptionView struct {
	State        string `json:"state"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	RemainingSec int64  `json:"remaining_sec"`
	GraceEndsAt  string `json:"grace_ends_at,omitempty"`
	Extensions   int    `json:"extensions"`
	LockedAt     string `json:"locked_at,omitempty"`
}

func toSubscriptionJSON(s *model.Subscription) *subscriptionJSON {
	if s == nil {
		return nil
	}
	return &subscriptionJSON{
		GrantedAt:   formatTime(s.GrantedAt),
		ExpiresAt:   formatTime(s.ExpiresAt),
		RenewedAt:   formatTime(s.RenewedAt),
		LockedAt:    formatTime(s.LockedAt),
		GraceEndsAt: formatTime(s.GraceEndsAt),
		Extensions:  s.Extensions,
	}
}

// toSummaryJSON renders the cheap derived view.
func toSummaryJSON(sum service.WorkspaceSummary) *summaryJSON {
	j := &summaryJSON{
		State:          sum.State,
		AgeSec:         sum.AgeSec,
		CreatedAt:      formatTime(sum.CreatedAt),
		LastActivityAt: formatTime(sum.LastActivityAt),
		Runs:           runsSummaryJSON{Total: sum.RunsTotal, Active: sum.RunsActive},
		Disk:           diskSummaryJSON{UsedBytes: sum.DiskUsedBytes, LimitBytes: sum.DiskLimitBytes},
		Profile:        sum.Profile,
	}
	if sum.Subscription != nil {
		sv := sum.Subscription
		j.Subscription = &subscriptionView{
			State:        sv.State,
			ExpiresAt:    formatTime(sv.ExpiresAt),
			RemainingSec: sv.RemainingSec,
			GraceEndsAt:  formatTime(sv.GraceEndsAt),
			Extensions:   sv.Extensions,
			LockedAt:     formatTime(sv.LockedAt),
		}
	}
	return j
}

type limitsJSON struct {
	CPUCores           float64 `json:"cpu_cores"`
	MemoryMB           int     `json:"memory_mb"`
	DiskMB             int     `json:"disk_mb"`
	MaxWallTimeSec     int     `json:"max_wall_time_sec"`
	MaxRuns            int     `json:"max_runs"`
	AllowNet           bool    `json:"allow_net"`
	AllowUnlimitedRuns bool    `json:"allow_unlimited_runs"`
	AllowTunnels       bool    `json:"allow_tunnels"`
}

// toWorkspaceJSON renders a workspace and its cheap derived summary. It is a
// method on Server because the summary needs the service (runs, disk, the
// clock). The status is the DERIVED status, so a lapsed-but-not-yet-reaped
// workspace already reads `locked` and cannot contradict enforcement.
func (s *Server) toWorkspaceJSON(w *model.Workspace) workspaceJSON {
	now := s.svc.Now()
	j := workspaceJSON{
		ID:        w.ID,
		OwnerID:   w.OwnerID,
		Name:      w.Name,
		Profile:   w.Profile,
		CreatedAt: w.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Status:    w.EffectiveStatus(now),
		Limits:    toLimitsJSON(w.Limits),
	}
	if !w.LastActivityAt.IsZero() {
		j.LastActivityAt = w.LastActivityAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	j.Subscription = toSubscriptionJSON(w.Subscription)
	j.Summary = toSummaryJSON(s.svc.Summary(w, now))
	return j
}

func toLimitsJSON(l model.Limits) limitsJSON {
	return limitsJSON{
		CPUCores:           l.CPUCores,
		MemoryMB:           l.MemoryMB,
		DiskMB:             l.DiskMB,
		MaxWallTimeSec:     l.MaxWallTimeSec,
		MaxRuns:            l.MaxRuns,
		AllowNet:           l.AllowNet,
		AllowUnlimitedRuns: l.AllowUnlimitedRuns,
		AllowTunnels:       l.AllowTunnels,
	}
}

// formatTime renders t as RFC3339 UTC, or "" for the zero time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// createWorkspaceRequest is the POST /v1/workspaces body.
type createWorkspaceRequest struct {
	Name string `json:"name" validate:"required,min=1,max=128"`
	// Profile names the tool profile the workspace should get (e.g. "python",
	// "minimal"). It is a TOP-LEVEL field, not under limits: a profile selects
	// the toolchain, which is orthogonal to resource limits. Omit it to use the
	// server's configured default. An unknown name is 422 unknown_profile.
	Profile string      `json:"profile"`
	Limits  *limitsJSON `json:"limits"`
	// Subscription grants the workspace a term: either a duration (the engine
	// computes expires_at = now + duration) or an absolute expires_at (the
	// client owns its calendar). Omit it to get the configured default (which is
	// no expiry unless subscription.default_duration is set).
	Subscription *subscriptionRequestJSON `json:"subscription"`
}

// subscriptionRequestJSON is the create/extend subscription body. Pointers
// distinguish an absent field from an empty one; duration and expires_at are
// mutually exclusive.
type subscriptionRequestJSON struct {
	Duration  *string `json:"duration"`
	ExpiresAt *string `json:"expires_at"`
}

// toSubscriptionRequest parses the JSON form into the service request. A
// malformed expires_at is a 422 subscription_invalid.
func (r *subscriptionRequestJSON) toSubscriptionRequest() (service.SubscriptionRequest, error) {
	var out service.SubscriptionRequest
	if r == nil {
		return out, nil
	}
	out.Duration = r.Duration
	if r.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *r.ExpiresAt)
		if err != nil {
			return out, model.Wrap(422, model.CodeSubscriptionInvalid,
				"expires_at must be an RFC3339 timestamp",
				map[string]string{"field": "expires_at", "error": err.Error()})
		}
		out.ExpiresAt = &t
	}
	return out, nil
}

func (r *createWorkspaceRequest) toLimits() *model.Limits {
	if r.Limits == nil {
		return nil
	}
	return &model.Limits{
		CPUCores:           r.Limits.CPUCores,
		MemoryMB:           r.Limits.MemoryMB,
		DiskMB:             r.Limits.DiskMB,
		MaxWallTimeSec:     r.Limits.MaxWallTimeSec,
		MaxRuns:            r.Limits.MaxRuns,
		AllowNet:           r.Limits.AllowNet,
		AllowUnlimitedRuns: r.Limits.AllowUnlimitedRuns,
		AllowTunnels:       r.Limits.AllowTunnels,
	}
}

// patchLimitsJSON is the partial limits object for PATCH. Every field is a
// pointer so an absent field and an explicit null both mean "keep the current
// value"; only a present, non-null value is applied.
type patchLimitsJSON struct {
	CPUCores           *float64 `json:"cpu_cores"`
	MemoryMB           *int     `json:"memory_mb"`
	DiskMB             *int     `json:"disk_mb"`
	MaxWallTimeSec     *int     `json:"max_wall_time_sec"`
	MaxRuns            *int     `json:"max_runs"`
	AllowNet           *bool    `json:"allow_net"`
	AllowUnlimitedRuns *bool    `json:"allow_unlimited_runs"`
	AllowTunnels       *bool    `json:"allow_tunnels"`
}

// patchWorkspaceRequest is the PATCH /v1/workspaces/{id} body.
type patchWorkspaceRequest struct {
	Name   *string          `json:"name"`
	Limits *patchLimitsJSON `json:"limits"`
}

// toUpdate converts the request into a service update descriptor.
func (r *patchWorkspaceRequest) toUpdate() service.WorkspaceUpdate {
	upd := service.WorkspaceUpdate{Name: r.Name}
	if r.Limits != nil {
		upd.Limits = &service.LimitUpdate{
			CPUCores:           r.Limits.CPUCores,
			MemoryMB:           r.Limits.MemoryMB,
			DiskMB:             r.Limits.DiskMB,
			MaxWallTimeSec:     r.Limits.MaxWallTimeSec,
			MaxRuns:            r.Limits.MaxRuns,
			AllowNet:           r.Limits.AllowNet,
			AllowUnlimitedRuns: r.Limits.AllowUnlimitedRuns,
			AllowTunnels:       r.Limits.AllowTunnels,
		}
	}
	return upd
}

// validateStruct runs validator and converts failures to an API error.
func validateStruct(v any) error {
	if err := validate.Struct(v); err != nil {
		details := map[string]string{}
		if ve, ok := err.(validator.ValidationErrors); ok {
			for _, fe := range ve {
				details[fe.Field()] = fe.Tag()
			}
		}
		return model.Wrap(http.StatusBadRequest, model.CodeValidation, "validation failed", details)
	}
	return nil
}
