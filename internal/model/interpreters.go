package model

import (
	"path/filepath"
	"sort"
	"strings"
)

// Interpreter describes a file interpreter the sandbox image is verified to
// provide. Extensions are lowercase and include the leading dot.
type Interpreter struct {
	// Name is the interpreter's command name as resolvable on PATH inside the
	// sandbox (e.g. "python3").
	Name string
	// Exts are the file extensions this interpreter runs.
	Exts []string
}

// Interpreters is the curated allowlist of interpreters the sandbox image is
// verified to provide (see docs/VERIFICATION.md). It serves two purposes:
// mapping a file extension to the interpreter that runs it (the `runnable`
// file metadata and the default for a kind=file run), and validating an
// explicit `interpreter` override.
//
// It is deliberately an allowlist, not a free-form command path: the caller of
// the run API is a backend service, but an interpreter field that accepted an
// arbitrary path would be an arbitrary-execution primitive. Every entry was
// measured executable inside the real sandbox before being listed here.
var Interpreters = []Interpreter{
	{Name: "python3", Exts: []string{".py"}},
	{Name: "node", Exts: []string{".js", ".mjs", ".cjs"}},
	{Name: "bash", Exts: []string{".sh", ".bash"}},
	{Name: "perl", Exts: []string{".pl", ".pm"}},
}

var (
	allowedInterpreters = func() map[string]bool {
		m := make(map[string]bool, len(Interpreters))
		for _, in := range Interpreters {
			m[in.Name] = true
		}
		return m
	}()
	extInterpreter = func() map[string]string {
		m := map[string]string{}
		for _, in := range Interpreters {
			for _, e := range in.Exts {
				m[e] = in.Name
			}
		}
		return m
	}()
)

// IsAllowedInterpreter reports whether name is in the interpreter allowlist.
func IsAllowedInterpreter(name string) bool { return allowedInterpreters[name] }

// InterpreterForFile returns the interpreter that runs a file with the given
// name by extension, or "" when the extension is not in the supported set.
func InterpreterForFile(name string) string {
	return extInterpreter[strings.ToLower(filepath.Ext(name))]
}

// SupportedInterpreters returns the allowlist interpreter names, sorted.
func SupportedInterpreters() []string {
	out := make([]string, 0, len(Interpreters))
	for _, in := range Interpreters {
		out = append(out, in.Name)
	}
	sort.Strings(out)
	return out
}

// SupportedExtensions returns every mapped extension, sorted.
func SupportedExtensions() []string {
	out := make([]string, 0, len(extInterpreter))
	for e := range extInterpreter {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
