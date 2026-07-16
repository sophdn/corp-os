package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// This file is the STEP-0 measurement for bug 1066
// (atomic-coding-floor-tool-spec-overhead-leaves-no-window-…). The bug's stated
// premise — "fixed tool-spec overhead ~5977 dominates the 6144 budget" — was
// corrected by the calibration fix (a8f6e2a / bf1fd40), which split the conflated
// overhead and tokenRatio. The question this test answers empirically: with the
// calibration fix in place, on a REAL multi-round whole-file-read coding carry on
// the Qwen 8192 floor (budget 6144), where does the budget actually go, and does a
// real source-file read still overflow the floor → escalate to Opus?
//
// The mechanism under test is the within-turn bound (boundWithinTurn →
// evictToolResults), which runs at the TOP of every round before the model call.
// It is what keeps accumulated whole-file fs.read tool-results from overflowing the
// floor window across rounds. The floor-fit guard (router.WillServeFloor +
// EscalateForOverflow) is the escalation path the bug reports the worker riding.

// floorBudget / floorWindow mirror the Qwen floor the bug names: an 8192-token
// window with a compaction budget of 6144 (window·3/4, see
// cmd/corpos compactionBudgetForWindow).
const (
	measureFloorWindow = 8192
	measureFloorBudget = 6144
)

// codingCarryAdapter is a provider-measured adapter that emulates the Qwen floor
// for a coding carry: it reports a realistic provider InputTokens (a real
// tokenizer, denser than len/4 on code — the calibration ratio path) and drives a
// fixed script of fs.read rounds followed by an fs.edit, then ends the turn. It
// records the per-call provider-measured input so the test can see the live window
// pressure round by round.
type codingCarryAdapter struct {
	// fixedOverhead is the provider-measured cost of the offered tool specs + the
	// chat-template scaffolding, independent of the transcript. The calibration
	// pins toolSpecTokens to this on the first call.
	fixedOverhead int
	// codeRatio is how much denser the real tokenizer counts the transcript than
	// our len/4 estimate (code is ~1.5–2.5× denser). The provider InputTokens is
	// fixedOverhead + ratio·estimate.
	codeRatio float64
	// script is the per-round response. Once exhausted the adapter ends the turn.
	script []model.Response
	round  int
	// measured records the provider InputTokens reported on each Complete call.
	measured []int
	// sawSpecs records whether specs were offered (they are, every call).
	calls int
}

func (a *codingCarryAdapter) Model() string   { return "qwen-floor" }
func (a *codingCarryAdapter) Available() bool { return true }

func (a *codingCarryAdapter) Complete(_ context.Context, msgs []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	a.calls++
	est := estimateTokens(msgs)
	input := a.fixedOverhead + int(float64(est)*a.codeRatio)
	a.measured = append(a.measured, input)

	var resp model.Response
	if a.round < len(a.script) {
		resp = a.script[a.round]
	} else {
		resp = model.Response{Model: a.Model(), Text: "done", StopReason: model.StopEndTurn}
	}
	a.round++
	resp.Model = a.Model()
	resp.Usage.InputTokens = input
	return resp, nil
}

// realFileBody returns a body of n characters approximating a real source file
// (read.go is ~13671 chars ≈ 3400 tok at len/4, denser at the real tokenizer).
func realFileBody(n int) string { return strings.Repeat("func f() { return x }\n", n/22+1)[:n] }

