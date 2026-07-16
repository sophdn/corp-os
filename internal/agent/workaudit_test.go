package agent

import (
	"context"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

func TestLooksLikeProseToolCall(t *testing.T) {
	yes := []string{
		`I'll run {"surface":"fs","action":"write","params":{"path":"x"}} now.`,
		"```json\n{\n  \"action\": \"edit\",\n  \"arguments\": {}\n}\n```",
		`Calling the tool: {"tool":"fs","action":"read"}`,
	}
	for _, s := range yes {
		if !looksLikeProseToolCall(s) {
			t.Errorf("expected prose-tool-call match: %q", s)
		}
	}
	no := []string{
		"",
		"I fixed the bug by adding a type guard to UnmarshalJSON.",
		`The config has an "action" field but this is plain prose.`, // action key but no envelope key, no brace nearby? has no '{'
		"Here is some JSON: {\"name\": \"value\"}",                  // a brace + a key, but no action key
	}
	for _, s := range no {
		if looksLikeProseToolCall(s) {
			t.Errorf("did not expect a match: %q", s)
		}
	}
}

func TestCountMutations(t *testing.T) {
	d := []tool.Result{
		{Call: tool.Call{Surface: "fs", Action: "write"}, OK: true},
		{Call: tool.Call{Surface: "fs", Action: "read"}, OK: true},  // not mutating
		{Call: tool.Call{Surface: "fs", Action: "edit"}, OK: false}, // failed → not counted
		{Call: tool.Call{Surface: "fs", Action: "move"}, OK: true},
		{Call: tool.Call{Surface: "work", Action: "forge"}, OK: true}, // not fs
	}
	if got := countMutations(d); got != 2 {
		t.Fatalf("countMutations = %d, want 2", got)
	}
}

func TestWorkAuditAssess(t *testing.T) {
	mut := []tool.Result{{Call: tool.Call{Surface: "fs", Action: "write"}, OK: true}}

	// prose tool call → fabrication, regardless of RequireMutation.
	if v := (WorkAudit{}).assess(`{"surface":"fs","action":"write"}`, mut, false); v == "" {
		t.Fatal("prose tool call should be flagged")
	}
	// RequireMutation + zero mutations → no-work.
	if v := (WorkAudit{RequireMutation: true}).assess("all done", nil, false); v == "" {
		t.Fatal("zero mutations on a mutation-expecting task should be flagged")
	}
	// RequireMutation + a real mutation → sound.
	if v := (WorkAudit{RequireMutation: true}).assess("fixed it", mut, false); v != "" {
		t.Fatalf("genuine work should pass; got %q", v)
	}
	// No RequireMutation + zero mutations + honest prose → sound (read-only duty).
	if v := (WorkAudit{}).assess("here is the summary", nil, false); v != "" {
		t.Fatalf("read-only done-claim should pass; got %q", v)
	}
}

func TestRanGate(t *testing.T) {
	exec := func(cmd string, ok bool) tool.Result {
		return tool.Result{Call: tool.Call{Surface: "sys", Action: "exec", Params: map[string]any{"command": cmd}}, OK: ok}
	}
	if !ranGate([]tool.Result{exec("go test ./internal/cost/", true)}) {
		t.Error("a successful `go test` sys.exec should count as a gate run")
	}
	if !ranGate([]tool.Result{exec("gofmt -s -l .", true)}) {
		t.Error("gofmt should count as a gate run")
	}
	if ranGate([]tool.Result{exec("go test ./...", false)}) {
		t.Error("a FAILED sys.exec must not count as a gate run")
	}
	if ranGate([]tool.Result{
		exec("ls -la", true), // non-gate command
		{Call: tool.Call{Surface: "fs", Action: "read"}, OK: true}, // non-sys surface
	}) {
		t.Error("a non-gate command / non-sys surface must not count as a gate run")
	}
}

func TestVerificationFabrication(t *testing.T) {
	mut := []tool.Result{{Call: tool.Call{Surface: "fs", Action: "edit"}, OK: true}}
	gate := tool.Result{Call: tool.Call{Surface: "sys", Action: "exec", Params: map[string]any{"command": "go test ./internal/cost/"}}, OK: true}
	a := WorkAudit{RequireMutation: true}

	// Mutated code + claims tests pass + NO gate run → fabrication (the Run-20 false green).
	if v := a.assess("All tests pass. The issue has been resolved.\n--- PASS: TestX\nok  corpos/internal/cost", mut, false); v == "" {
		t.Error("a verified-claim with no gate run should be flagged as verification-fabrication")
	}
	// Same claim, but a real gate DID run → sound (the legitimately-gated case is exempt).
	if v := a.assess("All tests pass.", append([]tool.Result{gate}, mut...), false); v != "" {
		t.Errorf("a verification claim backed by a real gate run should pass; got %q", v)
	}
	// Mutated code, no verification claim → sound (genuine edit, honest about not verifying).
	if v := a.assess("Edited the file; you should run the tests next.", mut, false); v != "" {
		t.Errorf("a mutation without a verification claim should pass; got %q", v)
	}
	// No RequireMutation → the signal never fires even on a bare verification claim.
	if v := (WorkAudit{}).assess("all tests pass", mut, false); v != "" {
		t.Errorf("verification-fabrication must not fire without RequireMutation; got %q", v)
	}
}

// The self-verify cwd trap fix: when the LOOP owns an authoritative verify gate (loopGated),
// a worker that landed a real edit and optimistically claims the tests pass — WITHOUT running
// a gate itself — must NOT be flagged as verification-fabrication. The loop runs the real gate
// right after, so the backstop is redundant; firing it pre-empts the gate and traps a gated
// coding worker (told by its own prompt not to run the gate) into a Fabricated thrash whose
// correct fix never merges.
func TestVerificationFabrication_SuppressedWhenLoopGated(t *testing.T) {
	mut := []tool.Result{{Call: tool.Call{Surface: "fs", Action: "edit"}, OK: true}}
	a := WorkAudit{RequireMutation: true}
	claim := "All tests pass. The fix returns a + b now."

	// Ungated (a bare worker): the backstop fires — the original Run-20 protection stands.
	if v := a.assess(claim, mut, false); v == "" {
		t.Fatal("ungated: a verified-claim with no gate run must still be flagged")
	}
	// Gated (the loop will run the authoritative gate itself): the backstop is suppressed.
	if v := a.assess(claim, mut, true); v != "" {
		t.Fatalf("gated: the loop's own gate is the real check — the claim must not be flagged; got %q", v)
	}
	// Suppression is NARROW: no-work still fires when gated (a done-claim with zero edits — a
	// vacuous green on unchanged code the gate would pass — must still be caught).
	if v := a.assess("done, all green", nil, true); v == "" {
		t.Fatal("gated: a no-work done-claim (zero mutations) must still be flagged")
	}
	// And prose-narrated tool calls still fire when gated (never a real dispatch).
	if v := a.assess(`{"surface":"fs","action":"edit","params":{}}`, nil, true); v == "" {
		t.Fatal("gated: a prose-narrated tool call must still be flagged")
	}
}

// Loop wiring: a done-claim on a mutation-expecting task that wrote nothing is refused.
// WithFabricationReprompts(0) disables the bug-1078 in-turn re-prompt so this asserts the
// guard's terminal-verdict contract directly (the re-prompt path is covered separately in
// loop_fabrication_reprompt_test.go).
func TestLoopWorkAudit_NoWorkRefused(t *testing.T) {
	m := model.NewEcho("qwen", model.Response{Text: "the regression test passes", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil,
		WithWorkAudit(WorkAudit{RequireMutation: true}), WithFabricationReprompts(0)).
		Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated == "" {
		t.Fatalf("expected a no-work verdict, got none (Text=%q)", res.Text)
	}
}

// Loop wiring: a done-claim narrating a tool-call envelope in prose is refused even when
// the audit does not require mutation. WithFabricationReprompts(0) disables the bug-1078
// in-turn re-prompt so this asserts the guard's terminal-verdict contract directly.
func TestLoopWorkAudit_ProseToolCallRefused(t *testing.T) {
	m := model.NewEcho("qwen", model.Response{
		Text:       `I ran {"surface":"fs","action":"write","params":{"path":"task.go"}} and the test passes.`,
		StopReason: model.StopEndTurn,
	})
	res, err := New(single(m), &fakeProvider{}, nil, WithWorkAudit(WorkAudit{}), WithFabricationReprompts(0)).
		Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated == "" {
		t.Fatal("expected a prose-tool-call fabrication verdict")
	}
}

// Loop wiring: a genuine mutation followed by a clean done-claim is NOT refused.
func TestLoopWorkAudit_GenuineWorkPasses(t *testing.T) {
	m := model.NewEcho("qwen",
		model.Response{
			ToolCalls:  []tool.Call{{ID: "c1", Surface: "fs", Action: "write", Params: map[string]any{"path": "x.go"}}},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "fixed the bug", StopReason: model.StopEndTurn},
	)
	res, err := New(single(m), &fakeProvider{}, nil, WithWorkAudit(WorkAudit{RequireMutation: true})).
		Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("genuine work should not be flagged; got %q", res.Fabricated)
	}
	if res.Text != "fixed the bug" {
		t.Fatalf("Text = %q", res.Text)
	}
}

// Loop wiring: with no audit configured, a no-work done-claim passes through (read-only).
func TestLoopWorkAudit_OffByDefault(t *testing.T) {
	m := model.NewEcho("qwen", model.Response{Text: "summary done", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil).Run(context.Background(), "summarize")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("no audit → no verdict; got %q", res.Fabricated)
	}
}
