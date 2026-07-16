package mcp

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/tool"
)

func TestScope_Allows(t *testing.T) {
	t.Parallel()
	scope := Scope{
		"fs":   {"read", "write", "edit", "grep", "glob", "ls"},
		"sys":  {"exec"},
		"work": {}, // whole-surface grant
	}
	cases := []struct {
		surface, action string
		want            bool
	}{
		{"fs", "read", true},
		{"fs", "remove", false}, // granted surface, ungranted action
		{"sys", "exec", true},
		{"sys", "ps", false},             // granted surface, ungranted action
		{"work", "task_stamp_sha", true}, // whole-surface grant allows any action
		{"work", "forge_delete", true},
		{"knowledge", "vault_read", false}, // surface absent → denied
	}
	for _, c := range cases {
		if got := scope.Allows(c.surface, c.action); got != c.want {
			t.Errorf("Allows(%q,%q)=%v want %v", c.surface, c.action, got, c.want)
		}
	}
}

func TestScope_Allows_NilAndEmpty(t *testing.T) {
	t.Parallel()
	var nilScope Scope
	if nilScope.Allows("fs", "read") {
		t.Error("nil scope must deny everything")
	}
	if (Scope{}).Allows("fs", "read") {
		t.Error("empty scope must deny everything")
	}
}

// TestScoped_DeniesUngrantedSurface is the bug-1044 regression: a profile-scoped
// run must NOT be able to dispatch a surface its profile did not grant — the call
// is denied at the dispatch boundary, never reaches the inner provider, and comes
// back as an adaptable tool_error.
func TestScoped_DeniesUngrantedSurface(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{label: "inner"}
	// The run-6d coding profile: fs + sys only, no work surface.
	scoped := NewScoped(inner, Scope{
		"fs":  {"read", "write", "edit", "grep", "glob", "ls"},
		"sys": {"exec"},
	})

	// The exact unscoped WRITE run-6d emitted against the live ledger.
	res := scoped.Dispatch(context.Background(), tool.Call{
		Surface: "work", Action: "task_stamp_sha",
		Params: map[string]any{"id": 2905, "commit_sha": "deadbeef"},
	})

	if res.OK {
		t.Fatal("ungranted work.task_stamp_sha must be denied, got OK")
	}
	if res.ErrorClass != tool.ClassTool {
		t.Errorf("denial class = %q, want %q (an adaptable tool_error)", res.ErrorClass, tool.ClassTool)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("denied call must NOT reach the inner provider; inner saw %d calls", len(inner.calls))
	}
	msg := errMsg(t, res)
	if !strings.Contains(msg, "work") || !strings.Contains(msg, "profile scope") {
		t.Errorf("denial message %q should name the surface and that it's out of scope", msg)
	}
	// The message lists the surfaces in scope so the model can recover.
	if !strings.Contains(msg, "fs") || !strings.Contains(msg, "sys") {
		t.Errorf("denial message %q should list the in-scope surfaces", msg)
	}
}

// TestScoped_DeniesUngrantedAction checks action-level enforcement: a surface in
// scope for some actions still denies an action it did not grant, with a message
// distinct from the whole-surface denial.
func TestScoped_DeniesUngrantedAction(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{label: "inner"}
	scoped := NewScoped(inner, Scope{"fs": {"read", "grep"}})

	res := scoped.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "remove"})
	if res.OK {
		t.Fatal("ungranted fs.remove must be denied")
	}
	if len(inner.calls) != 0 {
		t.Fatalf("denied action must NOT reach the inner provider; inner saw %d calls", len(inner.calls))
	}
	msg := errMsg(t, res)
	if !strings.Contains(msg, "remove") || !strings.Contains(msg, "fs") {
		t.Errorf("action denial message %q should name the action and surface", msg)
	}
}

// TestScoped_AllowsGrantedCall confirms a granted call passes through unchanged to
// the inner provider (the decorator is transparent in the allow case).
func TestScoped_AllowsGrantedCall(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{label: "inner"}
	scoped := NewScoped(inner, Scope{
		"fs":   {"read", "grep"},
		"work": {}, // whole-surface
	})

	if res := scoped.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "read"}); !res.OK {
		t.Fatalf("granted fs.read must pass through, got error: %v", res.Value)
	}
	// Whole-surface grant lets any action through.
	if res := scoped.Dispatch(context.Background(), tool.Call{Surface: "work", Action: "task_start"}); !res.OK {
		t.Fatalf("whole-surface work grant must allow work.task_start, got error: %v", res.Value)
	}
	if len(inner.calls) != 2 {
		t.Fatalf("both granted calls must reach the inner provider; inner saw %d", len(inner.calls))
	}
}

// errMsg pulls the error string out of a failed Result's {"error": …} body.
func errMsg(t *testing.T, res tool.Result) string {
	t.Helper()
	m, ok := res.Value.(map[string]any)
	if !ok {
		t.Fatalf("result Value is not a map: %#v", res.Value)
	}
	s, ok := m["error"].(string)
	if !ok {
		t.Fatalf("result Value has no string error: %#v", res.Value)
	}
	return s
}
