package model

import (
	"errors"
	"fmt"
)

// Error codes used in the JSON error envelope and OpenAPI spec.
const (
	CodeUnauthorized    = "unauthorized"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeValidation      = "validation"
	CodeUnprocessable   = "unprocessable"
	CodePayloadTooLarge = "payload_too_large"
	CodeTimeout         = "timeout"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"

	// CodeWorkspaceSuspended is returned when work is requested for a
	// workspace that has been suspended. It has no existing equivalent: the
	// condition is a policy refusal (the workspace exists and is owned), not a
	// conflict with concurrent state, so it gets its own code and HTTP 409.
	CodeWorkspaceSuspended = "workspace_suspended"
)

// Error is the canonical API error, carrying an HTTP mapping and a code.
type Error struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
	Status  int               `json:"-"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New constructs an API error.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Wrap constructs an API error with details.
func Wrap(status int, code, message string, details map[string]string) *Error {
	return &Error{Status: status, Code: code, Message: message, Details: details}
}

// AsError extracts an *Error from err, returning a wrapped internal error when
// err is not already an API error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if err == nil {
		return nil
	}
	return New(500, CodeInternal, "internal server error")
}

// Sentinel errors for common cases.
var (
	ErrNotFound          = New(404, CodeNotFound, "resource not found")
	ErrForbidden         = New(403, CodeForbidden, "forbidden")
	ErrUnauth            = New(401, CodeUnauthorized, "missing or invalid credentials")
	ErrNsjailUnavailable = New(503, CodeUnavailable, "nsjail is not available")
)
