package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

type runJSON struct {
	ID          string      `json:"id"`
	WorkspaceID string      `json:"workspace_id"`
	Kind        string      `json:"kind"`
	Argv        []string    `json:"argv"`
	Status      string      `json:"status"`
	ExitCode    *int        `json:"exit_code"`
	StartedAt   string      `json:"started_at"`
	EndedAt     string      `json:"ended_at"`
	Logs        runLogsJSON `json:"logs"`
}

type runLogsJSON struct {
	StdoutBytes int64 `json:"stdout_bytes"`
	StderrBytes int64 `json:"stderr_bytes"`
	Truncated   bool  `json:"truncated"`
}

func toRunJSON(r *model.Run) runJSON {
	j := runJSON{
		ID:          r.ID,
		WorkspaceID: r.WorkspaceID,
		Kind:        string(r.Kind),
		Argv:        r.Argv,
		Status:      string(r.Status),
		ExitCode:    r.ExitCode,
		StartedAt:   r.StartedAt.UTC().Format(time.RFC3339),
		Logs: runLogsJSON{
			StdoutBytes: r.Logs.StdoutBytes,
			StderrBytes: r.Logs.StderrBytes,
			Truncated:   r.Logs.Truncated,
		},
	}
	if !r.EndedAt.IsZero() {
		j.EndedAt = r.EndedAt.UTC().Format(time.RFC3339)
	}
	return j
}

type createRunRequest struct {
	Kind       string            `json:"kind" validate:"required,oneof=file command"`
	File       string            `json:"file"`
	Args       []string          `json:"args"`
	Argv       []string          `json:"argv"`
	Env        map[string]string `json:"env"`
	TimeoutSec int               `json:"timeout_sec"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	var req createRunRequest
	if err := decodeJSON(w, r, s.cfg.Caps.JSONBodyBytes(), &req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if err := validateStruct(&req); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	spec := service.StartRunSpec{
		Kind:    model.RunKind(req.Kind),
		File:    req.File,
		Args:    req.Args,
		Argv:    req.Argv,
		Env:     req.Env,
		Timeout: req.TimeoutSec,
	}
	if err := validateRunSpec(spec); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	run, err := s.svc.StartRun(ws, spec)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	s.metrics.runsTotal.WithLabelValues(string(run.Kind)).Inc()
	writeJSON(w, http.StatusAccepted, map[string]any{"run": toRunJSON(run)})
}

func validateRunSpec(spec service.StartRunSpec) error {
	switch spec.Kind {
	case model.RunKindFile:
		if spec.File == "" {
			return model.Wrap(400, model.CodeValidation, "file is required for kind=file", nil)
		}
	case model.RunKindCommand:
		if len(spec.Argv) == 0 {
			return model.Wrap(400, model.CodeValidation, "argv is required for kind=command", nil)
		}
	default:
		return model.Wrap(400, model.CodeValidation, "kind must be file or command", nil)
	}
	return nil
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	filter, err := parseRunFilter(r)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	runs, total, err := s.svc.ListRuns(ws, filter)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		out = append(out, toRunJSON(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs":   out,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// parseRunFilter reads the runs list query parameters. Statuses accepts either
// repeated `status=` parameters or a single comma-separated value.
func parseRunFilter(r *http.Request) (service.RunFilter, error) {
	q := r.URL.Query()
	f := service.RunFilter{}
	raw := q["status"]
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !validRunStatus(part) {
				return f, model.Wrap(400, model.CodeValidation, "unknown run status filter",
					map[string]string{"status": part})
			}
			f.Statuses = append(f.Statuses, model.RunStatus(part))
		}
	}
	kind := q.Get("kind")
	if kind != "" {
		if kind != string(model.RunKindFile) && kind != string(model.RunKindCommand) {
			return f, model.Wrap(400, model.CodeValidation, "kind must be file or command",
				map[string]string{"kind": kind})
		}
		f.Kind = model.RunKind(kind)
	}
	f.Limit = int(parseInt64(q.Get("limit"), 0))
	f.Offset = int(parseInt64(q.Get("offset"), 0))
	if f.Limit < 0 || f.Offset < 0 {
		return f, model.Wrap(400, model.CodeValidation, "limit and offset must be >= 0", nil)
	}
	return f, nil
}

func validRunStatus(s string) bool {
	switch model.RunStatus(s) {
	case model.RunQueued, model.RunRunning, model.RunExited, model.RunKilled,
		model.RunTimeout, model.RunFailed, model.RunInterrupted:
		return true
	}
	return false
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	runID := chiURLParam(r, "runID")
	run, err := s.svc.GetRun(ws, runID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toRunJSON(run)})
}

func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	runID := chiURLParam(r, "runID")
	if _, err := s.svc.GetRun(ws, runID); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	_, killedStatus, err := s.svc.StopRun(ws.ID, runID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	run, err := s.svc.GetRun(ws, runID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	// Prefer the status driven by the stop when a live process was signalled;
	// the persisted record may not have caught up yet.
	if killedStatus == model.RunKilled {
		run.Status = model.RunKilled
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": toRunJSON(run)})
}

func (s *Server) handleGetRunLogs(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	runID := chiURLParam(r, "runID")
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = model.StreamStdout
	}
	if stream != model.StreamStdout && stream != model.StreamStderr {
		writeErrorWithCode(w, model.Wrap(400, model.CodeValidation, "stream must be stdout or stderr", nil))
		return
	}
	offset := parseInt64(r.URL.Query().Get("offset"), 0)
	limit := parseInt64(r.URL.Query().Get("limit"), 65536)
	chunk, err := s.svc.ReadLog(ws, runID, stream, offset, limit)
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

func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}

func (s *Server) handlePurgeRuns(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	deleted, err := s.svc.PurgeRuns(ws)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) handlePurgeRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	runID := chiURLParam(r, "runID")
	if err := s.svc.PurgeRun(ws, runID); err != nil {
		writeErrorWithCode(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseInt64(s string, def int64) int64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
