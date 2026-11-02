package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Registry is the set of profiles the service offers. It is built once at
// startup from the embedded defaults plus any operator-supplied directory.
type Registry struct {
	byName map[string]*Profile
	order  []string
}

// NewRegistry builds a registry from the embedded built-in profiles and,
// optionally, an operator directory of YAML profiles.
//
// The embedded profiles always load first, so the service starts with no extra
// files. When dir is non-empty, every *.yaml / *.yml file in it is loaded and
// OVERLAYS a same-named built-in (an operator can refine the shipped python
// profile) or adds a new one (node, go, ruby, ...). A malformed file, a name
// mismatch between the file stem and the profile's `name`, or a duplicate
// profile is a hard error: a profile is a security-relevant allowlist, so an
// unreadable one must fail loudly rather than silently ship a broken workspace.
func NewRegistry(builtins map[string][]byte, dir string) (*Registry, error) {
	r := &Registry{byName: map[string]*Profile{}}
	// Deterministic order: sort the embedded stems.
	stems := make([]string, 0, len(builtins))
	for stem := range builtins {
		stems = append(stems, stem)
	}
	sort.Strings(stems)
	for _, stem := range stems {
		p, err := parseProfile(builtins[stem])
		if err != nil {
			return nil, fmt.Errorf("built-in profile %q: %w", stem, err)
		}
		if p.Name != stem {
			return nil, fmt.Errorf("built-in profile file %q declares name %q; they must match", stem, p.Name)
		}
		r.byName[p.Name] = p
	}
	if dir != "" {
		if err := r.loadDir(dir); err != nil {
			return nil, err
		}
	}
	if len(r.byName) == 0 {
		return nil, fmt.Errorf("no profiles are configured")
	}
	for name := range r.byName {
		r.order = append(r.order, name)
	}
	sort.Strings(r.order)
	return r, nil
}

// loadDir overlays every profile file in dir onto the registry.
func (r *Registry) loadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read profiles dir %q: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read profile %q: %w", name, err)
		}
		p, err := parseProfile(raw)
		if err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
		if p.Name != stem {
			return fmt.Errorf("profile file %q declares name %q; they must match", name, p.Name)
		}
		r.byName[p.Name] = p
	}
	return nil
}

// parseProfile decodes and validates one profile document.
func parseProfile(raw []byte) (*Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Get returns a profile by name.
func (r *Registry) Get(name string) (*Profile, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Has reports whether a profile name exists.
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// Names returns every profile name, sorted.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// All returns every profile in deterministic (name-sorted) order.
func (r *Registry) All() []*Profile {
	out := make([]*Profile, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}
