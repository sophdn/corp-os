package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverBothShapesSkipReadme(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "alpha", "SKILL.md"), "---\nname: alpha\ndescription: the alpha skill\n---\nAlpha body.")
	writeFile(t, filepath.Join(dir, "beta.md"), "# Beta\n\nBeta body.")
	writeFile(t, filepath.Join(dir, "README.md"), "ignore me")

	got, err := New(dir).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("names = %q, %q; want alpha, beta", got[0].Name, got[1].Name)
	}
	if got[0].Description != "the alpha skill" {
		t.Errorf("alpha description = %q (want frontmatter)", got[0].Description)
	}
	if got[1].Description != "Beta" {
		t.Errorf("beta description = %q (want firstHeading)", got[1].Description)
	}
}

func TestSelectByNameAndAll(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "A")
	writeFile(t, filepath.Join(dir, "b.md"), "B")
	l := New(dir)

	all, _ := l.Select(nil)
	if len(all) != 2 {
		t.Errorf("Select(nil) = %d, want 2", len(all))
	}
	one, _ := l.Select([]string{"b", "missing"})
	if len(one) != 1 || one[0].Name != "b" {
		t.Errorf("Select by name = %+v, want [b]", one)
	}
}

func TestSystemPromptAndDigest(t *testing.T) {
	s := []Skill{{Name: "x", Body: "do x"}}
	sp := SystemPrompt(s)
	if !strings.Contains(sp, "# Skill: x") || !strings.Contains(sp, "do x") {
		t.Errorf("system prompt missing skill: %q", sp)
	}
	d1 := Digest(s)
	if len(d1) != 16 {
		t.Errorf("digest len = %d, want 16", len(d1))
	}
	if d1 == Digest([]Skill{{Name: "x", Body: "do x DIFFERENT"}}) {
		t.Error("digest should change with body")
	}
}

func TestSystemPromptWithin_FullWhenFits(t *testing.T) {
	s := []Skill{{Name: "x", Description: "d", Body: "## Core\nrule\n\n## More\nfull body detail"}}
	// No cap and a generous cap both keep the full body.
	for _, cap := range []int{0, 100000} {
		sp := SystemPromptWithin(s, cap)
		if !strings.Contains(sp, "# Skill: x") {
			t.Errorf("cap=%d: missing full-tier header: %q", cap, sp)
		}
		if !strings.Contains(sp, "full body detail") {
			t.Errorf("cap=%d: full body should be present: %q", cap, sp)
		}
		if strings.Contains(sp, "terse") {
			t.Errorf("cap=%d: should not be terse when it fits", cap)
		}
	}
}

func TestSystemPromptWithin_TerseWhenOverBudget(t *testing.T) {
	long := "## Core\nThe one load-bearing rule.\n\n## Details\n" + strings.Repeat("verbose detail line\n", 100)
	s := []Skill{{Name: "big", Description: "a big discipline", Body: long}}
	sp := SystemPromptWithin(s, 60) // preamble fits, full body does not

	if !strings.Contains(sp, "terse") {
		t.Errorf("over-budget skill should inject the terse tier: %q", sp)
	}
	if !strings.Contains(sp, "The one load-bearing rule.") {
		t.Errorf("terse tier should carry the ## Core: %q", sp)
	}
	if strings.Contains(sp, "verbose detail line") {
		t.Errorf("terse tier must not carry the full body")
	}
	// Never dropped: the skill is still named.
	if !strings.Contains(sp, "big") {
		t.Errorf("skill must still be represented when terse: %q", sp)
	}
}

func TestSystemPromptWithin_NoSilentDrop(t *testing.T) {
	body := strings.Repeat("x", 4000) // ~1000 tok each, far over a tiny cap
	s := []Skill{
		{Name: "alpha", Description: "a", Body: body},
		{Name: "beta", Description: "b", Body: body},
	}
	sp := SystemPromptWithin(s, 50)
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(sp, name) {
			t.Errorf("skill %q silently dropped: %q", name, sp)
		}
	}
}

func TestTerseBody_FallbackOutlineWhenNoCore(t *testing.T) {
	s := Skill{Name: "n", Description: "the desc", Body: "## Alpha\nstuff\n\n## Beta\nmore"}
	got := terseBody(s)
	if !strings.Contains(got, "the desc") {
		t.Errorf("fallback should include description: %q", got)
	}
	if !strings.Contains(got, "Covers: Alpha; Beta.") {
		t.Errorf("fallback should outline headings: %q", got)
	}
}

func TestCoreSectionAndHeadings(t *testing.T) {
	body := "intro\n## Core\nrule one\nrule two\n## Next\nother"
	if got := coreSection(body); got != "rule one\nrule two" {
		t.Errorf("coreSection = %q, want the Core block only", got)
	}
	if got := coreSection("## Alpha\nx"); got != "" {
		t.Errorf("coreSection without a Core heading = %q, want empty", got)
	}
	heads := headings(body)
	if len(heads) != 2 || heads[0] != "Core" || heads[1] != "Next" {
		t.Errorf("headings = %v, want [Core Next]", heads)
	}
}

func TestAbsentTreeYieldsNoSkills(t *testing.T) {
	got, err := New(filepath.Join(t.TempDir(), "nope")).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("absent tree should yield 0 skills, got %d", len(got))
	}
}

func TestDescriptionFallsBackToName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "empty.md"), "   \n  \n") // no frontmatter, no heading
	got, err := New(dir).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Description != "empty" {
		t.Errorf("description should fall back to the name: %+v", got)
	}
}

func TestNonDirectoryTreeYieldsNoSkills(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	writeFile(t, f, "x")
	got, err := New(f).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a non-directory tree should yield 0 skills, got %d", len(got))
	}
}
