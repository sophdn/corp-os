package routing

import (
	"context"
	"testing"
)

// fixedClassifier returns a constant label, so a routing test isolates the heuristic
// short-circuits from the classifier.
type fixedClassifier struct{ label string }

func (f fixedClassifier) Classify(context.Context, string) (string, error) { return f.label, nil }

// TestRouteTestAuthoringDutyShortCircuits is the bug 1089 regression: a "write/add a
// test" duty routes to the test-authoring profile (which protects production, permits
// *_test.go), NOT atomic-coding-chain (which protects test files and would deny the
// deliverable). Production-fix duties that merely mention tests stay in the coding lane.
func TestRouteTestAuthoringDutyShortCircuits(t *testing.T) {
	r := NewRouter(fixedClassifier{label: LabelNoTrigger}, nil, "")

	testAuthoring := []string{
		"add a test for the recencyStartForBudget helper",
		"write unit tests covering the router escalation edges",
		"author a *_test.go for the compaction budget path",
		"extend the e2e test coverage for the spawn flow",
	}
	for _, duty := range testAuthoring {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("Route(%q): %v", duty, err)
		}
		if d.Profile != defaultTestAuthoringProfile {
			t.Errorf("Route(%q) = %q, want %q", duty, d.Profile, defaultTestAuthoringProfile)
		}
		if d.Label != testAuthoringHeuristicLabel {
			t.Errorf("Route(%q) label = %q, want %q", duty, d.Label, testAuthoringHeuristicLabel)
		}
	}

	// Production-fix duties (even when they name a test) stay in the coding lane, which
	// protects test files so a bug-fix can't fake green by editing the gate.
	codingLane := []string{
		"fix the failing test in internal/agent",
		"debug the bug that makes the test red",
		"fix the parser so the test passes",
	}
	for _, duty := range codingLane {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("Route(%q): %v", duty, err)
		}
		if d.Profile != defaultCodingProfile {
			t.Errorf("Route(%q) = %q, want the coding lane %q", duty, d.Profile, defaultCodingProfile)
		}
	}
}

// TestRouteTestRevisionDutyShortCircuits is the bug 1101 regression: a test-REVISION
// duty — strengthening an EXISTING *_test.go ("improve/strengthen/expand the test", "the
// test is too simple") — has no create/add/write authoring verb, yet its deliverable is
// still the test file. It must route to the test-authoring profile (which can edit the
// test), NOT atomic-coding-chain (which protects *_test.go and would deny the edit, so
// the worker thrashed protect-path denials up to Opus in the decompose-admin rehearsal).
func TestRouteTestRevisionDutyShortCircuits(t *testing.T) {
	r := NewRouter(fixedClassifier{label: LabelNoTrigger}, nil, "")

	revisionDuties := []string{
		"improve the test for the recencyStartForBudget helper",
		"strengthen the router escalation-edge tests",
		"expand the test coverage for the spawn flow",
		"enhance the existing characterization test",
		"harden the e2e test against flakiness",
		"broaden the tests to cover the error path",
		"flesh out the unit tests for the parser",
		"the test characterization_test.go you created is too simple, add cases",
		"the *_test.go is too weak — it needs more coverage",
	}
	for _, duty := range revisionDuties {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("Route(%q): %v", duty, err)
		}
		if d.Profile != defaultTestAuthoringProfile {
			t.Errorf("Route(%q) = %q, want the test-authoring lane %q", duty, d.Profile, defaultTestAuthoringProfile)
		}
		if d.Label != testAuthoringHeuristicLabel {
			t.Errorf("Route(%q) label = %q, want %q", duty, d.Label, testAuthoringHeuristicLabel)
		}
	}

	// A production bug-fix that merely MENTIONS a test (revision-flavoured verb but a
	// production-fix signal present) must still route to the coding lane — the revision
	// extension must not steal real bug-fixes that protect their test gate.
	productionFixMentioningTest := []string{
		"improve the retry logic in internal/agent so the failing test passes",
		"strengthen input validation to fix the bug the test caught",
		"expand the cache in src/cache.go, then repair the broken test",
	}
	for _, duty := range productionFixMentioningTest {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("Route(%q): %v", duty, err)
		}
		if d.Profile != defaultCodingProfile {
			t.Errorf("Route(%q) = %q, want the coding lane %q (production-fix precedence)", duty, d.Profile, defaultCodingProfile)
		}
	}

	// A revision-flavoured verb with NO test noun is a prose/synthesis duty and must NOT
	// be pulled into the test-authoring lane.
	nonTest := []string{
		"improve the documentation for the router package",
		"strengthen the argument in the design doc",
		"expand the README onboarding section",
	}
	for _, duty := range nonTest {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("Route(%q): %v", duty, err)
		}
		if d.Profile == defaultTestAuthoringProfile {
			t.Errorf("Route(%q) = %q, a non-test prose duty must not route to the test-authoring lane", duty, d.Profile)
		}
	}
}
