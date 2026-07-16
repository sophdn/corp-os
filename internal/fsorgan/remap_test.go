package fsorgan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// seedFile creates dir/rel (with parents) holding content, returning its abs path.
func seedFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRecoverWorktreePath_UniqueTail(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "go/internal/fs/read.go", "package fs\n")
	want := filepath.Join("go", "internal", "fs", "read.go")
	// An absolute source path AND the slash-dropped form both recover to the same tail.
	for _, in := range []string{
		"/tmp/proj/go/internal/fs/read.go",
		"tmp/proj/go/internal/fs/read.go",
	} {
		got, ok := recoverWorktreePath(root, in)
		if !ok || got != want {
			t.Fatalf("recover(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
}

func TestRecoverWorktreePath_AmbiguousRefuses(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "go/util/read.go", "x")
	seedFile(t, root, "util/read.go", "x")
	// Path "/x/go/util/read.go": tails go/util/read.go AND util/read.go both exist → refuse.
	if _, ok := recoverWorktreePath(root, "/x/go/util/read.go"); ok {
		t.Fatal("two existing tails must refuse to guess")
	}
}

func TestRecoverWorktreePath_NoMatchAndGuards(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "go/read.go", "x")
	if _, ok := recoverWorktreePath(root, "/a/b/absent.go"); ok {
		t.Fatal("no existing tail must return false")
	}
	if _, ok := recoverWorktreePath(root, "read.go"); ok {
		t.Fatal("a bare filename is too generic — must return false")
	}
	if _, ok := recoverWorktreePath("", "go/read.go"); ok {
		t.Fatal("empty root must return false")
	}
}

func TestRemapWorktreePaths_RewritesDoubledPath(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "go/internal/fs/read.go", "package fs\n")
	orig := map[string]any{"file_path": "tmp/proj/go/internal/fs/read.go", "old_string": "x"}
	out := remapWorktreePaths(root, orig)
	if out["file_path"] != filepath.Join("go", "internal", "fs", "read.go") {
		t.Fatalf("file_path not remapped: %v", out["file_path"])
	}
	// The caller's map must not be mutated (copy-on-write).
	if orig["file_path"] != "tmp/proj/go/internal/fs/read.go" {
		t.Fatalf("caller map was mutated: %v", orig["file_path"])
	}
	// Non-path params carry through the copy.
	if out["old_string"] != "x" {
		t.Fatalf("non-path param lost: %v", out["old_string"])
	}
}

func TestRemapWorktreePaths_LeavesCorrectAndNewPaths(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "go/read.go", "x")
	// A correct existing path is left untouched (same map identity — no copy).
	good := map[string]any{"file_path": "go/read.go"}
	if out := remapWorktreePaths(root, good); out["file_path"] != "go/read.go" {
		t.Fatalf("correct path was altered: %v", out["file_path"])
	}
	// A genuinely-new file (no existing tail) is left as-is so creation targets it.
	newf := map[string]any{"file_path": "go/brand_new.go"}
	if out := remapWorktreePaths(root, newf); out["file_path"] != "go/brand_new.go" {
		t.Fatalf("new-file path was altered: %v", out["file_path"])
	}
	// Unsandboxed (root == "") is a no-op.
	if out := remapWorktreePaths("", newf); out["file_path"] != "go/brand_new.go" {
		t.Fatalf("unsandboxed remap should be a no-op")
	}
}

// TestDispatch_RemapsAbsoluteSourcePathEndToEnd is the real fix: a worker rooted at its
// worktree that names a file by its absolute SOURCE-repo path (the bug 1020 doubled-path
// failure) now reads and edits it, because Dispatch remaps the path to the worktree tail.
func TestDispatch_RemapsAbsoluteSourcePathEndToEnd(t *testing.T) {
	root := t.TempDir()
	seedFile(t, root, "go/internal/fs/read.go", "package fs\nconst answer = 41\n")
	ctx := WithRoot(context.Background(), root)
	abs := "/tmp/rehearsal/go/internal/fs/read.go" // absolute source path the worker used

	// Same provider instance so the whole-file read's state carries to the edit.
	p := New()
	if rr := p.Dispatch(ctx, tool.Call{Surface: Surface, Action: "read", Params: map[string]any{"file_path": abs}}); !rr.OK {
		t.Fatalf("read via absolute source path should succeed after remap: %v", rr.Value)
	}
	ed := p.Dispatch(ctx, tool.Call{Surface: Surface, Action: "edit", Params: map[string]any{
		"file_path": abs, "old_string": "answer = 41", "new_string": "answer = 42",
	}})
	if !ed.OK {
		t.Fatalf("edit via absolute source path should land after remap: %v", ed.Value)
	}
	got, _ := os.ReadFile(filepath.Join(root, "go/internal/fs/read.go"))
	if !strings.Contains(string(got), "answer = 42") {
		t.Fatalf("edit did not land; file = %q", got)
	}
}
