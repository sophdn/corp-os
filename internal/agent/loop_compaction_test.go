package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// TestLoopCompactionLongSession is the chain's acceptance test: drive a long
// multi-turn session past the budget under a configured compactor and assert it
// stays within budget while preserving continuity (the goal anchor never drops,
// summaries never stack, and no tool_result is orphaned).
func TestLoopCompactionLongSession(t *testing.T) {
	// The driving model: turn 0 emits a tool call (so a tool_use/tool_result pair
	// exists to evict), then every turn ends with a plain answer (Echo falls back to
	// echoing the last user prompt once its script is exhausted).
	drive := model.NewEcho("qwen",
		model.Response{ToolCalls: []tool.Call{{ID: "c1", Surface: "work", Action: "task_read"}}, StopReason: model.StopToolUse},
		model.Response{Text: "ack", StopReason: model.StopEndTurn},
	)
	sum := &recSummarizer{out: "[earlier turns summarized]"}

	const budget = 600
	loop := New(single(drive), &fakeProvider{}, nil,
		WithSystemPrompt("SYS"),
		WithCompaction(budget, 2, sum),
	)

	const goal = "GOAL-ANCHOR establish the persistent fact FACT-42"
	prompts := []string{goal}
	for i := 1; i < 10; i++ {
		prompts = append(prompts, fmt.Sprintf("turn %d %s", i, strings.Repeat("z", 200)))
	}

	compactions := 0
	for i, p := range prompts {
		res, err := loop.Run(context.Background(), p)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if res.Compaction != nil {
			compactions++
			if res.Compaction.TokensAfter > budget {
				t.Errorf("turn %d: post-compaction size %d exceeds budget %d", i, res.Compaction.TokensAfter, budget)
			}
			if res.Compaction.GroupsEvicted < 1 {
				t.Errorf("turn %d: compaction evicted nothing", i)
			}
		}
	}

	if compactions == 0 {
		t.Fatal("a long session never compacted — budget never enforced")
	}
	// The live size signal is reported (and bounded near budget) once compaction
	// has been keeping the transcript in check.
	if got := loop.ContextTokens(); got <= 0 {
		t.Errorf("ContextTokens with a compactor = %d, want > 0", got)
	}

	final := loop.Transcript()
	// Continuity: exactly one summary message, carrying the verbatim goal anchor.
	markers, anchorPresent := 0, false
	for _, m := range final {
		if strings.HasPrefix(m.Content, compactionMarker) {
			markers++
			if strings.Contains(m.Content, goal) {
				anchorPresent = true
			}
		}
	}
	if markers != 1 {
		t.Errorf("want exactly 1 rolling summary, got %d", markers)
	}
	if !anchorPresent {
		t.Error("the active-goal anchor was lost across compaction")
	}
	// The early fact reached the summary path (continuity is summarized, not dropped).
	foundFact := false
	for _, g := range sum.got {
		if strings.Contains(g, "FACT-42") {
			foundFact = true
		}
	}
	if !foundFact {
		t.Error("the evicted goal turn never reached the summarizer")
	}
	// No orphaned tool_result survives in the compacted transcript.
	assertNoOrphanToolResult(t, final)
}

// TestLoopCompactionDisabled confirms a zero budget leaves the loop uncompacted
// (no compactor, ContextTokens reports 0, transcript grows normally).
func TestLoopCompactionDisabled(t *testing.T) {
	loop := New(single(model.NewEcho("qwen")), &fakeProvider{}, nil,
		WithSystemPrompt("SYS"),
		WithCompaction(0, 2, &recSummarizer{}), // budget 0 → no compactor
	)
	if _, err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if loop.ContextTokens() != 0 {
		t.Errorf("ContextTokens with no compactor = %d, want 0", loop.ContextTokens())
	}
	if res, _ := loop.Run(context.Background(), "again"); res.Compaction != nil {
		t.Error("compaction fired with no compactor configured")
	}
}

