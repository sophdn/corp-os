package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/risk"
	"corpos/internal/tool"
)

func TestPathMatches(t *testing.T) {
	yes := [][2]string{
		{"internal/foo/bar.go", "internal/**"},
		{"internal/foo/bar.go", "**/*.go"},
		{"internal/foo/bar.go", "internal/foo/*.go"},
		{"internal/foo/bar_test.go", "**/*_test.go"},
		{"a/b/c/d.go", "a/**/d.go"},
	}
	for _, c := range yes {
		if !pathMatches(c[0], c[1]) {
			t.Errorf("%q should match %q", c[0], c[1])
		}
	}
	no := [][2]string{
		{"internal/baz/bar.go", "internal/foo/**"},
		{"internal/foo/bar.go", "**/*_test.go"},
		{"foo.go", "internal/**"},
	}
	for _, c := range no {
		if pathMatches(c[0], c[1]) {
			t.Errorf("%q should NOT match %q", c[0], c[1])
		}
	}
	if !matchesAny("x/y_test.go", []string{"a/**", "**/*_test.go"}) {
		t.Error("matchesAny should hit the second pattern")
	}
}

func TestProtectedPathGate(t *testing.T) {
	g := ProtectedPathGate{Protected: []string{"**/*_test.go"}}
	// Mutating write to a protected path → denied.
	deny := tool.Call{Surface: "fs", Action: "write", Params: map[string]any{"path": "internal/store/store_test.go"}}
	if ok, reason := g.Approve(deny, risk.Verdict{Class: risk.ClassMutating}); ok || reason == "" {
		t.Fatalf("write to protected test path should be denied, got ok=%v", ok)
	}
	// Mutating write to a non-protected path → allowed.
	allow := tool.Call{Surface: "fs", Action: "write", Params: map[string]any{"file_path": "internal/store/store.go"}}
	if ok, _ := g.Approve(allow, risk.Verdict{Class: risk.ClassMutating}); !ok {
		t.Fatal("write to a non-protected path should be allowed")
	}
	// Non-mutating verdict → allowed regardless.
	if ok, _ := g.Approve(deny, risk.Verdict{Class: risk.ClassSafe}); !ok {
		t.Fatal("non-mutating call should pass the scope-of-mutation gate")
	}
	// Non-fs surface → allowed.
	other := tool.Call{Surface: "work", Action: "forge"}
	if ok, _ := g.Approve(other, risk.Verdict{Class: risk.ClassMutating}); !ok {
		t.Fatal("non-fs mutating call is out of this gate's scope")
	}
}

func TestChangedPaths(t *testing.T) {
	diff := "diff --git a/internal/store/store.go b/internal/store/store.go\n@@ -1 +1 @@\n-x\n+y\n" +
		"diff --git a/internal/store/store_test.go b/internal/store/store_test.go\n+evil\n"
	got := changedPaths(diff)
	if len(got) != 2 || got[0] != "internal/store/store.go" || got[1] != "internal/store/store_test.go" {
		t.Fatalf("changedPaths = %v", got)
	}
	if len(changedPaths("no diff headers here")) != 0 {
		t.Fatal("non-diff text yields no paths")
	}
}

// writingWorker writes a fixed file into the worktree (to exercise gate integrity).
type writingWorker struct {
	rel     string
	content string
}

func (w writingWorker) Attempt(_ context.Context, _ AtomicTask, dir string, _ Feedback) AttemptResult {
	_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, w.rel)), 0o750)
	_ = os.WriteFile(filepath.Join(dir, w.rel), []byte(w.content), 0o600)
	return AttemptResult{Note: "wrote " + w.rel}
}

func TestGateIntegrityViolationRejectsTamper(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	o := New(WithRunner(ExecRunner{}), WithRepo(r))
	o.model = writingWorker{rel: "internal/store/store_test.go", content: "package store // tampered\n"}

	// The gate would PASS (`true`), but tampering with a protected *_test.go must be
	// caught first — a worker cannot certify itself by rewriting its own oracle.
	chain := Chain{Slug: "c", TargetRepo: repo, BaseBranch: "main",
		Tasks: []AtomicTask{{Slug: "impl", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"true"}}, Protected: []string{"**/*_test.go"}}}}
	st, err := o.Start(context.Background(), chain, "gi1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	if st.ATs[0].WorkerStatus != WorkerGateIntegrityViolation {
		t.Fatalf("tampering with a protected gate path must be rejected, got %q (%s)", st.ATs[0].WorkerStatus, st.ATs[0].Diagnostic)
	}
	if st.Status != ChainFailed {
		t.Fatalf("chain should fail on a gate-integrity violation, got %q", st.Status)
	}
}

