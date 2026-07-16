package profile

import (
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Registry is a loaded, validated set of job-profiles keyed by name. It is the
// corpos-native profile library the orchestrator reads to stamp workers.
type Registry struct {
	profiles map[string]JobProfile
}

// newRegistry builds a Registry from a slice of profiles, validating each and
// rejecting duplicate names. It is the shared validation path for every loader.
func newRegistry(profiles []JobProfile) (*Registry, error) {
	m := make(map[string]JobProfile, len(profiles))
	for _, p := range profiles {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, dup := m[p.Name]; dup {
			return nil, fmt.Errorf("duplicate profile name %q", p.Name)
		}
		m[p.Name] = p
	}
	return &Registry{profiles: m}, nil
}

// mergeProfiles overlays `overlay` onto `base`. A same-named overlay entry EXTENDS
// the base profile's Skills (union: base order first, then the overlay's new skills);
// every other field stays from base, so a same-named overlay entry may be partial
// (just name + skills). A new-named overlay entry is appended as a full profile
// (validated downstream by newRegistry). This is the gitignored userlib/ wiring path:
// it adds operator skills/profiles without touching the committed library/ files, so a
// vanilla clone (empty overlay) is unchanged. base is never mutated.
func mergeProfiles(base, overlay []JobProfile) ([]JobProfile, error) {
	out := make([]JobProfile, len(base))
	copy(out, base)
	idx := make(map[string]int, len(out))
	for i, p := range out {
		idx[p.Name] = i
	}
	for _, o := range overlay {
		if strings.TrimSpace(o.Name) == "" {
			return nil, fmt.Errorf("userlib overlay profile has no name")
		}
		if i, ok := idx[o.Name]; ok {
			out[i].Skills = unionSkills(out[i].Skills, o.Skills)
			continue
		}
		idx[o.Name] = len(out)
		out = append(out, o)
	}
	return out, nil
}

// unionSkills returns a∪b preserving order (a first), dropping empties and dups.
func unionSkills(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// Get returns the named profile and whether it was found.
func (r *Registry) Get(name string) (JobProfile, bool) {
	p, ok := r.profiles[name]
	return p, ok
}

// Names returns the registered profile names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.profiles))
	for n := range r.profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Len reports how many profiles are registered.
func (r *Registry) Len() int { return len(r.profiles) }

// Load reads every *.toml file in dir as one job-profile and returns a validated
// registry. An absent dir is an error (the caller asked for a profile dir that
// must exist); a malformed or invalid profile fails the whole load so a partial
// library is never silently served. Each file must define exactly one profile.
func Load(dir string) (*Registry, error) {
	profiles, err := loadDir(os.DirFS(dir), ".", dir)
	if err != nil {
		return nil, err
	}
	return newRegistry(profiles)
}

// loadDir reads all *.toml profiles under root within fsys. label names the
// source in errors (a filesystem path or "builtin"). It is the shared decode
// path for both the on-disk Load and the embedded Builtin loader.
func loadDir(fsys fs.FS, root, label string) ([]JobProfile, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("read profile dir %s: %w", label, err)
	}
	var profiles []JobProfile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := path(root, e.Name())
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read profile %s: %w", e.Name(), err)
		}
		var p JobProfile
		if err := toml.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("parse profile %s: %w", e.Name(), err)
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// path joins an fs.FS dir and entry name (fs.FS always uses forward slashes,
// independent of the host OS, so filepath.Join would be wrong for embed.FS).
func path(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}
