package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/cost"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

// costingAdapter ends the turn in one call but reports a fixed provider cost, so a
// spawned worker's spend can be asserted against the shared meter.
type costingAdapter struct{ usd float64 }

func (a costingAdapter) Model() string   { return "coster" }
func (a costingAdapter) Available() bool { return true }
func (a costingAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{
		Model:      "coster",
		Text:       "done",
		StopReason: model.StopEndTurn,
		Usage:      model.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: a.usd, CostReported: true},
	}, nil
}

// A wired cost meter makes the breaker bound the CUMULATIVE tree cost, not just this
// loop's own ledger: pre-loading the meter with spawned-worker spend (the money a
// delegating orchestrator's own ledger never sees) trips the orchestrator's breaker
// even though its OWN calls stay cheap — the exact gap bug 1124 describes.
func TestCostMeterBoundsTreeNotJustOwnLedger(t *testing.T) {
	m := alwaysCalls{
		call:  tool.Call{ID: "c", Surface: "fs", Action: "grep"},
		usage: model.Usage{InputTokens: 10, OutputTokens: 1, CostUSD: 0.10, CostReported: true},
	}
	meter := cost.NewMeter(1.0)
	meter.Add(0.85) // worker spend already on the shared tree total

	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(100), WithCostMeter(meter),
	).Run(context.Background(), "delegate the work")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "tree cost ceiling") {
		t.Fatalf("Stopped = %q, want a tree-cost-ceiling verdict", res.Stopped)
	}
	// The orchestrator's OWN ledger is far below the ceiling — only the shared tree
	// total tripped it. This is what the per-loop breaker could not catch.
	if res.CostUSD >= 1.0 {
		t.Fatalf("own ledger = $%.4f; the tree ceiling, not the own ledger, should fire", res.CostUSD)
	}
	if !meter.Exceeded() {
		t.Fatalf("tree meter total $%.4f should have reached the ceiling", meter.Total())
	}
}

// Without a meter the loop keeps its prior behavior: the per-loop cost breaker alone
// bounds it, and the no-meter path adds nothing to any shared total.
func TestNoCostMeterLeavesPerLoopBreakerUnchanged(t *testing.T) {
	m := alwaysCalls{
		call:  tool.Call{ID: "c", Surface: "fs", Action: "grep"},
		usage: model.Usage{InputTokens: 10, OutputTokens: 1, CostUSD: 0.5, CostReported: true},
	}
	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(100), WithCircuitBreaker(CircuitBreaker{MaxCostUSD: 1.0}),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "cost ceiling") || strings.Contains(res.Stopped, "tree cost") {
		t.Fatalf("Stopped = %q, want the per-loop (non-tree) cost-ceiling verdict", res.Stopped)
	}
}

// WithSpawnerCostMeter threads the shared meter into each worker the spawner builds, so
// a worker's own spend accrues to the shared tree total (bug 1124, the option-(b) half).
func TestSpawnerThreadsCostMeterIntoWorker(t *testing.T) {
	meter := cost.NewMeter(0) // tracking-only: assert accrual, not a trip
	s := NewSpawner(&fakeProvider{}, nilProject, nil, costingAdapter{usd: 0.30}, WithSpawnerCostMeter(meter))
	p := &profile.JobProfile{Name: "leaf", Tier: profile.TierLocal}

	if _, err := s.Run(context.Background(), p, "duty"); err != nil {
		t.Fatalf("worker run: %v", err)
	}
	if got := meter.Total(); got < 0.29 || got > 0.31 {
		t.Fatalf("worker spend did not accrue to the shared meter: total $%.4f, want ~0.30", got)
	}
}

// WithSpawnerCostMeter ignores a nil meter (workers stay unmetered — prior behavior).
func TestWithSpawnerCostMeterIgnoresNil(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, costingAdapter{usd: 0.1}, WithSpawnerCostMeter(nil))
	if s.meter != nil {
		t.Fatal("a nil meter must be ignored, leaving workers unmetered")
	}
}
