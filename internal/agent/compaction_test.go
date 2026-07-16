package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// recSummarizer is a fake summarizer adapter: it records every span it is asked
// to summarize and returns a fixed short summary, so a test can assert both the
// continuity input (what reached the summarizer) and a bounded output.
type recSummarizer struct {
	got []string
	out string
	err error
}

func (r *recSummarizer) Model() string   { return "sum" }
func (r *recSummarizer) Available() bool { return true }
func (r *recSummarizer) Complete(_ context.Context, msgs []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	if r.err != nil {
		return model.Response{}, r.err
	}
	r.got = append(r.got, msgs[len(msgs)-1].Content)
	out := r.out
	if out == "" {
		out = "[summary]"
	}
	return model.Response{Model: "sum", Text: out, StopReason: model.StopEndTurn}, nil
}

func sys(c string) model.ChatMessage { return model.ChatMessage{Role: model.RoleSystem, Content: c} }
func usr(c string) model.ChatMessage { return model.ChatMessage{Role: model.RoleUser, Content: c} }
func asst(c string) model.ChatMessage {
	return model.ChatMessage{Role: model.RoleAssistant, Content: c}
}

// bigTool builds a tool-result message with a large body (the multi-round bloat).
func bigTool(id, name string, n int) model.ChatMessage {
	return model.ChatMessage{Role: model.RoleTool, ToolCallID: id, Name: name, Content: strings.Repeat("x", n)}
}

func TestEvictToolResultsBoundsAndPreserves(t *testing.T) {
	// One turn: goal + many (assistant, big tool-result) rounds, no interior RoleUser.
	transcript := []model.ChatMessage{sys("SYSTEM"), usr("fix task_stamp_sha")}
	for i := 0; i < 8; i++ {
		transcript = append(transcript,
			model.ChatMessage{Role: model.RoleAssistant, ToolCalls: []tool.Call{{Surface: "fs", Action: "grep"}}},
			bigTool("c", "fs", 400)) // ~100 tok each: 8 over budget, keeping 3 fits
	}
	budget := 800
	before := estimateTokens(transcript)
	n := evictToolResults(transcript, budget, 0, minKeepToolResults, minKeepToolResults, 1.0)
	if n == 0 {
		t.Fatal("expected eviction when far over budget")
	}
	if after := estimateTokens(transcript); after > budget {
		t.Errorf("after eviction size %d still over budget %d", after, budget)
	}
	if estimateTokens(transcript) >= before {
		t.Error("eviction should have shrunk the transcript")
	}
	// The goal anchor (RoleUser) is untouched.
	if transcript[1].Role != model.RoleUser || transcript[1].Content != "fix task_stamp_sha" {
		t.Error("goal anchor must never be evicted")
	}
	// Eviction stops as soon as it's under budget, so it keeps AT LEAST the floor
	// (older ones first). Evicted results keep their tool_call id (no orphaned
	// tool_use), and the most-recent minKeepToolResults results are always verbatim.
	var toolMsgs []model.ChatMessage
	kept := 0
	for _, m := range transcript {
		if m.Role != model.RoleTool {
			continue
		}
		toolMsgs = append(toolMsgs, m)
		if strings.HasPrefix(m.Content, evictedToolResultMarker) {
			if m.ToolCallID != "c" {
				t.Error("evicted result lost its tool_call id (would orphan the tool_use)")
			}
		} else {
			kept++
		}
	}
	if kept < minKeepToolResults {
		t.Errorf("kept %d results verbatim, want at least the floor %d", kept, minKeepToolResults)
	}
	for _, m := range toolMsgs[len(toolMsgs)-minKeepToolResults:] {
		if strings.HasPrefix(m.Content, evictedToolResultMarker) {
			t.Error("the most-recent results must never be elided")
		}
	}
}

func TestEvictToolResultsNoopUnderBudget(t *testing.T) {
	transcript := []model.ChatMessage{usr("goal"), bigTool("c", "fs", 40)}
	if n := evictToolResults(transcript, 100000, 0, 3, hardKeepToolResults, 1.0); n != 0 {
		t.Errorf("under budget should not evict, got %d", n)
	}
}

