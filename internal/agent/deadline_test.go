package agent

import (
	"context"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// TestExtendTurnDeadline checks the helper: it pushes the deadline out by extra, ignores the
// PARENT's own deadline expiry (we extended past it), but still propagates a GENUINE cancellation.
func TestExtendTurnDeadline(t *testing.T) {
	parent, pcancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer pcancel()
	pdl, _ := parent.Deadline()
	ext, ecancel := extendTurnDeadline(parent, time.Second)
	defer ecancel()
	edl, ok := ext.Deadline()
	if !ok || edl.Sub(pdl) < 900*time.Millisecond {
		t.Fatalf("extended deadline %v is not ~1s past the parent's %v", edl, pdl)
	}
	// The parent's own 40ms deadline expiring must NOT cancel the extended context.
	time.Sleep(90 * time.Millisecond)
	if ext.Err() != nil {
		t.Errorf("extended ctx cancelled by the parent's deadline expiry, want still live: %v", ext.Err())
	}

	// A genuine cancellation of the parent DOES propagate to the extended context.
	parent2, p2cancel := context.WithTimeout(context.Background(), time.Hour)
	ext2, e2cancel := extendTurnDeadline(parent2, time.Second)
	defer e2cancel()
	p2cancel()
	deadline := time.Now().Add(2 * time.Second)
	for ext2.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if ext2.Err() == nil {
		t.Error("extended ctx not cancelled after a genuine parent cancellation")
	}

	// No deadline on the parent → a no-op passthrough.
	if got, _ := extendTurnDeadline(context.Background(), time.Second); got != context.Background() {
		t.Error("a deadline-less parent should pass through unchanged")
	}
}

// deadlineAwareAdapter errors with ctx.Err() when the context is spent (mimicking a real model
// call hitting a deadline), otherwise returns the next scripted response.
type deadlineAwareAdapter struct {
	responses []model.Response
	i         int
}

func (a *deadlineAwareAdapter) Model() string   { return "fake" }
func (a *deadlineAwareAdapter) Available() bool { return true }
func (a *deadlineAwareAdapter) Complete(ctx context.Context, _ []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	r := a.responses[a.i]
	if a.i < len(a.responses)-1 {
		a.i++
	}
	return r, nil
}

// slowSpawnProvider blocks for sleep on an agent.spawn dispatch (the worker would run here on its
// own budget), and answers other calls instantly.
type slowSpawnProvider struct{ sleep time.Duration }

func (p slowSpawnProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	if c.Surface == spawnSurface && c.Action == spawnAction {
		time.Sleep(p.sleep)
		return tool.Result{Call: c, OK: true, Value: map[string]any{"answer": "subtask done"}}
	}
	return tool.Result{Call: c, OK: true, Value: map[string]any{}}
}

// TestRun_ExtendsDeadlineAcrossBlockingSpawn is the Run-39/40 regression: a spawn that BLOCKS
// longer than the per-turn budget (the worker ran on its own budget) must NOT starve the
// orchestrator's synthesis turn — the deadline extends by the block, so the post-spawn model call
// still runs and the run completes cleanly instead of ending on a false per-turn timeout.
func TestRun_ExtendsDeadlineAcrossBlockingSpawn(t *testing.T) {
	adapter := &deadlineAwareAdapter{responses: []model.Response{
		{ToolCalls: []tool.Call{{ID: "s", Surface: spawnSurface, Action: spawnAction}}, StopReason: model.StopToolUse},
		{Text: "all done", StopReason: model.StopEndTurn},
	}}
	l := New(single(adapter), slowSpawnProvider{sleep: 400 * time.Millisecond}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	res, err := l.Run(ctx, "delegate it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ModelFault != "" {
		t.Errorf("run ended on model fault %q — the blocking-spawn wall-clock should have extended the per-turn deadline", res.ModelFault)
	}
	if res.Text != "all done" {
		t.Errorf("Text = %q, want the synthesis answer 'all done' (the post-spawn turn must still run)", res.Text)
	}
}
