package router

import (
	"testing"

	"corpos/internal/model"
)

// fakeBudget is a StrongBudget stub: a shared counter with an explicit cap, so a test can prove
// two independent routers draw strong-rung turns from ONE pool.
type fakeBudget struct {
	cap   int
	count int
}

func (b *fakeBudget) Exhausted() bool {
	if b.cap <= 0 {
		return false
	}
	return b.count >= b.cap
}
func (b *fakeBudget) Take() { b.count++ }

// climbToStrong drives a two-rung router onto the strong rung via a routine (Observe) tool-error
// escalation, the same path a coding worker takes.
func climbToStrong(r *Router) {
	r.Observe(Signals{ToolErrors: 9})
}

// TestSharedStrongBudgetBoundsAcrossRouters is the bug-1165(b) core proof: a strong-turn budget
// shared by two SEPARATE routers (the respawn case — each worker builds a fresh router) pools the
// strong-rung turns, so once the run's budget is spent a fresh worker can no longer climb to Opus,
// even though its OWN per-worker bound has room. Without the shared budget each router's WithBoundedTop
// resets, and every respawn re-climbs.
func TestSharedStrongBudgetBoundsAcrossRouters(t *testing.T) {
	budget := &fakeBudget{cap: 2} // the whole run gets 2 strong turns

	// Worker 1: generous per-worker bound (5) so the SHARED budget is the binding constraint.
	w1 := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()), WithBoundedTop(5), WithSharedStrongBudget(budget))
	climbToStrong(w1)
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 turn 1: got %q, want strong (budget has room)", got)
	}
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 turn 2: got %q, want strong (budget's 2nd and last)", got)
	}
	if budget.count != 2 {
		t.Fatalf("after 2 served strong turns the shared budget count = %d, want 2", budget.count)
	}
	// The shared budget is now spent. w1 is refused a 3rd strong turn despite its per-worker bound (5)
	// having room — proof the SHARED pool binds.
	if got := w1.NextAdapter().Model(); got != "cheap" {
		t.Fatalf("w1 turn 3: got %q, want cheap (shared budget spent bounces the climb)", got)
	}
	if w1.BoundBlocked() == 0 {
		t.Fatal("a refusal driven by the spent shared budget must record a bound block")
	}

	// Worker 2 (the respawn): a FRESH router with its own fresh per-worker bound, same shared budget.
	// It must NOT be able to reach Opus at all — the run's strong turns are already spent.
	w2 := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()), WithBoundedTop(5), WithSharedStrongBudget(budget))
	climbToStrong(w2)
	if got := w2.NextAdapter().Model(); got != "cheap" {
		t.Fatalf("w2 (respawn) got %q, want cheap — a respawn must not re-climb Opus on a spent shared budget", got)
	}
	if budget.count != 2 {
		t.Fatalf("a refused climb must not consume budget; count = %d, want 2", budget.count)
	}
}