// TestEvictToolResultsFallsBelowSoftFloorUnderWindowPressure is the bug-1066 unit:
// when even the soft keep window's whole-file reads overflow the budget, eviction
// abandons the soft floor down to hardKeepToolResults so a single read fits. This is
// the failure the within-turn bound had — keeping 3 (or recencyTurns) whole-file
// reads when the floor window can physically hold only ~one.
func TestEvictToolResultsFallsBelowSoftFloorUnderWindowPressure(t *testing.T) {
	// Three whole-file reads (~3400 est tok each) on a 6144 budget: any two together
	// overflow, so the soft floor of 3 cannot be honored — eviction must fall to the
	// hard floor of 1.
	transcript := []model.ChatMessage{sys("SYSTEM"), usr("fix read.go")}
	for i := 0; i < 3; i++ {
		transcript = append(transcript,
			model.ChatMessage{Role: model.RoleAssistant, ToolCalls: []tool.Call{{Surface: "fs", Action: "read"}}},
			bigTool("r", "fs", 13671)) // a real read.go-sized whole-file read
	}
	const (
		budget   = 6144
		overhead = 1300
		softKeep = minKeepToolResults // 3 — larger than the window can hold
	)
	n := evictToolResults(transcript, budget, overhead, softKeep, hardKeepToolResults, 1.0)
	if n == 0 {
		t.Fatal("expected eviction below the soft floor when reads overflow the window")
	}
	// Under budget after eviction (the whole point — the carry now fits the floor).
	if got := overhead + estimateTokens(transcript); got > budget {
		t.Errorf("after eviction size %d still over budget %d — hard floor did not create headroom", got, budget)
	}
	// Exactly hardKeepToolResults results survive verbatim (the immediate working one).
	kept := 0
	for _, m := range transcript {
		if m.Role == model.RoleTool && !strings.HasPrefix(m.Content, evictedToolResultMarker) {
			kept++
		}
	}
	if kept != hardKeepToolResults {
		t.Errorf("kept %d results verbatim, want the hard floor %d", kept, hardKeepToolResults)
	}
	// The most-recent read is the one preserved (oldest-first eviction).
	last := transcript[len(transcript)-1]
	if last.Role != model.RoleTool || strings.HasPrefix(last.Content, evictedToolResultMarker) {
		t.Error("the most-recent result must be the one kept verbatim")
	}
}

// TestEvictToolResultsRatioCorrectsUnderEstimate is the bug-1066 ratio unit: a
// transcript whose len/4 estimate is UNDER budget but whose ratio-corrected size is
// OVER must still evict, because the floor-fit guard checks the ratio-corrected size.
// Before the fix, eviction used the raw estimate and skipped, letting a code-dense
// transcript pass eviction yet overflow the window.
func TestEvictToolResultsRatioCorrectsUnderEstimate(t *testing.T) {
	transcript := []model.ChatMessage{sys("SYSTEM"), usr("goal")}
	for i := 0; i < 5; i++ {
		transcript = append(transcript,
			model.ChatMessage{Role: model.RoleAssistant, ToolCalls: []tool.Call{{Surface: "fs", Action: "read"}}},
			bigTool("r", "fs", 2000)) // ~500 est tok each
	}
	raw := estimateTokens(transcript)
	budget := raw + 100 // raw estimate is UNDER budget...
	// ...but at ratio 2.0 the corrected size is ~2× over.
	if int(float64(raw)*2.0) <= budget {
		t.Fatalf("test setup: ratio-corrected size %d not over budget %d", int(float64(raw)*2.0), budget)
	}
	if n := evictToolResults(transcript, budget, 0, minKeepToolResults, hardKeepToolResults, 2.0); n == 0 {
		t.Error("ratio-corrected size over budget should evict even when the raw estimate fits")
	}
}

func TestEstimateTokens(t *testing.T) {
	got := estimateTokens([]model.ChatMessage{
		{Role: model.RoleUser, Content: strings.Repeat("x", 40)}, // 40/4 + 4 = 14
		{Role: model.RoleAssistant, ToolCalls: []tool.Call{
			{Surface: "work", Action: "task_read", Params: map[string]any{"id": 7}}, // (4+9)/4=3 + params + 8
		}},
	})
	if got < 14 {
		t.Errorf("estimateTokens = %d, want >= 14", got)
	}
}

func TestParamTokens(t *testing.T) {
	if paramTokens(nil) != 0 {
		t.Error("nil params should estimate 0")
	}
	if paramTokens(map[string]any{"k": "value"}) <= 0 {
		t.Error("non-empty params should estimate > 0")
	}
	// An unmarshalable value falls back to 0 rather than guessing.
	if paramTokens(map[string]any{"bad": make(chan int)}) != 0 {
		t.Error("unmarshalable params should estimate 0")
	}
}

