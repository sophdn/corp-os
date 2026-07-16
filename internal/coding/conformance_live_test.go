package coding

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
)

// matcherGoal is the prose the profile-matcher core was built from (see drive_profilematch).
const matcherGoal = `In package profilematch, build a deterministic profile matcher. Define a type Candidate with fields Name string and Keywords []string, and a function Match(prompt string, candidates []Candidate, fallback string) (string, int) that scores each candidate by the NUMBER of its Keywords that appear as whole, case-insensitive words in prompt, and returns the Name of the HIGHEST-scoring candidate together with that score. When no candidate has any keyword match (best score 0), it returns fallback and 0. When two candidates tie on score, the candidate earlier in the slice wins. Whole-word matching means the keyword "bug" matches "a bug here" but not "debugger".`

// underSpecifiedMatcher is the draft the feature pipeline actually produced on the local floor:
// it PASSES its one-example-per-behavior oracles but violates the spec three ways — the score
// is hard-coded to 1 (never counted), the FIRST match is returned (not the highest-scoring),
// and matching is case-SENSITIVE (`word == keyword`).
const underSpecifiedMatcher = `package profilematch

import "strings"

type Candidate struct {
	Name     string
	Keywords []string
}

func Match(prompt string, candidates []Candidate, fallback string) (string, int) {
	var firstMatch *Candidate
	for i, candidate := range candidates {
		for _, keyword := range candidate.Keywords {
			for _, word := range strings.Fields(prompt) {
				if word == keyword && firstMatch == nil {
					firstMatch = &candidates[i]
				}
			}
		}
	}
	if firstMatch != nil {
		return firstMatch.Name, 1
	}
	return fallback, 0
}`

// correctMatcher is the corrected implementation (counts keywords, returns the highest scorer,
// case-insensitive, tie→earlier).
const correctMatcher = `package profilematch

import "strings"

type Candidate struct {
	Name     string
	Keywords []string
}

func Match(prompt string, candidates []Candidate, fallback string) (string, int) {
	words := make(map[string]struct{})
	for _, w := range strings.Fields(prompt) {
		words[strings.ToLower(w)] = struct{}{}
	}
	bestName, bestScore := fallback, 0
	for _, c := range candidates {
		score := 0
		counted := make(map[string]struct{})
		for _, kw := range c.Keywords {
			lk := strings.ToLower(kw)
			if _, dup := counted[lk]; dup {
				continue
			}
			counted[lk] = struct{}{}
			if _, ok := words[lk]; ok {
				score++
			}
		}
		if score > bestScore {
			bestName, bestScore = c.Name, score
		}
	}
	return bestName, bestScore
}`

// TestLiveConformanceCriticOnMatcher is the efficacy proof for the "green but under-specified"
// fix: the critic, given the prose spec + the pipeline's own under-specified draft, must find a
// real counterexample (recall), and must NOT flag the corrected implementation (precision). The
// critic runs on the mid reasoning tier (Gemini) — it has no deterministic backstop, so it is
// not a local-floor job. Gated: CORPOS_LIVE=1 + OPENROUTER_API_KEY.
func TestLiveConformanceCriticOnMatcher(t *testing.T) {
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 (and source corpos.env) to run the live conformance critic")
	}
	or := os.Getenv("OPENROUTER_API_KEY")
	if or == "" {
		t.Skip("OPENROUTER_API_KEY required for the mid-tier critic")
	}
	mid := model.NewOpenAICompat("google/gemini-3.1-flash-lite", "https://openrouter.ai/api/v1",
		model.WithOACKey(or), model.WithOACRequireKey(), model.WithOACUsageAccounting())
	critic := NewConformanceCritic(mid)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// RECALL: the under-specified draft must be caught with a concrete counterexample.
	bad, err := critic.Probe(ctx, matcherGoal, underSpecifiedMatcher)
	if err != nil {
		t.Fatalf("probe(under-specified) infra error: %v", err)
	}
	t.Logf("=== under-specified draft verdict: conforms=%v ===\n%s", bad.Conforms, bad.Counterexample)
	if bad.Conforms {
		t.Fatalf("critic FAILED to catch the under-specified draft (hard-coded score / first-not-highest / case-sensitive)")
	}
	if strings.TrimSpace(bad.Counterexample) == "" {
		t.Fatal("a violation must carry a counterexample")
	}

	// PRECISION: the corrected implementation should pass (a false positive here is a critic
	// imprecision — logged, not hard-failed, since a single adversarial probe can over-reach).
	good, err := critic.Probe(ctx, matcherGoal, correctMatcher)
	if err != nil {
		t.Fatalf("probe(correct) infra error: %v", err)
	}
	t.Logf("=== correct impl verdict: conforms=%v ===\n%s", good.Conforms, good.Counterexample)
	if !good.Conforms {
		t.Logf("NOTE: critic false-positive on the correct impl (precision miss) — review the counterexample above")
	}
}
