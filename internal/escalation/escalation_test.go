package escalation

import (
	"context"
	"testing"

	"corpos/internal/router"
	"corpos/internal/tool"
)

// fakeDispatcher returns a scripted Result and records the last call.
type fakeDispatcher struct {
	res  tool.Result
	last tool.Call
}

func (f *fakeDispatcher) Dispatch(_ context.Context, call tool.Call) tool.Result {
	f.last = call
	return f.res
}

func okResult(v any) tool.Result  { return tool.Result{OK: true, Value: v} }
func errResult(v any) tool.Result { return tool.Result{OK: false, Value: v} }

func fl(v float64) *float64 { return &v }

func TestProposeSuccessSendsFullPayload(t *testing.T) {
	f := &fakeDispatcher{res: okResult(map[string]any{"ok": true, "event_id": "evt-1"})}
	c := New(f)
	id, err := c.Propose(context.Background(), Proposal{
		Trigger: "repeated_tool_error", FromModel: "local", ToModel: "mid",
		SessionID: "sess", TurnIndex: 3, StateBefore: "cheap", StateAfter: "escalated",
		TriggerDetail: "tool_errors=5", FiredThreshold: fl(3), ProjectID: "mcp-servers", Reason: "why",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "evt-1" {
		t.Errorf("event id = %q, want evt-1", id)
	}
	if f.last.Surface != "admin" || f.last.Action != "escalation_propose" {
		t.Errorf("dispatched %s.%s, want admin.escalation_propose", f.last.Surface, f.last.Action)
	}
	p := f.last.Params
	if p["trigger"] != "repeated_tool_error" || p["turn_index"] != 3 ||
		p["state_before"] != "cheap" || p["state_after"] != "escalated" {
		t.Errorf("core params wrong: %+v", p)
	}
	if p["trigger_detail"] != "tool_errors=5" || p["fired_threshold"] != 3.0 ||
		p["project_id"] != "mcp-servers" || p["reason"] != "why" {
		t.Errorf("optional params wrong: %+v", p)
	}
	if f.last.Rationale != "why" {
		t.Errorf("rationale = %q, want why", f.last.Rationale)
	}
}

func TestProposeOmitsEmptyOptionals(t *testing.T) {
	f := &fakeDispatcher{res: okResult(map[string]any{"event_id": ""})}
	c := New(f)
	if _, err := c.Propose(context.Background(), Proposal{
		Trigger: "parse_failure", FromModel: "a", ToModel: "b", SessionID: "s",
		StateBefore: "cheap", StateAfter: "escalated",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, k := range []string{"trigger_detail", "fired_threshold", "project_id", "reason"} {
		if _, present := f.last.Params[k]; present {
			t.Errorf("empty optional %q should be omitted", k)
		}
	}
}

func TestProposeErrorResult(t *testing.T) {
	f := &fakeDispatcher{res: errResult(map[string]any{"error": "boom"})}
	c := New(f)
	if _, err := c.Propose(context.Background(), Proposal{}); err == nil {
		t.Fatal("expected an error from a failed dispatch")
	}
}

func TestProposeUnexpectedShape(t *testing.T) {
	f := &fakeDispatcher{res: okResult([]any{1, 2})}
	c := New(f)
	if _, err := c.Propose(context.Background(), Proposal{}); err == nil {
		t.Fatal("expected an error on a non-map success response")
	}
}

func TestThresholdsBuildsConfig(t *testing.T) {
	rows := []any{
		map[string]any{"trigger_kind": "repeated_tool_error", "threshold_value": 3.0, "enabled": true, "de_escalation_turns": 2.0},
		map[string]any{"trigger_kind": "low_confidence", "threshold_value": 0.35, "enabled": true, "de_escalation_turns": 4.0},
		map[string]any{"trigger_kind": "parse_failure", "threshold_value": 2.0, "enabled": false, "de_escalation_turns": 2.0},
		"garbage-row-skipped",
		map[string]any{"threshold_value": 1.0}, // no trigger_kind → skipped
	}
	f := &fakeDispatcher{res: okResult(rows)}
	c := New(f)
	cfg, err := c.Thresholds(context.Background(), "mcp-servers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.last.Params["project_id"] != "mcp-servers" {
		t.Errorf("project_id not forwarded: %+v", f.last.Params)
	}
	if cfg.DeEscalationTurns != 4 { // max across rows
		t.Errorf("K = %d, want 4 (max)", cfg.DeEscalationTurns)
	}
	if tc := cfg.Triggers[router.TriggerRepeatedToolError]; !tc.Enabled || tc.ThresholdValue != 3 {
		t.Errorf("repeated_tool_error = %+v", tc)
	}
	if tc := cfg.Triggers[router.TriggerParseFailure]; tc.Enabled {
		t.Errorf("parse_failure should be disabled, got %+v", tc)
	}
	if len(cfg.Triggers) != 3 {
		t.Errorf("built %d triggers, want 3 (two garbage rows skipped)", len(cfg.Triggers))
	}
}

func TestThresholdsEmptyTableFallsBackToDefault(t *testing.T) {
	f := &fakeDispatcher{res: okResult([]any{})}
	c := New(f)
	cfg, err := c.Thresholds(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Triggers) != len(router.DefaultConfig().Triggers) {
		t.Error("an empty table should yield the built-in DefaultConfig")
	}
	if _, present := f.last.Params["project_id"]; present {
		t.Error("an empty project_id should be omitted from the request")
	}
}

func TestThresholdsErrorReturnsDefaultAndError(t *testing.T) {
	f := &fakeDispatcher{res: errResult(map[string]any{"error": "unreachable"})}
	c := New(f)
	cfg, err := c.Thresholds(context.Background(), "p")
	if err == nil {
		t.Fatal("expected an error on a failed dispatch")
	}
	if len(cfg.Triggers) != len(router.DefaultConfig().Triggers) {
		t.Error("a failed fetch should still return a usable DefaultConfig")
	}
}

func TestThresholdsUnexpectedShapeReturnsDefaultAndError(t *testing.T) {
	f := &fakeDispatcher{res: okResult(map[string]any{"not": "a list"})}
	c := New(f)
	cfg, err := c.Thresholds(context.Background(), "p")
	if err == nil {
		t.Fatal("expected an error on a non-list response")
	}
	if len(cfg.Triggers) == 0 {
		t.Error("should fall back to DefaultConfig")
	}
}

func TestThresholdsZeroKDefaultsToTwo(t *testing.T) {
	rows := []any{
		map[string]any{"trigger_kind": "repeated_tool_error", "threshold_value": 3.0, "enabled": true, "de_escalation_turns": 0.0},
	}
	c := New(&fakeDispatcher{res: okResult(rows)})
	cfg, _ := c.Thresholds(context.Background(), "")
	if cfg.DeEscalationTurns != 2 {
		t.Errorf("a zero K should default to 2, got %d", cfg.DeEscalationTurns)
	}
}

func TestCoercionHelpers(t *testing.T) {
	if toFloat(int(3)) != 3 || toFloat(int64(4)) != 4 || toFloat("x") != 0 {
		t.Error("toFloat coercions wrong")
	}
	if toInt(float64(5)) != 5 || toInt(int64(6)) != 6 || toInt("x") != 0 {
		t.Error("toInt coercions wrong")
	}
	if !toBool(float64(1)) || toBool(float64(0)) || !toBool(int(1)) || toBool("x") {
		t.Error("toBool coercions wrong")
	}
	if errText("plain") != "plain" {
		t.Error("errText fallback wrong")
	}
}
