package agent

import (
	"context"
	"strings"
	"testing"

	"time"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// alwaysDoneAdapter always claims success with no tool calls — a worker that self-reports
// done every turn, regardless of the gate.
type alwaysDoneAdapter struct{ text string }

func (a alwaysDoneAdapter) Model() string   { return "done-claimer" }
func (a alwaysDoneAdapter) Available() bool { return true }
func (a alwaysDoneAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{Model: "done-claimer", Text: a.text, StopReason: model.StopEndTurn}, nil
}

// On an exhausted verify-revise budget the terminal verdict is unverified/escalate, never
// done — and the worker's self-reported success does NOT override it (T7): the gate keeps
// failing while the model insists it passed.
func TestLoopVerifyExhaustEscalates(t *testing.T) {
	m := alwaysDoneAdapter{text: "All done — the tests pass!"}
	g := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 2, run: func(context.Context, []string, string, time.Duration) (int, string) {
		return 1, "FAIL: TestX"
	}}
	l := New(router.New(m, m), &fakeProvider{}, nil, WithVerify(g))
	res, err := l.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.VerifyFailed {
		t.Fatal("want VerifyFailed")
	}
	if !strings.HasPrefix(res.Escalate, "unverified/escalate:") {
		t.Fatalf("want an unverified/escalate verdict, got %q", res.Escalate)
	}
	// The model insisted it was done; the verdict stands regardless of the self-report.
	if res.Text != "All done — the tests pass!" {
		t.Fatalf("self-report preserved but not overriding: text = %q", res.Text)
	}
}

// alwaysToolAdapter never claims done — it keeps emitting a non-mutating tool call, so the
// no-progress breaker trips.
type alwaysToolAdapter struct{}

func (alwaysToolAdapter) Model() string   { return "loop" }
func (alwaysToolAdapter) Available() bool { return true }
func (alwaysToolAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{Model: "loop", ToolCalls: []tool.Call{{ID: "c", Surface: "fs", Action: "grep"}}, StopReason: model.StopToolUse}, nil
}

// When the no-progress breaker trips with a verify gate configured (verification still
// unsatisfied), the terminal verdict is also unverified/escalate (T7).
func TestLoopBreakerWithVerifyEscalates(t *testing.T) {
	m := alwaysToolAdapter{}
	g := &VerifyGate{Command: []string{"go", "test"}}
	l := New(router.New(m, m), &fakeProvider{}, nil,
		WithVerify(g),
		WithCircuitBreaker(CircuitBreaker{NoProgressRounds: 2}))
	res, err := l.Run(context.Background(), "loop forever")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stopped == "" {
		t.Fatal("want a breaker Stopped verdict")
	}
	if !strings.HasPrefix(res.Escalate, "unverified/escalate:") {
		t.Fatalf("breaker with verify should escalate, got %q", res.Escalate)
	}
}

// Without a verify gate, the breaker trips with Stopped but no escalate verdict (there is
// no gate to be unverified against).
func TestLoopBreakerWithoutVerifyDoesNotEscalate(t *testing.T) {
	m := alwaysToolAdapter{}
	l := New(router.New(m, m), &fakeProvider{}, nil, WithCircuitBreaker(CircuitBreaker{NoProgressRounds: 2}))
	res, err := l.Run(context.Background(), "loop forever")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stopped == "" {
		t.Fatal("want a breaker Stopped verdict")
	}
	if res.Escalate != "" {
		t.Fatalf("no verify gate → no escalate verdict, got %q", res.Escalate)
	}
}
