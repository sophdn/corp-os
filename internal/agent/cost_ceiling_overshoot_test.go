package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/cost"
	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// costingRunaway emits a priced tool call every round (never claiming done) and
// counts how many times it was asked to complete, so a test can assert exactly how
// many priced calls of THIS rung landed — e.g. how many costly escalated-rung calls
// slipped past a cost ceiling.
type costingRunaway struct {
	id    string
	call  tool.Call
	usage model.Usage
	calls int
}

func (r *costingRunaway) Model() string   { return r.id }
func (r *costingRunaway) Available() bool { return true }
func (r *costingRunaway) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	r.calls++
	return model.Response{Model: r.id, ToolCalls: []tool.Call{r.call}, Usage: r.usage, StopReason: model.StopToolUse}, nil
}

// TestBreakerCostCeilingOvershootBoundedToOneCall pins the SOFT-ceiling contract of
// -max-cost-usd (bug 1142): the breaker checks cumulative spend BEFORE each round's
// single priced call, so realized spend reaches the ceiling and then overshoots it by
// AT MOST one model call's cost — never more. This is the precise bound the original
// "did not bind" framing lacked; the ceiling binds, it is just reactive-by-one-call.
func TestBreakerCostCeilingOvershootBoundedToOneCall(t *testing.T) {
	const perCall = 0.10
	const cap = 0.25
	m := alwaysCalls{
		call:  tool.Call{ID: "c", Surface: "fs", Action: "grep"},
		usage: model.Usage{InputTokens: 10, OutputTokens: 1, CostUSD: perCall, CostReported: true},
	}
	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(1000), WithCircuitBreaker(CircuitBreaker{MaxCostUSD: cap}),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "cost ceiling") {
		t.Fatalf("Stopped = %q, want a cost-ceiling verdict", res.Stopped)
	}
	// It binds: spend reached the ceiling (a much larger MaxRounds proves the breaker,
	// not the round guard, stopped it).
	if res.CostUSD < cap {
		t.Errorf("ceiling should fire only at/after the cap; spent $%.4f < cap $%.2f", res.CostUSD, cap)
	}
	// It is a SOFT ceiling: overshoot is at most one call's cost. More than that would
	// mean a priced call ran without an interleaved breaker check.
	if res.CostUSD >= cap+perCall {
		t.Errorf("overshoot exceeded one call: spent $%.4f, cap $%.2f, per-call $%.2f — the breaker must fire before every priced call", res.CostUSD, cap, perCall)
	}
}

// TestBreakerCostCeilingFiresBetweenEscalatedCalls is the direct regression for bug
// 1142's open question — "verify whether the standalone check fires between escalated
// calls" (the report observed 2 Opus turns past a $0.05 cap). The floor rung spends
// nothing, faults, and escalates to a costly strong rung; the strong rung then emits a
// priced call every round. Because the breaker runs before EACH round's call, at most
// ONE strong-rung call lands past the ceiling — never two.
func TestBreakerCostCeilingFiresBetweenEscalatedCalls(t *testing.T) {
	const strongPerCall = 0.12 // an Opus-shaped rung cost
	const cap = 0.05           // the report's tiny cap
	grep := model.Response{Model: "qwen", ToolCalls: []tool.Call{{ID: "g", Surface: "fs", Action: "grep"}}, StopReason: model.StopToolUse}
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{resp: grep}, {resp: grep}, {err: overflowErr}}}
	strong := &costingRunaway{
		id:    "opus",
		call:  tool.Call{ID: "g", Surface: "fs", Action: "grep"},
		usage: model.Usage{InputTokens: 10, OutputTokens: 1, CostUSD: strongPerCall, CostReported: true},
	}
	res, err := New(router.New(floor, strong), &fakeProvider{}, nil,
		WithMaxRounds(1000), WithCircuitBreaker(CircuitBreaker{MaxCostUSD: cap}),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "cost ceiling") {
		t.Fatalf("Stopped = %q, want a cost-ceiling verdict", res.Stopped)
	}
	if strong.calls == 0 {
		t.Fatal("the run never reached the strong rung — the escalation setup is wrong, not exercising the between-escalated-calls path")
	}
	// The crux: only ONE costly strong-rung call may land past the tiny cap. Two would
	// reproduce the reported "2 Opus turns past $0.05".
	if strong.calls > 1 {
		t.Errorf("%d strong-rung calls landed past the $%.2f cap — the cost breaker must fire between escalated calls, allowing at most one", strong.calls, cap)
	}
	if res.CostUSD >= cap+strongPerCall {
		t.Errorf("overshoot exceeded one strong call: spent $%.4f, cap $%.2f, strong per-call $%.2f", res.CostUSD, cap, strongPerCall)
	}
}

// TestTreeCostCeilingOvershootBoundedToOneCall is the shared-meter (spawn-tree) analogue
// of the one-call overshoot bound (bug 1142 + 1124): a run whose tree meter is already
// near its ceiling stops after at most one further priced call, so realized tree spend
// overshoots the ceiling by no more than one call's cost.
func TestTreeCostCeilingOvershootBoundedToOneCall(t *testing.T) {
	const perCall = 0.10
	const ceiling = 1.0
	m := alwaysCalls{
		call:  tool.Call{ID: "c", Surface: "fs", Action: "grep"},
		usage: model.Usage{InputTokens: 10, OutputTokens: 1, CostUSD: perCall, CostReported: true},
	}
	meter := cost.NewMeter(ceiling)
	meter.Add(0.85) // spawned-worker spend already on the shared tree total

	res, err := New(single(m), &fakeProvider{}, nil,
		WithMaxRounds(1000), WithCostMeter(meter),
	).Run(context.Background(), "delegate the work")
	if err != nil {
		t.Fatalf("breaker stop must be a clean return, got err: %v", err)
	}
	if !strings.Contains(res.Stopped, "tree cost ceiling") {
		t.Fatalf("Stopped = %q, want a tree-cost-ceiling verdict", res.Stopped)
	}
	if total := meter.Total(); total < ceiling {
		t.Errorf("tree ceiling should fire only at/after $%.2f; total $%.4f", ceiling, total)
	}
	if total := meter.Total(); total >= ceiling+perCall {
		t.Errorf("tree overshoot exceeded one call: total $%.4f, ceiling $%.2f, per-call $%.2f", total, ceiling, perCall)
	}
}
