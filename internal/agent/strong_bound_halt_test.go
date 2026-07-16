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

// varyingRunaway emits a DIFFERENT tool call every round (never done) — the bug-1087
// frontier signature: Opus did real-but-non-converging work, varying its calls, so the
// repeated-identical-read breaker (bug 1088) does NOT catch it. Only the strong-bound
// halt can stop it, which is exactly what this test pins.
type varyingRunaway struct{ n int }

func (a *varyingRunaway) Model() string   { return "frontier" }
func (a *varyingRunaway) Available() bool { return true }
func (a *varyingRunaway) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	a.n++
	return model.Response{Model: "frontier", ToolCalls: []tool.Call{
		{ID: fmt.Sprintf("g%d", a.n), Surface: "fs", Action: "grep", Params: map[string]any{"q": fmt.Sprintf("term-%d", a.n)}},
	}}, nil
}

// TestStrongBoundHalts is the bug 1087 regression: a worker that escalates onto the
// bounded frontier rung and stays stuck there must serve at most -strong-bound turns,
// then HALT with an honest verdict — never the 12/2, $3.45 overrun the stickyTop
// bypass used to allow. The floor thrashes identical greps (lifting it onto the sticky
// frontier), and the frontier does varying-but-non-converging work; the only thing that
// caps its turns is the strong-bound halt.
func TestStrongBoundHalts(t *testing.T) {
	const bound = 1
	floor := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	strong := &varyingRunaway{}
	rt := router.New(floor, strong, router.WithBoundedTop(bound))
	loop := New(rt, &fakeProvider{}, []tool.Spec{{Name: "fs"}}, WithMaxRounds(6))

	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("the strong-bound halt is a clean honest stop, not an error: %v", err)
	}
	// The invariant the bound exists to protect: the frontier never serves more turns
	// than its budget. Pre-fix the stickyTop bypass let this run to ~maxRounds.
	if served := rt.BoundedTurns(); served > bound {
		t.Fatalf("frontier served %d turns against a bound of %d — the bound was not enforced", served, bound)
	}
	if !strings.Contains(res.Stopped, "strong-bound reached") {
		t.Fatalf("Stopped = %q, want an honest strong-bound verdict", res.Stopped)
	}
	// The telemetry "N/bound" line can never exceed the bound.
	if rt.BoundedTurns() > rt.BoundMax() {
		t.Fatalf("bounded turns %d exceed the cap %d", rt.BoundedTurns(), rt.BoundMax())
	}
}