func TestCurrentSizeAddsOverhead(t *testing.T) {
	c := &Compactor{budget: 100, recencyTurns: 2, summarizer: &recSummarizer{}}
	msgs := []model.ChatMessage{usr("short")}
	est := estimateTokens(msgs)
	// The total size is the fixed tool-spec overhead plus the transcript estimate —
	// the same units before and after a rewrite (the bug was an inconsistent floor).
	if got := c.currentSize(msgs, 500); got != est+500 {
		t.Errorf("currentSize with overhead 500 = %d, want %d", got, est+500)
	}
	// Overhead 0 (not yet calibrated) yields the transcript estimate alone.
	if got := c.currentSize(msgs, 0); got != est {
		t.Errorf("currentSize with zero overhead = %d, want estimate %d", got, est)
	}
}

// TestCompactSizesAreOverheadInclusiveAndConsistent is the regression test for the
// tool-spec-overhead bug: before AND after sizes must both include the fixed
// overhead (so the notice no longer overstates the reduction), the event must
// carry Budget + Overhead, and OverBudget must reflect that overhead the compaction
// could not evict.
func TestCompactSizesAreOverheadInclusiveAndConsistent(t *testing.T) {
	const overhead = 5000
	c := &Compactor{budget: 4000, recencyTurns: 2, summarizer: &recSummarizer{out: "[s]"}}
	in := longTranscript(6)

	out, _, ev, err := c.compact(context.Background(), in, firstUserContent(in), "", overhead, 7)
	if err != nil || ev == nil {
		t.Fatalf("expected a compaction: ev=%v err=%v", ev, err)
	}
	// Both endpoints include the overhead, in the same units.
	if want := overhead + estimateTokens(in); ev.TokensBefore != want {
		t.Errorf("TokensBefore = %d, want overhead+estimate = %d", ev.TokensBefore, want)
	}
	if want := overhead + estimateTokens(out); ev.TokensAfter != want {
		t.Errorf("TokensAfter = %d, want overhead+estimate = %d", ev.TokensAfter, want)
	}
	if ev.Overhead != overhead || ev.Budget != 4000 {
		t.Errorf("event missing budget/overhead: %+v", ev)
	}
	// Overhead (5000) alone exceeds the budget (4000), so compaction cannot get
	// under budget — the honest saturation signal, not a misleading 5000→300.
	if !ev.OverBudget() {
		t.Errorf("OverBudget should be true: after=%d budget=%d", ev.TokensAfter, ev.Budget)
	}
	if ev.TokensAfter < overhead {
		t.Errorf("after-size %d dropped below the un-evictable overhead %d", ev.TokensAfter, overhead)
	}
}

func TestCompactUnderBudgetNoOp(t *testing.T) {
	c := &Compactor{budget: 100000, recencyTurns: 2, summarizer: &recSummarizer{}}
	in := []model.ChatMessage{sys("system"), usr("goal"), asst("a")}
	out, summary, ev, err := c.compact(context.Background(), in, "goal", "", 0, 1)
	if err != nil || ev != nil {
		t.Fatalf("under budget should be a no-op: ev=%v err=%v", ev, err)
	}
	if len(out) != len(in) || summary != "" {
		t.Errorf("under budget should not change the transcript")
	}
}

// longTranscript builds a system prompt + n turn-groups; group 1 carries a
// tool_use/tool_result pair so eviction across a paired group is exercised.
func longTranscript(n int) []model.ChatMessage {
	out := []model.ChatMessage{sys("SYSTEM PROMPT")}
	for i := 0; i < n; i++ {
		out = append(out, usr(fmt.Sprintf("user turn %d %s", i, strings.Repeat("x", 80))))
		if i == 1 {
			call := tool.Call{ID: "c1", Surface: "work", Action: "task_read"}
			out = append(out, model.ChatMessage{Role: model.RoleAssistant, ToolCalls: []tool.Call{call}})
			out = append(out, model.ChatMessage{Role: model.RoleTool, ToolCallID: "c1", Name: "work", Content: "result"})
		}
		out = append(out, asst(fmt.Sprintf("assistant turn %d %s", i, strings.Repeat("y", 80))))
	}
	return out
}

