package skills

import (
	"strings"
	"testing"
)

// coredCodingSkills are the high-traffic coding-path disciplines that MUST author
// a tier-1 `## Core` section, so a narrow-window floor/mid worker gets the real
// load-bearing rules instead of the description+outline fallback. Only the
// embedded library/ (vanilla, committed) skills are listed: go-conventions and
// the other per-language skills live in the gitignored userlib/ overlay and are
// absent in a vanilla clone, so asserting them here would fail the gate on CI.
var coredCodingSkills = []string{
	"coding-philosophy",
	"code-standards",
	"dependency-vetting-discipline",
}

// coreTokenBudget caps a `## Core` section's estimated token cost. The convention
// targets ~300–500 tok (the floor's per-skill share); this upper bound leaves
// headroom while still failing a Core that smuggled in the whole body.
const coreTokenBudget = 700

// TestCodingSkillsAuthorACore is the acceptance gate for the tiered-skill
// backfill: every high-traffic coding skill carries a real `## Core` that the
// terse injector returns verbatim (not the outline fallback), and it fits a
// floor worker's window with headroom.
func TestCodingSkillsAuthorACore(t *testing.T) {
	l, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	skills, err := l.Select(coredCodingSkills)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(skills) != len(coredCodingSkills) {
		t.Fatalf("selected %d skills, want %d (a coding skill is missing from the embedded library)", len(skills), len(coredCodingSkills))
	}
	for _, s := range skills {
		core := coreSection(s.Body)
		if core == "" {
			t.Errorf("%s: no `## Core` section — a floor worker would get the outline fallback, losing the rules", s.Name)
			continue
		}
		// terseBody must PREFER the authored core (not the description+outline).
		if got := terseBody(s); got != core {
			t.Errorf("%s: terseBody did not return the `## Core` section\n got: %q", s.Name, oneLineFold(got))
		}
		if tok := estTokens(core); tok > coreTokenBudget {
			t.Errorf("%s: `## Core` is %d tok, over the %d budget — keep it terse (the load-bearing rules, not the whole body)", s.Name, tok, coreTokenBudget)
		}
		if tok := estTokens(core); tok < 40 {
			t.Errorf("%s: `## Core` is only %d tok — looks like a stub, not the load-bearing rules", s.Name, tok)
		}
	}
}

// TestNarrowWindowInjectsCore confirms that on a window too small for the full
// body, SystemPromptWithin injects the skill under the terse header and the
// emitted text IS the `## Core` rules — the end-to-end floor-worker path.
func TestNarrowWindowInjectsCore(t *testing.T) {
	l, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	skills, err := l.Select([]string{"coding-philosophy"})
	if err != nil || len(skills) != 1 {
		t.Fatalf("Select coding-philosophy: %v (got %d)", err, len(skills))
	}
	s := skills[0]
	core := coreSection(s.Body)
	if core == "" {
		t.Fatal("coding-philosophy has no `## Core` to inject")
	}
	// A budget below the full body forces the terse tier.
	budget := estTokens(systemPromptPreamble) + estTokens(core) + 64
	out := SystemPromptWithin(skills, budget)
	if !strings.Contains(out, "Skill (terse") {
		t.Errorf("expected the terse header on a narrow window:\n%s", out)
	}
	// A distinctive core rule must be present; the full-body-only "References"
	// section must not (proving the body was elided, not the core).
	if !strings.Contains(out, "No escape-hatches") {
		t.Errorf("narrow-window prompt is missing a `## Core` rule:\n%s", out)
	}
	if estTokens(out) > budget {
		t.Errorf("narrow-window prompt = %d tok, over budget %d", estTokens(out), budget)
	}
}

// oneLineFold collapses whitespace for a readable single-line failure message.
func oneLineFold(s string) string { return strings.Join(strings.Fields(s), " ") }
