package arcreview

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/tool"
)

type fakeDisp struct {
	review      tool.Result
	reviewCalls int
	forgeCalls  []tool.Call
}

func (f *fakeDisp) Dispatch(_ context.Context, c tool.Call) tool.Result {
	switch c.Action {
	case "review_arc_for_filing":
		f.reviewCalls++
		return f.review
	case "forge":
		f.forgeCalls = append(f.forgeCalls, c)
	}
	return tool.Result{OK: true, Value: map[string]any{"ok": true}}
}

func userMsg(s string) model.ChatMessage { return model.ChatMessage{Role: model.RoleUser, Content: s} }
func asstMsg(s string) model.ChatMessage {
	return model.ChatMessage{Role: model.RoleAssistant, Content: s}
}
func transcriptOf(m ...model.ChatMessage) *[]model.ChatMessage { return &m }

func TestDetectShape(t *testing.T) {
	cases := map[string]string{
		"are we done here":    "done",
		"thanks!":             "thanks",
		"ok wrapping up now":  "wrapping",
		"that's all":          "thats_all",
		"looks good to me":    "looks_good",
		"session end please":  "session_end",
		"anything else?":      "any_else",
		"/clear":              "clear_command",
		"let's keep building": "",
		"":                    "",
	}
	for in, want := range cases {
		if got := detectShape(in); got != want {
			t.Errorf("detectShape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLastUserMessage(t *testing.T) {
	if lastUserMessage(nil) != "" {
		t.Error("nil transcript should yield empty")
	}
	tr := transcriptOf(asstMsg("hi"), model.ChatMessage{Role: model.RoleTool, Content: "t"})
	if lastUserMessage(tr) != "" {
		t.Error("no user message should yield empty")
	}
	tr2 := transcriptOf(userMsg("first"), asstMsg("a"), userMsg("last"), asstMsg("b"))
	if got := lastUserMessage(tr2); got != "last" {
		t.Errorf("lastUserMessage = %q, want last", got)
	}
}

func TestNoTriggerDoesNotCallReview(t *testing.T) {
	fd := &fakeDisp{}
	r := New(fd, "s", WithTurnThreshold(5))
	c := &hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("keep going"), asstMsg("ok"))}
	r.PostTurnHook()(c)
	if fd.reviewCalls != 0 {
		t.Error("a sub-threshold turn with no shape must not fire a review")
	}
	if r.turnsSinceReview != 1 {
		t.Errorf("counter = %d, want 1", r.turnsSinceReview)
	}
}

func TestShapeFiresBelowThreshold(t *testing.T) {
	fd := &fakeDisp{review: tool.Result{OK: true, Value: reviewResult{Status: "skipped"}}}
	r := New(fd, "s", WithTurnThreshold(99))
	c := &hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("thanks, that's all"), asstMsg("bye"))}
	r.PostTurnHook()(c)
	if fd.reviewCalls != 1 {
		t.Error("an arc-close shape should fire even below the turn threshold")
	}
}

