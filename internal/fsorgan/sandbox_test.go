package fsorgan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// dispatchIn issues an fs action under a sandbox root (empty root = unsandboxed).
func dispatchIn(p *Provider, root, action string, params map[string]any) tool.Result {
	ctx := WithRoot(context.Background(), root)
	return p.Dispatch(ctx, tool.Call{Surface: Surface, Action: action, Params: params})
}

func TestWithRoot_EmptyIsNoOp(t *testing.T) {
	base := context.Background()
	if got := WithRoot(base, ""); got != base {
		t.Fatal("WithRoot with empty root should return the same context")
	}
	if got := WithRoot(base, "   "); got != base {
		t.Fatal("WithRoot with whitespace root should return the same context")
	}
	if r := rootFromContext(base); r != "" {
		t.Fatalf("rootFromContext on a bare context = %q, want empty", r)
	}
	//nolint:staticcheck // explicitly exercising the nil-context guard
	if r := rootFromContext(nil); r != "" {
		t.Fatalf("rootFromContext(nil) = %q, want empty", r)
	}
}

func TestWithRoot_RoundTrips(t *testing.T) {
	ctx := WithRoot(context.Background(), "/work/tree")
	if r := rootFromContext(ctx); r != "/work/tree" {
		t.Fatalf("rootFromContext = %q, want /work/tree", r)
	}
}

func TestResolveWithin_NoRootPassthrough(t *testing.T) {
	// With no root the expanded path is returned unchanged (CWD-relative, the
	// pre-sandbox behavior). A relative path stays relative.
	got, err := resolveWithin("", "some/rel/path.go")
	if err != nil || got != "some/rel/path.go" {
		t.Fatalf("resolveWithin no-root = %q err=%v, want passthrough", got, err)
	}
}

func TestResolveWithin_RelativeJoinedUnderRoot(t *testing.T) {
	root := t.TempDir()
	got, err := resolveWithin(root, "internal/intmath/gcd.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(root, "internal/intmath/gcd.go")
	if got != want {
		t.Fatalf("resolveWithin = %q, want %q", got, want)
	}
}

func TestResolveWithin_AbsoluteUnderRootKept(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "pkg/x.go")
	got, err := resolveWithin(root, abs)
	if err != nil || got != filepath.Clean(abs) {
		t.Fatalf("resolveWithin abs-under-root = %q err=%v, want %q", got, err, abs)
	}
}

func TestResolveWithin_RejectsRelativeEscape(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../escape.go", "a/../../escape.go", ".."} {
		if _, err := resolveWithin(root, p); err == nil {
			t.Fatalf("resolveWithin(%q) should have rejected the escape", p)
		}
	}
}

func TestResolveWithin_RejectsAbsoluteOutside(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"/etc/passwd", filepath.Dir(root)} {
		if _, err := resolveWithin(root, p); err == nil {
			t.Fatalf("resolveWithin(%q) should have rejected an absolute-outside path", p)
		}
	}
}

func TestResolveWithin_RootItselfAllowed(t *testing.T) {
	root := t.TempDir()
	got, err := resolveWithin(root, ".")
	if err != nil || got != filepath.Clean(root) {
		t.Fatalf("resolveWithin(root, \".\") = %q err=%v, want %q", got, err, root)
	}
}

func TestResolveWithin_TildeEscapesRoot(t *testing.T) {
	// A "~/x" expands to an absolute home path — outside any worktree root, so the
	// sandbox rejects it (a worker cannot reach $HOME through a tilde).
	if home, err := os.UserHomeDir(); err != nil || home == "" {
		t.Skip("no home dir to exercise tilde expansion")
	}
	root := t.TempDir()
	if _, err := resolveWithin(root, "~/secret"); err == nil {
		t.Fatal("resolveWithin should reject a tilde path that escapes the root")
	}
}

// TestSandbox_RelativeWriteStaysInRoot is the bug-1081 regression: a worker that
// emits a RELATIVE path must write UNDER its worktree, never into the process CWD.
func TestSandbox_RelativeWriteStaysInRoot(t *testing.T) {
	root := t.TempDir()
	rel := "internal/intmath/gcd.go"
	r := dispatchIn(New(), root, "write", map[string]any{"file_path": rel, "content": "package intmath\n"})
	m := mustValue(t, r)
	landed := filepath.Join(root, rel)
	if got := m["file_path"].(string); got != landed {
		t.Fatalf("write landed at %q, want %q", got, landed)
	}
	if _, err := os.Stat(landed); err != nil {
		t.Fatalf("file should exist under the worktree root: %v", err)
	}
	// And it must NOT have escaped to the process CWD.
	if _, err := os.Stat(rel); err == nil {
		_ = os.RemoveAll("internal") // defensive cleanup if the guard ever regresses
		t.Fatal("relative write escaped the sandbox into the process CWD")
	}
}

