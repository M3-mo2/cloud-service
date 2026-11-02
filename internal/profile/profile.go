// Package profile defines the workspace profile model: a NAMED, explicit
// ALLOWLIST of the tools (and the data/library trees those tools need) that a
// workspace is given.
//
// This replaces the previous model, in which every workspace received a copy of
// almost the whole host /usr/bin and /usr/sbin with only the engine/platform
// names removed (a denylist). A profile is data, not a code path: adding a
// language means adding a YAML file under configs/profiles/ (or a directory an
// operator points `profiles.dir` at), never editing the engine.
//
// The critical design constraint is that a tool is not just its binary. python3
// needs the whole /usr/lib/python3.12 stdlib tree; git needs /usr/lib/git-core;
// perl needs its perl-base and /usr/share/perl trees; ssh needs its config.
// The schema therefore has ENTRY SETS: `commands` names binaries, and `paths`
// names files, directories and whole trees. The engine reproduces each entry
// set exactly, and every tool is verified by EXECUTING it inside a real
// workspace, never by checking that a file exists.
package profile

import (
	"fmt"
	"sort"
	"strings"
)

// Profile is one named, reviewable allowlist.
type Profile struct {
	// Name is the profile's identifier. It is the value a create request names
	// and the value the workspace record carries. It must equal the file stem.
	Name string `yaml:"name" json:"name"`
	// Description is a human-readable summary, surfaced in GET /v1/meta so a
	// client can choose.
	Description string `yaml:"description" json:"description"`
	// Languages lists the language runtimes this profile provides (e.g.
	// "python", "node"). It is informational and surfaces in GET /v1/meta.
	Languages []string `yaml:"languages" json:"languages"`
	// Provides is a short list of the tool families this profile exposes. It is
	// informational; the authoritative set is Commands plus Aliases.
	Provides []string `yaml:"provides,omitempty" json:"provides,omitempty"`

	// Commands are command NAMES resolved on the host PATH and exposed under
	// the same name inside the workspace. Each is copied as a real file (its
	// final symlink target), so a symlinked host command like /usr/bin/python3
	// (-> python3.12) becomes a working /usr/bin/python3.
	Commands []string `yaml:"commands" json:"commands"`
	// Aliases create extra names inside the workspace's bin directory as
	// symlinks to a command that IS listed in Commands (or an engine-provided
	// base command). They never pull in a new host binary: an alias whose target
	// is not otherwise present is a load error.
	Aliases map[string]string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// Paths are absolute host paths (a file, a directory, or a whole tree)
	// reproduced in the workspace. A path under /usr is folded into the
	// workspace's /usr tree; any other absolute path is mounted read-only at
	// the same location. This is how a tool's data/library trees are attached:
	// /usr/lib/python3.12, /usr/lib/git-core, /usr/share/perl, /etc/ssh/ssh_config.
	Paths []string `yaml:"paths,omitempty" json:"paths,omitempty"`

	// Notes is free-form documentation for the profile (e.g. where a PTY tool
	// does and does not work). It surfaces in GET /v1/meta.
	Notes string `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// Validate checks a profile's internal consistency. It is called at load time
// so a malformed profile fails loudly at startup rather than producing a
// half-built workspace later.
func (p *Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("profile has no name")
	}
	if !validName(p.Name) {
		return fmt.Errorf("profile name %q must be lowercase letters, digits, '-' or '_'", p.Name)
	}
	// Duplicate detection is over the profile's OWN command list. A base
	// command may be listed explicitly (it is simply idempotent when the tree is
	// materialized); it is not a duplicate.
	seen := map[string]bool{}
	for _, c := range p.Commands {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("profile %q has an empty command name", p.Name)
		}
		if strings.ContainsRune(c, '/') {
			return fmt.Errorf("profile %q command %q must be a bare name, not a path", p.Name, c)
		}
		if seen[c] {
			return fmt.Errorf("profile %q lists command %q twice", p.Name, c)
		}
		seen[c] = true
	}
	for alias, target := range p.Aliases {
		if strings.TrimSpace(alias) == "" || strings.ContainsRune(alias, '/') {
			return fmt.Errorf("profile %q has an invalid alias %q", p.Name, alias)
		}
		if seen[alias] {
			return fmt.Errorf("profile %q alias %q collides with a command", p.Name, alias)
		}
		// The target must be a listed command or a base command the engine
		// always provides; otherwise the alias would dangle or smuggle in a
		// binary the profile did not review.
		if !seen[target] && !IsBaseCommand(target) {
			return fmt.Errorf("profile %q alias %q targets %q, which is not a listed command", p.Name, alias, target)
		}
	}
	for _, pth := range p.Paths {
		if !strings.HasPrefix(pth, "/") {
			return fmt.Errorf("profile %q path %q must be absolute", p.Name, pth)
		}
		if strings.Contains(pth, "..") {
			return fmt.Errorf("profile %q path %q must not contain '..'", p.Name, pth)
		}
	}
	return nil
}

// validName reports whether s is a legal profile name.
func validName(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return s != ""
}

// CommandNames returns every command name the profile exposes, including the
// engine base commands and aliases, sorted. It is the reviewable "what does a
// tenant get" list (modulo the data trees in Paths, which carry no commands).
func (p *Profile) CommandNames() []string {
	set := map[string]bool{}
	for _, c := range BaseCommands {
		set[c] = true
	}
	for _, c := range p.Commands {
		set[c] = true
	}
	for a := range p.Aliases {
		set[a] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// baseCommands are the commands EVERY profile provides, regardless of its own
// list. The sandbox shells (sessions default to /bin/sh) and the engine's own
// helpers need a POSIX shell to exist. They are deliberately tiny: a profile
// that wants more names it explicitly.
var BaseCommands = []string{"sh"}

// IsBaseCommand reports whether name is one of the engine's always-provided
// commands.
func IsBaseCommand(name string) bool {
	for _, c := range BaseCommands {
		if c == name {
			return true
		}
	}
	return false
}
