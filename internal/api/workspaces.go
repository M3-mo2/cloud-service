package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"sandboxd/internal/auth"
	"sandboxd/internal/model"
)

func (s *Server) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	pid, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, model.ErrUnauth)
		return "", false
	}
	return pid, true
}

func (s *Server) ownedWorkspace(w http.ResponseWriter, r *http.Request) (*model.Workspace, bool) {
	pid, ok := s.principal(w, r)
	if !ok {
		return nil, false
	}
	id := chi.URLParam(r, "workspaceID")
	ws, err := s.svc.GetOwnedWorkspace(id, pid)
	if err != nil {
		writeErrorWithCode(w, err)
		return nil, false
	}
	return ws, true
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req createWorkspaceRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	sub, err := req.Subscription.toSubscriptionRequest()
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	ws, err := s.svc.CreateWorkspaceWithSubscription(pid, req.Name, req.Profile, req.toLimits(), sub)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": s.toWorkspaceJSON(ws)})
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.principal(w, r)
	if !ok {
		return
	}
	list, err := s.svc.ListWorkspaces(pid)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]workspaceJSON, 0, len(list))
	for _, ws := range list {
		out = append(out, s.toWorkspaceJSON(ws))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": s.toWorkspaceJSON(ws)})
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	if err := s.svc.DeleteWorkspace(ws.ID, ws.OwnerID); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePatchWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req patchWorkspaceRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	updated, err := s.svc.UpdateWorkspace(ws.ID, req.toUpdate())
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": s.toWorkspaceJSON(updated)})
}

func (s *Server) handleSuspendWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	updated, stopped, changed, err := s.svc.SuspendWorkspace(ws.ID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace":         s.toWorkspaceJSON(updated),
		"changed":           changed,
		"stopped_run_ids":   stopped,
		"stopped_run_count": len(stopped),
	})
}

func (s *Server) handleResumeWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	updated, changed, err := s.svc.ResumeWorkspace(ws.ID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": s.toWorkspaceJSON(updated),
		"changed":   changed,
	})
}

func (s *Server) handleStopWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	summary, err := s.svc.StopWorkspace(ws.ID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	// Re-read so the reported status is current (stop leaves it active).
	updated, err := s.svc.GetOwnedWorkspace(ws.ID, ws.OwnerID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace":             s.toWorkspaceJSON(updated),
		"stopped_run_ids":       summary.RunIDs,
		"stopped_run_count":     len(summary.RunIDs),
		"stopped_session_ids":   summary.SessionIDs,
		"stopped_session_count": len(summary.SessionIDs),
		"stopped_process_ids":   summary.ProcessIDs,
		"stopped_process_count": len(summary.ProcessIDs),
	})
}

// handleExtendSubscription implements POST
// /v1/workspaces/{id}/subscription/extend: the renewal path. While the term is
// live it ADDS to the expiry (an early renewer is not penalised); after it has
// lapsed it starts from now AND unlocks the workspace, restoring service.
func (s *Server) handleExtendSubscription(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req subscriptionRequestJSON
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	sub, err := req.toSubscriptionRequest()
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	updated, err := s.svc.ExtendSubscription(ws.ID, sub)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": s.toWorkspaceJSON(updated)})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	u, err := s.svc.Usage(ws)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id":        ws.ID,
		"status":              u.Status,
		"created_at":          u.CreatedAt.UTC().Format(time.RFC3339),
		"last_activity_at":    formatTime(u.LastActivityAt),
		"disk_bytes":          u.DiskBytes,
		"disk_bytes_complete": u.DiskBytesComplete,
		"disk_mb":             u.DiskMB,
		"disk_percent":        u.DiskPercent,
		"runs_total":          u.RunsTotal,
		"runs_by_status":      u.RunsByStatus,
		"active_runs":         u.ActiveRuns,
		"max_runs":            u.MaxRuns,
		"limits":              toLimitsJSON(u.Limits),
	})
}
