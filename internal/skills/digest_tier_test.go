package skills

import (
	"strings"
	"testing"
)

// Bug (coding-worker-zero-tool-calls): SystemPromptWithin enforced maxTokens only for
// the FULL tier — the terse tier appended each skill's `## Core` unconditionally, so a
// profile with several big-Core disciplines (the coding worker) blew far past the budget
// even when "terse", bloating the local-window worker's preamble. A third DIGEST tier
// (one-line description) keeps the preamble within budget while still representing every
// skill.

func bigCoreSkill(name string) Skill {
	return Skill{
		Name:        name,
		Description: "one-line summary of " + name,
		Body:        "## Core\n" + strings.Repeat("a load-bearing rule. ", 400) + "\n\n## More\ndetail",
	}
}

func TestSystemPromptWithin_DigestTierWhenTerseExceedsBudget(t *testing.T) {
	skills := []Skill{bigCoreSkill("alpha"), bigCoreSkill("beta"), bigCoreSkill("gamma")}
	const budget = 120 // far too small for even one big terse Core
	out := SystemPromptWithin(skills, budget)

	for _, s := range skills {
		if !strings.Contains(out, s.Name) {
			t.Fatalf("skill %q must still be represented (nothing silently dropped)", s.Name)
		}
		if !strings.Contains(out, "one-line summary of "+s.Name) {
			t.Fatalf("digest must carry skill %q's description", s.Name)
		}
	}
	// The big Core must NOT have been inlined (that was the bloat).
	if strings.Contains(out, strings.Repeat("a load-bearing rule. ", 5)) {
		t.Fatal("a big terse Core must not be inlined under a tiny budget — it should digest")
	}
	if !strings.Contains(out, "digest") {
		t.Fatal("the digested skills should be marked as a digest tier")
	}
	// The preamble stays bounded (it blew ~8.7k tok before; assert it's now small).
	if got := estTokens(out); got > budget*4 {
		t.Fatalf("digest tier should keep the preamble small, got ~%d tok (budget %d)", got, budget)
	}
}

func TestSystemPromptWithin_TiersDegrade(t *testing.T) {
	// A skill with a SMALL Core but a BIG body: full is large, terse (the small Core) is tiny.
	s := Skill{
		Name:        "solo",
		Description: "desc of solo",
		Body:        "## Core\nthe one load-bearing rule.\n\n## Detail\n" + strings.Repeat("filler. ", 2000),
	}
	// No budget (<=0) always injects the full body.
	if out := SystemPromptWithin([]Skill{s}, 0); !strings.Contains(out, "## Detail") {
		t.Fatal("maxTokens<=0 must inject the full body")
	}
	// A mid budget that fits the small terse Core but not the big full body → terse tier.
	terse := terseBody(s)
	mid := estTokens(systemPromptPreamble) + estTokens(terse) + estTokens(s.Name) + 20
	out := SystemPromptWithin([]Skill{s}, mid)
	if !strings.Contains(out, "terse") || strings.Contains(out, "digest") || strings.Contains(out, "## Detail") {
		t.Fatalf("a budget fitting the terse Core (not the full body) should use the terse tier, got %d tok", estTokens(out))
	}
}
