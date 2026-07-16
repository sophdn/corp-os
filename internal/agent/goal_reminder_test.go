package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

func TestRefreshGoalReminderDedupsAndTails(t *testing.T) {
	goal := "fix task_stamp_sha auto-close"
	tr := []model.ChatMessage{
		{Role: model.RoleSystem, Content: "sys"},
		{Role: model.RoleUser, Content: goal},
		{Role: model.RoleAssistant, Content: "thinking"},
	}
	tr = refreshGoalReminder(tr, goal)
	if last := tr[len(tr)-1]; !strings.HasPrefix(last.Content, goalReminderMarker) || last.Role != model.RoleUser {
		t.Fatalf("reminder should be a RoleUser message at the tail, got %+v", last)
	}
	if !strings.Contains(tr[len(tr)-1].Content, goal) {
		t.Error("reminder should carry the goal text")
	}
	// A second refresh must DROP the stale reminder, not stack — still exactly one.
	tr = append(tr, model.ChatMessage{Role: model.RoleAssistant, Content: "more"})
	tr = refreshGoalReminder(tr, goal)
	count := 0
	for _, m := range tr {
		if strings.HasPrefix(m.Content, goalReminderMarker) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reminders must not stack: found %d", count)
	}
	if !strings.HasPrefix(tr[len(tr)-1].Content, goalReminderMarker) {
		t.Error("the refreshed reminder must be back at the tail")
	}
}

func TestRefreshGoalReminderNoAnchorNoop(t *testing.T) {
	tr := []model.ChatMessage{{Role: model.RoleUser, Content: "x"}}
	if got := refreshGoalReminder(tr, "  "); len(got) != 1 {
		t.Errorf("empty anchor should be a no-op, got %d msgs", len(got))
	}
}

// recordingRunaway emits a tool call every turn and records each transcript it was
// asked to complete, so a test can assert the goal anchor is in context at every
// model turn.
type recordingRunaway struct {
	call tool.Call
	seen [][]model.ChatMessage
}

func (r *recordingRunaway) Model() string   { return "rec" }
func (r *recordingRunaway) Available() bool { return true }
func (r *recordingRunaway) Complete(_ context.Context, msgs []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	r.seen = append(r.seen, append([]model.ChatMessage(nil), msgs...))
	return model.Response{Model: "rec", ToolCalls: []tool.Call{r.call}, StopReason: model.StopToolUse}, nil
}

// TestGoalReminderResurfacedAndAlwaysPresent: across a long loop the goal is in
// context at every model turn (3), and the periodic reminder re-surfaces it while
// never stacking (1).
func TestGoalReminderResurfacedAndAlwaysPresent(t *testing.T) {
	rec := &recordingRunaway{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	loop := New(single(rec), &fakeProvider{}, nil, WithMaxRounds(10), WithGoalReminder(3))
	const goal = "fix task_stamp_sha auto-close"

	if _, err := loop.Run(context.Background(), goal); err == nil {
		t.Fatal("perpetual caller should hit max rounds")
	}

	// (3) the goal anchor is present in every model turn's context.
	for i, msgs := range rec.seen {
		present := false
		for _, m := range msgs {
			if strings.Contains(m.Content, goal) {
				present = true
				break
			}
		}
		if !present {
			t.Fatalf("model turn %d had no goal anchor in context", i)
		}
	}

	// The reminder actually re-surfaced (some later turn saw it near the tail).
	resurfaced := false
	for _, msgs := range rec.seen {
		if len(msgs) > 0 && strings.HasPrefix(msgs[len(msgs)-1].Content, goalReminderMarker) {
			resurfaced = true
			break
		}
	}
	if !resurfaced {
		t.Error("goal reminder should re-surface at the tail under a long loop")
	}

	// (1)/terse: never stacked — at most one reminder lives in the transcript.
	count := 0
	for _, m := range loop.Transcript() {
		if strings.HasPrefix(m.Content, goalReminderMarker) {
			count++
		}
	}
	if count > 1 {
		t.Errorf("reminders must not stack: found %d in the final transcript", count)
	}
}
