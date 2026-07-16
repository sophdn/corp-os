package gitenv

import (
	"strings"
	"testing"
)

func TestClean_StripsWorktreeBindingVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/some/.git")
	t.Setenv("GIT_WORK_TREE", "/some")
	t.Setenv("GIT_INDEX_FILE", "/some/.git/index")
	t.Setenv("GIT_COMMON_DIR", "/some/.git")
	t.Setenv("GIT_PREFIX", "sub/")
	t.Setenv("CORPOS_KEEP_ME", "yes")

	out := Clean()
	for _, kv := range out {
		for _, p := range ContextVars {
			if strings.HasPrefix(kv, p) {
				t.Errorf("Clean left a worktree-binding var: %q", kv)
			}
		}
	}
	// A non-git var must survive.
	kept := false
	for _, kv := range out {
		if kv == "CORPOS_KEEP_ME=yes" {
			kept = true
		}
	}
	if !kept {
		t.Error("Clean dropped a non-git var")
	}
}

func TestClean_NoGitVars(t *testing.T) {
	// With no git vars set, Clean is a pass-through that preserves a normal var.
	t.Setenv("CORPOS_ONLY", "1")
	out := Clean()
	found := false
	for _, kv := range out {
		if kv == "CORPOS_ONLY=1" {
			found = true
		}
	}
	if !found {
		t.Error("Clean should preserve non-git environment")
	}
}

func TestHasAnyPrefix(t *testing.T) {
	if !hasAnyPrefix("GIT_DIR=x", ContextVars) {
		t.Error("expected a GIT_DIR= match")
	}
	if hasAnyPrefix("PATH=/usr/bin", ContextVars) {
		t.Error("PATH must not match a git context prefix")
	}
	if hasAnyPrefix("anything", nil) {
		t.Error("an empty prefix set must never match")
	}
}
