package main

import (
	"flag"
	"strings"
	"testing"

	"corpos/internal/cost"
)

// TestBuildAdapterDefaults locks the per-provider default models to the canonical
// ladder. In particular the anthropic fallback must default to Opus, never the
// bring-up-era Haiku — the silent default that let a stale rehearsal example put
// haiku back in the loop. Regression for bug
// corpos-locked-model-ladder-not-encoded-as-runtime-default.
func TestBuildAdapterDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		provider string
		want     string
	}{
		{"anthropic", canonicalStrongModel},
		{"openrouter", canonicalMidModel},
		{"openai", canonicalLowModel},
	}
	for _, c := range cases {
		a, err := buildAdapter(c.provider, "", "")
		if err != nil {
			t.Fatalf("buildAdapter(%q): %v", c.provider, err)
		}
		if a.Model() != c.want {
			t.Errorf("buildAdapter(%q) default model = %q, want %q", c.provider, a.Model(), c.want)
		}
	}
	if canonicalStrongModel == "claude-haiku-4-5-20251001" {
		t.Fatal("canonical strong rung must be Opus, not the bring-up-era Haiku")
	}
}

// TestLadderOverridden verifies a bare invocation reads as canonical (no override)
// while any explicit ladder flag reads as an override — so main applies the
// canonical ladder only when the operator did not choose one.
func TestLadderOverridden(t *testing.T) {
	t.Parallel()
	mkFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		for _, n := range []string{"mid-provider", "mid-model", "mid-model-url", "strong-provider", "strong-model", "strong-model-url"} {
			fs.String(n, "", "")
		}
		fs.String("prompt", "", "")
		fs.String("profile", "", "")
		return fs
	}
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare run is canonical", []string{"-prompt", "hi"}, false},
		{"no args is canonical", nil, false},
		{"strong-provider overrides", []string{"-strong-provider", "anthropic"}, true},
		{"strong-model overrides", []string{"-strong-model", "claude-haiku-4-5-20251001"}, true},
		{"mid-provider overrides", []string{"-mid-provider", "openrouter"}, true},
		{"mid-model overrides", []string{"-mid-model", "x"}, true},
		{"non-ladder flag is not an override", []string{"-profile", "orchestrate"}, false},
	}
	for _, c := range cases {
		fs := mkFS()
		if err := fs.Parse(c.args); err != nil {
			t.Fatalf("%s: parse: %v", c.name, err)
		}
		if got := ladderOverridden(fs); got != c.want {
			t.Errorf("%s: ladderOverridden = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCanonicalLadderString names all three locked rungs in cheapest→strongest order.
func TestCanonicalLadderString(t *testing.T) {
	t.Parallel()
	s := canonicalLadderString()
	for _, want := range []string{canonicalLowModel, canonicalMidModel, canonicalStrongModel} {
		if !strings.Contains(s, want) {
			t.Errorf("canonicalLadderString %q missing %q", s, want)
		}
	}
	if i, j := strings.Index(s, canonicalMidModel), strings.Index(s, canonicalStrongModel); i > j {
		t.Errorf("ladder out of order: mid (%d) after strong (%d) in %q", i, j, s)
	}
}

// TestCanonicalCodingRung keeps the atomic-coding-chain intermediate rung in the
// same source-of-truth block as the rest of the ladder (acceptance d): it is
// DeepSeek-V3.2, an OpenRouter model id.
func TestCanonicalCodingRung(t *testing.T) {
	t.Parallel()
	if !strings.HasPrefix(canonicalCodingRung, "deepseek/") {
		t.Errorf("coding rung = %q, want a deepseek/* model", canonicalCodingRung)
	}
}

// TestCanonicalRungsPricedAndTiered asserts every canonical ladder rung id
// resolves to BOTH a cost price-table entry and a ladder-tier classification.
// The ladder consts (this file's block) and the cost tables (internal/cost) are
// separate hand-maintained sources of truth with nothing else asserting they
// stay aligned — exactly the gap that let the coding rung ship [UNPRICED] in
// -run-rate (bug 1104). This is the regression guard (bug 1108): adding a future
// canonical rung without pricing/tiering it now breaks the gate. It references
// the ladder consts directly (no duplicated id list) so there is one definition.
func TestCanonicalRungsPricedAndTiered(t *testing.T) {
	t.Parallel()
	rungs := map[string]string{
		"low":    canonicalLowModel,
		"mid":    canonicalMidModel,
		"strong": canonicalStrongModel,
		"coding": canonicalCodingRung,
	}
	for name, id := range rungs {
		if !cost.IsPriced(id) {
			t.Errorf("canonical %s rung %q has no cost price-table entry — it would report UNPRICED in -run-rate", name, id)
		}
		if cost.Classify(id) == cost.TierUnknown {
			t.Errorf("canonical %s rung %q classifies as TierUnknown — no tier-table/family entry", name, id)
		}
	}
}
