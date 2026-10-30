package api

import (
	"net/http"

	"github.com/go-playground/validator/v10"

	"sandboxd/internal/model"
)

var validate = validator.New(validator.WithRequiredStructEnabled())

// workspaceJSON is the API representation of a workspace.
type workspaceJSON struct {
	ID        string     `json:"id"`
	OwnerID   string     `json:"owner_id"`
	Name      string     `json:"name"`
	CreatedAt string     `json:"created_at"`
	Status    string     `json:"status"`
	Limits    limitsJSON `json:"limits"`
}

type limitsJSON struct {
	CPUCores       float64 `json:"cpu_cores"`
	MemoryMB       int     `json:"memory_mb"`
	DiskMB         int     `json:"disk_mb"`
	MaxWallTimeSec int     `json:"max_wall_time_sec"`
	MaxRuns        int     `json:"max_runs"`
	AllowNet       bool    `json:"allow_net"`
}

func toWorkspaceJSON(w *model.Workspace) workspaceJSON {
	return workspaceJSON{
		ID:        w.ID,
		OwnerID:   w.OwnerID,
		Name:      w.Name,
		CreatedAt: w.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Status:    w.Status,
		Limits: limitsJSON{
			CPUCores:       w.Limits.CPUCores,
			MemoryMB:       w.Limits.MemoryMB,
			DiskMB:         w.Limits.DiskMB,
			MaxWallTimeSec: w.Limits.MaxWallTimeSec,
			MaxRuns:        w.Limits.MaxRuns,
			AllowNet:       w.Limits.AllowNet,
		},
	}
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
		CPUCores:       r.Limits.CPUCores,
		MemoryMB:       r.Limits.MemoryMB,
		DiskMB:         r.Limits.DiskMB,
		MaxWallTimeSec: r.Limits.MaxWallTimeSec,
		MaxRuns:        r.Limits.MaxRuns,
		AllowNet:       r.Limits.AllowNet,
	}
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
