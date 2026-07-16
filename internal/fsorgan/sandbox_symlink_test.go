package fsorgan

import (
	"os"
	"path/filepath"
	"testing"
)

// Bug 1106: the sandbox confined paths lexically (filepath.Rel + ".." check) with no
// EvalSymlinks, so a symlink created INSIDE the worktree pointing at an external
// target passed containment and fs.* followed it out. resolveWithin must resolve
// symlinks on the existing path prefix and reject a real path that lands outside the
// (symlink-resolved) root, while still allowing same-worktree symlinks and new files.

func TestResolveWithin_SymlinkEscapingWorktreeRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := resolveWithin(root, "escape/secret"); err == nil {
		t.Fatal("a path through a symlink that escapes the worktree must be rejected")
	}
	// The symlink itself (pointing outside) must also be rejected as a target.
	if _, err := resolveWithin(root, "escape"); err == nil {
		t.Fatal("a symlink whose target is outside the worktree must be rejected")
	}
}

func TestResolveWithin_SameWorktreeSymlinkAllowed(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "f.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// A symlink whose target stays inside the worktree is legitimate and must resolve.
	if _, err := resolveWithin(root, "alias/f.txt"); err != nil {
		t.Fatalf("a same-worktree symlink must be allowed: %v", err)
	}
}

func TestResolveWithin_NewFileUnderRootAllowed(t *testing.T) {
	root := t.TempDir()
	// A not-yet-created file (and a not-yet-created parent dir) under root must resolve
	// — confinement checks the existing prefix, not the leaf.
	if _, err := resolveWithin(root, "newdir/new.txt"); err != nil {
		t.Fatalf("a new path under root must be allowed: %v", err)
	}
}
