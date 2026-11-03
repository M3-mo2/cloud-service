package archive

import (
	"errors"

	"sandboxd/internal/model"
)

// Error is a typed archive refusal carrying a STABLE code (one of the
// model.CodeArchive* / model.CodeUnsupportedArchive constants), a message and
// optional details. The API layer maps it to the documented HTTP status.
type Error struct {
	Code    string
	Message string
	Details map[string]string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newError(code, msg string) *Error { return &Error{Code: code, Message: msg} }

func newErrorDetails(code, msg string, details map[string]string) *Error {
	return &Error{Code: code, Message: msg, Details: details}
}

// Common refusal constructors, kept so every caller uses the same wording.
func errUnsupported(sig string) *Error {
	return newErrorDetails(model.CodeUnsupportedArchive,
		"unsupported archive format", map[string]string{"signature": sig})
}

func errEncrypted() *Error {
	return newError(model.CodeArchiveEncrypted, "archive is encrypted and no password is available")
}

func errMultivolume() *Error {
	return newError(model.CodeArchiveMultivolume, "multi-volume archives are not supported")
}

func errTooLarge(detail string) *Error {
	return newErrorDetails(model.CodeArchiveTooLarge, "archive exceeds the configured size limit",
		map[string]string{"limit": detail})
}

func errTooManyEntries(max int) *Error {
	return newErrorDetails(model.CodeArchiveTooManyEntries, "archive has too many entries",
		map[string]string{"max_entries": itoa(max)})
}

func errRatio(name string) *Error {
	return newErrorDetails(model.CodeArchiveRatioExceeded, "archive entry exceeds the compression-ratio limit",
		map[string]string{"entry": name})
}

func errUnsafe(name string) *Error {
	return newErrorDetails(model.CodeArchiveUnsafeEntry, "archive entry is unsafe",
		map[string]string{"entry": name})
}

func errEntryExists(name string) *Error {
	return newErrorDetails(model.CodeArchiveEntryExists, "archive entry already exists",
		map[string]string{"entry": name})
}

// ErrEntryExists is returned by a Writer when an entry collides with an
// existing file and overwrite is false; the engine maps it to
// archive_entry_exists.
var ErrEntryExists = errors.New("archive entry exists")

func itoa(v int) string {
	// tiny dependency-free itoa for detail maps.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
