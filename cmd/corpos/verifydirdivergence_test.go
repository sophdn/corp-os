package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyDirDivergent pins the startup guard for bug
// corpos-fs-cwd-vs-verify-dir-silent-nonconvergence: only trees that are genuinely
// disjoint (neither contains the other) are divergent, so the default single-repo
// ergonomics and the shipped verify-dir-under-CWD demos still start.
func TestVerifyDirDivergent(t *testing.T) {
	root := t.TempDir()

	mkdir := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	repo := mkdir("repo")          // the common single-repo case
	sub := mkdir("repo", "calc")   // a subdir of repo (the demo shape: verify-dir under CWD)
	sibling := mkdir("scenario")   // a disjoint sibling tree (the repro shape)
	repoNeighbor := mkdir("repoX") // shares a name prefix with repo but is NOT under it

	cases := []struct {
		name          string
		cwd           string
		verifyDir     string
		wantDivergent bool
	}{
		{"equal (default single repo)", repo, repo, false},
		{"verify-dir under cwd (demo)", repo, sub, false},
		{"cwd under verify-dir", sub, repo, false},
		{"disjoint siblings (repro)", repo, sibling, true},
		{"name-prefix sibling is not a child", repo, repoNeighbor, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			div, rc, rv := verifyDirDivergent(tc.cwd, tc.verifyDir)
			if div != tc.wantDivergent {
				t.Fatalf("verifyDirDivergent(%q, %q) divergent=%v, want %v", tc.cwd, tc.verifyDir, div, tc.wantDivergent)
			}
			// The returned paths are always resolved (absolute, symlink-evaluated).
			if rc == "" || rv == "" {
				t.Fatalf("expected resolved paths, got cwd=%q verify=%q", rc, rv)
			}
			if wantCWD := resolvePath(tc.cwd); rc != wantCWD {
				t.Fatalf("resolvedCWD = %q, want %q", rc, wantCWD)
			}
		})
	}

	// A relative verify-dir is resolved against the process CWD before the check, so a
	// relative path that names the same tree is NOT flagged divergent.
	t.Run("relative verify-dir resolves against process cwd", func(t *testing.T) {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if div, _, _ := verifyDirDivergent(wd, "."); div {
			t.Fatal(`verifyDirDivergent(wd, ".") = divergent, want not divergent`)
		}
	})
}

// TestVerifyDirDivergentResolvesSymlinks ensures a symlinked verify-dir pointing into
// the CWD tree is not misread as disjoint (the guard must not fire on /tmp-vs-symlink
// aliasing).
func TestVerifyDirDivergentResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "repo")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "repo-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// CWD is the real dir; verify-dir is a symlink to it — same tree, must not diverge.
	if div, _, _ := verifyDirDivergent(real, link); div {
		t.Fatal("symlink alias of the same tree read as divergent")
	}
}

// TestPathContains covers the containment primitive directly, including the
// shared-name-prefix trap filepath.Rel guards against.
func TestPathContains(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/a/b", "/a/b", true},    // equal
		{"/a/b", "/a/b/c", true},  // child under parent
		{"/a/b/c", "/a/b", false}, // parent under child
		{"/a/b", "/a/c", false},   // siblings
		{"/a/b", "/a/bc", false},  // shared name prefix, NOT a child
		{"/a", "/a/b/c/d", true},  // deep descendant
	}
	for _, tc := range cases {
		if got := pathContains(tc.parent, tc.child); got != tc.want {
			t.Errorf("pathContains(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}
