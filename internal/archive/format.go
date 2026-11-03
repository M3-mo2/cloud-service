// Package archive implements native (in-engine) archive handling: format
// detection from content, validated entry iteration for ZIP and RAR, and
// budgeted writing into a workspace. It never shells out to a host tool: this
// host has no RAR tool at all, and the security boundary must live in the
// engine. See docs/DECISIONS.md ADR 92.
package archive

import (
	"encoding/hex"
	"strings"
)

// Format names a supported archive container.
type Format string

// BuildMarker is an opaque string embedded in the binary and logged at startup.
// An acceptance harness greps the built binary for it as a positive control,
// proving the tested artifact is a fresh build of the intended commit. It names
// no engine.
const BuildMarker = "zcp-archive-native-v0.0.20"

// Supported formats.
const (
	FormatZip Format = "zip"
	FormatRAR Format = "rar"
)

// Signatures (magic bytes). Detection is ALWAYS from content, never from the
// filename: a .zip that is really a RAR must still work, and an unknown format
// is refused naming the detected signature.
var (
	zipSigLocal   = []byte{0x50, 0x4B, 0x03, 0x04} // local file header
	zipSigEmpty   = []byte{0x50, 0x4B, 0x05, 0x06} // end of central directory (empty)
	zipSigSpanned = []byte{0x50, 0x4B, 0x07, 0x08} // spanned archive
	rarSigPrefix  = []byte{'R', 'a', 'r', '!', 0x1a, 0x07}
)

// Detect returns the format named by the content's magic bytes, or "" when no
// supported format matches. The second return is a short human-readable
// signature (hex of the leading bytes, or "empty") for the refusal detail.
func Detect(data []byte) (Format, string) {
	if len(data) == 0 {
		return "", "empty"
	}
	if hasPrefix(data, zipSigLocal) || hasPrefix(data, zipSigEmpty) || hasPrefix(data, zipSigSpanned) {
		return FormatZip, sigString(data)
	}
	if hasPrefix(data, rarSigPrefix) {
		// RAR4 signature is "Rar!\x1a\x07\x00"; RAR5 is "Rar!\x1a\x07\x01\x00".
		// Both share the 6-byte prefix above; the format is the same reader.
		return FormatRAR, sigString(data)
	}
	return "", sigString(data)
}

func hasPrefix(data, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	return string(data[:len(prefix)]) == string(prefix)
}

// sigString renders the leading bytes for a refusal detail. It is bounded and
// purely informational; it is never used to make a safety decision.
func sigString(data []byte) string {
	n := len(data)
	if n > 8 {
		n = 8
	}
	return "0x" + strings.ToUpper(hex.EncodeToString(data[:n]))
}
