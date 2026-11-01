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
	ID             string     `json:"id"`
	OwnerID        string     `json:"owner_id"`
	Name           string     `json:"name"`
	CreatedAt      string     `json:"created_at"`
	Status         string     `json:"status"`
	Limits         limitsJSON `json:"limits"`
	LastActivityAt string     `json:"last_activity_at,omitempty"`
}

type limitsJSON struct {
	CPUCores           float64 `json:"cpu_cores"`
	MemoryMB           int     `json:"memory_mb"`
	DiskMB             int     `json:"disk_mb"`
	MaxWallTimeSec     int     `json:"max_wall_time_sec"`
	MaxRuns            int     `json:"max_runs"`
	AllowNet           bool    `json:"allow_net"`
	AllowUnlimitedRuns bool    `json:"allow_unlimited_runs"`
}

func toWorkspaceJSON(w *model.Workspace) workspaceJSON {
	j := workspaceJSON{
		ID:        w.ID,
		OwnerID:   w.OwnerID,
		Name:      w.Name,
		CreatedAt: w.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Status:    w.Status,
		Limits:    toLimitsJSON(w.Limits),
	}
	if !w.LastActivityAt.IsZero() {
		j.LastActivityAt = w.LastActivityAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
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
	Name   string      `json:"name" validate:"required,min=1,max=128"`
	Limits *limitsJSON `json:"limits"`
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