func TestCompactEvictsAndPreserves(t *testing.T) {
	sum := &recSummarizer{out: "[earlier turns]"}
	c := &Compactor{budget: 150, recencyTurns: 2, summarizer: sum}
	in := longTranscript(6)
	goal := firstUserContent(in)

	out, summary, ev, err := c.compact(context.Background(), in, goal, "", 0, 5)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if ev == nil {
		t.Fatal("expected a compaction event")
	}
	if ev.TokensAfter >= ev.TokensBefore {
		t.Errorf("compaction did not shrink: before=%d after=%d", ev.TokensBefore, ev.TokensAfter)
	}
	if summary != "[earlier turns]" {
		t.Errorf("summary body = %q", summary)
	}
	// Exactly one marker (summary) message, and it carries the verbatim goal.
	markers := 0
	var markerMsg string
	for _, m := range out {
		if strings.HasPrefix(m.Content, compactionMarker) {
			markers++
			markerMsg = m.Content
		}
	}
	if markers != 1 {
		t.Fatalf("want exactly 1 summary message, got %d", markers)
	}
	if !strings.Contains(markerMsg, goal) {
		t.Errorf("summary message missing verbatim goal anchor %q", goal)
	}
	// The seeded system prompt is preserved as head.
	if out[0].Role != model.RoleSystem || out[0].Content != "SYSTEM PROMPT" {
		t.Errorf("head system prompt not preserved: %+v", out[0])
	}
	// No orphaned tool_result.
	assertNoOrphanToolResult(t, out)
	// The evicted span (which includes the tool pair, group 1) reached the summarizer.
	if len(sum.got) != 1 || !strings.Contains(sum.got[0], "user turn 1") {
		t.Errorf("evicted content not fed to summarizer: %v", sum.got)
	}
}

func TestCompactDoesNotStackSummaries(t *testing.T) {
	sum := &recSummarizer{out: "S2"}
	c := &Compactor{budget: 150, recencyTurns: 2, summarizer: sum}
	// A transcript that already carries a prior summary marker right after the head.
	in := []model.ChatMessage{sys("SYS"), {Role: model.RoleSystem, Content: compactionMarker + "\nold summary"}}
	in = append(in, longTranscript(5)[1:]...) // append turn-groups (skip the duplicate SYS)

	out, summary, ev, err := c.compact(context.Background(), in, "goal", "S1-prev", 0, 9)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if ev == nil {
		t.Fatal("expected compaction")
	}
	if summary != "S2" {
		t.Errorf("new summary body = %q", summary)
	}
	markers := 0
	for _, m := range out {
		if strings.HasPrefix(m.Content, compactionMarker) {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("summaries stacked: want 1 marker, got %d", markers)
	}
	// The prior summary text was carried into the new summarization input.
	if len(sum.got) != 1 || !strings.Contains(sum.got[0], "S1-prev") {
		t.Errorf("prior summary not folded into the new one: %v", sum.got)
	}
}

func TestCompactTooFewTurnsNoOp(t *testing.T) {
	c := &Compactor{budget: 1, recencyTurns: 5, summarizer: &recSummarizer{}}
	in := longTranscript(3) // 3 groups, K=5 → cannot evict
	out, _, ev, err := c.compact(context.Background(), in, "goal", "", 0, 1)
	if err != nil || ev != nil {
		t.Fatalf("too-few-turns should be a no-op: ev=%v err=%v", ev, err)
	}
	if len(out) != len(in) {
		t.Errorf("transcript changed on no-op")
	}
}

func TestCompactSummarizerErrorPropagates(t *testing.T) {
	c := &Compactor{budget: 150, recencyTurns: 2, summarizer: &recSummarizer{err: errors.New("boom")}}
	in := longTranscript(6)
	out, summary, ev, err := c.compact(context.Background(), in, "goal", "prev", 0, 1)
	if err == nil {
		t.Fatal("want summarizer error")
	}
	if ev != nil || summary != "prev" || len(out) != len(in) {
		t.Errorf("on error the inputs must be returned unchanged")
	}
}

func TestFirstUserContent(t *testing.T) {
	if firstUserContent([]model.ChatMessage{sys("s")}) != "" {
		t.Error("no user message should yield empty anchor")
	}
	if got := firstUserContent([]model.ChatMessage{sys("s"), usr("goal"), usr("second")}); got != "goal" {
		t.Errorf("firstUserContent = %q, want goal", got)
	}
}

// assertNoOrphanToolResult verifies every RoleTool message is preceded by a
// RoleAssistant carrying a matching tool_use id — the Anthropic-400 invariant.
func assertNoOrphanToolResult(t *testing.T, msgs []model.ChatMessage) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Role == model.RoleAssistant {
			for _, c := range m.ToolCalls {
				seen[c.ID] = true
			}
		}
		if m.Role == model.RoleTool && !seen[m.ToolCallID] {
			t.Errorf("orphaned tool_result %q (no preceding tool_use)", m.ToolCallID)
		}
	}
}
