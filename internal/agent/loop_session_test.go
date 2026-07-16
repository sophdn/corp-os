package agent

import (
	"context"
	"math"
	"testing"

	"corpos/internal/model"
	"corpos/internal/session"
)

func tempStore(t *testing.T) *session.Store {
	t.Helper()
	st, err := session.Create(t.TempDir(), session.Header{Project: "p", ModelCheap: "c", ModelStrong: "s"})
	if err != nil {
		t.Fatalf("session.Create: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRun_PersistsTurnMessagesToStore(t *testing.T) {
	st := tempStore(t)
	m := model.NewEcho("qwen", model.Response{Text: "hi", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil, WithSystemPrompt("sys"), WithStore(st)).
		Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PersistErr != nil {
		t.Fatalf("unexpected PersistErr: %v", res.PersistErr)
	}
	msgs, err := st.Messages()
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	// The system prompt is NOT persisted (it is re-seeded on resume); only the
	// turn's user + assistant messages are.
	if len(msgs) != 2 {
		t.Fatalf("persisted %d messages, want 2 (user+assistant): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != model.RoleUser || msgs[0].Content != "hello" {
		t.Errorf("msg0 = %+v, want user/hello", msgs[0])
	}
	if msgs[1].Role != model.RoleAssistant || msgs[1].Content != "hi" {
		t.Errorf("msg1 = %+v, want assistant/hi", msgs[1])
	}
	if msgs[0].TurnIndex != 0 {
		t.Errorf("turn index = %d, want 0", msgs[0].TurnIndex)
	}
}

func TestRun_PersistErrorIsSurfacedNonFatally(t *testing.T) {
	st := tempStore(t)
	_ = st.Close() // writes now fail, but the turn must still succeed

	m := model.NewEcho("qwen", model.Response{Text: "answer", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil, WithStore(st)).Run(context.Background(), "q")
	if err != nil {
		t.Fatalf("a persistence failure must not abort the turn, got err %v", err)
	}
	if res.Text != "answer" {
		t.Errorf("answer lost on persist failure: %q", res.Text)
	}
	if res.PersistErr == nil {
		t.Error("a closed store should surface PersistErr")
	}
}

func TestWithResumed_SeedsTranscriptAfterSystemPromptAndContinuesTurns(t *testing.T) {
	st := tempStore(t)
	history := []model.ChatMessage{
		{Role: model.RoleUser, Content: "earlier"},
		{Role: model.RoleAssistant, Content: "prior"},
	}
	rec := &recAdapter{}
	res, err := New(single(rec), &fakeProvider{}, nil,
		WithSystemPrompt("sys"), WithStore(st), WithResumed(history, 5)).
		Run(context.Background(), "now")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PersistErr != nil {
		t.Fatalf("PersistErr: %v", res.PersistErr)
	}
	// The model sees: system, the two resumed turns, then the new user prompt.
	want := []string{"sys", "earlier", "prior", "now"}
	if len(rec.got) != len(want) {
		t.Fatalf("transcript len = %d, want %d: %+v", len(rec.got), len(want), rec.got)
	}
	for i, w := range want {
		if rec.got[i].Content != w {
			t.Errorf("transcript[%d] = %q, want %q", i, rec.got[i].Content, w)
		}
	}
	// Turn numbering continues from 5, so the new messages persist at turn 5.
	msgs, _ := st.Messages()
	if len(msgs) == 0 || msgs[len(msgs)-1].TurnIndex != 5 {
		t.Errorf("resumed turn index = %v, want 5", msgs)
	}
}

func TestResumeState_FiltersToConversationThread(t *testing.T) {
	msgs := []session.Message{
		{TurnIndex: 0, Role: model.RoleSystem, Content: "sys"},   // dropped (re-seeded)
		{TurnIndex: 0, Role: model.RoleUser, Content: "u1"},      // kept
		{TurnIndex: 0, Role: model.RoleAssistant, Content: ""},   // dropped (empty/tool-only)
		{TurnIndex: 0, Role: model.RoleTool, Content: "toolout"}, // dropped (no pairing)
		{TurnIndex: 0, Role: model.RoleAssistant, Content: "a1"}, // kept
		{TurnIndex: 3, Role: model.RoleUser, Content: "u2"},      // kept; sets max turn
	}
	history, nextTurn := ResumeState(msgs)
	wantContent := []string{"u1", "a1", "u2"}
	if len(history) != len(wantContent) {
		t.Fatalf("history = %+v, want contents %v", history, wantContent)
	}
	for i, w := range wantContent {
		if history[i].Content != w {
			t.Errorf("history[%d] = %q, want %q", i, history[i].Content, w)
		}
	}
	if nextTurn != 4 {
		t.Errorf("nextTurn = %d, want 4 (max turn 3 + 1)", nextTurn)
	}
}

func TestLoop_CostTracksPerModelSpend(t *testing.T) {
	// A priced model id (Haiku) with known usage so the ledger prices it:
	// 1000/1000*0.001 + 1000/1000*0.005 = 0.006.
	m := model.NewEcho("claude-haiku-4-5-20251001",
		model.Response{Text: "hi", StopReason: model.StopEndTurn,
			Usage: model.Usage{InputTokens: 1000, OutputTokens: 1000}})
	loop := New(single(m), &fakeProvider{}, nil)
	res, err := loop.Run(context.Background(), "q")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	total, breakdown := loop.Cost()
	const want = 0.006
	if math.Abs(total-want) > 1e-9 {
		t.Errorf("total = %v, want %v", total, want)
	}
	if math.Abs(res.CostUSD-total) > 1e-9 {
		t.Errorf("Result.CostUSD %v disagrees with Cost() total %v", res.CostUSD, total)
	}
	if len(breakdown) != 1 {
		t.Fatalf("breakdown len = %d, want 1: %+v", len(breakdown), breakdown)
	}
	if breakdown[0].Model != "claude-haiku-4-5-20251001" || !breakdown[0].Priced {
		t.Errorf("breakdown[0] = %+v, want priced Haiku", breakdown[0])
	}
}

func TestResumeState_EmptyInput(t *testing.T) {
	history, nextTurn := ResumeState(nil)
	if history != nil {
		t.Errorf("history = %+v, want nil", history)
	}
	if nextTurn != 0 {
		t.Errorf("nextTurn = %d, want 0", nextTurn)
	}
}
