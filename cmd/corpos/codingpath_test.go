package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitRepoRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "internal", "cost")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks: t.TempDir may sit under a symlinked /tmp (macOS, some Linux),
	// so compare resolved paths.
	wantRoot, _ := filepath.EvalSymlinks(root)

	if got, _ := filepath.EvalSymlinks(gitRepoRoot(nested)); got != wantRoot {
		t.Errorf("gitRepoRoot(nested) = %q, want repo root %q", got, wantRoot)
	}
	if got, _ := filepath.EvalSymlinks(gitRepoRoot(root)); got != wantRoot {
		t.Errorf("gitRepoRoot(root) = %q, want %q", got, wantRoot)
	}
	if got := gitRepoRoot(""); got != "" {
		t.Errorf("gitRepoRoot(\"\") = %q, want empty", got)
	}
	// A directory with no .git ancestor (the temp root's parent chain has none of
	// our marker) resolves to "" — use a sibling tree without a .git.
	bare := t.TempDir()
	if got := gitRepoRoot(bare); got != "" {
		t.Errorf("gitRepoRoot(bare) = %q, want empty (no .git ancestor)", got)
	}
}

func TestCodingPathOffReason(t *testing.T) {
	cases := []struct {
		targetDir   string
		haveStrong  bool
		wantContain string
	}{
		{"", false, "no git repo at CWD and no strong rung"},
		{"", true, "no git repo at CWD"},
		{"/some/repo", false, "no strong rung"},
	}
	for _, c := range cases {
		if got := codingPathOffReason(c.targetDir, c.haveStrong); !strings.Contains(got, c.wantContain) {
			t.Errorf("codingPathOffReason(%q,%v) = %q, want it to contain %q", c.targetDir, c.haveStrong, got, c.wantContain)
		}
	}
}
