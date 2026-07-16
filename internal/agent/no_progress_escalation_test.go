package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// errProvider (a structured tool error on every dispatch — the run-8 signature
// where fs.edit hit "No changes to make" and sys.exec was rejected) is defined in
// loop_router_test.go and shared here.

// TestNoProgressBreakerEscalatesBeforeHardStop is the regression for
// no-progress-breaker-pre-empts-repeated-tool-error-escalation-mid-turn (run-8): a
// floor worker that thrashes structured tool errors within a single turn never
// claims done, so the turn-boundary Observe never folds the repeated_tool_error
// trigger. Before the fix the no-progress breaker hard-stopped the stuck floor and
// the strong rung never got a turn (0 escalations, the run-8 finding). Now the
// breaker lifts the rung first when a higher one is unused, so the strong rung
// actually runs before the run dies.
func TestNoProgressBreakerEscalatesBeforeHardStop(t *testing.T) {
	// The floor emits an fs.edit every round; errProvider rejects it (the run-8
	// old_string==new_string "No changes to make" signature), so no file ever lands
	// and the no-progress stall accrues.
	floor := alwaysCalls{call: tool.Call{ID: "e", Surface: "fs", Action: "edit"}}
	strong := &recordingRunaway{call: tool.Call{ID: "e", Surface: "fs", Action: "edit"}}
	loop := New(router.New(floor, strong), errProvider{}, nil,
		WithMaxRounds(30), WithCircuitBreaker(CircuitBreaker{NoProgressRounds: 3}),
	)
	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	// The run still ends honestly — no file ever landed, and once the strong rung is
	// also stalled (no higher rung) the breaker hard-stops with its verdict.
	if !strings.Contains(res.Stopped, "no file written") {
		t.Fatalf("Stopped = %q, want a no-progress verdict", res.Stopped)
	}
	// The fix: the stalled floor lifted the rung instead of dying, so the
	// freshly-escalated strong rung got at least one turn (its fresh stall window
	// comes from the breaker's reset). Without the fix the breaker hard-stops at the
	// floor and the strong rung records nothing.
	if len(strong.seen) == 0 {
		t.Error("the strong rung got 0 turns — the no-progress breaker hard-stopped the floor instead of escalating")
	}
}

// TestNoProgressBreakerHardStopsAtTopRung: with no higher rung to lift to, the
// no-progress breaker keeps its honest hard-stop (the escalatable path is taken but
// EscalateForNoProgress returns no edge, so the run still terminates with a verdict).
func TestNoProgressBreakerHardStopsAtTopRung(t *testing.T) {
	floor := alwaysCalls{call: tool.Call{ID: "e", Surface: "fs", Action: "edit"}}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), errProvider{}, nil,
		WithMaxRounds(30), WithCircuitBreaker(CircuitBreaker{NoProgressRounds: 3}),
	)
	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "no file written") {
		t.Fatalf("Stopped = %q, want a no-progress verdict at the top rung", res.Stopped)
	}
}
