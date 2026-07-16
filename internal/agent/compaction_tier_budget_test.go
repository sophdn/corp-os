package agent

import (
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
)

// tierBudgetTranscript returns n tool-result messages of `chars` bytes each,
// preceded by a user/assistant turn so the goal anchor exists.
func tierBudgetTranscript(n, chars int) []model.ChatMessage {
	msgs := []model.ChatMessage{
		{Role: model.RoleUser, Content: "implement the feature"},
		{Role: model.RoleAssistant, Content: "reading the sources"},
	}
	body := strings.Repeat("x", chars)
	for i := 0; i < n; i++ {
		msgs = append(msgs, model.ChatMessage{Role: model.RoleTool, Name: "fs", Content: body})
	}
	return msgs
}

// TestCompactionBudget_TracksActiveRung is the GAP-2a regression for bug 1088: the
// compaction budget must follow the rung the loop is ACTUALLY serving on, not stay
// pinned to the floor model's window. A floor worker that escalates to a wide-window
// tier must get that tier's budget — otherwise escalation buys a better model but the
// same starved 6144-token budget, and every source-file read is evicted on arrival.
func TestCompactionBudget_TracksActiveRung(t *testing.T) {
	floor := model.NewEcho("floor", model.Response{Text: "done", StopReason: model.StopEndTurn})
	strong := model.NewEcho("strong", model.Response{Text: "done", StopReason: model.StopEndTurn})
	rt := router.NewLadder([]model.Adapter{floor, strong}, 0)

	const floorBudget, strongBudget = 6144, 200000
	l := New(rt, vProvider{}, nil,
		WithCompaction(floorBudget, 2, floor),
		WithTierBudgets([]int{floorBudget, strongBudget}),
	)
	l.toolSpecTokens = 0

	// ~15k tokens of tool results: over the floor budget, well under the strong one.
	l.transcript = tierBudgetTranscript(10, 6000)

	// On the floor rung the within-turn bound is starved and MUST evict.
	l.syncBudgetToRung()
	if l.compactor.budget != floorBudget {
		t.Fatalf("floor rung budget = %d, want %d", l.compactor.budget, floorBudget)
	}
	if ev := l.boundWithinTurn(); ev == nil {
		t.Fatal("floor rung: expected eviction of the over-budget transcript, got none")
	}

	// Escalate to the strong (wide-window) rung and re-seed. The budget must now
	// track that rung, and the same transcript must NOT be evicted.
	l.transcript = tierBudgetTranscript(10, 6000)
	if edge := rt.EscalateForNoProgress(5); edge.Direction != router.EdgeEscalate {
		t.Fatalf("escalation did not fire: %+v", edge)
	}
	if rt.CurrentRung() != 1 {
		t.Fatalf("router did not climb to the strong rung: cur=%d", rt.CurrentRung())
	}
	l.syncBudgetToRung()
	if l.compactor.budget != strongBudget {
		t.Fatalf("escalated budget = %d, want the strong rung's %d (budget stayed pinned to the floor)", l.compactor.budget, strongBudget)
	}
	if ev := l.boundWithinTurn(); ev != nil {
		t.Fatalf("strong rung: a transcript that fits the wide window was evicted (%d results) — escalation did not buy context", ev.GroupsEvicted)
	}
}
