package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// alwaysSameRead is a worker stuck in the read→evict→re-read loop: it re-issues the
// EXACT same fs.read every round (only the provider-assigned id varies) and never
// claims done — the run-1 pathology where a too-big read is evicted on arrival and
// the worker dutifully re-reads it forever.
type alwaysSameRead struct{ n int }

func (a *alwaysSameRead) Model() string   { return "floor" }
func (a *alwaysSameRead) Available() bool { return true }
func (a *alwaysSameRead) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	a.n++
	return model.Response{
		Model:     "floor",
		ToolCalls: []tool.Call{{ID: fmt.Sprintf("call-%d", a.n), Surface: "fs", Action: "read", Params: map[string]any{"path": "big.go"}}},
	}, nil
}

// TestRepeatedReadLoop_BreaksAtTopRung is the GAP-2b regression: with no higher rung
// to escalate to, a worker re-issuing identical reads must be stopped fast by the
// repeated-identical-call breaker — NOT left to spin to the per-cycle round cap.
func TestRepeatedReadLoop_BreaksAtTopRung(t *testing.T) {
	adapter := &alwaysSameRead{}
	l := New(router.NewLadder([]model.Adapter{adapter}, 0), vProvider{}, []tool.Spec{{Name: "fs"}}, WithMaxRounds(50))

	// A stuck re-read with no rung to escalate to is a terminal non-convergence: the
	// run ends with an honest verdict, NOT silently and NOT at the 50-round cap.
	_, err := l.Run(context.Background(), "go")
	if err == nil {
		t.Fatal("expected an honest terminal verdict for the stuck re-read loop")
	}
	if got := err.Error(); !strings.Contains(got, "re-reading") {
		t.Fatalf("verdict should name the re-read loop, not the generic round cap: %q", got)
	}
	// The breaker fires after maxRepeatedCallRounds identical rounds: round 0 seeds the
	// signature, rounds 1..3 repeat it, the 4th model call's check trips. Anything near
	// the 50-round cap means the loop spun instead of breaking.
	if adapter.n > maxRepeatedCallRounds+1 {
		t.Fatalf("re-read loop not broken fast: %d model calls (want <= %d)", adapter.n, maxRepeatedCallRounds+1)
	}
}

// doneAdapter claims done immediately — stands in for the wider-window rung that, once
// the breaker escalates onto it, carries the bigger budget that dissolves the loop.
type doneAdapter struct{ n int }

func (a *doneAdapter) Model() string   { return "strong" }
func (a *doneAdapter) Available() bool { return true }
func (a *doneAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	a.n++
	return model.Response{Model: "strong", Text: "done", StopReason: model.StopEndTurn}, nil
}

// TestRepeatedReadLoop_EscalatesForContext asserts the breaker's preferred branch:
// when a higher rung exists, the futile re-read loop escalates (post-GAP-2a that rung
// carries more context) rather than halting — the loop reaches the strong rung and
// finishes instead of dying on the floor.
func TestRepeatedReadLoop_EscalatesForContext(t *testing.T) {
	floor := &alwaysSameRead{}
	strong := &doneAdapter{}
	rt := router.NewLadder([]model.Adapter{floor, strong}, 0)
	l := New(rt, vProvider{}, []tool.Spec{{Name: "fs"}}, WithMaxRounds(50))

	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strong.n == 0 {
		t.Fatal("breaker never escalated: the strong rung was never reached")
	}
	if rt.CurrentRung() != 1 {
		t.Fatalf("router did not climb off the stuck floor: cur=%d", rt.CurrentRung())
	}
}
