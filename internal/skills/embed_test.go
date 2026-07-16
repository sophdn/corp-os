package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinDiscoversWellFormedSkills checks every embedded CORE skill is
// well-formed — no hardcoded slug list, so adding a core skill (drop a dir under
// library/) needs no edit here. The "which skills must exist" guarantee is owned
// by the profilehooks invariant test (every builtin-profile skill ∈ this set);
// this test owns "whatever is embedded is usable".
func TestBuiltinDiscoversWellFormedSkills(t *testing.T) {
	l, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	got, err := l.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("embedded core library discovered 0 skills")
	}
	for _, s := range got {
		if s.Name == "" {
			t.Errorf("embedded skill at %q has empty Name", s.Path)
		}
		if len(s.Body) == 0 {
			t.Errorf("embedded skill %q has empty body", s.Name)
		}
		if s.Description == "" || s.Description == s.Name {
			t.Errorf("embedded skill %q should carry a frontmatter description, got %q", s.Name, s.Description)
		}
		// Each embedded skill must round-trip through the injector's Select path.
		if sel, _ := l.Select([]string{s.Name}); len(sel) != 1 {
			t.Errorf("embedded skill %q does not resolve via Select (frontmatter name vs dir slug mismatch?)", s.Name)
		}
	}
}

func TestBuiltinWithOverrideDiskWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bug-filing-discipline", "SKILL.md"),
		"---\nname: bug-filing-discipline\ndescription: overridden on disk\n---\nSENTINEL BODY")
	writeFile(t, filepath.Join(dir, "disk-only", "SKILL.md"),
		"---\nname: disk-only\ndescription: only on disk\n---\ndisk-only body")

	l, err := BuiltinWithOverride(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, err := l.Discover()
	if err != nil {
		t.Fatal(err)
	}
	by := make(map[string]Skill, len(all))
	for _, s := range all {
		by[s.Name] = s
	}
	// disk override wins for a name present in both layers
	if got := by["bug-filing-discipline"].Body; got != "SENTINEL BODY" {
		t.Errorf("override did not win: bug-filing-discipline body = %q", got)
	}
	// union: a disk-only skill appears alongside the embedded baseline
	if _, ok := by["disk-only"]; !ok {
		t.Error("disk-only skill missing from the union")
	}
	// embedded-only skill still resolves (not clobbered by the overlay)
	if _, ok := by["spike"]; !ok {
		t.Error("embedded-only skill 'spike' missing after overlay")
	}
}

func TestBuiltinWithOverrideEmptyDirIsBaselineOnly(t *testing.T) {
	base, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	baseSet, _ := base.Discover()

	overlaid, err := BuiltinWithOverride("")
	if err != nil {
		t.Fatal(err)
	}
	overlaidSet, _ := overlaid.Discover()

	if len(overlaidSet) != len(baseSet) {
		t.Errorf("empty overlay changed the set: %d vs baseline %d", len(overlaidSet), len(baseSet))
	}
}

func TestBuiltinWithOverrideAbsentDirIsBaselineOnly(t *testing.T) {
	base, _ := Builtin()
	baseSet, _ := base.Discover()

	overlaid, err := BuiltinWithOverride(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	overlaidSet, _ := overlaid.Discover()
	if len(overlaidSet) != len(baseSet) {
		t.Errorf("absent overlay changed the set: %d vs baseline %d", len(overlaidSet), len(baseSet))
	}
}

// guard the embed actually shipped files (a stale //go:embed glob would yield an
// empty FS and silently inject nothing). Builtin merges library/ (vanilla) with
// userlib/ (operator, gitignored — possibly empty in a vanilla clone), so it must
// discover AT LEAST every library/ skill directory.
func TestEmbeddedLibraryIsNonEmpty(t *testing.T) {
	entries, err := os.ReadDir("library")
	if err != nil {
		t.Fatalf("on-disk library dir unreadable (source check): %v", err)
	}
	libDirs := 0
	for _, e := range entries {
		if e.IsDir() {
			libDirs++
		}
	}
	l, _ := Builtin()
	got, _ := l.Discover()
	if len(got) == 0 {
		t.Fatal("embedded library discovered 0 skills — //go:embed glob is wrong")
	}
	if len(got) < libDirs {
		t.Errorf("embedded discovered %d skills but library/ has %d skill dirs", len(got), libDirs)
	}
}
