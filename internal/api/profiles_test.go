package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestCreateWorkspaceProfileStoredAndEchoed proves a named profile is stored on
// the record and returned by every workspace endpoint.
func TestCreateWorkspaceProfileStoredAndEchoed(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "py", "profile": "python"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Workspace struct {
			ID      string `json:"id"`
			Profile string `json:"profile"`
		} `json:"workspace"`
	}](t, rec)
	if body.Workspace.Profile != "python" {
		t.Fatalf("profile = %q, want python", body.Workspace.Profile)
	}
	// GET echoes the same profile.
	get := h.do("GET", "/v1/workspaces/"+body.Workspace.ID, reqOpts{key: keyA})
	got := decode[struct {
		Workspace struct {
			Profile string `json:"profile"`
		} `json:"workspace"`
	}](t, get)
	if got.Workspace.Profile != "python" {
		t.Fatalf("GET profile = %q, want python", got.Workspace.Profile)
	}
}

// TestCreateWorkspaceDefaultProfileApplied proves a create that omits the
// profile gets the configured default and records it.
func TestCreateWorkspaceDefaultProfileApplied(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "d"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	body := decode[struct {
		Workspace struct {
			Profile string `json:"profile"`
		} `json:"workspace"`
	}](t, rec)
	if body.Workspace.Profile != h.cfg.Profiles.Default {
		t.Fatalf("profile = %q, want default %q", body.Workspace.Profile, h.cfg.Profiles.Default)
	}
}

// TestCreateWorkspaceUnknownProfile proves an unknown name is 422
// unknown_profile and nothing is created.
func TestCreateWorkspaceUnknownProfile(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "bad", "profile": "does-not-exist"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "unknown_profile" {
		t.Fatalf("code = %q, want unknown_profile", code)
	}
}

// TestPatchCannotChangeProfile proves the profile is immutable: a PATCH body
// that tries to change it is an unknown field (400) and the stored profile is
// unchanged.
func TestPatchCannotChangeProfile(t *testing.T) {
	h := newHarness(t)
	rec := h.doJSON("POST", "/v1/workspaces", keyA, map[string]any{"name": "py", "profile": "python"})
	wsID := decode[struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
	}](t, rec).Workspace.ID
	// `profile` is not a PATCH field at all.
	patch := h.doJSON("PATCH", "/v1/workspaces/"+wsID, keyA, map[string]any{"profile": "minimal"})
	if patch.Code != http.StatusBadRequest {
		t.Fatalf("patch with profile: %d %s", patch.Code, patch.Body.String())
	}
	get := h.do("GET", "/v1/workspaces/"+wsID, reqOpts{key: keyA})
	got := decode[struct {
		Workspace struct {
			Profile string `json:"profile"`
		} `json:"workspace"`
	}](t, get)
	if got.Workspace.Profile != "python" {
		t.Fatalf("profile changed to %q", got.Workspace.Profile)
	}
}

// TestMetaListsProfiles proves /v1/meta lists the offered profiles with the
// default and enough detail to choose.
func TestMetaListsProfiles(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/meta", reqOpts{key: keyA})
	var body struct {
		Profiles struct {
			Default  string `json:"default"`
			Ready    bool   `json:"ready"`
			Profiles []struct {
				Name        string   `json:"name"`
				Description string   `json:"description"`
				Commands    []string `json:"commands"`
				Languages   []string `json:"languages"`
			} `json:"profiles"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Profiles.Default != "python" {
		t.Fatalf("default = %q", body.Profiles.Default)
	}
	names := map[string]bool{}
	for _, p := range body.Profiles.Profiles {
		names[p.Name] = true
		if p.Description == "" {
			t.Errorf("profile %s has no description", p.Name)
		}
		if len(p.Commands) == 0 {
			t.Errorf("profile %s lists no commands", p.Name)
		}
	}
	if !names["python"] || !names["minimal"] {
		t.Fatalf("profiles = %v, want python + minimal", names)
	}
}
