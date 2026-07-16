package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/tool"
)

// TestProtectPathDenialIsNonEscalatableUsageClass is the bug 1095 regression (worker side):
// a protect-path denial — a worker trying to write a *_test.go on a test-protecting profile —
// must be ClassUsage, NOT ClassTool, so it does not feed the repeated_tool_error escalation.
// No stronger model can lift a protect-path denial; classifying it ClassTool is what thrashed
// a denied worker up all four tiers to Opus in the decompose-admin rehearsal.
func TestProtectPathDenialIsNonEscalatableUsageClass(t *testing.T) {
	m := model.NewEcho("worker",
		model.Response{
			ToolCalls: []tool.Call{{ID: "c", Surface: "fs", Action: "write",
				Params: map[string]any{"path": "internal/x/x_test.go", "content": "hack"}}},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "ok, adapting", StopReason: model.StopEndTurn},
	)
	fp := &fakeProvider{}
	h := hooks.NewSurface()
	if err := h.Register(hooks.PreToolUse, "protect", protectPathGuard([]string{"**/*_test.go"})); err != nil {
		t.Fatalf("register hook: %v", err)
	}
	res, err := New(single(m), fp, nil, WithHooks(h)).Run(context.Background(), "improve the test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fp.calls != 0 {
		t.Errorf("a protected write must never reach the provider (%d dispatched)", fp.calls)
	}
	if len(res.Dispatches) != 1 || res.Dispatches[0].OK {
		t.Fatalf("expected one denied dispatch, got %+v", res.Dispatches)
	}
	if res.Dispatches[0].ErrorClass != tool.ClassUsage {
		t.Errorf("protect-path denial class = %q, want usage_error (non-escalatable)", res.Dispatches[0].ErrorClass)
	}
	// The escalation source must not see it: ToolErrors stays 0, UsageErrors counts it.
	if tally := tool.Tally(res.Dispatches); tally.ToolErrors != 0 || tally.UsageErrors != 1 {
		t.Errorf("a protect-path denial must not count toward repeated_tool_error; got ToolErrors=%d UsageErrors=%d", tally.ToolErrors, tally.UsageErrors)
	}
}

// TestVerifyGateRunnableUnrunnableWrapsTypedError asserts the spawn-side classification seam:
// a non-runnable go gate (module-less working dir) returns an error wrapping
// ErrVerifyGateUnrunnable, so the orchestrator boundary can classify it ClassUsage via
// errors.Is (bug 1095). A runnable gate (module at root) returns nil — behavior-preserving.
func TestVerifyGateRunnableUnrunnableWrapsTypedError(t *testing.T) {
	moduleless := t.TempDir() // no go.mod at/above or in the subtree
	err := VerifyGateRunnable([]string{"go", "build", "./..."}, moduleless)
	if err == nil {
		t.Fatal("expected an unrunnable-gate error for a module-less dir")
	}
	if !errors.Is(err, ErrVerifyGateUnrunnable) {
		t.Errorf("unrunnable-gate error must wrap ErrVerifyGateUnrunnable (for ClassUsage), got %v", err)
	}

	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := VerifyGateRunnable([]string{"go", "build", "./..."}, mod); err != nil {
		t.Errorf("a module-at-root dir must be runnable, got %v", err)
	}
}
