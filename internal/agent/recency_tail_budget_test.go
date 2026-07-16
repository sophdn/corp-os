package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"corpos/internal/model"
)

// TestCompactToBudget_TokenBoundedRecencyTail is the bug 1091 regression: when the
// recent turn-groups are large (frontier verify-revise turns), the verbatim recency
// tail must be bounded by TOKENS, not a fixed group COUNT. Otherwise the tail alone
// exceeds the budget and compaction can never get the transcript under budget however
// much older history it summarizes (the run-2 "still over budget" state, ~2x window).
func TestCompactToBudget_TokenBoundedRecencyTail(t *testing.T) {
	const budget = 5000
	c := &Compactor{budget: budget, recencyTurns: 6, summarizer: &recSummarizer{out: "[s]"}, tokenRatio: 1.0}

	in := []model.ChatMessage{sys("SYSTEM")}
	// Eight large turn-groups, ~1500 est-tokens each (a ~6000-char assistant body). The
	// old count-based tail would keep six of them verbatim (~9000 tokens) — far over the
	// 5000 budget — and report "still over budget".
	for i := 0; i < 8; i++ {
		in = append(in, usr(fmt.Sprintf("turn %d", i)), asst(strings.Repeat("x", 6000)))
	}

	out, _, ev, err := c.compact(context.Background(), in, "goal", "", 0, 8)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if ev == nil {
		t.Fatal("expected a compaction event")
	}
	if ev.TokensAfter > budget {
		t.Fatalf("compacted transcript still over budget: %d > %d — the verbatim tail was not token-bounded", ev.TokensAfter, budget)
	}
	// Coherence floor: the most recent turn is always kept verbatim in the tail.
	if !containsUserTurn(out, "turn 7") {
		t.Fatal("the latest turn must be kept verbatim in the recency tail")
	}
	// And it really did fold older groups into the summary (not a no-op).
	if ev.GroupsEvicted == 0 {
		t.Fatal("expected older turn-groups to be summarized")
	}
}

// TestCompactToBudget_CapStillBinds guards the constraint that recencyTurns stays the
// upper CAP on the verbatim tail: when the turns are small enough that the cap (not the
// token budget) is the binding limit, the tail keeps exactly recencyTurns groups — the
// pre-fix behaviour for the common small-turn case is preserved.
func TestCompactToBudget_CapStillBinds(t *testing.T) {
	const budget = 2800
	c := &Compactor{budget: budget, recencyTurns: 3, summarizer: &recSummarizer{out: "[s]"}, tokenRatio: 1.0}

	in := []model.ChatMessage{sys("SYSTEM")}
	// Six moderate groups (~500 est-tokens each): the three latest fit the token tail, so
	// the cap binds at 3 while the older three are summarized.
	for i := 0; i < 6; i++ {
		in = append(in, usr(fmt.Sprintf("turn %d", i)), asst(strings.Repeat("y", 2000)))
	}
	out, _, ev, err := c.compact(context.Background(), in, "goal", "", 0, 6)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if ev == nil {
		t.Fatal("expected compaction (six small groups exceed the tiny budget)")
	}
	// The cap (recencyTurns=3) still bounds the verbatim tail, so the three latest turns
	// are kept while older ones are summarized.
	for _, turn := range []string{"turn 3", "turn 4", "turn 5"} {
		if !containsUserTurn(out, turn) {
			t.Fatalf("small-turn run should keep %q verbatim", turn)
		}
	}
	if containsUserTurn(out, "turn 0") {
		t.Fatal("the oldest group should have been summarized, not kept verbatim")
	}
}

func containsUserTurn(msgs []model.ChatMessage, content string) bool {
	for _, m := range msgs {
		if m.Role == model.RoleUser && m.Content == content {
			return true
		}
	}
	return false
}