// TestSharedStrongBudgetBoundsStickyEscalation is the regression for the bug-1165(b) FIRST-CUT MISS:
// a worker that climbs to Opus via a retry_exhaustion / no-progress escalation sets stickyTop, and the
// per-worker bound deliberately bypasses on stickyTop (the run-10 trap). The first cut let that bypass
// also skip the SHARED budget, so respawns climbing via the (dominant) sticky path re-climbed Opus and
// blew the pool (live: strong 85% of spend). The shared budget must bind REGARDLESS of stickyTop.
func TestSharedStrongBudgetBoundsStickyEscalation(t *testing.T) {
	budget := &fakeBudget{cap: 2}

	// Worker 1 climbs to Opus via a STICKY retry-exhaustion escalation and serves the pool's 2 turns.
	w1 := New(stub{"cheap", true}, stub{"strong", true}, WithBoundedTop(5), WithSharedStrongBudget(budget))
	w1.EscalateForRetryExhaustion(9) // cheap→strong, sets stickyTop
	if !w1.stickyTop {
		t.Fatal("precondition: retry-exhaustion escalation to the top rung must set stickyTop")
	}
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 sticky turn 1: got %q, want strong", got)
	}
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 sticky turn 2: got %q, want strong", got)
	}
	if budget.count != 2 {
		t.Fatalf("shared budget count = %d, want 2", budget.count)
	}
	// With the pool spent, even w1 (sticky, per-worker bound of 5 untouched) must now HALT rather than
	// keep serving unbounded frontier — StrongBoundExhausted fires off the SHARED pool, not the bound.
	if !w1.StrongBoundExhausted() {
		t.Fatal("with the shared pool spent, a sticky worker on the frontier must report StrongBoundExhausted (halt)")
	}

	// Worker 2 (a respawn) climbs the SAME sticky path. The pool is spent, so it must be refused Opus —
	// the exact re-climb the first cut allowed through the stickyTop bypass.
	w2 := New(stub{"cheap", true}, stub{"strong", true}, WithBoundedTop(5), WithSharedStrongBudget(budget))
	w2.EscalateForRetryExhaustion(9)
	if got := w2.NextAdapter().Model(); got == "strong" {
		t.Fatal("respawn re-climbed Opus via the sticky path on a spent shared pool — the stickyTop bypass must NOT skip the shared budget")
	}
	if !w2.StrongBudgetExhausted() {
		t.Fatal("StrongBudgetExhausted must report the shared pool as the halt reason for an accurate verdict")
	}
	if budget.count != 2 {
		t.Fatalf("a refused sticky climb must not consume budget; count = %d, want 2", budget.count)
	}
}

// A zero-cap shared budget tracks strong turns but never refuses — both routers serve Opus freely
// (the -max-strong-turns 0 tracking-only default).
func TestSharedStrongBudgetZeroCapNeverBinds(t *testing.T) {
	budget := &fakeBudget{cap: 0}
	for i := 0; i < 3; i++ {
		r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()), WithBoundedTop(2), WithSharedStrongBudget(budget))
		climbToStrong(r)
		if got := r.NextAdapter().Model(); got != "strong" {
			t.Fatalf("router %d: got %q, want strong (zero-cap budget never binds)", i, got)
		}
	}
	if budget.count != 3 {
		t.Fatalf("zero-cap budget still tallies: count = %d, want 3", budget.count)
	}
}

// A shared strong budget guards the frontier even WITHOUT a per-worker WithBoundedTop: on its own it
// marks the top rung guarded, so once the pool is spent the climb is refused. (main pairs it with
// WithBoundedTop, but the mechanism must stand alone.)
func TestSharedStrongBudgetGuardsTopWithoutPerWorkerBound(t *testing.T) {
	budget := &fakeBudget{cap: 1}
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()), WithSharedStrongBudget(budget))
	climbToStrong(r)
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Fatalf("turn 1: got %q, want strong (pool has room, no per-worker bound needed)", got)
	}
	if got := r.NextAdapter().Model(); got != "cheap" {
		t.Fatalf("turn 2: got %q, want cheap (shared pool spent bounces the climb without any WithBoundedTop)", got)
	}
	if budget.count != 1 {
		t.Fatalf("budget count = %d, want 1", budget.count)
	}
}

// WithSharedStrongBudget is a safe no-op on a nil budget or a single-tier ladder (no strong rung to
// guard) — wiring it never panics or mis-guards the floor.
func TestSharedStrongBudgetNilAndSingleTierNoOp(t *testing.T) {
	// nil budget: behaves exactly like a plain bounded router.
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()), WithBoundedTop(1), WithSharedStrongBudget(nil))
	climbToStrong(r)
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Fatalf("nil-budget turn 1: got %q, want strong", got)
	}
	if got := r.NextAdapter().Model(); got != "cheap" {
		t.Fatalf("nil-budget turn 2: got %q, want cheap (per-worker bound of 1 spent)", got)
	}

	// single-tier ladder: no strong rung, so the budget guards nothing and the floor still serves.
	budget := &fakeBudget{cap: 0}
	s := NewLadder([]model.Adapter{stub{"solo", true}}, 0, WithSharedStrongBudget(budget))
	if got := s.NextAdapter().Model(); got != "solo" {
		t.Fatalf("single-tier: got %q, want solo", got)
	}
	if budget.count != 0 {
		t.Fatalf("a single-tier ladder has no strong rung to tally; count = %d, want 0", budget.count)
	}
}
