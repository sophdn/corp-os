package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cannedRunner returns a fixed Stdout for any command — used to test the porcelain
// parsing of changedGoFiles without a real git tree.
type cannedRunner struct{ out string }

func (r cannedRunner) Run(context.Context, []string, string, time.Duration) CommandResult {
	return CommandResult{Stdout: r.out}
}

func TestChangedGoFiles_ParsesPorcelainAndFiltersGo(t *testing.T) {
	status := " M a.go\n" + // modified
		"?? b.go\n" + // untracked
		" M c.txt\n" + // not Go → skip
		"R  old.go -> new.go\n" + // rename → new path
		" D gone.go\n" + // deleted → skip (nothing to format)
		"A  added.go\n" // staged add
	o := New(WithRunner(cannedRunner{out: status}))
	got := o.changedGoFiles(context.Background(), "anydir")
	want := []string{"a.go", "b.go", "new.go", "added.go"}
	if len(got) != len(want) {
		t.Fatalf("changedGoFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("changedGoFiles = %v, want %v", got, want)
		}
	}
}

// End-to-end: gofmtChangedGo normalizes a gofmt-dirty .go file the worker wrote, so the
// deliverable is gofmt-clean before the gate (bug gofmt-dirty-go-edit-on-exact-match).
func TestGofmtChangedGo_NormalizesDirtyGoFile(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt unavailable")
	}
	dir := initGitTarget(t) // real git repo (skips if git absent)
	// A parseable but gofmt-dirty file: the function body is not indented.
	dirty := "package p\nfunc f() int {\nx := 1\nreturn x\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(dirty), 0o600); err != nil {
		t.Fatalf("seed dirty file: %v", err)
	}
	o := New(WithRunner(ExecRunner{}), WithGateTimeout(30*time.Second), WithGofmtNormalize())

	o.gofmtChangedGo(context.Background(), dir)

	if out := (ExecRunner{}).Run(context.Background(), []string{"gofmt", "-l", "f.go"}, dir, 30*time.Second); strings.TrimSpace(out.Stdout) != "" {
		t.Fatalf("f.go still gofmt-dirty after normalize: gofmt -l = %q", out.Stdout)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	if !strings.Contains(string(got), "\tx := 1") {
		t.Fatalf("body was not reindented by gofmt:\n%s", got)
	}
}
