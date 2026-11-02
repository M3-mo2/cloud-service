package profile

import (
	"strings"
	"testing"
)

func TestRegistryLoadsBuiltins(t *testing.T) {
	reg, err := NewRegistry(map[string][]byte{
		"python":  []byte("name: python\ncommands: [python3, git]\n"),
		"minimal": []byte("name: minimal\ncommands: [bash, ls]\n"),
	}, "")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := reg.Names(); len(got) != 2 || got[0] != "minimal" || got[1] != "python" {
		t.Fatalf("names = %v, want [minimal python]", got)
	}
	p, ok := reg.Get("python")
	if !ok {
		t.Fatal("python profile not found")
	}
	// The base command is always present, even when not listed.
	names := p.CommandNames()
	if !contains(names, "sh") || !contains(names, "python3") || !contains(names, "git") {
		t.Fatalf("CommandNames = %v, want sh + python3 + git", names)
	}
}

func TestValidateRejectsCommandPath(t *testing.T) {
	// A command must be a bare name resolved on PATH, never a path (which would
	// be an arbitrary-execution primitive).
	p := &Profile{Name: "x", Commands: []string{"/usr/bin/evil"}}
	if err := p.Validate(); err == nil {
		t.Fatal("a command with a slash must be rejected")
	}
}

func TestValidateAliasTarget(t *testing.T) {
	// An alias to a listed command is fine.
	ok := &Profile{Name: "x", Commands: []string{"python3"}, Aliases: map[string]string{"python": "python3"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid alias rejected: %v", err)
	}
	// An alias to a base command is fine (the engine always provides it).
	base := &Profile{Name: "x", Aliases: map[string]string{"sh2": "sh"}}
	if err := base.Validate(); err != nil {
		t.Fatalf("alias to base command rejected: %v", err)
	}
	// An alias to an unlisted, non-base command could smuggle in a binary.
	bad := &Profile{Name: "x", Commands: []string{"ls"}, Aliases: map[string]string{"py": "python3"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("alias to an unlisted command must be rejected")
	}
}

func TestValidateDuplicateCommand(t *testing.T) {
	p := &Profile{Name: "x", Commands: []string{"ls", "ls"}}
	if err := p.Validate(); err == nil {
		t.Fatal("a duplicated command must be rejected")
	}
	// Listing a base command explicitly is idempotent, not a duplicate.
	ok := &Profile{Name: "x", Commands: []string{"sh", "ls"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("explicit base command rejected: %v", err)
	}
}

func TestValidatePath(t *testing.T) {
	bad := &Profile{Name: "x", Paths: []string{"/usr/lib/../../etc"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("a path containing .. must be rejected")
	}
	rel := &Profile{Name: "x", Paths: []string{"usr/lib"}}
	if err := rel.Validate(); err == nil {
		t.Fatal("a non-absolute path must be rejected")
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := parseProfile([]byte("name: x\nnot_a_field: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "not_a_field") {
		t.Fatalf("unknown field must be rejected, got %v", err)
	}
}

func TestNewRegistryRejectsNameMismatch(t *testing.T) {
	_, err := NewRegistry(map[string][]byte{"python": []byte("name: other\ncommmands: []\n")}, "")
	if err == nil {
		t.Fatal("a file whose stem != name must be rejected")
	}
}

func TestNewRegistryRequiresProfiles(t *testing.T) {
	if _, err := NewRegistry(map[string][]byte{}, ""); err == nil {
		t.Fatal("an empty registry must be rejected")
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
