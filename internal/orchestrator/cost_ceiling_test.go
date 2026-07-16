package orchestrator

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/cost"
)

// The spawn tool refuses to dispatch another worker once the shared tree cost has
// reached the ceiling (bug 1124): the orchestrator's per-loop breaker only guards its
// own next MODEL call, but a spawn is a mid-round TOOL call, so the spawn tool itself
// must check the shared meter before delegating.
func TestDispatchRefusesSpawnPastCostCeiling(t *testing.T) {
	meter := cost.NewMeter(1.0)
	meter.Add(1.0) // the tree is already at the ceiling
	sp := NewSpawnProvider(leafSpawner("answer"), testRegistry(t), WithCostMeter(meter))

	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "leaf", "duty": "do the thing"}))
	if res.OK {
		t.Fatal("spawning past the cost ceiling must fail, not dispatch a worker")
	}
	if msg := errText(t, res); !strings.Contains(msg, "cost ceiling") {
		t.Fatalf("error = %q, want a cost-ceiling refusal", msg)
	}
}

// Under the ceiling the spawn proceeds normally — the meter check is a guard, not a block.
func TestDispatchUnderCostCeilingStillSpawns(t *testing.T) {
	meter := cost.NewMeter(1.0)
	meter.Add(0.5)
	sp := NewSpawnProvider(leafSpawner("answer"), testRegistry(t), WithCostMeter(meter))

	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "leaf", "duty": "do the thing"}))
	if !res.OK {
		t.Fatalf("under the ceiling the spawn should proceed, got failure: %+v", res.Value)
	}
}

// WithCostMeter ignores a nil meter (no pre-spawn check), matching the package's other
// nil-tolerant options.
func TestWithCostMeterIgnoresNil(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("answer"), testRegistry(t), WithCostMeter(nil))
	if sp.meter != nil {
		t.Fatal("a nil meter must be ignored")
	}
}
