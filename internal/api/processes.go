package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"sandboxd/internal/model"
	"sandboxd/internal/service"
)

// processJSON is the API representation of a live process in a workspace.
type processJSON struct {
	ID        string   `json:"id"`
	PID       int      `json:"pid"`
	PPID      int      `json:"ppid"`
	Argv      []string `json:"argv"`
	Kind      string   `json:"kind"`
	SourceID  string   `json:"source_id"`
	StartedAt string   `json:"started_at"`
}

func toProcessJSON(p service.Process) processJSON {
	return processJSON{
		ID:        p.ID,
		PID:       p.PID,
		PPID:      p.PPID,
		Argv:      argvOrEmptyJSON(p.Argv),
		Kind:      p.Kind,
		SourceID:  p.SourceID,
		StartedAt: formatTime(p.StartedAt),
	}
}

func argvOrEmptyJSON(argv []string) []string {
	if argv == nil {
		return []string{}
	}
	return argv
}

// handleListProcesses returns every live process belonging to a workspace,
// including processes started from a session. An empty inventory is an honest
// empty list, never an error.
func (s *Server) handleListProcesses(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	procs, err := s.svc.ListProcesses(ws)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]processJSON, 0, len(procs))
	for _, p := range procs {
		out = append(out, toProcessJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"processes": out,
		"count":     len(out),
	})
}

// handleStopProcess stops the process tree of one process, addressed by its
// stable id. It is idempotent (an exited process is reported as gone) and
// returns 404 for an id that does not name a process in this workspace.
func (s *Server) handleStopProcess(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	processID := chi.URLParam(r, "processID")
	res, err := s.svc.StopProcess(ws, processID)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	if !res.Found {
		writeErrorWithCode(w, model.New(404, model.CodeNotFound, "process not found"))
		return
	}
	status := "gone"
	if res.Stopped {
		status = "stopped"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"process_id": res.ID,
		"status":     status,
		"stopped":    res.Stopped,
		"stopped_at": time.Now().UTC().Format(time.RFC3339),
	})
}
