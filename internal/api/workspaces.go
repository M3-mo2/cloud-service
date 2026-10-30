package api

import (
	"net/http"

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
	ws, err := s.svc.CreateWorkspace(pid, req.Name, req.toLimits())
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": toWorkspaceJSON(ws)})
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
		out = append(out, toWorkspaceJSON(ws))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": toWorkspaceJSON(ws)})
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
