package agent

import (
	"context"
	"errors"
	"testing"

	"corpos/internal/escalation"
	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// recEmitter records Propose calls and returns a scripted id/err.
type recEmitter struct {
	calls []escalation.Proposal
	id    string
	err   error
}

func (r *recEmitter) Propose(_ context.Context, p escalation.Proposal) (string, error) {
	r.calls = append(r.calls, p)
	return r.id, r.err
}

// toolErrorThenAnswer is a cheap adapter that makes one tool call (which the
// errProvider fails → ToolErrors=1) then answers on the retry round.
func toolErrorThenAnswer(id string) model.Adapter {
	return model.NewEcho(id,
		model.Response{ToolCalls: []tool.Call{{ID: "c", Surface: "work", Action: "x"}}, StopReason: model.StopToolUse},
		model.Response{Text: id + "-answer", StopReason: model.StopEndTurn},
	)
}

func TestLoop_EscalateEdgeEmitsProposal(t *testing.T) {
	st := tempStore(t)
	cheap := toolErrorThenAnswer("cheap")
	strong := model.NewEcho("strong", model.Response{Text: "strong-answer", StopReason: model.StopEndTurn})
	rt := router.New(cheap, strong, router.WithConfig(router.Config{
		Triggers:          map[router.Trigger]router.TriggerConfig{router.TriggerRepeatedToolError: {ThresholdValue: 1, Enabled: true}},
		DeEscalationTurns: 2,
	}))
	em := &recEmitter{id: "evt-9"}
	loop := New(rt, errProvider{}, nil, WithStore(st), WithSession("sess", "mcp-servers"), WithEscalationEmitter(em))

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.calls) != 1 {
		t.Fatalf("emitter called %d times, want 1", len(em.calls))
	}
	p := em.calls[0]
	if p.Trigger != "repeated_tool_error" || p.FromModel != "cheap" || p.ToModel != "strong" {
		t.Errorf("proposal trigger/models wrong: %+v", p)
	}
	if p.StateBefore != "cheap" || p.StateAfter != "escalated" {
		t.Errorf("proposal states = %s→%s", p.StateBefore, p.StateAfter)
	}
	if p.SessionID != "sess" || p.ProjectID != "mcp-servers" {
		t.Errorf("proposal scope wrong: session=%s project=%s", p.SessionID, p.ProjectID)
	}
	if p.FiredThreshold == nil || *p.FiredThreshold != 1 {
		t.Errorf("fired threshold = %v, want 1", p.FiredThreshold)
	}

	esc, err := st.Escalations()
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}
	if len(esc) != 1 || esc[0].Edge != "escalate" || esc[0].Trigger != "repeated_tool_error" {
		t.Errorf("local escalation row wrong: %+v", esc)
	}
}

func TestLoop_ExplicitHandoffHookEscalates(t *testing.T) {
	conf := 0.9
	h := hooks.NewSurface()
	// A post_tool_use hook requests the strong tier and reports a confidence — both
	// harvested into the turn's signals before Observe.
	_ = h.Register(hooks.PostToolUse, "handoff", func(c *hooks.Context) {
		c.RequestEscalation = true
		c.EscalationConfidence = &conf
	})
	cheap := toolErrorThenAnswer("cheap") // makes a tool call so post_tool_use fires
	strong := model.NewEcho("strong", model.Response{Text: "s", StopReason: model.StopEndTurn})
	rt := router.New(cheap, strong, router.WithConfig(router.Config{
		Triggers:          map[router.Trigger]router.TriggerConfig{router.TriggerExplicitHandoff: {ThresholdValue: 1, Enabled: true}},
		DeEscalationTurns: 2,
	}))
	em := &recEmitter{id: "evt-h"}
	// fakeProvider succeeds (no tool error), so ONLY the explicit-handoff hook drives escalation.
	loop := New(rt, &fakeProvider{}, nil, WithHooks(h), WithEscalationEmitter(em))

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.State() != router.StateEscalated {
		t.Fatal("explicit handoff should have escalated")
	}
	if len(em.calls) != 1 || em.calls[0].Trigger != "explicit_handoff" {
		t.Errorf("want one explicit_handoff proposal, got %+v", em.calls)
	}
}

func TestLoop_DeescalateEdgeRecordsLocallyWithoutEmitting(t *testing.T) {
	st := tempStore(t)
	cheap := toolErrorThenAnswer("cheap")
	strong := model.NewEcho("strong", model.Response{Text: "strong-clean", StopReason: model.StopEndTurn})
	rt := router.New(cheap, strong, router.WithConfig(router.Config{
		Triggers:          map[router.Trigger]router.TriggerConfig{router.TriggerRepeatedToolError: {ThresholdValue: 1, Enabled: true}},
		DeEscalationTurns: 1, // de-escalate after a single clean turn
	}))
	em := &recEmitter{id: "evt-1"}
	loop := New(rt, errProvider{}, nil, WithStore(st), WithEscalationEmitter(em))

	if _, err := loop.Run(context.Background(), "trouble"); err != nil { // turn 0 → escalate
		t.Fatalf("turn 0: %v", err)
	}
	if _, err := loop.Run(context.Background(), "clean"); err != nil { // turn 1 (strong, clean) → de-escalate
		t.Fatalf("turn 1: %v", err)
	}
	if len(em.calls) != 1 {
		t.Errorf("de-escalation must not emit: emitter called %d times, want 1 (the escalate only)", len(em.calls))
	}
	esc, err := st.Escalations()
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}
	if len(esc) != 2 || esc[0].Edge != "escalate" || esc[1].Edge != "deescalate" {
		t.Fatalf("want [escalate, deescalate] rows, got %+v", esc)
	}
	if esc[1].Trigger != "de_escalated" || esc[1].FromModel != "strong" || esc[1].ToModel != "cheap" {
		t.Errorf("de-escalation row wrong: %+v", esc[1])
	}
}

func TestLoop_EmitterErrorLeavesEventIDEmpty(t *testing.T) {
	st := tempStore(t)
	cheap := toolErrorThenAnswer("cheap")
	strong := model.NewEcho("strong", model.Response{Text: "s", StopReason: model.StopEndTurn})
	r := router.New(cheap, strong, router.WithConfig(router.Config{
		Triggers:          map[router.Trigger]router.TriggerConfig{router.TriggerRepeatedToolError: {ThresholdValue: 1, Enabled: true}},
		DeEscalationTurns: 2,
	}))
	em := &recEmitter{err: errors.New("toolkit down")}
	loop := New(r, errProvider{}, nil, WithStore(st), WithEscalationEmitter(em))

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("an emitter failure must not abort the turn: %v", err)
	}
	// The local escalation row still lands (the event id is just empty).
	esc, err := st.Escalations()
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}
	if len(esc) != 1 || esc[0].Edge != "escalate" {
		t.Errorf("escalation row should still be recorded on emit failure: %+v", esc)
	}
}
