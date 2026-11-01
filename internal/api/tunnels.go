package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"sandboxd/internal/model"
)

// tunnelJSON is the API representation of a tunnel.
type tunnelJSON struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Port        int    `json:"port"`
	URL         string `json:"url"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

func toTunnelJSON(t *model.Tunnel) tunnelJSON {
	return tunnelJSON{
		ID:          t.ID,
		WorkspaceID: t.WorkspaceID,
		Port:        t.Port,
		URL:         t.URL,
		Status:      t.Status,
		CreatedAt:   formatTime(t.CreatedAt),
	}
}

// createTunnelRequest is the POST /v1/workspaces/{id}/tunnels body.
type createTunnelRequest struct {
	Port int `json:"port" validate:"required,min=1,max=65535"`
}

func (s *Server) handleCreateTunnel(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req createTunnelRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	tunnel, err := s.svc.CreateTunnel(ws, req.Port)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tunnel": toTunnelJSON(tunnel)})
}

func (s *Server) handleListTunnels(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	tunnels, err := s.svc.ListTunnels(ws)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]tunnelJSON, 0, len(tunnels))
	for _, t := range tunnels {
		out = append(out, toTunnelJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnels": out})
}

func (s *Server) handleGetTunnel(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	tunnel, err := s.svc.GetTunnel(ws, chi.URLParam(r, "tunnelID"))
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnel": toTunnelJSON(tunnel)})
}

func (s *Server) handleDeleteTunnel(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	if err := s.svc.DeleteTunnel(ws, chi.URLParam(r, "tunnelID")); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