func TestFiresOnThresholdForgesAndQueuesReminders(t *testing.T) {
	rr := reviewResult{
		Status:   "fired",
		Triggers: []string{"counter_user_turns_2"},
		EventID:  "evt-1",
		Partition: partition{
			AutoExecute: []filingDecision{
				{Action: "forge_bug", Payload: json.RawMessage(`{"title":"Login Bug","problem_statement":"x"}`), Confidence: 0.9},
				{Action: "forge_suggestion", Payload: json.RawMessage(`{"title":"Nice Idea"}`), Confidence: 0.8},
				{Action: "memory_write", Payload: json.RawMessage(`{"name":"foo"}`)}, // body-heavy in auto → refused
			},
			StagedForAuthoring: []filingDecision{{Action: "forge_vault_note", Payload: json.RawMessage(`{"title":"Note"}`), Reasoning: "worth keeping"}},
			SurfaceForConfirm:  []filingDecision{{Action: "forge_bug", Payload: json.RawMessage(`{"title":"Maybe"}`), Confidence: 0.5, Reasoning: "uncertain"}},
		},
	}
	fd := &fakeDisp{review: tool.Result{OK: true, Value: rr}}
	r := New(fd, "sess", WithTurnThreshold(2))
	c := &hooks.Context{SessionID: "sess", Transcript: transcriptOf(userMsg("keep going"), asstMsg("ok"))}
	post := r.PostTurnHook()

	post(c) // turn 1 — below threshold, no shape
	if fd.reviewCalls != 0 {
		t.Fatal("must not fire on turn 1")
	}
	post(c) // turn 2 — threshold reached → fire

	if len(fd.forgeCalls) != 2 {
		t.Fatalf("want 2 forges (bug + suggestion; memory_write refused), got %d", len(fd.forgeCalls))
	}
	if fd.forgeCalls[0].Params["schema_name"] != "bug" || fd.forgeCalls[1].Params["schema_name"] != "suggestion" {
		t.Errorf("forge schemas wrong: %v, %v", fd.forgeCalls[0].Params["schema_name"], fd.forgeCalls[1].Params["schema_name"])
	}
	if fd.forgeCalls[0].Params["slug"] != "login-bug" {
		t.Errorf("slug = %v, want login-bug", fd.forgeCalls[0].Params["slug"])
	}
	fields := fd.forgeCalls[0].Params["fields"].(map[string]any)
	if fields["qwen_task_id"] != "arc-review-decisions" {
		t.Error("arc-review attribution not stamped onto the forged row")
	}
	if r.turnsSinceReview != 0 {
		t.Error("counter must reset after a fire")
	}
	if len(r.pending) != 2 {
		t.Fatalf("want 2 queued reminders (author + confirm), got %d", len(r.pending))
	}

	// PreUserPromptHook drains the reminders into the next turn.
	pc := &hooks.Context{}
	r.PreUserPromptHook()(pc)
	if len(pc.SystemPromptAdditions) != 2 {
		t.Errorf("reminders not surfaced: %d additions", len(pc.SystemPromptAdditions))
	}
	if len(r.pending) != 0 {
		t.Error("pending reminders not cleared after drain")
	}
	if !strings.Contains(pc.SystemPromptAdditions[0], "AUTHOR") || !strings.Contains(pc.SystemPromptAdditions[1], "confirm") {
		t.Error("reminder text shape wrong")
	}
}

func TestPreUserPromptNoopWhenEmpty(t *testing.T) {
	r := New(&fakeDisp{}, "s")
	pc := &hooks.Context{}
	r.PreUserPromptHook()(pc)
	if pc.SystemPromptAdditions != nil {
		t.Error("no reminders should add nothing")
	}
}

func TestNonFiredStatusDoesNothing(t *testing.T) {
	fd := &fakeDisp{review: tool.Result{OK: true, Value: reviewResult{Status: "debounced"}}}
	r := New(fd, "s", WithTurnThreshold(1))
	r.PostTurnHook()(&hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("go"), asstMsg("ok"))})
	if len(fd.forgeCalls) != 0 || len(r.pending) != 0 {
		t.Error("a non-fired status must not forge or queue reminders")
	}
	if r.turnsSinceReview != 0 {
		t.Error("the counter still resets on any fire attempt")
	}
}

func TestDispatcherFailureIsFailOpen(t *testing.T) {
	fd := &fakeDisp{review: tool.Result{OK: false, Value: map[string]any{"error": "unreachable"}}}
	r := New(fd, "s", WithTurnThreshold(1))
	r.PostTurnHook()(&hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("go"), asstMsg("ok"))})
	if len(fd.forgeCalls) != 0 {
		t.Error("an unreachable toolkit must fail open (no forge)")
	}
}

func TestMalformedResultIsFailOpen(t *testing.T) {
	// A value that cannot decode into reviewResult (a JSON array where an object
	// is expected) must fail open.
	fd := &fakeDisp{review: tool.Result{OK: true, Value: []any{1, 2, 3}}}
	r := New(fd, "s", WithTurnThreshold(1))
	r.PostTurnHook()(&hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("go"), asstMsg("ok"))})
	if len(fd.forgeCalls) != 0 {
		t.Error("a malformed result must fail open")
	}
}

func TestForgeSkipsBadPayload(t *testing.T) {
	rr := reviewResult{Status: "fired", Partition: partition{
		AutoExecute: []filingDecision{
			{Action: "forge_bug", Payload: json.RawMessage(`not-json`)},
			{Action: "forge_bug", Payload: json.RawMessage(`{}`)}, // empty fields
		},
	}}
	fd := &fakeDisp{review: tool.Result{OK: true, Value: rr}}
	r := New(fd, "s", WithTurnThreshold(1))
	r.PostTurnHook()(&hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("go"), asstMsg("ok"))})
	if len(fd.forgeCalls) != 0 {
		t.Error("an undecodable or empty payload must not produce a forge call")
	}
}

