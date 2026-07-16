package repl

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/agent"
	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// recordingModel captures the transcript it is asked to complete on each turn
// and returns a canned answer — a sans-IO stand-in for a live model.
type recordingModel struct {
	seen  [][]model.ChatMessage
	reply string
}

func (r *recordingModel) Model() string   { return "rec" }
func (r *recordingModel) Available() bool { return true }
func (r *recordingModel) Complete(_ context.Context, msgs []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	r.seen = append(r.seen, append([]model.ChatMessage(nil), msgs...))
	return model.Response{Model: "rec", Text: r.reply, StopReason: model.StopEndTurn}, nil
}

// nopProvider answers every dispatch with an empty ok result (no tools fire in
// this test).
type nopProvider struct{}

func (nopProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: true, Value: map[string]any{}}
}

// TestRun_ContextCarriesAcrossREPLTurns wires the REPL to a REAL agent.Loop (with
// a recording fake model + no-op provider) and proves the conversation carries:
// turn 2's transcript contains turn 1's user message and assistant reply. This
// is the end-to-end "context carry" guarantee, exercised with no live model or
// toolkit-server.
func TestRun_ContextCarriesAcrossREPLTurns(t *testing.T) {
	rm := &recordingModel{reply: "ack"}
	loop := agent.New(router.New(rm, rm), nopProvider{}, nil, agent.WithSystemPrompt("sys"))
	defer loop.Close()

	var out, errOut strings.Builder
	in := strings.NewReader("my name is Sophi\nwhat is my name\nexit\n")
	if err := Run(context.Background(), in, &out, &errOut, loop, Config{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(rm.seen) != 2 {
		t.Fatalf("model saw %d turns, want 2", len(rm.seen))
	}
	var second strings.Builder
	for _, m := range rm.seen[1] {
		second.WriteString(m.Role + ":" + m.Content + "\n")
	}
	got := second.String()
	if !strings.Contains(got, "user:my name is Sophi") {
		t.Errorf("turn-2 transcript missing turn-1 user message:\n%s", got)
	}
	if !strings.Contains(got, "assistant:ack") {
		t.Errorf("turn-2 transcript missing turn-1 assistant reply:\n%s", got)
	}
	if !strings.Contains(got, "user:what is my name") {
		t.Errorf("turn-2 transcript missing turn-2 prompt:\n%s", got)
	}
	// Both answers were printed to out.
	if strings.Count(out.String(), "ack") != 2 {
		t.Errorf("want two printed answers, got: %q", out.String())
	}
}