// TestMeasure_FloorCodingCarryBudgetSplit is the measurement: drive a multi-round
// whole-file-read coding carry on the Qwen floor and report where the 6144 budget
// goes (fixed overhead vs transcript) and whether the floor overflows → escalates.
func TestMeasure_FloorCodingCarryBudgetSplit(t *testing.T) {
	// A provider that returns a real whole-file read (~3400 tok) for fs.read and a
	// small ok for fs.edit — the accumulation the bug names.
	const fileChars = 13671 // read.go size
	prov := &codingFileProvider{body: realFileBody(fileChars)}

	// Script: read file A, read file B, read file C (whole-file each), then edit.
	// This is the "accumulated whole-file fs.read across rounds" the corrected scope
	// names as the real residual.
	script := []model.Response{
		{ToolCalls: []tool.Call{{ID: "r1", Surface: "fs", Action: "read", Params: map[string]any{"path": "internal/tool/read.go"}}}, StopReason: model.StopToolUse},
		{ToolCalls: []tool.Call{{ID: "r2", Surface: "fs", Action: "read", Params: map[string]any{"path": "internal/agent/loop.go"}}}, StopReason: model.StopToolUse},
		{ToolCalls: []tool.Call{{ID: "r3", Surface: "fs", Action: "read", Params: map[string]any{"path": "internal/mcp/enrich.go"}}}, StopReason: model.StopToolUse},
		{ToolCalls: []tool.Call{{ID: "e1", Surface: "fs", Action: "edit", Params: map[string]any{"path": "internal/tool/read.go"}}}, StopReason: model.StopToolUse},
	}

	// The realistic floor: a provider-measured overhead in the corrected range
	// (real ~1169 per the calibration commit, NOT ~5977) and a code density ratio.
	adapter := &codingCarryAdapter{fixedOverhead: 1169, codeRatio: 1.8, script: script}

	// Build the loop the way main.go does for a floor worker: a compactor at the
	// floor budget and the floor window set so the floor-fit guard is live. A
	// two-tier router (floor → strong) lets us observe an overflow escalation.
	strong := &recAdapter{}
	r := router.New(adapter, strong)
	sum := &recSummarizer{out: "[summary]"}
	loop := New(r, prov, nil,
		WithSystemPrompt(strings.Repeat("system-prompt ", 40)),
		WithCompaction(measureFloorBudget, 6, sum),
		WithFloorWindow(measureFloorWindow),
	)

	res, err := loop.Run(context.Background(), "fix the bug in internal/tool/read.go: the non-string symbol seed")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Report the measurement.
	t.Logf("MEASUREMENT — Qwen floor coding carry (window=%d budget=%d):", measureFloorWindow, measureFloorBudget)
	t.Logf("  calibrated toolSpecTokens (fixed overhead) = %d", loop.toolSpecTokens)
	t.Logf("  compactor tokenRatio (code density)        = %.2f", loop.compactor.tokenRatio)
	t.Logf("  per-round provider-measured InputTokens    = %v", adapter.measured)
	t.Logf("  final live ContextTokens                   = %d", loop.ContextTokens())
	t.Logf("  strong-rung (Opus) calls (escalations)     = %d", len(strong.got))
	if res.Compaction != nil {
		t.Logf("  last compaction: before=%d after=%d budget=%d overhead=%d evicted=%d",
			res.Compaction.TokensBefore, res.Compaction.TokensAfter, res.Compaction.Budget,
			res.Compaction.Overhead, res.Compaction.GroupsEvicted)
	}

	// Where does the budget go? Fixed overhead is the calibrated toolSpecTokens; the
	// rest of the budget is transcript headroom.
	transcriptHeadroom := measureFloorBudget - loop.toolSpecTokens
	t.Logf("  budget split: overhead=%d (%.0f%%)  transcript-headroom=%d (%.0f%%)",
		loop.toolSpecTokens, 100*float64(loop.toolSpecTokens)/measureFloorBudget,
		transcriptHeadroom, 100*float64(transcriptHeadroom)/measureFloorBudget)

	// VERDICT assertions: with the calibration fix, the fixed overhead must NOT
	// dominate (it is a few hundred-to-~1k tok, leaving most of the budget for
	// transcript — the corrected reading, contra the bug's ~5977).
	if loop.toolSpecTokens > measureFloorBudget/2 {
		t.Errorf("fixed overhead %d still dominates >half the budget %d — premise NOT corrected",
			loop.toolSpecTokens, measureFloorBudget)
	}
	// The within-turn bound must keep the carry on the floor: no overflow escalation
	// to the strong rung for a 3-file read + edit, because evictToolResults elides
	// the stale whole-file reads as they accumulate.
	if len(strong.got) > 0 {
		t.Errorf("coding carry overflowed the floor → escalated to strong rung %d times; "+
			"the within-turn bound failed to keep the read-heavy turn under budget", len(strong.got))
	}
	// And every provider-measured input stayed within the floor window (the physical
	// constraint the bug says is violated).
	for i, m := range adapter.measured {
		if m >= measureFloorWindow {
			t.Errorf("round %d provider input %d >= floor window %d — real overflow", i, m, measureFloorWindow)
		}
	}
}

// codingFileProvider returns a configurable body for fs.read and a small ok for
// everything else, emulating a real whole-file read.
type codingFileProvider struct {
	body  string
	calls int
}

func (p *codingFileProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	p.calls++
	if c.Surface == "fs" && c.Action == "read" {
		return tool.Result{Call: c, OK: true, Value: p.body}
	}
	return tool.Result{Call: c, OK: true, Value: map[string]any{"ok": true}}
}
