package agent

import (
	"testing"

	"corpos/internal/cost"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/router"
)

// climbToStrong drives a local→mid→strong worker router up to the strong rung via two ROUTINE
// (Observe / repeated_tool_error) escalations — a NON-sticky climb.
func climbToStrong(r *router.Router) {
	r.Observe(router.Signals{ToolErrors: 9}) // local→mid
	r.Observe(router.Signals{ToolErrors: 9}) // mid→strong
}

// climbToStrongSticky drives the same router to the strong rung via retry-exhaustion escalations —
// the STICKY path (sets stickyTop). This is how a coding worker actually reaches Opus in a live run
// (retry_exhaustion / no-progress / overflow all set stickyTop). It matters because the per-worker
// bound DELIBERATELY bypasses on stickyTop (the run-10 trap), so any policy that must bind regardless
// of stickyTop — the shared strong-turn budget — is exercised ONLY down this path. Testing a
// router-policy through the routine path alone is the gap that shipped bug-1165(b)'s broken first cut
// (see TESTING.md, "policy-path matrix").
func climbToStrongSticky(r *router.Router) {
	r.EscalateForRetryExhaustion(9) // local→mid
	r.EscalateForRetryExhaustion(9) // mid→strong (sets stickyTop)
}

// TestSpawnerThreadsSharedStrongTurnBudget proves the spawner wires ONE strong-turn budget into every
// worker's router (bug 1165(b)): two routers built from the same spawner (the first coding worker and
// its respawn) share the pool, so once the run's strong turns are spent a respawn can no longer climb
// to Opus — even though each worker's OWN WithStrongBound has room. This is the wiring the router-level
// TestSharedStrongBudgetBoundsAcrossRouters proves in isolation.
func TestSpawnerThreadsSharedStrongTurnBudget(t *testing.T) {
	budget := cost.NewStrongTurnBudget(2) // the whole run gets 2 strong turns
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithStrongTier(model.NewEcho("strong"), 1),
		WithStrongBound(5), // generous per-worker bound so the SHARED budget is the binding constraint
		WithSpawnerStrongTurnBudget(budget))
	p := &profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}}

	// Worker 1 climbs local→mid→strong and serves both of the run's strong turns.
	w1 := s.routerFor(p)
	climbToStrong(w1)
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 turn 1: got %q, want strong", got)
	}
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 turn 2: got %q, want strong", got)
	}
	if budget.Count() != 2 {
		t.Fatalf("shared budget count after 2 strong turns = %d, want 2", budget.Count())
	}

	// Worker 2 (the respawn): a FRESH router with a fresh per-worker bound, same shared pool — it
	// must not reach strong at all.
	w2 := s.routerFor(p)
	climbToStrong(w2)
	if got := w2.NextAdapter().Model(); got == "strong" {
		t.Fatal("respawn worker re-climbed Opus on a spent shared strong budget — the pool did not bind across workers")
	}
	if budget.Count() != 2 {
		t.Fatalf("a refused climb must not consume budget; count = %d, want 2", budget.Count())
	}
}

// TestSpawnerThreadsSharedStrongTurnBudget_StickyPath is the wired-spawner counterpart to the
// router-unit TestSharedStrongBudgetBoundsStickyEscalation. It closes the exact gap that shipped a
// broken bug-1165(b) first cut: the shared budget was proven through the spawner via the ROUTINE path
// (above) and through the STICKY path only in router ISOLATION — never through the spawner wiring AND
// the sticky path together, which is the layer the live run exercised. A coding worker reaches Opus via
// retry_exhaustion (sticky), and the per-worker bound's stickyTop bypass was skipping the shared budget,
// so every unit test passed while the live run rode Opus unbounded. This test drives the spawner-threaded
// budget down the sticky path and asserts a respawn is still refused Opus.
func TestSpawnerThreadsSharedStrongTurnBudget_StickyPath(t *testing.T) {
	budget := cost.NewStrongTurnBudget(2)
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithStrongTier(model.NewEcho("strong"), 1),
		WithStrongBound(5), // generous per-worker bound so the SHARED budget is the binding constraint
		WithSpawnerStrongTurnBudget(budget))
	p := &profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}}

	// Worker 1 climbs the STICKY path (retry_exhaustion) and serves the pool's 2 strong turns.
	w1 := s.routerFor(p)
	climbToStrongSticky(w1)
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 sticky turn 1: got %q, want strong", got)
	}
	if got := w1.NextAdapter().Model(); got != "strong" {
		t.Fatalf("w1 sticky turn 2: got %q, want strong", got)
	}
	if budget.Count() != 2 {
		t.Fatalf("shared budget count after 2 sticky strong turns = %d, want 2", budget.Count())
	}

	// Worker 2 (respawn) climbs the SAME sticky path: the pool is spent, so despite the per-worker
	// bound's stickyTop bypass it MUST be refused Opus — the exact re-climb the first cut allowed.
	w2 := s.routerFor(p)
	climbToStrongSticky(w2)
	if got := w2.NextAdapter().Model(); got == "strong" {
		t.Fatal("respawn re-climbed Opus via the sticky path on a spent shared pool through the spawner wiring — the stickyTop bypass must not skip the shared budget")
	}
	if budget.Count() != 2 {
		t.Fatalf("a refused sticky climb must not consume budget; count = %d, want 2", budget.Count())
	}
}
