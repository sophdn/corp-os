package agent

import (
	"context"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// growingAdapter reports a per-call input-token count that GROWS across rounds —
// standing in for a turn whose transcript fills with code the len/4 estimate
// under-counts. The first calls make a tool call; the last ends the turn.
type growingAdapter struct {
	inputs []int
	n      int
}

func (g *growingAdapter) Model() string   { return "grow" }
func (g *growingAdapter) Available() bool { return true }
func (g *growingAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	i := g.n
	if i >= len(g.inputs) {
		i = len(g.inputs) - 1
	}
	g.n++
	r := model.Response{Model: "grow", Usage: model.Usage{InputTokens: g.inputs[i]}}
	if g.n < len(g.inputs) {
		r.ToolCalls = []tool.Call{{ID: "c", Surface: "fs", Action: "read"}}
		r.StopReason = model.StopToolUse
	} else {
		r.Text = "done"
		r.StopReason = model.StopEndTurn
	}
	return r, nil
}

// TestCalibrationDoesNotInflateOverhead is the regression for the conflation bug:
// a LATER code-heavy turn (large measured input vs a small len/4 estimate) must
// NOT grow the fixed tool-spec overhead — that residual belongs in the compactor's
// tokenRatio, not in "overhead". The old max(input−estimate) inflated overhead to
// the largest under-estimate; the fix pins overhead to the first call.
func TestCalibrationDoesNotInflateOverhead(t *testing.T) {
	sum := &recSummarizer{out: "[s]"}
	// Call 1 input 1000 (small transcript → overhead ≈ 1000); call 2 input 8000
	// (code accumulated, but the transcript len/4 estimate is still tiny).
	loop := New(single(&growingAdapter{inputs: []int{1000, 8000}}), &fakeProvider{}, nil,
		WithSystemPrompt("SYS"), WithCompaction(100000, 2, sum)) // budget high → no compaction interferes
	if _, err := loop.Run(context.Background(), "fix it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Overhead pinned near the first call's residual (~1000), NOT grown toward 8000.
	if loop.toolSpecTokens > 2000 {
		t.Errorf("toolSpecTokens = %d — a later code-heavy turn inflated overhead (conflation bug not fixed)", loop.toolSpecTokens)
	}
	if loop.toolSpecTokens < 500 {
		t.Errorf("toolSpecTokens = %d — first-call overhead not calibrated", loop.toolSpecTokens)
	}
	// The under-estimate landed in the tokenRatio instead.
	if loop.compactor.tokenRatio <= 1.0 {
		t.Errorf("tokenRatio = %v — the transcript under-estimate was not folded into the ratio", loop.compactor.tokenRatio)
	}
}

func TestObserveTokenRatioMaxAndClamp(t *testing.T) {
	c := &Compactor{tokenRatio: 1.0}
	c.observeTokenRatio(1.5)
	if c.tokenRatio != 1.5 {
		t.Errorf("ratio = %v, want 1.5", c.tokenRatio)
	}
	c.observeTokenRatio(1.2) // lower → keep the max
	if c.tokenRatio != 1.5 {
		t.Errorf("ratio = %v, want max kept at 1.5", c.tokenRatio)
	}
	c.observeTokenRatio(99) // above cap → clamp
	if c.tokenRatio != maxTokenRatio {
		t.Errorf("ratio = %v, want clamp to %v", c.tokenRatio, maxTokenRatio)
	}
}

func TestCurrentSizeAppliesRatio(t *testing.T) {
	msgs := []model.ChatMessage{{Role: model.RoleUser, Content: "0123456789012345"}} // 16 chars → est 4+4=8
	base := (&Compactor{tokenRatio: 1.0}).currentSize(msgs, 100)
	scaled := (&Compactor{tokenRatio: 2.0}).currentSize(msgs, 100)
	// overhead (100) is not scaled; the transcript estimate is.
	if scaled-100 != 2*(base-100) {
		t.Errorf("ratio not applied to transcript estimate: base=%d scaled=%d", base, scaled)
	}
}
