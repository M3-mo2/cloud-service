package api

import (
	"errors"
	"net/http"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

// listPackagesResponse is the GET /packages body.
type listPackagesResponse struct {
	Packages        []service.PrivatePackage `json:"packages"`
	Count           int                      `json:"count"`
	Truncated       bool                     `json:"truncated"`
	SharedEnabled   bool                     `json:"shared_enabled"`
	SharedAvailable bool                     `json:"shared_available"`
	SharedPath      string                   `json:"shared_path"`
	PrivatePath     string                   `json:"private_path"`
}

func (s *Server) handleListPackages(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	pkgs, truncated, err := s.svc.ListPrivatePackages(ws)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	tier := s.svc.PackageTierFor(ws)
	writeJSON(w, http.StatusOK, listPackagesResponse{
		Packages:        pkgs,
		Count:           len(pkgs),
		Truncated:       truncated,
		SharedEnabled:   tier.SharedEnabled,
		SharedAvailable: tier.SharedAvailable,
		SharedPath:      tier.SharedPath,
		PrivatePath:     tier.PrivatePath,
	})
}

// installPackagesRequest is the POST /packages/install body.
type installPackagesRequest struct {
	Packages []string `json:"packages"`
}

func (s *Server) handleInstallPackages(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req installPackagesRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	res, err := s.svc.InstallPackages(ws, req.Packages)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code":   res.ExitCode,
		"output":      res.Output,
		"stdout":      res.Output,
		"timed_out":   res.TimedOut,
		"truncated":   res.Truncated,
		"duration_ms": res.DurationMS,
		"target":      service.PrivatePackagesMount,
	})
}

func (s *Server) handleDeletePackages(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	name := q.Get("name")
	all := q.Get("all") == "true"

	switch {
	case all && name != "":
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation,
			"use either ?name= or ?all=true, not both", nil))
		return
	case all:
		removed, err := s.svc.RemoveAllPackages(ws)
		if err != nil {
			writeErrorWithCode(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "all": true})
		return
	case name == "":
		// Refuse an ambiguous empty delete: removing everything requires the
		// explicit ?all=true flag so it can never be a footgun.
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation,
			"name is required, or pass all=true to remove every private package", nil))
		return
	default:
		if err := s.svc.RemovePackage(ws, name); err != nil {
			if errors.Is(err, service.ErrPackageNotFound) {
				writeErrorWithCode(w, service.ErrPackageNotFound)
				return
			}
			writeErrorWithCode(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
