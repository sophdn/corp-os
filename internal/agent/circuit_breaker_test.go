package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// alwaysCalls is a controllable runaway: it emits the same tool call every turn
// with a fixed per-call usage, so a circuit-breaker test can drive cost, tokens,
// or writeless rounds deterministically. It never claims done on its own.
type alwaysCalls struct {
	call  tool.Call
	usage model.Usage
}

func (a alwaysCalls) Model() string   { return "runaway" }
func (a alwaysCalls) Available() bool { return true }
func (a alwaysCalls) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{Model: "runaway", ToolCalls: []tool.Call{a.call}, Usage: a.usage, StopReason: model.StopToolUse}, nil
}

// TestBreakerCostCeiling: a per-run cost ceiling ends the turn with an honest
// verdict instead of a runaway (the run-6d $4.64 problem).
func TestBreakerCostCeiling(t *testing.T) {
	m := alwaysCalls{
		call:  tool.Call{ID: "c", Surface: "fs", Action: "grep"},
		usage: model.Usage{InputTokens: 1000, OutputTokens: 100, CostUSD: 0.5, CostReported: true},
	}
	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(100), WithCircuitBreaker(CircuitBreaker{MaxCostUSD: 1.0}),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "cost ceiling") {
		t.Fatalf("Stopped = %q, want a cost-ceiling verdict", res.Stopped)
	}
	if res.CostUSD < 1.0 {
		t.Errorf("verdict should fire at/after the ceiling; spent $%.4f", res.CostUSD)
	}
}

// TestBreakerNoProgress: a writeless grep-thrash run (run-6d's signature) stops
// after N rounds with no file written.
func TestBreakerNoProgress(t *testing.T) {
	m := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(100), WithCircuitBreaker(CircuitBreaker{NoProgressRounds: 3}),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "no file written") {
		t.Fatalf("Stopped = %q, want a no-progress verdict", res.Stopped)
	}
}

// TestBreakerTokenBudget: a per-run token budget stops the run with a verdict.
func TestBreakerTokenBudget(t *testing.T) {
	m := alwaysCalls{
		call:  tool.Call{ID: "c", Surface: "fs", Action: "grep"},
		usage: model.Usage{InputTokens: 500},
	}
	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(100), WithCircuitBreaker(CircuitBreaker{MaxTokens: 1000}),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "token budget") {
		t.Fatalf("Stopped = %q, want a token-budget verdict", res.Stopped)
	}
}

// TestBreakerWriteResetsProgress: a run that writes a file every round must NOT
// trip the no-progress detector — it only runs out via the ordinary max-rounds
// guard, with no breaker verdict.
func TestBreakerWriteResetsProgress(t *testing.T) {
	m := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "write"}}
	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(4), WithCircuitBreaker(CircuitBreaker{NoProgressRounds: 2}),
	).Run(context.Background(), "fix the bug")
	if err == nil {
		t.Fatal("a perpetual writer should exhaust max rounds, not stop clean")
	}
	if res.Stopped != "" {
		t.Errorf("writes should reset progress; no-progress breaker must not fire, got %q", res.Stopped)
	}
}

// TestBreakerResetsOnEscalation is the run-7 finding: a mid-turn escalation must
// reset the no-progress stall counter, so a freshly-escalated (more capable) rung
// gets its full budget instead of being guillotined by the floor's accumulated
// stall. Floor stalls for 2 grep rounds then faults (→ escalate to strong); the
// strong rung must then get at least one round before the breaker stops the run.
func TestBreakerResetsOnEscalation(t *testing.T) {
	grep := model.Response{Model: "qwen", ToolCalls: []tool.Call{{ID: "g", Surface: "fs", Action: "grep"}}, StopReason: model.StopToolUse}
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{resp: grep}, {resp: grep}, {err: overflowErr}}}
	strong := &recordingRunaway{call: tool.Call{ID: "g", Surface: "fs", Action: "grep"}}
	loop := New(router.New(floor, strong), &fakeProvider{}, nil,
		WithMaxRounds(30), WithCircuitBreaker(CircuitBreaker{NoProgressRounds: 3}),
	)
	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "no file written") {
		t.Fatalf("Stopped = %q, want a no-progress verdict", res.Stopped)
	}
	// Without the escalation reset the strong rung is guillotined at 0 rounds; with
	// it, escalation grants a fresh budget so the strong rung actually runs.
	if len(strong.seen) == 0 {
		t.Error("the freshly-escalated strong rung got 0 rounds — breaker guillotined the escalation tail")
	}
}

// TestBreakerDisabledByDefault: a zero-value breaker is a no-op — a writeless
// runaway falls through to the ordinary max-rounds guard with no verdict.
func TestBreakerDisabledByDefault(t *testing.T) {
	m := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	res, err := New(single(m), &fakeProvider{}, nil, WithMaxRounds(3)).Run(context.Background(), "fix the bug")
	if err == nil {
		t.Fatal("with no breaker the runaway should hit the max-rounds guard")
	}
	if res.Stopped != "" {
		t.Errorf("no breaker configured → Stopped must be empty, got %q", res.Stopped)
	}
}
