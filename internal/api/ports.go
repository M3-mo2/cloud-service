package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

// portJSON is the API representation of a published port.
type portJSON struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Name        string `json:"name,omitempty"`
	HostPort    int    `json:"host_port"`
	Host        string `json:"host"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

func toPortJSON(p *model.PublishedPort) portJSON {
	return portJSON{
		ID:          p.ID,
		WorkspaceID: p.WorkspaceID,
		Port:        p.Port,
		Protocol:    p.Protocol,
		Name:        p.Name,
		HostPort:    p.HostPort,
		Host:        p.Host,
		Status:      p.Status,
		CreatedAt:   formatTime(p.CreatedAt),
	}
}

// publishPortRequest is the POST /v1/workspaces/{id}/ports body.
type publishPortRequest struct {
	Port     int    `json:"port" validate:"required,min=1,max=65535"`
	Protocol string `json:"protocol" validate:"omitempty,oneof=tcp"`
	Name     string `json:"name" validate:"omitempty,max=128"`
}

func (s *Server) handlePublishPort(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req publishPortRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	port, err := s.svc.PublishPort(ws, service.PublishSpec{
		Port:     req.Port,
		Protocol: req.Protocol,
		Name:     req.Name,
	})
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"port": toPortJSON(port)})
}

func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	ports, err := s.svc.ListPorts(ws)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]portJSON, 0, len(ports))
	for _, p := range ports {
		out = append(out, toPortJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ports": out})
}

func (s *Server) handleGetPort(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	port, err := s.svc.GetPort(ws, chi.URLParam(r, "portID"))
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"port": toPortJSON(port)})
}

func (s *Server) handleUnpublishPort(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	if err := s.svc.UnpublishPort(ws, chi.URLParam(r, "portID")); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
