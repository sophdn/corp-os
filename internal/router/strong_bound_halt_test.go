package router

import "testing"

// TestStrongBoundExhausted is the bug 1087 signal: once the frontier (bounded top)
// rung has served its full turn budget as the no-progress last resort, the router
// reports that the loop should HALT. This replaces the old behaviour where the
// stickyTop bypass let the frontier serve unbounded turns (Opus served 12 against a
// bound of 2, $3.45 unbudgeted). NextAdapter still keeps the rung — it never bounces a
// proven-stuck floor — but the loop reads this signal to stop instead of overspending.
func TestStrongBoundExhausted(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithBoundedTop(2)) // top bounded to 2
	if r.StrongBoundExhausted() {
		t.Fatal("a fresh router resting on the floor is not bound-exhausted")
	}
	r.EscalateForNoProgress(10) // cheap→strong (top) → sticky last resort
	if !r.stickyTop {
		t.Fatal("precondition: the no-progress escalation should have set stickyTop")
	}
	if r.StrongBoundExhausted() {
		t.Fatal("on the top rung with no bounded turns served yet, not exhausted")
	}
	r.NextAdapter() // bounded turn 1 of 2
	if r.StrongBoundExhausted() {
		t.Fatalf("after 1 of 2 bounded turns the bound is not yet spent (served=%d)", r.BoundedTurns())
	}
	r.NextAdapter() // bounded turn 2 of 2 → bound now spent
	if !r.StrongBoundExhausted() {
		t.Fatalf("after the full bound (%d/%d) the router must signal halt", r.BoundedTurns(), r.BoundMax())
	}
}

// TestStrongBoundExhausted_OnlyWhenStuckAndBounded: the halt signal is specific to the
// stuck-on-frontier last resort. An unbounded top never signals it, and a ROUTINE
// escalation (not sticky) is bounded by NextAdapter's bounce, not the loop halt — so it
// must not report exhausted either.
func TestStrongBoundExhausted_OnlyWhenStuckAndBounded(t *testing.T) {
	// No bound configured → never exhausted, however many turns the top rung serves.
	u := New(stub{"cheap", true}, stub{"strong", true})
	u.EscalateForNoProgress(10)
	u.NextAdapter()
	u.NextAdapter()
	if u.StrongBoundExhausted() {
		t.Fatal("an unbounded top rung never reports bound-exhausted")
	}

	// A routine (Observe) escalation does not set stickyTop, so the bound is enforced
	// by the bounce in NextAdapter — the loop-halt signal must stay false.
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()), WithBoundedTop(1))
	r.Observe(Signals{ToolErrors: 9}) // cheap→strong (routine, NOT sticky)
	r.NextAdapter()                   // serves the one bounded turn
	if r.StrongBoundExhausted() {
		t.Fatal("a non-sticky routine escalation must not trigger the strong-bound halt")
	}
}