func TestMaterializeWritesUserAssistantOnly(t *testing.T) {
	tr := transcriptOf(
		model.ChatMessage{Role: model.RoleSystem, Content: "sys"},
		userMsg("hello"),
		model.ChatMessage{Role: model.RoleTool, Content: "tool-out"},
		asstMsg("hi there"),
		userMsg("   "), // whitespace-only → skipped
	)
	path, cleanup, err := materialize(tr)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	var roles []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row map[string]string
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("row not valid json: %v", err)
		}
		roles = append(roles, row["role"])
	}
	if len(roles) != 2 || roles[0] != "user" || roles[1] != "assistant" {
		t.Errorf("materialized roles = %v, want [user assistant]", roles)
	}
	cleanup() // idempotent cleanup is safe
}

func TestMaterializeNilTranscript(t *testing.T) {
	path, cleanup, err := materialize(nil)
	if err != nil {
		t.Fatalf("materialize(nil): %v", err)
	}
	defer cleanup()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("nil transcript should write an empty file, got %d bytes", len(b))
	}
}

func TestMaterializeCreateTempErrorAndFireFailOpen(t *testing.T) {
	// Point the temp dir at a non-existent path so CreateTemp fails.
	t.Setenv("TMPDIR", "/nonexistent/corpos-arc-test")
	if _, _, err := materialize(transcriptOf(userMsg("hi"))); err == nil {
		t.Error("materialize should error when the temp dir is unwritable")
	}
	// A fire that can't materialise must fail open: no review call.
	fd := &fakeDisp{}
	r := New(fd, "s", WithTurnThreshold(1))
	r.PostTurnHook()(&hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("go"), asstMsg("ok"))})
	if fd.reviewCalls != 0 {
		t.Error("a materialisation failure must fail open before the review call")
	}
}

func TestFiredWithOnlyAutoExecuteQueuesNoReminders(t *testing.T) {
	rr := reviewResult{Status: "fired", Partition: partition{
		AutoExecute: []filingDecision{{Action: "forge_bug", Payload: json.RawMessage(`{"title":"B"}`)}},
	}}
	fd := &fakeDisp{review: tool.Result{OK: true, Value: rr}}
	r := New(fd, "s", WithTurnThreshold(1))
	r.PostTurnHook()(&hooks.Context{SessionID: "s", Transcript: transcriptOf(userMsg("go"), asstMsg("ok"))})
	if len(fd.forgeCalls) != 1 {
		t.Errorf("the auto_execute bug should forge, got %d", len(fd.forgeCalls))
	}
	if len(r.pending) != 0 {
		t.Error("no staged/confirm decisions should queue no reminders")
	}
}

func TestDeriveSlug(t *testing.T) {
	if got := deriveSlug(map[string]any{"name": "my-memory-slug"}); got != "my-memory-slug" {
		t.Errorf("name should win: %q", got)
	}
	if got := deriveSlug(map[string]any{"title": "Some Big Title!"}); got != "some-big-title" {
		t.Errorf("title kebab: %q", got)
	}
	if got := deriveSlug(map[string]any{}); got != "arc-review-filing" {
		t.Errorf("fallback: %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := deriveSlug(map[string]any{"name": long}); len(got) != 80 {
		t.Errorf("slug should cap at 80, got %d", len(got))
	}
}

func TestOptionsGuardrails(t *testing.T) {
	r := New(&fakeDisp{}, "s", WithTurnThreshold(0), WithTimeout(0))
	if r.threshold != DefaultTurnThreshold {
		t.Errorf("threshold 0 should keep the default, got %d", r.threshold)
	}
	if r.timeout != defaultTimeout {
		t.Errorf("timeout 0 should keep the default, got %v", r.timeout)
	}
	r2 := New(&fakeDisp{}, "s", WithTurnThreshold(3), WithTimeout(time.Second))
	if r2.threshold != 3 || r2.timeout != time.Second {
		t.Errorf("options not applied: %d %v", r2.threshold, r2.timeout)
	}
}