func TestGateIntegrityAllowsNonProtectedWrite(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	o := New(WithRunner(ExecRunner{}), WithRepo(r))
	o.model = writingWorker{rel: "internal/store/store.go", content: "package store\n"}
	chain := Chain{Slug: "c", TargetRepo: repo, BaseBranch: "main",
		Tasks: []AtomicTask{{Slug: "impl", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"true"}}, Protected: []string{"**/*_test.go"}}}}
	st, _ := o.Start(context.Background(), chain, "gi2")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("writing a non-protected file should succeed, got %q (%s)", st.Status, st.ATs[0].Diagnostic)
	}
}

func TestGateIntegrityNoopRepoSkips(t *testing.T) {
	// Without a diff-capable repo there is nothing to check → no violation.
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	if hits := o.gateIntegrityViolations(context.Background(), AtomicTask{Protected: []string{"**"}}, "/x", "sha"); hits != nil {
		t.Fatalf("noop repo should yield no integrity hits, got %v", hits)
	}
	// No protected paths → skip.
	if hits := o.gateIntegrityViolations(context.Background(), AtomicTask{}, "/x", "sha"); hits != nil {
		t.Fatalf("no protected paths → no hits, got %v", hits)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSwapReferenceTestsAndCopyTree(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"internal/store/store.go":      "package store\n",
		"internal/store/store_test.go": "package store // IMPL's own test\n",
		".git/config":                  "[core]\n",
	})
	ref := t.TempDir()
	writeTree(t, ref, map[string]string{"internal/store/store_test.go": "package store // REFERENCE test\n"})

	work := t.TempDir()
	if err := copyTree(src, work); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git must be skipped by copyTree")
	}
	if err := swapReferenceTests(ref, work, "internal/store"); err != nil {
		t.Fatalf("swap: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(work, "internal/store/store_test.go"))
	if !strings.Contains(string(got), "REFERENCE test") {
		t.Fatalf("reference test not swapped in: %q", got)
	}
	// Missing package dirs are skipped, not errors.
	if err := swapReferenceTests(ref, work, "internal/nope"); err != nil {
		t.Fatalf("missing pkg should be skipped, got %v", err)
	}
}

func TestForwardParityClassification(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"internal/store/store.go": "package store\n", "internal/store/store_test.go": "package store\n"})
	ref := t.TempDir()
	writeTree(t, ref, map[string]string{"internal/store/store_test.go": "package store\n"})
	pkgs := []string{"internal/store"}

	// compiled + passed → real pass.
	pass := funcRunner(func(cmd []string, _ string) CommandResult {
		return CommandResult{Command: cmd, Stdout: "ok\tcorpos/internal/store\n"}
	})
	res, err := ForwardParity(context.Background(), pass, src, ref, pkgs, nil, nil)
	if err != nil {
		t.Fatalf("parity: %v", err)
	}
	if !res.Compiled || !res.Passed || res.FalsePass {
		t.Fatalf("pass case: %+v", res)
	}

	// compiled + failed (unexplained) → false pass.
	fail := funcRunner(func(cmd []string, _ string) CommandResult {
		return CommandResult{Command: cmd, ExitCode: 1, Stdout: "--- FAIL: TestList (0.00s)\n    list_test.go:9: got 0 want 3\nFAIL\tcorpos/internal/store\n"}
	})
	res, _ = ForwardParity(context.Background(), fail, src, ref, pkgs, nil, nil)
	if !res.Compiled || res.Passed || !res.FalsePass {
		t.Fatalf("false-pass case: %+v", res)
	}

	// compiled + failed but allowlisted → NOT a false pass, recorded.
	res, _ = ForwardParity(context.Background(), fail, src, ref, pkgs, nil, []string{"TestList"})
	if res.FalsePass || len(res.AllowlistedFailures) == 0 {
		t.Fatalf("allowlisted case should not be a false pass: %+v", res)
	}

	// not compiled → API divergence, not a false pass.
	bld := funcRunner(func(cmd []string, _ string) CommandResult {
		return CommandResult{Command: cmd, ExitCode: 2, Stdout: "# corpos/internal/store [build failed]\n"}
	})
	res, _ = ForwardParity(context.Background(), bld, src, ref, pkgs, nil, nil)
	if res.Compiled || res.FalsePass {
		t.Fatalf("build-failed case should be !compiled and not false-pass: %+v", res)
	}
}
