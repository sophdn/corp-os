package agent

import (
	"context"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
)

// TestExhaustionEscalatesToHigherRung is the fix for coding-worker-thrashes-to-tool-round-
// exhaustion-without-escalating: a worker that keeps MUTATING but never claims done exhausts
// its per-cycle round budget on the floor (the no-progress breaker never trips — it
// "progresses" every round — and with no verify gate stuck-verify never trips either). Rather
// than dying on the floor, the loop must lift to a stronger rung with a fresh budget; here the
// stronger rung converges, so the run succeeds instead of erroring.
func TestExhaustionEscalatesToHigherRung(t *testing.T) {
	floor := alwaysEditAdapter{} // perpetual editor: mutates every round, never claims done
	strong := model.NewEcho("strong", model.Response{Text: "fixed", StopReason: model.StopEndTurn})

	res, err := New(router.New(floor, strong), &fakeProvider{}, nil, WithMaxRounds(3)).
		Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("floor exhaustion must escalate to the stronger rung, not error: %v", err)
	}
	if res.Text != "fixed" {
		t.Fatalf("the converged answer must come from the escalated rung; got %q", res.Text)
	}
}

// TestExhaustionWithNoHigherRungStillErrors: when there is genuinely no rung to climb to, a
// perpetual non-converging worker still ends with the honest exhaustion verdict (the backstop
// found no green and the ladder is spent).
func TestExhaustionWithNoHigherRungStillErrors(t *testing.T) {
	floor := alwaysEditAdapter{}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), &fakeProvider{}, nil, WithMaxRounds(3))
	if _, err := loop.Run(context.Background(), "fix it"); err == nil {
		t.Fatal("a single-rung perpetual non-converger must still report exceeded-max-rounds")
	}
}
