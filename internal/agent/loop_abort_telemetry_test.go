package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// abortScript drives a turn that opens its telemetry row on round 0 (a tool call,
// which sets turnModel and fires recordTurnStart) and then hits a fatal,
// non-recoverable model fault on round 1 (a plain error routes through the
// recovery default → recoverAbort). It is the mid-turn fatal-exit shape the
// suggestion targets: the turn started, did work, then the model died.
func abortScript() *scriptAdapter {
	return &scriptAdapter{id: "qwen", steps: []scriptStep{
		{resp: model.Response{Model: "qwen",
			ToolCalls:  []tool.Call{{ID: "c1", Surface: "work", Action: "x"}},
			StopReason: model.StopToolUse}},
		{err: errors.New("boom")}, // fatal, non-recoverable → aborts the turn
	}}
}

// TestRun_ModelErrorClosesTurnRowWithAbortMarker asserts that when a turn is
// aborted by a fatal mid-turn model error, the turn telemetry row is still CLOSED
// (ended_at stamped) and carries an `aborted` marker so it is distinguishable from
// an in-flight turn — instead of leaking an open row (ended_at NULL).
func TestRun_ModelErrorClosesTurnRowWithAbortMarker(t *testing.T) {
	st := tempStore(t)

	_, err := New(single(abortScript()), &fakeProvider{}, nil, WithStore(st)).
		Run(context.Background(), "go")
	if err == nil {
		t.Fatal("a fatal model error must propagate")
	}
	if !strings.Contains(err.Error(), "model turn") {
		t.Errorf("returned error %q does not carry the model-turn cause", err)
	}

	turns, terr := st.Turns()
	if terr != nil {
		t.Fatalf("Turns: %v", terr)
	}
	if len(turns) != 1 {
		t.Fatalf("turn rows = %d, want 1: %+v", len(turns), turns)
	}
	if turns[0].EndedAt == "" {
		t.Error("aborted turn row left open (ended_at NULL); want it CLOSED")
	}
	if !strings.Contains(turns[0].SignalsJSON, "aborted") {
		t.Errorf("signals_json %q lacks an `aborted` marker", turns[0].SignalsJSON)
	}
}

// TestRun_ModelErrorFoldsPersistErrorIntoReturnedError asserts that a telemetry/
// message persist failure occurring while a turn is aborted is still observable:
// it is folded into the returned error (errors.Join), not silently dropped behind
// a bare Result{}. A closed store fails every write, standing in for the seeded
// persist error.
func TestRun_ModelErrorFoldsPersistErrorIntoReturnedError(t *testing.T) {
	st := tempStore(t)
	_ = st.Close() // every subsequent store write now fails

	_, err := New(single(abortScript()), &fakeProvider{}, nil, WithStore(st)).
		Run(context.Background(), "go")
	if err == nil {
		t.Fatal("want an error from the aborted turn")
	}
	if !strings.Contains(err.Error(), "model turn") {
		t.Errorf("returned error %q lost the model-turn cause", err)
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Errorf("returned error %q does not surface the dropped persist failure", err)
	}
}
