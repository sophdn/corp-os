package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// initGoModuleTarget creates a temp git repo with a go.mod and a BUGGY calc package
// committed on main (Add subtracts instead of adds), returning the repo dir and the base
// SHA — the pre-fix tree the red-before-green replay overlays test files onto.
func initGoModuleTarget(t *testing.T) (dir, baseSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(cleanGitEnv(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "init", "-q", "-b", "main")
	write("go.mod", "module example.com/m\n\ngo 1.26\n")
	write("calc/calc.go", "package calc\n\n// Add is BUGGY: it subtracts.\nfunc Add(a, b int) int { return a - b }\n")
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "base (buggy)")
	baseSHA = run("git", "rev-parse", "HEAD")
	return dir, trimNL(baseSHA)
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// TestRunTestsAtRefWithOverlay_RedAndGreen exercises the real git overlay-and-replay: a
// legit regression test FAILS on the pre-fix tree (red, exit != 0) while a tautological one
// that asserts the buggy behavior PASSES (exit 0) — the signal the gate uses to reject it.
func TestRunTestsAtRefWithOverlay_RedAndGreen(t *testing.T) {
	repoDir, baseSHA := initGoModuleTarget(t)
	r := NewGitRepo(ExecRunner{}, repoDir, t.TempDir())

	// srcDir holds the worker's added test file at its repo-relative path. Both a legit
	// test (asserts Add==3) and a tautological one (asserts the buggy Add==1) live in it.
	srcDir := t.TempDir()
	testBody := `package calc

import "testing"

func TestLegit(t *testing.T) {
	if Add(2, 1) != 3 {
		t.Fatalf("Add(2,1)=%d, want 3", Add(2, 1))
	}
}

func TestTautology(t *testing.T) {
	if Add(2, 1) != 1 {
		t.Fatalf("expected buggy result 1")
	}
}
`
	if err := os.MkdirAll(filepath.Join(srcDir, "calc"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "calc", "calc_test.go"), []byte(testBody), 0o600); err != nil {
		t.Fatal(err)
	}

	overlay := []string{"calc/calc_test.go"}
	specs := []testRunSpec{
		{Pkg: "calc", Funcs: []string{"TestLegit"}},
		{Pkg: "calc", Funcs: []string{"TestTautology"}},
	}
	results, err := r.RunTestsAtRefWithOverlay(context.Background(), baseSHA, srcDir, overlay, specs, 60*time.Second)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].ExitCode == 0 {
		t.Fatalf("legit test should FAIL on pre-fix tree (red), got exit 0:\n%s%s", results[0].Stdout, results[0].Stderr)
	}
	if results[1].ExitCode != 0 {
		t.Fatalf("tautological test should PASS on pre-fix tree (green), got exit %d:\n%s%s",
			results[1].ExitCode, results[1].Stdout, results[1].Stderr)
	}

	// The throwaway worktree was reaped (no redgreen-* dirs left under worktreeDir).
	entries, _ := os.ReadDir(r.worktreeDir)
	for _, e := range entries {
		t.Fatalf("leftover scratch worktree: %s", e.Name())
	}
}

// A deleted/absent overlay source is skipped, not a hard error.
func TestRunTestsAtRefWithOverlay_MissingOverlaySkipped(t *testing.T) {
	repoDir, baseSHA := initGoModuleTarget(t)
	r := NewGitRepo(ExecRunner{}, repoDir, t.TempDir())
	// Overlay names a file that does not exist in srcDir; the run still proceeds (the
	// committed calc package has no tests, so `go test` is a no-op pass).
	results, err := r.RunTestsAtRefWithOverlay(context.Background(), baseSHA, t.TempDir(),
		[]string{"calc/gone_test.go"}, []testRunSpec{{Pkg: "calc", Funcs: []string{"TestNope"}}}, 60*time.Second)
	if err != nil {
		t.Fatalf("missing overlay should be skipped, got err: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
}

// A bad ref is surfaced as an error (the scratch worktree cannot be created).
func TestRunTestsAtRefWithOverlay_BadRef(t *testing.T) {
	repoDir, _ := initGoModuleTarget(t)
	r := NewGitRepo(ExecRunner{}, repoDir, t.TempDir())
	if _, err := r.RunTestsAtRefWithOverlay(context.Background(), "deadbeefdeadbeef", t.TempDir(),
		nil, []testRunSpec{{Pkg: "calc", Funcs: []string{"X"}}}, 30*time.Second); err == nil {
		t.Fatal("want error for a non-existent ref")
	}
}
