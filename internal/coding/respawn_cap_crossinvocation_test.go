package coding

import (
	"context"
	"errors"
	"testing"

	"corpos/internal/agent"
	"corpos/internal/cost"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

// noopProvider is an inert tool provider: the model workers in this test never reach a
// tool dispatch (the spawn is bounded/aborted before or during the first turn), so the
// provider only has to satisfy the interface.
type noopProvider struct{}

func (noopProvider) Dispatch(context.Context, tool.Call) tool.Result { return tool.Result{} }

// TestSharedSpawnBudgetBoundsRespawnsAcrossInvocations is the load-bearing regression for
// bug 1151's documented decision: each agent-orchestrate re-invocation of the coding path
// builds a FRESH coding organ + ModelWorker (buildCodingPath rebuilds the organ), but they
// all reuse the ONE spawner the run wired the tree-wide SpawnBudget onto. So cross-invocation
// coding respawns are bounded by -max-spawns even though no precise per-atom cap exists.
//
// Two INDEPENDENTLY-constructed ModelWorkers (standing in for two coding-path invocations)
// share one budget(cap=2)-wired spawner: the two spawns from the first worker consume the
// budget, and the first spawn from the SECOND worker is refused with ErrSpawnBudgetExhausted.
// If a future refactor gave each organ its own budget, this second-worker spawn would wrongly
// succeed and the count would climb past the cap.
func TestSharedSpawnBudgetBoundsRespawnsAcrossInvocations(t *testing.T) {
	adapter := model.NewEcho("w", model.Response{Text: "done", StopReason: model.StopEndTurn})
	budget := cost.NewSpawnBudget(2)
	// One spawner for the whole run — exactly how buildCodingPath captures a single spawner
	// and every fresh organ's NewModelWorker(spawner, …) reuses it.
	sp := agent.NewSpawner(noopProvider{}, nil, nil, adapter, agent.WithSpawnerSpawnBudget(budget))
	p := &profile.JobProfile{Name: "coding", Tier: profile.TierLocal}
	at := AtomicTask{Slug: "a", Goal: "build foo", Worker: WorkerConfig{Kind: WorkerModel}}

	// Invocation 1: a fresh organ's worker spends the two-spawn budget. The attempts may end
	// in a (non-budget) command error — the point is only that they were PERMITTED to spawn
	// (Reserve runs before any setup), so they must not be the budget-exhausted refusal.
	w1 := NewModelWorker(sp, p)
	for i := 1; i <= 2; i++ {
		res := w1.Attempt(context.Background(), at, "", Feedback{})
		if res.CommandErr != nil && errors.Is(res.CommandErr, agent.ErrSpawnBudgetExhausted) {
			t.Fatalf("invocation-1 attempt %d was within budget but got refused: %v", i, res.CommandErr)
		}
	}
	if budget.Count() != 2 {
		t.Fatalf("both in-budget spawns must reserve; Count = %d, want 2", budget.Count())
	}

	// Invocation 2: a DIFFERENT fresh worker. Because it shares the budget, its first spawn is
	// refused — the cross-invocation backstop bug 1151 relies on instead of a precise per-atom cap.
	w2 := NewModelWorker(sp, p)
	res := w2.Attempt(context.Background(), at, "", Feedback{})
	if res.CommandErr == nil || !errors.Is(res.CommandErr, agent.ErrSpawnBudgetExhausted) {
		t.Fatalf("a second-invocation spawn past the shared budget must be refused with ErrSpawnBudgetExhausted, got %v", res.CommandErr)
	}
	if budget.Count() != 2 {
		t.Errorf("a refused cross-invocation spawn must not consume budget; Count = %d, want 2", budget.Count())
	}
}