// TestLoopCompactionSummarizerErrorBestEffort confirms a summarizer failure does
// not abort the turn — compaction is skipped and the turn still answers.
func TestLoopCompactionSummarizerErrorBestEffort(t *testing.T) {
	sum := &recSummarizer{err: errSummarize}
	loop := New(single(model.NewEcho("qwen")), &fakeProvider{}, nil,
		WithSystemPrompt("SYS"),
		WithCompaction(1, 1, sum), // tiny budget so a compaction is attempted
	)
	// Two long turns: the second turn's boundary attempts a compaction that errors.
	if _, err := loop.Run(context.Background(), strings.Repeat("a", 400)); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	res, err := loop.Run(context.Background(), strings.Repeat("b", 400))
	if err != nil {
		t.Fatalf("Run 2 must still succeed despite a summarizer error: %v", err)
	}
	if res.Compaction != nil {
		t.Error("a failed summarization should yield no compaction event")
	}
}

// TestLoopCompactionResumeGoalAnchor confirms the goal anchor is derived from a
// resumed transcript whose first prompt predates this loop instance.
func TestLoopCompactionResumeGoalAnchor(t *testing.T) {
	history := []model.ChatMessage{usr("RESUMED-GOAL"), asst("ok")}
	sum := &recSummarizer{out: "[s]"}
	loop := New(single(model.NewEcho("qwen")), &fakeProvider{}, nil,
		WithSystemPrompt("SYS"),
		WithResumed(history, 1),
		WithCompaction(1, 1, sum),
	)
	for i := 0; i < 4; i++ {
		if _, err := loop.Run(context.Background(), fmt.Sprintf("p%d %s", i, strings.Repeat("q", 120))); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	for _, m := range loop.Transcript() {
		if strings.HasPrefix(m.Content, compactionMarker) && strings.Contains(m.Content, "RESUMED-GOAL") {
			return // anchor correctly derived from the resumed history
		}
	}
	t.Error("resumed goal anchor not preserved in the rolling summary")
}

var errSummarize = fmt.Errorf("summarizer down")

// measuredAdapter reports a fixed provider input-token count regardless of the
// transcript size — standing in for the tool-spec overhead the real provider folds
// into its measured count.
type measuredAdapter struct{ input int }

func (m *measuredAdapter) Model() string   { return "meas" }
func (m *measuredAdapter) Available() bool { return true }
func (m *measuredAdapter) Complete(_ context.Context, _ []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	return model.Response{Model: "meas", Text: "ok", Usage: model.Usage{InputTokens: m.input}, StopReason: model.StopEndTurn}, nil
}

// TestLoopCompactionCalibratesToolSpecOverhead is the loop-level regression test
// for the tool-spec-overhead bug: the loop calibrates the fixed overhead from the
// measured input count, folds it into the size signal (so ContextTokens and the
// compaction sizes include it), keeps it across compactions, and reports honest
// over-budget saturation when the overhead alone exceeds the budget.
func TestLoopCompactionCalibratesToolSpecOverhead(t *testing.T) {
	const measured = 5000
	sum := &recSummarizer{out: "[s]"}
	loop := New(single(&measuredAdapter{input: measured}), &fakeProvider{}, nil,
		WithSystemPrompt("SYS"),
		WithCompaction(4000, 2, sum), // budget below the overhead → saturated but honest
	)

	var events []*CompactionEvent
	for i := 0; i < 6; i++ {
		res, err := loop.Run(context.Background(), fmt.Sprintf("turn %d", i))
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if res.Compaction != nil {
			events = append(events, res.Compaction)
		}
	}

	if len(events) == 0 {
		t.Fatal("overhead never triggered a compaction")
	}
	// ContextTokens now reflects the calibrated overhead, not just the tiny transcript.
	if got := loop.ContextTokens(); got < measured-1000 {
		t.Errorf("ContextTokens = %d, expected to include the ~%d overhead", got, measured)
	}
	for _, ev := range events {
		if ev.Overhead < measured-1000 {
			t.Errorf("event overhead %d did not reflect the ~%d measured overhead", ev.Overhead, measured)
		}
		if ev.Budget != 4000 {
			t.Errorf("event budget = %d, want 4000", ev.Budget)
		}
		// Budget (4000) is below the overhead (~5000), so every compaction is honestly
		// still over budget — the after-size includes the un-evictable overhead.
		if !ev.OverBudget() {
			t.Errorf("event should be over budget: after=%d budget=%d", ev.TokensAfter, ev.Budget)
		}
	}
}
