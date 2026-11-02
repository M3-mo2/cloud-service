package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sandboxd/internal/profile"
)

// TestMaterializeProfileAllowlist proves the materializer places a listed
// command, drops an engine-named host entry, and materializes a same-directory
// symlink target as real bytes so the name works standalone.
func TestMaterializeProfileAllowlist(t *testing.T) {
	root := toolFixtureRoot()
	p := &profile.Profile{Name: "t", Commands: []string{"python3", "bash"}, Aliases: map[string]string{"python": "python3"}}
	dir := t.TempDir()
	o, err := materializeProfile(dir, root, p)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// python3 is a same-dir symlink to python3.12 in the fixture; it must be a
	// real file in the tree (not a dangling link to a name not in the profile).
	b, err := os.ReadFile(filepath.Join(o.UsrRoot, "bin", "python3"))
	if err != nil {
		t.Fatalf("python3 not materialized as a file: %v", err)
	}
	if !strings.Contains(string(b), "py") {
		t.Fatalf("python3 content = %q", string(b))
	}
	// The alias is a symlink to the listed command.
	if _, err := os.Lstat(filepath.Join(o.UsrRoot, "bin", "python")); err != nil {
		t.Fatalf("alias python not materialized: %v", err)
	}
	// Commands set includes base sh + the listed + the alias.
	for _, want := range []string{"sh", "python3", "bash", "python"} {
		if !o.Commands[want] {
			t.Errorf("commands set missing %q: %v", want, o.Commands)
		}
	}
	// An engine-named host entry (nsjail) must never appear, even though the
	// fixture has one in /usr/bin.
	if _, err := os.Lstat(filepath.Join(o.UsrRoot, "bin", "nsjail")); err == nil {
		t.Fatal("engine tool nsjail leaked into the profile tree")
	}
	// The conventional subtrees exist (the template binds them mandatorily).
	for _, sub := range []string{"sbin", "lib", "lib64"} {
		if fi, err := os.Stat(filepath.Join(o.UsrRoot, sub)); err != nil || !fi.IsDir() {
			t.Errorf("conventional subtree %s missing", sub)
		}
	}
}

// TestMaterializeProfileRejectsEngineNamed proves a profile that lists an
// engine tool name is refused (fail loud) rather than shipped.
func TestMaterializeProfileRejectsEngineNamed(t *testing.T) {
	p := &profile.Profile{Name: "t", Commands: []string{"nsjail"}}
	_, err := materializeProfile(t.TempDir(), toolFixtureRoot(), p)
	if err == nil || !strings.Contains(err.Error(), "engine or platform") {
		t.Fatalf("engine-named command must be refused, got %v", err)
	}
}

// TestProfileCommandResolution proves a bare command name resolves to its jail
// path (/usr/bin/<name>) so an alias without a host executable works, and a
// name outside the profile is a clean command-not-found.
func TestProfileCommandResolution(t *testing.T) {
	r := NewNsjailRunner(testCfg(), "", t.TempDir())
	pf, ok := r.profiles.Get("minimal")
	if !ok {
		t.Fatal("minimal profile missing")
	}
	got, err := r.resolveProfileCommand(pf, "ls")
	if err != nil {
		t.Fatalf("listed command ls: %v", err)
	}
	if got != binJailPath+"/ls" {
		t.Fatalf("ls resolved to %q, want %s/ls", got, binJailPath)
	}
	if _, err := r.resolveProfileCommand(pf, "python3"); err == nil {
		t.Fatal("a command outside the minimal profile must be command-not-found")
	}
}
