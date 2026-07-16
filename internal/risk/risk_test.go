package risk

import (
	"strings"
	"testing"

	"corpos/internal/hooks"
	"corpos/internal/shellsafe"
	"corpos/internal/tool"
)

// TestUnsafeExec_ShellShapeCorpus pins the build-test risk gate to the SAME shared shell-shape
// decision table the sys.exec allowlist asserts (bug 1107): the two must agree on which shapes
// escape the head check. The benign corpus heads (go, echo, diff, safe-git status) are all in
// the gate's tighter auto-approve set, so the shape verdict is what's under test here.
func TestUnsafeExec_ShellShapeCorpus(t *testing.T) {
	for _, c := range shellsafe.Corpus {
		reason := unsafeExecReason(c.Command)
		if (reason != "") != c.Reject {
			t.Errorf("unsafeExecReason(%q) = %q (reject=%v), want reject=%v — %s", c.Command, reason, reason != "", c.Reject, c.Note)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		surface, action string
		wantClass       Class
		wantGated       bool
	}{
		{"sys", "exec", ClassDestructive, true},
		{"sys", "ps", ClassSafe, false},
		{"sys", "containers", ClassSafe, false},
		{"fs", "write", ClassMutating, false}, // classified, but NOT gated (prove-the-failure)
		{"fs", "edit", ClassMutating, false},
		{"fs", "move", ClassMutating, false},     // relocation sits with write/edit: classified, ungated
		{"fs", "remove", ClassDestructive, true}, // deletion is destructive — gated like work.forge_delete
		{"fs", "read", ClassSafe, false},
		{"fs", "grep", ClassSafe, false},
		{"work", "forge_delete", ClassDestructive, true},
		{"work", "task_complete", ClassSafe, false},
		{"work", "forge", ClassSafe, false},
		{"knowledge", "vault_search", ClassSafe, false},
		{"measure", "classify_bug_severity", ClassSafe, false},
	}
	for _, c := range cases {
		v := Classify(tool.Call{Surface: c.surface, Action: c.action})
		if v.Class != c.wantClass || v.Gated != c.wantGated {
			t.Errorf("Classify(%s.%s) = {%s, gated=%v}, want {%s, gated=%v}",
				c.surface, c.action, v.Class, v.Gated, c.wantClass, c.wantGated)
		}
		if v.Gated && v.Reason == "" {
			t.Errorf("Classify(%s.%s) gated but gave no reason", c.surface, c.action)
		}
	}
}

func TestAllowAllPermitsEverything(t *testing.T) {
	t.Parallel()
	ok, _ := AllowAll{}.Approve(tool.Call{Surface: "sys", Action: "exec"}, Verdict{Gated: true})
	if !ok {
		t.Error("AllowAll should permit a gated call")
	}
}

func TestDenyGatedBlocksWithActionableReason(t *testing.T) {
	t.Parallel()
	ok, reason := DenyGated().Approve(
		tool.Call{Surface: "sys", Action: "exec"},
		Verdict{Class: ClassDestructive, Reason: "sys.exec runs an arbitrary (allowlisted) shell command"})
	if ok {
		t.Fatal("DenyGated should block a gated call")
	}
	if reason == "" {
		t.Error("blocked call should carry a reason")
	}
	for _, want := range []string{"sys.exec", "risk-gate=off"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q should mention %q", reason, want)
		}
	}
}

func TestConfirmFunc(t *testing.T) {
	t.Parallel()
	allow := ConfirmFunc(func(tool.Call, Verdict) bool { return true })
	if ok, _ := allow.Approve(tool.Call{}, Verdict{}); !ok {
		t.Error("ConfirmFunc returning true should permit")
	}
	deny := ConfirmFunc(func(tool.Call, Verdict) bool { return false })
	if ok, reason := deny.Approve(tool.Call{Surface: "sys", Action: "exec"}, Verdict{Class: ClassDestructive}); ok || reason == "" {
		t.Errorf("ConfirmFunc returning false should block with a reason; got ok=%v reason=%q", ok, reason)
	}
}

func TestGuard_VetoesGatedCall(t *testing.T) {
	t.Parallel()
	guard := Guard(DenyGated())
	c := &hooks.Context{ToolCall: &tool.Call{Surface: "sys", Action: "exec"}}
	guard(c)
	if !c.DenyToolCall {
		t.Fatal("guard should veto a gated sys.exec under DenyGated")
	}
	if c.DenyReason == "" {
		t.Error("vetoed call should carry a reason")
	}
}

func TestGuard_AllowsSafeAndOptOut(t *testing.T) {
	t.Parallel()
	// Safe call under the fail-closed gate: not vetoed.
	c := &hooks.Context{ToolCall: &tool.Call{Surface: "fs", Action: "read"}}
	Guard(DenyGated())(c)
	if c.DenyToolCall {
		t.Error("safe fs.read must not be vetoed")
	}
	// fs.write is classified mutating but NOT gated — even under DenyGated it
	// passes (prove-the-failure posture; we don't pre-bar low-tier writes).
	c = &hooks.Context{ToolCall: &tool.Call{Surface: "fs", Action: "write"}}
	Guard(DenyGated())(c)
	if c.DenyToolCall {
		t.Error("fs.write must not be vetoed by default (mutation is ungated)")
	}
	// fs.move is classified mutating but NOT gated — passes under DenyGated.
	c = &hooks.Context{ToolCall: &tool.Call{Surface: "fs", Action: "move"}}
	Guard(DenyGated())(c)
	if c.DenyToolCall {
		t.Error("fs.move must not be vetoed by default (mutation is ungated)")
	}
	// fs.remove IS destructive + gated — vetoed under the fail-closed gate.
	c = &hooks.Context{ToolCall: &tool.Call{Surface: "fs", Action: "remove"}}
	Guard(DenyGated())(c)
	if !c.DenyToolCall {
		t.Error("fs.remove must be vetoed under DenyGated (destructive + gated)")
	}
	// Gated call under AllowAll: not vetoed.
	c = &hooks.Context{ToolCall: &tool.Call{Surface: "sys", Action: "exec"}}
	Guard(AllowAll{})(c)
	if c.DenyToolCall {
		t.Error("AllowAll should not veto even a gated call")
	}
	// Nil tool call: no-op.
	c = &hooks.Context{}
	Guard(DenyGated())(c)
	if c.DenyToolCall {
		t.Error("nil ToolCall should be a no-op")
	}
}
