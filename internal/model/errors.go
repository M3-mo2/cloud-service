package model

import (
	"errors"
	"fmt"
)

// Error codes used in the JSON error envelope and OpenAPI spec.
const (
	CodeUnauthorized  = "unauthorized"
	CodeForbidden     = "forbidden"
	CodeNotFound      = "not_found"
	CodeConflict      = "conflict"
	CodeValidation    = "validation"
	CodeUnprocessable = "unprocessable"
	// CodeUnknownProfile is returned (HTTP 422) when a workspace create names a
	// profile that is not configured. It is distinct from a general
	// validation/unprocessable error: the field parsed and the name is
	// well-formed, but it is not one of the offered profiles. The message and
	// details name the request's value so a client can correct it.
	CodeUnknownProfile  = "unknown_profile"
	CodeNotDirectory    = "not_directory"
	CodePayloadTooLarge = "payload_too_large"
	CodeTimeout         = "timeout"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"

	// CodeWorkspaceSuspended is returned when work is requested for a
	// workspace that has been suspended. It has no existing equivalent: the
	// condition is a policy refusal (the workspace exists and is owned), not a
	// conflict with concurrent state, so it gets its own code and HTTP 409.
	CodeWorkspaceSuspended = "workspace_suspended"

	// CodeTooManySessions is returned when a workspace already hosts the
	// configured maximum number of concurrent terminal sessions. It is a
	// per-workspace policy limit (409), distinct from the workspace-level
	// suspension refusal.
	CodeTooManySessions = "too_many_sessions"

	// CodeSessionNeedsInterpreter is returned (HTTP 422) when a terminal session
	// is requested for a workspace whose profile does not provide python3. The
	// PTY relay that backs a session is a python3 program, so a session on a
	// shell-only profile (e.g. "minimal") is refused with this stable,
	// actionable reason rather than a generic command-not-found.
	CodeSessionNeedsInterpreter = "session_needs_interpreter"

	// CodeSessionEnded is returned when input, a signal or a resize is sent to
	// a session that has already reached a terminal state but whose record is
	// still retained (a shell that exited on its own, an idle/lifetime timeout,
	// ...). Deleting a session removes its record, so subsequent operations
	// return not_found instead.
	CodeSessionEnded = "session_ended"

	// CodeTunnelsDisabled is returned (HTTP 422) when a tunnel is created for a
	// workspace that has not opted in (limits.allow_tunnels=false) or when the
	// server-level tunnels.enabled switch is false. Creating a tunnel therefore
	// requires both keys, exactly like allow_unlimited_runs. It is also returned
	// when a workspace has not opted in to egress (limits.allow_net), which
	// cloudflared needs.
	CodeTunnelsDisabled = "tunnels_disabled"

	// CodeTooManyTunnels is returned (HTTP 422) when creating a tunnel would
	// exceed tunnels.max_per_workspace. The message names the limit; nothing
	// extra is created.
	CodeTooManyTunnels = "too_many_tunnels"

	// CodeTunnelURLUnavailable is returned (HTTP 504) when cloudflared was
	// started but did not print a public URL within tunnels.url_timeout_sec.
	// The engine never fabricates a URL, so the create fails with this
	// machine-readable reason instead of returning a URL it did not observe.
	CodeTunnelURLUnavailable = "tunnel_url_unavailable"

	// CodeSubscriptionExpired is returned (HTTP 403) when work is requested
	// for a workspace whose subscription term has lapsed and the reaper has
	// locked it. It is DISTINCT from workspace_suspended (409): suspension is a
	// manual, resume-able operator action; a lock is the automatic consequence
	// of a lapsed term and the way out is a renewal (extend). Reads, the
	// summary, usage, file reads and manual delete remain allowed while locked.
	CodeSubscriptionExpired = "subscription_expired"

	// CodeSubscriptionInvalid is returned (HTTP 422) when a create/extend names
	// a subscription that cannot be parsed or applied: an unparseable, zero or
	// negative duration, a non-future expires_at, or neither/both of
	// duration and expires_at. The details name the offending field.
	CodeSubscriptionInvalid = "subscription_invalid"

	// CodeMaxDurationExceeded is returned (HTTP 422) when a create/extend would
	// put the expiry further than subscription.max_duration in the future.
	CodeMaxDurationExceeded = "max_duration_exceeded"

	// CodeSubscriptionWouldShorten is returned (HTTP 422) when an extension
	// supplies an absolute expires_at EARLIER than a still-live term. The engine
	// never silently shortens a paid term.
	CodeSubscriptionWouldShorten = "subscription_would_shorten"

	// Archive extraction refusal codes. They are STABLE and documented (see
	// docs/API.md, docs/DECISIONS.md ADR 92). Every archive operation refuses
	// with one of these rather than a generic error, so a client can act on it
	// and the safety rules stay inspectable. Archive handling lives only in the
	// engine (internal/archive); no shell tool is invoked, so the refusals are
	// deterministic and identical on every host.

	// CodeUnsupportedArchive is returned (HTTP 415) when the uploaded/selected
	// archive's CONTENT (magic bytes) matches no supported format. The detected
	// signature is named in the details; the filename is never trusted.
	CodeUnsupportedArchive = "unsupported_archive"
	// CodeArchiveEncrypted is returned (HTTP 422) when the archive is encrypted
	// (ZIP general-purpose flag bit 0, or a RAR archive/entry that needs a
	// password). The engine holds no password, so emitting possibly-garbage
	// plaintext is never an option.
	CodeArchiveEncrypted = "archive_encrypted"
	// CodeArchiveMultivolume is returned (HTTP 422) when a RAR archive declares
	// itself multi-volume (partN.rar): the engine extracts single-volume
	// archives only, so it refuses rather than truncating silently.
	CodeArchiveMultivolume = "archive_multivolume"
	// CodeArchiveTooLarge is returned (HTTP 413) when the archive's total
	// uncompressed size exceeds archives.max_total_mb.
	CodeArchiveTooLarge = "archive_too_large"
	// CodeArchiveTooManyEntries is returned (HTTP 413) when the archive holds
	// more entries than archives.max_entries.
	CodeArchiveTooManyEntries = "archive_too_many_entries"
	// CodeArchiveRatioExceeded is returned (HTTP 413) when an entry's
	// uncompressed/compressed ratio exceeds archives.max_ratio (a bomb guard).
	CodeArchiveRatioExceeded = "archive_ratio_exceeded"
	// CodeArchiveUnsafeEntry is returned (HTTP 422) when an entry name escapes
	// the destination (traversal, absolute, drive letter, escaping symlink or
	// hardlink, or an unsafe collision).
	CodeArchiveUnsafeEntry = "archive_unsafe_entry"
	// CodeArchiveEntryExists is returned (HTTP 409) when an entry would
	// overwrite an existing file and overwrite is false.
	CodeArchiveEntryExists = "archive_entry_exists"

	// CodeWriteRateExceeded is returned (HTTP 429) when the engine's own write
	// path (a file upload/write or an archive extraction) is asked to add bytes
	// to a workspace whose SUSTAINED disk growth has exceeded its configured
	// write_rate_mb_s ceiling. It is distinct from a disk-quota refusal (the
	// disk is not full) and from a validation error (the request is well
	// formed): the workspace is momentarily writing too fast. It is only ever
	// produced while disk.write_rate_enabled is on; the ceiling is SAMPLED, so
	// the write that crosses it is not refused by a kernel boundary (see ADR 95).
	CodeWriteRateExceeded = "write_rate_exceeded"
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
