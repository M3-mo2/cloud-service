package api

import (
	"net/http"

	"sandboxd/internal/metrics"
	"sandboxd/internal/model"
)

// metricsEnabled reports whether the metrics routes are served.
func (s *Server) metricsEnabled() bool { return s.cfg.Metrics.Enabled && s.svc.Metrics() != nil }

// metricsUnavailable refuses the route when the feature is disabled. A disabled
// feature reads as "not found" rather than a 403, so the surface is simply
// absent.
func (s *Server) metricsUnavailable(w http.ResponseWriter) {
	writeError(w, model.New(404, model.CodeNotFound, "metrics are disabled"))
}

// ownerScope builds the scope for a service-wide stream: every workspace the
// principal owns. It never leaks another owner's data.
func (s *Server) ownerScope(w http.ResponseWriter, r *http.Request) (metrics.Scope, bool) {
	pid, ok := s.principal(w, r)
	if !ok {
		return metrics.Scope{}, false
	}
	return metrics.Scope{OwnerID: pid}, true
}

// workspaceScope builds the scope for a single-workspace stream, after an
// ownership check. The scope pins the one workspace.
func (s *Server) workspaceScope(w http.ResponseWriter, r *http.Request) (metrics.Scope, bool) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return metrics.Scope{}, false
	}
	return metrics.Scope{OwnerID: ws.OwnerID, WorkspaceID: ws.ID}, true
}

// resolveInterval validates the ?interval= override against the floor. A value
// below the floor is refused with 422 (never silently raised); a value above the
// ceiling is clamped. An absent value uses the sampler's configured default.
func (s *Server) resolveInterval(w http.ResponseWriter, r *http.Request) bool {
	raw := r.URL.Query().Get("interval")
	if raw == "" {
		return true
	}
	sampler := s.svc.Metrics()
	if _, err := sampler.FormatOverride(raw); err != nil {
		writeError(w, model.Wrap(422, model.CodeUnprocessable, err.Error(),
			map[string]string{"interval": raw, "min_interval": sampler.MinInterval().String()}))
		return false
	}
	return true
}

// handleMetricsStream is the service-wide SSE stream (every workspace the
// principal owns).
func (s *Server) handleMetricsStream(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEnabled() {
		s.metricsUnavailable(w)
		return
	}
	if !s.resolveInterval(w, r) {
		return
	}
	scope, ok := s.ownerScope(w, r)
	if !ok {
		return
	}
	s.serveMetricsStream(w, r, scope)
}

// handleWorkspaceMetricsStream is the single-workspace SSE stream.
func (s *Server) handleWorkspaceMetricsStream(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEnabled() {
		s.metricsUnavailable(w)
		return
	}
	if !s.resolveInterval(w, r) {
		return
	}
	scope, ok := s.workspaceScope(w, r)
	if !ok {
		return
	}
	s.serveMetricsStream(w, r, scope)
}

// serveMetricsStream subscribes, writes the SSE header, flushes an initial
// comment, then pumps frames until the client disconnects or the server shuts
// down. It guarantees the subscriber is removed on every exit path (defer), so
// one leaked subscriber cannot keep the sampler alive forever.
func (s *Server) serveMetricsStream(w http.ResponseWriter, r *http.Request, scope metrics.Scope) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, model.New(500, model.CodeInternal, "streaming unsupported"))
		return
	}
	sampler := s.svc.Metrics()
	sub, err := sampler.Subscribe(scope)
	if err != nil {
		if err == metrics.ErrTooManySubscribers {
			writeError(w, model.New(503, model.CodeUnavailable, "too many metrics subscribers"))
			return
		}
		writeError(w, model.New(503, model.CodeUnavailable, err.Error()))
		return
	}
	// The subscriber is removed on EVERY exit path, including a panic.
	defer sampler.Close(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// An initial comment line so the client's SSE parser sees data immediately,
	// before the first tick. Then flush (without a flush the client sees nothing
	// until the buffer fills).
	if _, err := w.Write([]byte(metrics.SSEInitialComment)); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-sub.Frames():
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// metricsSnapshotBody renders a one-shot snapshot as the exact frame shape the
// stream uses, so a polling client and a streaming client parse the same JSON.
func (s *Server) metricsSnapshotBody(scope metrics.Scope) metrics.Snapshot {
	snap := s.svc.MetricsSnapshot(scope)
	if snap.Workspaces == nil {
		snap.Workspaces = []metrics.WorkspaceSnapshot{}
	}
	return snap
}

// handleMetricsSnapshot is the service-wide one-shot snapshot.
func (s *Server) handleMetricsSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEnabled() {
		s.metricsUnavailable(w)
		return
	}
	scope, ok := s.ownerScope(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.metricsSnapshotBody(scope))
}

// handleWorkspaceMetricsSnapshot is the single-workspace one-shot snapshot.
func (s *Server) handleWorkspaceMetricsSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEnabled() {
		s.metricsUnavailable(w)
		return
	}
	scope, ok := s.workspaceScope(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.metricsSnapshotBody(scope))
}
