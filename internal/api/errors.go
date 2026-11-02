// Package api implements the HTTP/JSON surface of sandboxd.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"sandboxd/internal/executor"
	"sandboxd/internal/model"
	"sandboxd/internal/workspace"
)

// errorEnvelope is the standard JSON error body.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError renders an error using the standard envelope, mapping internal
// errors to API errors.
func writeError(w http.ResponseWriter, err error) {
	apiErr := mapError(err)
	writeJSON(w, apiErr.Status, errorEnvelope{Error: errorBody{
		Code:    apiErr.Code,
		Message: apiErr.Message,
		Details: apiErr.Details,
	}})
}

// mapError converts any error into an API error with an appropriate status.
func mapError(err error) *model.Error {
	if err == nil {
		return model.New(500, model.CodeInternal, "internal server error")
	}
	var apiErr *model.Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	var qe *workspace.QuotaError
	if errors.As(err, &qe) {
		return model.Wrap(507, "insufficient_storage", "disk quota exceeded", map[string]string{
			"used_bytes":  itoa(qe.Used),
			"limit_bytes": itoa(qe.Limit),
		})
	}
	if errors.Is(err, executor.ErrNsjailUnavailable) {
		return model.New(503, model.CodeUnavailable, "nsjail is not available")
	}
	if errors.Is(err, executor.ErrMaskUnavailable) {
		// Masking is on but could not be applied, so the run is refused rather
		// than launched with the host identity exposed. The reason is carried in
		// the message so /readyz and the client both see why.
		return model.Wrap(503, model.CodeUnavailable, "host-identity masking unavailable: "+err.Error(), nil)
	}
	var sbErr *executor.SandboxError
	if errors.As(err, &sbErr) {
		details := map[string]string{}
		if sbErr.Diagnostic != "" {
			details["nsjail"] = sbErr.Diagnostic
		}
		return model.Wrap(503, model.CodeUnavailable, "sandbox unavailable: nsjail could not set up or launch the sandbox", details)
	}
	if errors.Is(err, executor.ErrCommandNotFound) {
		return model.New(422, model.CodeUnprocessable, "command not found")
	}
	if errors.Is(err, executor.ErrInvalidCwd) {
		return model.New(422, model.CodeUnprocessable, "cwd escapes workspace root")
	}
	if workspace.IsUnsafe(err) {
		return model.New(422, model.CodeUnprocessable, "unsafe path: escapes workspace root")
	}
	if workspace.IsIsDirectory(err) {
		return model.New(422, model.CodeNotDirectory, "path is a directory")
	}
	if workspace.IsNotDirectory(err) {
		return model.New(422, model.CodeNotDirectory, "path is not a directory")
	}
	if workspace.IsRangeError(err) {
		return model.New(422, model.CodeUnprocessable, "invalid line range")
	}
	if workspace.IsTooLarge(err) {
		return model.New(413, model.CodePayloadTooLarge, "file exceeds maximum allowed size")
	}
	if workspace.IsNotFound(err) {
		return model.New(404, model.CodeNotFound, "file not found")
	}
	return model.New(500, model.CodeInternal, "internal server error")
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// decodeJSON decodes a JSON body with a size cap, rejecting unknown fields.
func decodeJSON(w http.ResponseWriter, r *http.Request, cap int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, cap)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return model.New(413, model.CodePayloadTooLarge, "request body too large")
		}
		return model.Wrap(400, model.CodeValidation, "invalid JSON body", map[string]string{"error": err.Error()})
	}
	return nil
}
