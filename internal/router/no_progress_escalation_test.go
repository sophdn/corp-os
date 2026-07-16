package router

import (
	"strings"
	"testing"
)

// TestEscalateForNoProgressClimbsOneRung: a no-progress stall lifts the rung
// mid-turn with the retry_exhaustion trigger and a stall-naming detail, regardless
// of escalation config (the breaker is the trigger, not the per-turn tally).
func TestEscalateForNoProgressClimbsOneRung(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0) // no escalation config needed
	e := r.EscalateForNoProgress(10)
	if e.Direction != EdgeEscalate || e.Trigger != TriggerRetryExhaustion {
		t.Fatalf("a stalled floor must escalate on retry_exhaustion, got %+v", e)
	}
	if e.FromModel != "local" || e.ToModel != "mid" {
		t.Errorf("edge models = %s→%s, want local→mid", e.FromModel, e.ToModel)
	}
	if e.StateBefore != "cheap" || e.StateAfter != "escalated" {
		t.Errorf("states = %s→%s, want cheap→escalated", e.StateBefore, e.StateAfter)
	}
	if e.FiredThreshold != 10 {
		t.Errorf("fired threshold = %v, want the stall length 10", e.FiredThreshold)
	}
	if !strings.Contains(e.Detail, "stalled_rounds=10") {
		t.Errorf("detail = %q, want it to name the stall length", e.Detail)
	}
	// A second stall above the floor climbs again (mid→strong, state_before escalated).
	e2 := r.EscalateForNoProgress(10)
	if e2.StateBefore != "escalated" || e2.FromModel != "mid" || e2.ToModel != "strong" {
		t.Errorf("second climb = %s %s→%s, want escalated mid→strong", e2.StateBefore, e2.FromModel, e2.ToModel)
	}
}

// TestEscalateForNoProgressNoopAtTopRung: at the top rung there is no higher rung
// to lift to, so the method emits no edge and the caller lets the breaker hard-stop.
func TestEscalateForNoProgressNoopAtTopRung(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true})
	if e := r.EscalateForNoProgress(5); e.ToModel != "strong" { // cheap → strong
		t.Fatalf("first climb should reach strong, got %+v", e)
	}
	if e := r.EscalateForNoProgress(5); e.Direction != EdgeNone { // strong is top
		t.Errorf("at the top rung a no-progress escalation must emit no edge, got %+v", e)
	}
}

// TestEscalateForNoProgressSticksTopRungPastBound is the run-10 fix at the ROUTER
// level: once a no-progress stall (the breaker's last resort) escalates onto the
// bounded top rung, NextAdapter no longer bounces the strong rung back into the stuck
// floor — it keeps serving the top rung. Total frontier CONSUMPTION is bounded one
// level up: the loop halts via Router.StrongBoundExhausted once the bound is spent
// (bug 1087, TestStrongBoundExhausted) — so this primitive's job is only "don't bounce
// to the dead floor", not "serve forever".
func TestEscalateForNoProgressSticksTopRungPastBound(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithBoundedTop(1)) // top bounded to 1 turn
	r.EscalateForNoProgress(10)                                            // cheap→strong (top) → sticky
	if !r.stickyTop {
		t.Fatal("a no-progress escalation onto the top rung should set stickyTop")
	}
	for i := 0; i < 4; i++ {
		if got := r.NextAdapter().Model(); got != "strong" {
			t.Fatalf("turn %d: stickyTop should keep the strong rung, got %s", i, got)
		}
	}
	if r.BoundBlocked() != 0 {
		t.Errorf("stickyTop should bypass the bound (0 blocks), got %d", r.BoundBlocked())
	}
}

// TestBoundStillGatesRoutineEscalation: a routine (Observe) escalation onto the
// bounded top rung still respects the bound — only the no-progress last resort
// sticks, so the frontier stays escalation-gated for the normal path.
func TestBoundStillGatesRoutineEscalation(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()), WithBoundedTop(1))
	r.Observe(Signals{ToolErrors: 9}) // cheap→strong (routine, NOT sticky)
	if r.stickyTop {
		t.Fatal("a routine escalation must not set stickyTop")
	}
	if got := r.NextAdapter().Model(); got != "strong" { // first top turn within bound
		t.Fatalf("first bounded turn should serve strong, got %s", got)
	}
	if got := r.NextAdapter().Model(); got == "strong" { // bound spent → bounced down
		t.Errorf("routine escalation past the bound must drop a rung, got %s", got)
	}
	if r.BoundBlocked() == 0 {
		t.Error("a routine climb past the bound should register a block")
	}
}

// TestStickyTopClearsOnDeescalation: a clean recovery clears stickyTop so the
// session returns to bounded frontier use (both de-escalation paths).
func TestStickyTopClearsOnDeescalation(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithBoundedTop(1))
	r.EscalateForNoProgress(10) // cheap→strong → sticky
	r.DeEscalateToFloor()
	if r.stickyTop {
		t.Error("DeEscalateToFloor must clear stickyTop")
	}

	r2 := New(stub{"cheap", true}, stub{"strong", true}, WithBoundedTop(1))
	r2.EscalateForNoProgress(10) // cheap→strong → sticky
	r2.Observe(Signals{})        // clean 1
	r2.Observe(Signals{})        // clean 2 (K=2) → de-escalate, clears sticky
	if r2.stickyTop {
		t.Error("Observe de-escalation must clear stickyTop")
	}
}
