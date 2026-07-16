package agent

import (
	"context"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

var fsSurfaces = map[string]bool{"fs": true, "sys": true}

func TestRecoverNarrated_FencedBlock(t *testing.T) {
	text := "I'll read the file.\n```json\n" +
		`{"name": "fs", "arguments": {"action": "read", "params": {"file_path": "internal/fs/read.go"}, "rationale": "inspect"}}` +
		"\n```\nThen I'll edit it."
	got := recoverNarrated(text, fsSurfaces)
	if len(got) != 1 {
		t.Fatalf("recovered %d calls, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Surface != "fs" || c.Action != "read" {
		t.Errorf("got %s.%s, want fs.read", c.Surface, c.Action)
	}
	if c.Params["file_path"] != "internal/fs/read.go" {
		t.Errorf("params not carried: %+v", c.Params)
	}
	if c.Rationale != "inspect" {
		t.Errorf("rationale = %q", c.Rationale)
	}
}

func TestRecoverNarrated_BareObject(t *testing.T) {
	text := `Let me run the test: {"name":"sys","arguments":{"action":"exec","params":{"command":"go test ./..."}}}`
	got := recoverNarrated(text, fsSurfaces)
	if len(got) != 1 || got[0].Surface != "sys" || got[0].Action != "exec" {
		t.Fatalf("want sys.exec, got %+v", got)
	}
}

func TestRecoverNarrated_FirstOfMany(t *testing.T) {
	// A model that narrates a batch — recover only the first; the loop dispatches
	// it, the model sees the result, and continues (one-step tool use).
	text := `{"name":"fs","arguments":{"action":"read","params":{"file_path":"a.go"}}}` +
		"\n" + `{"name":"fs","arguments":{"action":"edit","params":{"file_path":"a.go"}}}`
	got := recoverNarrated(text, fsSurfaces)
	if len(got) != 1 || got[0].Action != "read" {
		t.Fatalf("want only the first (read), got %+v", got)
	}
}

func TestRecoverNarrated_RejectsNonCalls(t *testing.T) {
	cases := map[string]string{
		"prose only":            "I will now read the file and edit the UnmarshalJSON method.",
		"json but not a call":   "Here's the config: {\"timeout\": 30, \"retries\": 3}",
		"name without action":   `{"name":"fs","arguments":{"params":{"file_path":"a.go"}}}`,
		"unknown surface":       `{"name":"database","arguments":{"action":"query"}}`,
		"braces inside strings": `{"note":"this { is not } a call","value":1}`,
	}
	for name, text := range cases {
		if got := recoverNarrated(text, fsSurfaces); got != nil {
			t.Errorf("%s: expected no recovery, got %+v", name, got)
		}
	}
}

func TestRecoverNarrated_NoSurfaceGateAllowsAny(t *testing.T) {
	// With no offered-surface set (nil), the shape gate alone applies — a valid
	// {name, arguments{action}} object is recovered.
	text := `{"name":"fs","arguments":{"action":"grep","params":{"pattern":"x"}}}`
	if got := recoverNarrated(text, nil); len(got) != 1 || got[0].Action != "grep" {
		t.Fatalf("want fs.grep with nil surface gate, got %+v", got)
	}
}

func TestBalancedObjects_QuotedBraces(t *testing.T) {
	// A { or } inside a JSON string must not skew brace depth.
	objs := balancedObjects(`pre {"a":"x}y{z","b":2} post`, false)
	if len(objs) != 1 || objs[0] != `{"a":"x}y{z","b":2}` {
		t.Fatalf("balancedObjects mishandled quoted braces: %q", objs)
	}
}

// recordingProvider captures every dispatched call so a test can prove a narrated
// call was recovered into a REAL dispatch (not discarded as a content answer).
type recordingProvider struct{ seen []tool.Call }

func (r *recordingProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	r.seen = append(r.seen, c)
	return tool.Result{Call: c, OK: true, Value: map[string]any{"ok": true}}
}

// TestLoopRecoversNarratedCallEndToEnd: a model whose first response NARRATES an
// fs.read as JSON text (empty structured ToolCalls) must have that call executed —
// the loop recovers it, dispatches it, and only then ends on the second response.
func TestLoopRecoversNarratedCallEndToEnd(t *testing.T) {
	narration := "Reading the file:\n```json\n" +
		`{"name":"fs","arguments":{"action":"read","params":{"file_path":"a.go"}}}` + "\n```"
	m := model.NewEcho("local",
		model.Response{Text: narration, StopReason: model.StopEndTurn},
		model.Response{Text: "done", StopReason: model.StopEndTurn},
	)
	rec := &recordingProvider{}
	loop := New(single(m), rec, []tool.Spec{{Name: "fs"}})
	if _, err := loop.Run(context.Background(), "fix it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.seen) != 1 {
		t.Fatalf("dispatched %d calls, want 1 (the recovered narration): %+v", len(rec.seen), rec.seen)
	}
	if rec.seen[0].Surface != "fs" || rec.seen[0].Action != "read" {
		t.Errorf("recovered call = %s.%s, want fs.read", rec.seen[0].Surface, rec.seen[0].Action)
	}
	// A recovered call MUST get a non-empty id: an empty tool_use.id breaks the
	// Anthropic adapter on escalation (live regression, bug-945 Coder-14B run).
	if rec.seen[0].ID == "" {
		t.Error("recovered call has no id — the Anthropic adapter rejects an empty tool_use.id on escalation")
	}
}