func TestSandbox_RejectsEscapingWrite(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../escape.go", "/tmp/corpos-escape-should-not-write.go"} {
		r := dispatchIn(New(), root, "write", map[string]any{"file_path": p, "content": "x"})
		if r.OK {
			t.Fatalf("write to %q should have been rejected", p)
		}
		msg := r.Value.(map[string]any)["error"].(string)
		if !strings.Contains(msg, "escapes the worktree sandbox") {
			t.Fatalf("error for %q = %q, want sandbox-escape message", p, msg)
		}
	}
	// Nothing leaked to the absolute target.
	if _, err := os.Stat("/tmp/corpos-escape-should-not-write.go"); err == nil {
		_ = os.Remove("/tmp/corpos-escape-should-not-write.go")
		t.Fatal("absolute escaping write must not land")
	}
}

func TestSandbox_ReadEditRemoveMoveUnderRoot(t *testing.T) {
	root := t.TempDir()
	p := New()
	// write → read → edit → move → remove, all by relative path under the root.
	if r := dispatchIn(p, root, "write", map[string]any{"file_path": "a.txt", "content": "one\n"}); !r.OK {
		t.Fatalf("write: %v", r.Value)
	}
	if r := dispatchIn(p, root, "read", map[string]any{"file_path": "a.txt"}); !r.OK {
		t.Fatalf("read: %v", r.Value)
	}
	if r := dispatchIn(p, root, "edit", map[string]any{"file_path": "a.txt", "old_string": "one", "new_string": "two"}); !r.OK {
		t.Fatalf("edit: %v", r.Value)
	}
	if r := dispatchIn(p, root, "move", map[string]any{"source": "a.txt", "dest": "b.txt"}); !r.OK {
		t.Fatalf("move: %v", r.Value)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Fatalf("moved file should exist under root: %v", err)
	}
	if r := dispatchIn(p, root, "remove", map[string]any{"file_path": "b.txt"}); !r.OK {
		t.Fatalf("remove: %v", r.Value)
	}
}

func TestSandbox_ReadRejectsEscape(t *testing.T) {
	root := t.TempDir()
	r := dispatchIn(New(), root, "read", map[string]any{"file_path": "../../etc/passwd"})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "escapes the worktree sandbox") {
		t.Fatalf("read escape not rejected: ok=%v val=%v", r.OK, r.Value)
	}
}

func TestSandbox_MutatorsRejectEscape(t *testing.T) {
	root := t.TempDir()
	p := New()
	cases := []struct {
		action string
		params map[string]any
	}{
		{"edit", map[string]any{"file_path": "../x.go", "old_string": "a", "new_string": "b"}},
		{"remove", map[string]any{"file_path": "../x.go"}},
		{"move", map[string]any{"source": "../x.go", "dest": "y.go"}},
		{"move", map[string]any{"source": "y.go", "dest": "../x.go"}},
	}
	for _, c := range cases {
		r := dispatchIn(p, root, c.action, c.params)
		if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "escapes the worktree sandbox") {
			t.Fatalf("%s with escaping path not rejected: ok=%v val=%v", c.action, r.OK, r.Value)
		}
	}
}

func TestSandbox_DirActionsDefaultToRoot(t *testing.T) {
	root := t.TempDir()
	p := New()
	if r := dispatchIn(p, root, "write", map[string]any{"file_path": "sub/x.go", "content": "package sub\nfunc F() {}\n"}); !r.OK {
		t.Fatalf("seed write: %v", r.Value)
	}
	// ls with no path defaults to the sandbox root (not the process CWD).
	ls := mustValue(t, dispatchIn(p, root, "ls", map[string]any{}))
	if ls["path"].(string) != filepath.Clean(root) {
		t.Fatalf("ls default path = %v, want root %q", ls["path"], root)
	}
	// glob with no path walks the root.
	g := mustValue(t, dispatchIn(p, root, "glob", map[string]any{"pattern": "**/*.go"}))
	if g["num_files"].(float64) != 1 {
		t.Fatalf("glob found %v files, want 1", g["num_files"])
	}
	// grep with no path searches the root.
	gr := mustValue(t, dispatchIn(p, root, "grep", map[string]any{"pattern": "func F", "output_mode": "files_with_matches"}))
	if gr["num_files"].(float64) != 1 {
		t.Fatalf("grep matched %v files, want 1", gr["num_files"])
	}
}

func TestSandbox_DirActionsRejectEscape(t *testing.T) {
	root := t.TempDir()
	p := New()
	for _, action := range []string{"ls", "glob", "grep"} {
		params := map[string]any{"path": "../.."}
		switch action {
		case "glob":
			params["pattern"] = "*"
		case "grep":
			params["pattern"] = "x"
		}
		r := dispatchIn(p, root, action, params)
		if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "escapes the worktree sandbox") {
			t.Fatalf("%s with escaping path not rejected: ok=%v val=%v", action, r.OK, r.Value)
		}
	}
}
