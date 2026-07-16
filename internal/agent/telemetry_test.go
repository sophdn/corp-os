package agent

import (
	"context"
	"testing"

	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/router"
	"corpos/internal/session"
	"corpos/internal/tool"
)

// toolErrProvider always fails a dispatch with a tool_error (to drive escalation).
type toolErrProvider struct{}

func (toolErrProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: false, ErrorClass: tool.ClassTool, Value: map[string]any{"error": "boom"}, LatencyMS: 4}
}

func TestLoopRecordsTelemetryAndEscalation(t *testing.T) {
	dir := t.TempDir()
	st, err := session.Create(dir, session.Header{Project: "p"})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Turn 1 runs on cheap and emits a tool call that errors → escalate. Turn 2
	// then runs on strong, so the model changes cheap→strong (an escalation edge).
	cheap := model.NewEcho("cheap",
		model.Response{ToolCalls: []tool.Call{{ID: "1", Surface: "work", Action: "x"}}, StopReason: model.StopToolUse},
		model.Response{Text: "t1 done", StopReason: model.StopEndTurn},
	)
	strong := model.NewEcho("strong", model.Response{Text: "t2 done", StopReason: model.StopEndTurn})
	rt := router.New(cheap, strong, router.WithEscalation(1, 2))
	loop := New(rt, toolErrProvider{}, nil, WithStore(st), WithSession(st.RunID(), "p"))

	if _, err := loop.Run(context.Background(), "turn one"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := loop.Run(context.Background(), "turn two"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	loop.Close()

	turns, calls, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if turns != 2 {
		t.Errorf("turn rows = %d, want 2", turns)
	}
	if calls != 1 {
		t.Errorf("tool-call rows = %d, want 1", calls)
	}
	esc, err := st.Escalations()
	if err != nil {
		t.Fatal(err)
	}
	if len(esc) != 1 || esc[0].Edge != "escalate" || esc[0].FromModel != "cheap" || esc[0].ToModel != "strong" {
		t.Errorf("escalations = %+v, want one escalate cheap→strong", esc)
	}
}

func TestParentRunIDContext(t *testing.T) {
	if ParentRunID(context.Background()) != "" {
		t.Error("an unstamped context should yield an empty parent run id")
	}
	ctx := WithParentRunID(context.Background(), "RID")
	if ParentRunID(ctx) != "RID" {
		t.Error("WithParentRunID/ParentRunID round-trip failed")
	}
}

func TestSpawnerChildStoreLinksAndTelemeters(t *testing.T) {
	dir := t.TempDir()
	var childID string
	factory := func(parentRunID, profileName, duty string) *session.Store {
		s, err := session.Create(dir, session.Header{Project: "p", ParentRunID: parentRunID, Profile: profileName, Duty: duty})
		if err != nil {
			t.Fatalf("child store create: %v", err)
		}
		childID = s.RunID()
		return s
	}
	m := model.NewEcho("q", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	s := NewSpawner(&fakeProvider{}, nilProject, nil, m, WithChildStore(factory))

	ctx := WithParentRunID(context.Background(), "ROOTID")
	if _, err := s.Run(ctx, &profile.JobProfile{Name: "task-lifecycle", Tier: profile.TierLocal}, "do a thing"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	re, err := session.Open(dir, childID)
	if err != nil {
		t.Fatalf("reopen child store: %v", err)
	}
	defer func() { _ = re.Close() }()
	h, err := re.HeaderRow()
	if err != nil {
		t.Fatal(err)
	}
	if h.ParentRunID != "ROOTID" || h.Profile != "task-lifecycle" || h.Duty != "do a thing" {
		t.Errorf("child header = %+v, want parent ROOTID / task-lifecycle / 'do a thing'", h)
	}
	if turns, _, err := re.Counts(); err != nil || turns < 1 {
		t.Errorf("worker should have recorded a turn row, got %d (err %v)", turns, err)
	}
}

func TestWithChildStoreIgnoresNil(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("q"), WithChildStore(nil))
	if s.childStore != nil {
		t.Error("a nil child-store factory must be ignored")
	}
}

func TestSpawnerRunWithoutChildStoreFactory(t *testing.T) {
	// No factory wired: the worker runs untelemetered without panicking.
	m := model.NewEcho("q", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	s := NewSpawner(&fakeProvider{}, nilProject, nil, m)
	if _, err := s.Run(WithParentRunID(context.Background(), "R"), &profile.JobProfile{Name: "x", Tier: profile.TierLocal}, "d"); err != nil {
		t.Fatalf("Run without a child-store factory: %v", err)
	}
}
