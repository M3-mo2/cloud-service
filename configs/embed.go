// Package configs holds compiled-in deployment assets.
package configs

import _ "embed"

// NSJailPythonTemplate is the nsjail configuration template for python runs.
// It is embedded so the server always has a valid template even when deployed
// without the configs/ directory.
//
//go:embed nsjail_python.cfg.tmpl
var NSJailPythonTemplate []byte

// ProfileMinimal and ProfilePython are the built-in workspace profiles. They
// are embedded so the service starts with no extra files. A profile is a named,
// reviewable allowlist of the tools (and the data/library trees they need) a
// workspace gets; adding a language means adding a file here, never editing the
// engine. See internal/profile and docs/DECISIONS.md.
//
//go:embed profiles/minimal.yaml
var ProfileMinimal []byte

//go:embed profiles/python.yaml
var ProfilePython []byte

// BuiltinProfiles maps a profile file stem to its embedded YAML. The stem must
// equal the profile's declared `name`.
func BuiltinProfiles() map[string][]byte {
	return map[string][]byte{
		"minimal": ProfileMinimal,
		"python":  ProfilePython,
	}
}
