package api

import (
	"net/http"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

type execRequest struct {
	Cmd        []string          `json:"cmd" validate:"required,min=1"`
	Cwd        string            `json:"cwd"`
	Env        map[string]string `json:"env"`
	TimeoutSec int               `json:"timeout_sec"`
	Stdin      string            `json:"stdin"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req execRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out, err := s.svc.Exec(ws, service.ExecSpec{
		Cmd:     req.Cmd,
		Cwd:     req.Cwd,
		Env:     req.Env,
		Timeout: req.TimeoutSec,
		Stdin:   req.Stdin,
	})
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code":   out.ExitCode,
		"stdout":      out.Stdout,
		"stderr":      out.Stderr,
		"duration_ms": out.DurationMS,
		"truncated":   out.Truncated,
	})
}

var _ = model.CodeValidation
