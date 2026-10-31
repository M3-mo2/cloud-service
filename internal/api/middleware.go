package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"sandboxd/internal/auth"
)

// respWriter captures the status code and error code for audit logging.
type respWriter struct {
	http.ResponseWriter
	status    int
	errorCode string
	wrote     bool
}

func (w *respWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = 200
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *respWriter) setErrorCode(code string) { w.errorCode = code }

// auditMiddleware logs one structured line per request.
func auditMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &respWriter{ResponseWriter: w, status: 200}
			var principalID string
			ctx := auth.WithPrincipalSink(r.Context(), &principalID)
			next.ServeHTTP(rw, r.WithContext(ctx))

			attrs := []slog.Attr{
				slog.Time("ts", start.UTC()),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			}
			if principalID != "" {
				attrs = append(attrs, slog.String("principal_id", principalID))
			}
			if wsID := chi.URLParam(r, "workspaceID"); wsID != "" {
				attrs = append(attrs, slog.String("workspace_id", wsID))
			}
			if runID := chi.URLParam(r, "runID"); runID != "" {
				attrs = append(attrs, slog.String("run_id", runID))
			}
			if rw.errorCode != "" {
				attrs = append(attrs, slog.String("error_code", rw.errorCode))
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
		})
	}
}

// writeErrorWithCode renders an API error and records the code for the audit log.
func writeErrorWithCode(w http.ResponseWriter, err error) {
	apiErr := mapError(err)
	if setter, ok := w.(interface{ setErrorCode(string) }); ok {
		setter.setErrorCode(apiErr.Code)
	}
	writeError(w, err)
}
