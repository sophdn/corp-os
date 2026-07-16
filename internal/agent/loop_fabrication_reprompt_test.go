package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// doneClaim is a turn with no tool calls (the agent reports done). Its text may
// carry prose the fabrication audit inspects.
func doneClaim(text string) model.Response {
	return model.Response{Text: text, StopReason: model.StopEndTurn}
}

// fsEditTurn is a turn that emits a REAL fs.edit tool call (a substantive mutation).
func fsEditTurn(id, path string) model.Response {
	return model.Response{
		ToolCalls:  []tool.Call{{ID: id, Surface: "fs", Action: "edit", Params: map[string]any{"path": path}}},
		StopReason: model.StopToolUse,
	}
}

// countReprompts returns how many corrective fabrication re-prompts the loop injected
// into its transcript (user turns carrying the recognizable preamble).
func countReprompts(l *Loop) int {
	n := 0
	for _, m := range l.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, fabricationRepromptPreamble) {
			n++
		}
	}
	return n
}

// Bug 1078: a no-work done-claim (zero fs.write/edit dispatches on a RequireMutation
// duty) must be a HARD per-attempt re-prompt WITHIN the same turn — not a terminal
// Fabricated result that respawns a fresh worker. Given a second chance with the
// explicit correction, the model emits the real fs.edit and the turn recovers green.
func TestLoop_NoWorkDoneClaim_RepromptsAndRecovers(t *testing.T) {
	m := model.NewEcho("qwen",
		doneClaim("All set — the fix is in."), // claims done, dispatched nothing
		fsEditTurn("e1", "prod.go"),           // re-prompted → actually edits
		doneClaim("Done; edited prod.go."),    // now backed by a real mutation
	)
	fs := &recordingWriteFS{}
	loop := New(single(m), fs, nil, WithWorkAudit(WorkAudit{RequireMutation: true}))
	res, err := loop.Run(context.Background(), "implement the fix")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("a recovered re-prompt must NOT report fabrication; got %q", res.Fabricated)
	}
	if len(fs.writes) != 1 {
		t.Fatalf("the re-prompted model should have dispatched exactly 1 fs.edit; got %d", len(fs.writes))
	}
	if n := countReprompts(loop); n != 1 {
		t.Fatalf("expected exactly 1 corrective re-prompt; got %d", n)
	}
}

// When the model keeps fabricating across the whole re-prompt budget, the loop gives
// up and DOES return Fabricated — but only AFTER spending the bounded re-prompts, not
// on the first claim. The budget is the hard backstop the operator seat escalates from.
func TestLoop_NoWorkDoneClaim_ExhaustsRepromptsThenFabricated(t *testing.T) {
	// Echo repeats a done-claim once its script is exhausted, so every turn is no-work.
	m := model.NewEcho("qwen", doneClaim("done (but nothing was written)"))
	fs := &recordingWriteFS{}
	loop := New(single(m), fs, nil,
		WithWorkAudit(WorkAudit{RequireMutation: true}),
		WithFabricationReprompts(2))
	res, err := loop.Run(context.Background(), "implement the fix")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated == "" {
		t.Fatal("a persistently fabricating worker must eventually report Fabricated")
	}
	if !strings.Contains(res.Fabricated, "no-work") {
		t.Fatalf("the verdict should name the no-work signal; got %q", res.Fabricated)
	}
	if n := countReprompts(loop); n != 2 {
		t.Fatalf("expected exactly 2 re-prompts (the budget) before giving up; got %d", n)
	}
}

// The prose-tool-call signal (the model narrated a tool-call envelope as text instead
// of emitting a real call, and it was not recoverable) is ALSO a re-prompt, not a
// terminal — the model gets told to emit a real call and recovers.
func TestLoop_ProseToolCallClaim_RepromptsAndRecovers(t *testing.T) {
	// A flat {"surface":..,"action":..} envelope: the prose audit matches it, but
	// recoverNarrated needs the {"name":..,"arguments":{"action":..}} shape, so it is
	// NOT auto-recovered — it falls through to the fabrication guard.
	prose := `I'll edit it now: {"surface": "fs", "action": "edit", "params": {"path": "prod.go"}}`
	m := model.NewEcho("qwen",
		doneClaim(prose),
		fsEditTurn("e1", "prod.go"),
		doneClaim("Done; edited prod.go."),
	)
	fs := &recordingWriteFS{}
	loop := New(single(m), fs, nil, WithWorkAudit(WorkAudit{RequireMutation: true}))
	res, err := loop.Run(context.Background(), "implement the fix")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("a recovered prose-tool-call re-prompt must NOT report fabrication; got %q", res.Fabricated)
	}
	if n := countReprompts(loop); n != 1 {
		t.Fatalf("expected exactly 1 corrective re-prompt for the prose signal; got %d", n)
	}
}

// A read-only loop (no RequireMutation audit) never re-prompts a clean done-claim:
// the re-prompt path is gated on the fabrication audit, so a summary/review that
// legitimately mutates nothing terminates normally.
func TestLoop_NoAudit_NoReprompt(t *testing.T) {
	m := model.NewEcho("qwen", doneClaim("here is the summary"))
	loop := New(single(m), &fakeProvider{}, nil)
	res, err := loop.Run(context.Background(), "summarize")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("a read-only duty must not be flagged; got %q", res.Fabricated)
	}
	if n := countReprompts(loop); n != 0 {
		t.Fatalf("a read-only duty must not be re-prompted; got %d", n)
	}
}
