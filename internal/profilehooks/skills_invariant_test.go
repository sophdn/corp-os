package profilehooks_test

import (
	"testing"

	"corpos/internal/profile"
	"corpos/internal/skills"
)

// TestBuiltinProfilesSkillsAreEmbedded is the lock-in regression: every skill slug
// that a builtin job-profile names MUST be present in corpos's embedded skills
// library, so a fresh install (no ~/.claude/skills tree) still injects each
// profile's governing disciplines. Add a profile that references an un-embedded
// skill — or remove a skill the library still ships for — and this fails the gate.
//
// Test-only imports of profile + skills: this asserts a cross-package contract
// without coupling the production profilehooks package to the profile package.
func TestBuiltinProfilesSkillsAreEmbedded(t *testing.T) {
	reg, err := profile.Builtin()
	if err != nil {
		t.Fatalf("profile.Builtin: %v", err)
	}
	lib, err := skills.Builtin()
	if err != nil {
		t.Fatalf("skills.Builtin: %v", err)
	}
	discovered, err := lib.Discover()
	if err != nil {
		t.Fatalf("skills.Discover: %v", err)
	}
	embedded := make(map[string]bool, len(discovered))
	for _, s := range discovered {
		embedded[s.Name] = true
	}

	referenced := false
	for _, name := range reg.Names() {
		p, ok := reg.Get(name)
		if !ok {
			t.Fatalf("registry inconsistent: Names() listed %q but Get() missed it", name)
		}
		for _, slug := range p.Skills {
			referenced = true
			if !embedded[slug] {
				t.Errorf("builtin profile %q references skill %q, which is NOT in the embedded skills library "+
					"(internal/skills/library/). Embed it so a fresh install can inject it.", name, slug)
			}
			// And it must actually resolve through the injector's path (Select by slug).
			if sel, _ := lib.Select([]string{slug}); len(sel) != 1 {
				t.Errorf("embedded skill %q (referenced by profile %q) did not resolve via Select — "+
					"frontmatter name probably disagrees with the directory slug", slug, name)
			}
		}
	}
	if !referenced {
		t.Fatal("no builtin profile referenced any skill — the invariant is vacuous; check profile.Builtin()")
	}
}
