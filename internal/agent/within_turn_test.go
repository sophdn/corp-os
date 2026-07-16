package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// TestLoopBoundsContextWithinTurn drives a long single turn (a perpetual caller
// against a huge-payload provider) and asserts the within-turn bound keeps the
// live context near budget instead of growing with the round count — the run-6b/6d
// mid-turn overflow. Older tool results are elided; recent ones are kept.
func TestLoopBoundsContextWithinTurn(t *testing.T) {
	m := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	huge := hugeProvider{payload: strings.Repeat("y", 100_000)}
	sum := &recSummarizer{}
	const budget = 2000 // per-result cap ≈ budget chars (~500 tok); keeping 3 ≈ 1500 tok
	loop := New(single(m), huge, nil, WithCompaction(budget, 1, sum), WithMaxRounds(15))

	if _, err := loop.Run(context.Background(), "fix the bug"); err == nil {
		t.Fatal("a perpetual caller should exhaust max rounds")
	}

	// Live context stayed bounded INDEPENDENT of the round count: 15 un-elided
	// results would be ~7500 tok; the bound holds it near budget regardless.
	if got := loop.ContextTokens(); got > budget*2 {
		t.Errorf("within-turn bound failed: live context %d tok, budget %d (15 rounds unbounded would be ~7500)", got, budget)
	}
	// Old results were elided to get there; the goal anchor survived verbatim.
	elided, goalSeen := 0, false
	for _, msg := range loop.Transcript() {
		if strings.HasPrefix(msg.Content, evictedToolResultMarker) {
			elided++
		}
		if msg.Content == "fix the bug" {
			goalSeen = true
		}
	}
	if elided == 0 {
		t.Error("expected old tool results to be elided within the turn")
	}
	if !goalSeen {
		t.Error("goal anchor must remain in the bounded context")
	}
}
