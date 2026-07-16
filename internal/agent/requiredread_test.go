package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// failReadProvider fails an fs.read of failPath (the run-5 shape: the required contract path is
// not in the container's mounts) and succeeds everything else.
type failReadProvider struct {
	failPath string
	reads    int
}

func (f *failReadProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	if c.Surface == "fs" && c.Action == "read" && fsCallPath(c) == f.failPath {
		f.reads++
		return tool.Result{Call: c, OK: false, ErrorClass: tool.ClassUsage,
			Value: map[string]any{"error": "path not in mounts"}}
	}
	return tool.Result{Call: c, OK: true, Value: map[string]any{"ok": true}}
}

// TestRequiredReadsAssess covers the pure verdict logic across the equivalence classes.
func TestRequiredReadsAssess(t *testing.T) {
	const p = "/repo/eval_test.go"
	failed := tool.Result{Call: readCall(p), OK: false}
	okRead := tool.Result{Call: readCall(p), OK: true}
	otherRead := tool.Result{Call: readCall("/repo/other.go"), OK: true}

	// Required read FAILED and never satisfied → refused, names the failed shape.
	if v := (RequiredReads{Paths: []string{p}}).assess([]tool.Result{failed}); v == "" ||
		!strings.Contains(v, "required-read-failed") || !strings.Contains(v, p) {
		t.Fatalf("a failed required read should be refused as required-read-failed; got %q", v)
	}
	// Required read NEVER attempted → refused as unsatisfied.
	if v := (RequiredReads{Paths: []string{p}}).assess([]tool.Result{otherRead}); v == "" ||
		!strings.Contains(v, "required-read-unsatisfied") {
		t.Fatalf("a never-attempted required read should be refused as required-read-unsatisfied; got %q", v)
	}
	// Required read succeeded (even after an earlier failure) → sound.
	if v := (RequiredReads{Paths: []string{p}}).assess([]tool.Result{failed, okRead}); v != "" {
		t.Fatalf("a satisfied required read should pass; got %q", v)
	}
	// No required paths declared → off (always sound).
	if v := (RequiredReads{}).assess([]tool.Result{failed}); v != "" {
		t.Fatalf("no declared required reads → no verdict; got %q", v)
	}
	// A WRITE to the required path does not satisfy the READ requirement.
	w := tool.Result{Call: tool.Call{Surface: "fs", Action: "write", Params: map[string]any{"file_path": p}}, OK: true}
	if v := (RequiredReads{Paths: []string{p}}).assess([]tool.Result{w}); v == "" {
		t.Fatal("a write to the required path must not satisfy the read requirement")
	}
}

// TestLoopRequiredRead_FailedReadRefusesFabricatedDone is the bug-1033 regression: a worker
// whose REQUIRED contract read fails, then claims done on an invented contract, must NOT
// register a clean success — the loop surfaces the required-read failure instead.
func TestLoopRequiredRead_FailedReadRefusesFabricatedDone(t *testing.T) {
	const contract = "/repo/eval_test.go"
	// Turn 1: try to read the contract (it will FAIL via the provider). Turn 2: claim done
	// on a fabricated contract (the run-5 false green).
	m := model.NewEcho("qwen",
		model.Response{
			ToolCalls:  []tool.Call{readCall(contract)},
			StopReason: model.StopToolUse,
		},
		model.Response{
			Text:       "I implemented Eval(string) (string, error) returning \"Evaluated: %s\"; the tests should pass, run them manually.",
			StopReason: model.StopEndTurn,
		},
	)
	res, err := New(single(m), &failReadProvider{failPath: contract}, nil,
		WithRequiredReads(RequiredReads{Paths: []string{contract}})).
		Run(context.Background(), "implement Eval; read eval_test.go first for the exact API")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated == "" {
		t.Fatalf("expected a required-read-failed verdict refusing the fabricated done, got none (Text=%q)", res.Text)
	}
	if !strings.Contains(res.Fabricated, "required-read-failed") {
		t.Fatalf("verdict should name the failed required read; got %q", res.Fabricated)
	}
}

// TestLoopRequiredRead_SatisfiedReadPasses: when the required read SUCCEEDS, a clean done is
// not refused by this guard (the guard only fires on an unsatisfied required read).
func TestLoopRequiredRead_SatisfiedReadPasses(t *testing.T) {
	const contract = "/repo/eval_test.go"
	m := model.NewEcho("qwen",
		model.Response{
			ToolCalls:  []tool.Call{readCall(contract)},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "implemented to the read contract", StopReason: model.StopEndTurn},
	)
	// fakeProvider succeeds every dispatch, so the required read is satisfied.
	res, err := New(single(m), &fakeProvider{}, nil,
		WithRequiredReads(RequiredReads{Paths: []string{contract}})).
		Run(context.Background(), "implement Eval; read eval_test.go first")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("a satisfied required read should not be flagged; got %q", res.Fabricated)
	}
}

// TestLoopRequiredRead_OffByDefault: with no required reads wired, a done-claim passes through.
func TestLoopRequiredRead_OffByDefault(t *testing.T) {
	m := model.NewEcho("qwen", model.Response{Text: "done", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil).Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("no required-read guard → no verdict; got %q", res.Fabricated)
	}
}
