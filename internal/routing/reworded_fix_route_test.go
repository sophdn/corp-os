package routing

import (
	"context"
	"testing"
)

// TestRewordedFixDutyStaysOutOfTestAuthoring is the bug 1161 routing regression: when the
// orchestrator RE-WORDS "fix bug X" into an investigation that drops the "fix" keyword but
// still asks to change production behavior AND author a regression test, the duty must reach
// the coding lane — NOT test-authoring-chain, where the worker (production-protected) would
// tamper the failing test to green. The diagnosis signals (investigate / why / failed to)
// keep such a reworded fix duty in the coding lane.
func TestRewordedFixDutyStaysOutOfTestAuthoring(t *testing.T) {
	r := NewRouter(fixedClassifier{label: LabelNoTrigger}, nil, "")
	// The verbatim reworded duty from run 01KXE5MERA0R2T093B2F0CQHAZ.
	reworded := "Investigate why the cost ceiling (max-cost-usd) failed to halt an escalated run despite a 0.05 cap. Check the breakerTrip (internal/agent/loop.go) and ensure it checks cumulative spend between each call. Create a regression test that demonstrates the breaker tripping mid-escalation. If this is a design limitation, document it clearly."
	d, err := r.Route(context.Background(), reworded)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Profile == defaultTestAuthoringProfile {
		t.Fatalf("a reworded fix duty must not route to test-authoring-chain (production-protected → oracle tamper); got %q", d.Profile)
	}
	if d.Profile != defaultCodingProfile {
		t.Errorf("Route = %q, want the coding lane %q", d.Profile, defaultCodingProfile)
	}

	// Guard the exclusion's blast radius: a PURE test-authoring duty that happens to name a
	// production symbol (the unit under test) is unaffected.
	pure := "author a table-driven test for the breakerTrip helper covering the cost ceiling"
	if d, _ := r.Route(context.Background(), pure); d.Profile != defaultTestAuthoringProfile {
		t.Errorf("pure test-authoring should still route to test-authoring, got %q", d.Profile)
	}
}
