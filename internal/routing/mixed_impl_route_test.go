package routing

import (
	"context"
	"testing"
)

// TestRouteMixedTestAndImplStaysOutOfTestAuthoring is the bug 1150 regression: a greenfield
// duty that asks to BOTH author a test AND implement the production it tests must NOT route
// to test-authoring-chain. That profile protects **/*.go, so it could never create the impl
// the gate needs and the run thrashes Qwen→gemini→deepseek→opus to timeout with nothing
// merged. The productionImplSignals exclusion sends such a mixed duty to the coding lane
// instead (a partial deliverable beats a thrash); the full deliverable needs the orchestrator
// to decompose it into an implement atom then a test-authoring atom.
func TestRouteMixedTestAndImplStaysOutOfTestAuthoring(t *testing.T) {
	r := NewRouter(fixedClassifier{label: LabelNoTrigger}, nil, "")
	mixed := []string{
		"Write a table-driven test calc/calc_test.go, then implement calc/calc.go so that go test passes.",
		"author a unit test for the parser and implement the parser package",
		"add a test and implement the function so the test passes",
	}
	for _, duty := range mixed {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("Route(%q): %v", duty, err)
		}
		if d.Profile == defaultTestAuthoringProfile {
			t.Errorf("Route(%q) = test-authoring-chain, but a mixed test+impl duty must NOT route there (production-protected → thrash)", duty)
		}
		if d.Profile != defaultCodingProfile {
			t.Errorf("Route(%q) = %q, want the coding lane %q (mixed duty falls through to coding)", duty, d.Profile, defaultCodingProfile)
		}
	}
}

// TestImplExclusionDoesNotBreakPureTestAuthoring guards the exclusion's blast radius: a
// PURE test-authoring duty (no implement/production signal) still routes to test-authoring.
func TestImplExclusionDoesNotBreakPureTestAuthoring(t *testing.T) {
	r := NewRouter(fixedClassifier{label: LabelNoTrigger}, nil, "")
	pure := []string{
		"author a table-driven test for the existing calc.Add function",
		"write unit tests covering the parser package",
		"add a test case for the divide-by-zero path",
	}
	for _, duty := range pure {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("Route(%q): %v", duty, err)
		}
		if d.Profile != defaultTestAuthoringProfile {
			t.Errorf("Route(%q) = %q, want test-authoring %q (a pure test-authoring duty must be unaffected by the impl exclusion)", duty, d.Profile, defaultTestAuthoringProfile)
		}
	}
}
