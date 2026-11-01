package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

// sessionJSON is the API representation of a terminal session.
type sessionJSON struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	Shell           string `json:"shell"`
	Cwd             string `json:"cwd"`
	Cols            int    `json:"cols"`
	Rows            int    `json:"rows"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	LastActivityAt  string `json:"last_activity_at"`
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at"`
	ExitCode        *int   `json:"exit_code"`
	Reason          string `json:"reason,omitempty"`
	OutputBytes     int64  `json:"output_bytes"`
	OutputTruncated bool   `json:"output_truncated"`
}

func toSessionJSON(s *model.Session) sessionJSON {
	return sessionJSON{
		ID:              s.ID,
		WorkspaceID:     s.WorkspaceID,
		Shell:           s.Shell,
		Cwd:             s.Cwd,
		Cols:            s.Cols,
		Rows:            s.Rows,
		Status:          string(s.Status),
		CreatedAt:       s.CreatedAt.UTC().Format(time.RFC3339),
		LastActivityAt:  formatTime(s.LastActivityAt),
		StartedAt:       formatTime(s.StartedAt),
		EndedAt:         formatTime(s.EndedAt),
		ExitCode:        s.ExitCode,
		Reason:          s.Reason,
		OutputBytes:     s.OutputBytes,
		OutputTruncated: s.OutputTruncated,
	}
}

type createSessionRequest struct {
	Shell      string            `json:"shell"`
	Cwd        string            `json:"cwd"`
	Cols       int               `json:"cols"`
	Rows       int               `json:"rows"`
	Env        map[string]string `json:"env"`
	TimeoutSec int               `json:"timeout_sec"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req createSessionRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	cols, rows, err := s.sessionSize(req.Cols, req.Rows)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	sess, err := s.svc.StartSession(ws, service.StartSessionSpec{
		Shell:      req.Shell,
		Cwd:        req.Cwd,
		Cols:       cols,
		Rows:       rows,
		Env:        req.Env,
		TimeoutSec: req.TimeoutSec,
	})
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": toSessionJSON(sess)})
}

// sessionSize applies the default size and the documented bounds. An absurd
// (or non-positive when explicitly set) value is rejected with 422.
func (s *Server) sessionSize(cols, rows int) (int, int, error) {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	if cols < model.MinSessionCols || cols > model.MaxSessionCols {
		return 0, 0, model.Wrap(422, model.CodeUnprocessable, "cols out of range",
			map[string]string{
				"field": "cols",
				"min":   itoa(int64(model.MinSessionCols)),
				"max":   itoa(int64(model.MaxSessionCols)),
			})
	}
	if rows < model.MinSessionRows || rows > model.MaxSessionRows {
		return 0, 0, model.Wrap(422, model.CodeUnprocessable, "rows out of range",
			map[string]string{
				"field": "rows",
				"min":   itoa(int64(model.MinSessionRows)),
				"max":   itoa(int64(model.MaxSessionRows)),
			})
	}
	return cols, rows, nil
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	filter, err := parseSessionFilter(r)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	sessions, total, err := s.svc.ListSessions(ws, filter)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]sessionJSON, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, toSessionJSON(sess))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": out,
		"total":    total,
		"limit":    filter.Limit,
		"offset":   filter.Offset,
	})
}

// parseSessionFilter reads the sessions list query parameters. Status accepts
// repeated or comma-separated values.
func parseSessionFilter(r *http.Request) (service.SessionFilter, error) {
	q := r.URL.Query()
	f := service.SessionFilter{}
	for _, v := range q["status"] {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !validSessionStatus(part) {
				return f, model.Wrap(400, model.CodeValidation, "unknown session status filter",
					map[string]string{"status": part})
			}
			f.Statuses = append(f.Statuses, model.SessionStatus(part))
		}
	}
	f.Limit = int(parseInt64(q.Get("limit"), 0))
	f.Offset = int(parseInt64(q.Get("offset"), 0))
	if f.Limit < 0 || f.Offset < 0 {
		return f, model.Wrap(400, model.CodeValidation, "limit and offset must be >= 0", nil)
	}
	return f, nil
}

func validSessionStatus(s string) bool {
	switch model.SessionStatus(s) {
	case model.SessionRunning, model.SessionExited, model.SessionKilled, model.SessionTimedOut:
		return true
	}
	return false
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	sess, err := s.svc.GetSession(ws, chi.URLParam(r, "sessionID"))
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": toSessionJSON(sess)})
}

type sessionInputRequest struct {
	Data string `json:"data"`
}

func (s *Server) handleSessionInput(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req sessionInputRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if req.Data == "" {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "data is required", nil))
		return
	}
	if err := s.svc.Input(ws, chi.URLParam(r, "sessionID"), req.Data); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionSignalRequest struct {
	Signal string `json:"signal"`
}

// sessionSignals is the documented signal set. The helper understands more, but
// the API accepts only these so the contract is stable.
var sessionSignals = map[string]bool{"INT": true, "TERM": true, "QUIT": true, "HUP": true}

func (s *Server) handleSessionSignal(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req sessionSignalRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	sig := strings.ToUpper(strings.TrimSpace(req.Signal))
	if !sessionSignals[sig] {
		writeErrorWithCode(w, model.Wrap(422, model.CodeUnprocessable,
			"signal must be one of INT, TERM, QUIT, HUP", map[string]string{"signal": req.Signal}))
		return
	}
	if err := s.svc.Signal(ws, chi.URLParam(r, "sessionID"), sig); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionResizeRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func (s *Server) handleSessionResize(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req sessionResizeRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	cols, rows, err := s.sessionSize(req.Cols, req.Rows)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := s.svc.Resize(ws, chi.URLParam(r, "sessionID"), cols, rows); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSessionOutput(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	offset := parseInt64(r.URL.Query().Get("offset"), 0)
	limit := parseInt64(r.URL.Query().Get("limit"), 65536)
	chunk, err := s.svc.ReadSessionOutput(ws, chi.URLParam(r, "sessionID"), offset, limit)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":      string(chunk.Data),
		"offset":    chunk.Offset,
		"next":      chunk.Next,
		"eof":       chunk.EOF,
		"truncated": chunk.Truncated,
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	sess, changed, err := s.svc.CloseSession(ws, sessionID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": toSessionJSON(sess),
		"changed": changed,
	})
}
