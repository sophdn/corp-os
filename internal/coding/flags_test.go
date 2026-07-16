package coding

import (
	"context"
	"strings"
	"testing"
)

// gitHeader builds a minimal `diff --git` file header (the form changedPaths parses).
func gitHeader(p string) string {
	return "diff --git a/" + p + " b/" + p + "\n"
}

func TestGateFlags_ProtectedPathEdit(t *testing.T) {
	diff := gitHeader("internal/fs/acceptance_test.go") + gitHeader("internal/fs/read.go")
	flags := gateFlags(diff, AtomicTask{Protected: []string{"internal/fs/acceptance_test.go"}})
	f, ok := findFlag(flags, FlagProtectedPathEdit)
	if !ok {
		t.Fatalf("want a protected-path-edit flag, got %+v", flags)
	}
	if !strings.Contains(f.Detail, "acceptance_test.go") {
		t.Fatalf("detail = %q", f.Detail)
	}
}

func TestGateFlags_TestOnlyDiff(t *testing.T) {
	diff := gitHeader("pkg/a_test.go") + gitHeader("pkg/b_test.go")
	flags := gateFlags(diff, AtomicTask{})
	if _, ok := findFlag(flags, FlagTestOnlyDiff); !ok {
		t.Fatalf("want a test-only-diff flag, got %+v", flags)
	}
}

// A diff that touches a production .go alongside tests is NOT test-only.
func TestGateFlags_MixedDiffNotTestOnly(t *testing.T) {
	diff := gitHeader("pkg/a_test.go") + gitHeader("pkg/a.go")
	flags := gateFlags(diff, AtomicTask{})
	if _, ok := findFlag(flags, FlagTestOnlyDiff); ok {
		t.Fatalf("mixed diff should not be test-only: %+v", flags)
	}
}

// A diff with no .go files at all (e.g. docs) raises no test-only flag.
func TestGateFlags_NonGoDiff(t *testing.T) {
	diff := gitHeader("README.md")
	if flags := gateFlags(diff, AtomicTask{}); len(flags) != 0 {
		t.Fatalf("non-go diff → no flags, got %+v", flags)
	}
}

func TestGateFlags_BothFlags(t *testing.T) {
	diff := gitHeader("gate/acceptance_test.go")
	flags := gateFlags(diff, AtomicTask{Protected: []string{"gate/**"}})
	if _, ok := findFlag(flags, FlagProtectedPathEdit); !ok {
		t.Fatal("want protected flag")
	}
	if _, ok := findFlag(flags, FlagTestOnlyDiff); !ok {
		t.Fatal("want test-only flag (the only changed .go is a test)")
	}
}

func TestGateFlags_EmptyDiff(t *testing.T) {
	if flags := gateFlags("", AtomicTask{Protected: []string{"**"}}); flags != nil {
		t.Fatalf("empty diff → nil flags, got %+v", flags)
	}
}

func TestFindFlag_NotFound(t *testing.T) {
	if _, ok := findFlag([]GateFlag{{Kind: FlagTestOnlyDiff}}, FlagProtectedPathEdit); ok {
		t.Fatal("findFlag should report absent")
	}
}

func TestWorktreeDiff(t *testing.T) {
	// NoopRepo is not a packageReader → "".
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	if got := o.worktreeDiff(context.Background(), "/x", "sha"); got != "" {
		t.Fatalf("NoopRepo worktreeDiff = %q, want empty", got)
	}
	// Empty parentSHA → "".
	if got := o.worktreeDiff(context.Background(), "/x", ""); got != "" {
		t.Fatalf("empty parentSHA worktreeDiff = %q", got)
	}
	// A diffable repo returns its diff; an error is swallowed to "".
	ok := &fakeRedGreenRepo{diff: gitHeader("p/a.go")}
	o2 := New(WithRepo(ok))
	if got := o2.worktreeDiff(context.Background(), "/x", "sha"); got == "" {
		t.Fatal("want non-empty diff from a diffable repo")
	}
	boom := &fakeRedGreenRepo{diffErr: context.Canceled}
	o3 := New(WithRepo(boom))
	if got := o3.worktreeDiff(context.Background(), "/x", "sha"); got != "" {
		t.Fatalf("diff error should swallow to empty, got %q", got)
	}
}

// runWorkerLoop wiring: a green gate whose only change is a test file (no production code)
// is rejected as a hollow green unless the AT opts out.
func TestRunWorkerLoop_TestOnlyDiffRejected(t *testing.T) {
	// A modified test body (no newly-added Test func) so the red-before-green tautology
	// check does not fire first; the test-only-diff flag is the operative signal.
	diff := gitHeader("p/a_test.go") + "@@ -1 +1 @@\n-\tx := 1\n+\tx := 2\n"
	repo := &fakeRedGreenRepo{diff: diff}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:          "fix",
		Gate:          [][]string{{"go", "test", "./..."}},
		Worker:        WorkerConfig{Kind: WorkerModel},
		MaxIterations: 1,
	}
	status, _, _, flags := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerTestOnlyDiff {
		t.Fatalf("status = %q, want WorkerTestOnlyDiff", status)
	}
	if _, ok := findFlag(flags, FlagTestOnlyDiff); !ok {
		t.Fatalf("flags should carry the test-only signal: %+v", flags)
	}
}

func TestRunWorkerLoop_TestOnlyDiffAllowed(t *testing.T) {
	diff := gitHeader("p/a_test.go") + "@@ -1 +1 @@\n-\tx := 1\n+\tx := 2\n"
	repo := &fakeRedGreenRepo{diff: diff}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:              "add-tests",
		Gate:              [][]string{{"go", "test", "./..."}},
		Worker:            WorkerConfig{Kind: WorkerModel},
		MaxIterations:     1,
		AllowTestOnlyDiff: true,
	}
	status, _, _, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerSuccess {
		t.Fatalf("status = %q, want WorkerSuccess (opted out)", status)
	}
}

// runWorkerLoop wiring: a worker edit to a protected gate path is denied before the gate is
// even trusted, surfaced via the protected-path flag.
func TestRunWorkerLoop_ProtectedPathFlagDenies(t *testing.T) {
	repo := &fakeRedGreenRepo{diff: gitHeader("gate/acceptance_test.go")}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:          "fix",
		Protected:     []string{"gate/**"},
		Gate:          [][]string{{"go", "test", "./..."}},
		Worker:        WorkerConfig{Kind: WorkerModel},
		MaxIterations: 1,
	}
	status, _, _, flags := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerGateIntegrityViolation {
		t.Fatalf("status = %q, want WorkerGateIntegrityViolation", status)
	}
	if _, ok := findFlag(flags, FlagProtectedPathEdit); !ok {
		t.Fatalf("flags should carry the protected-path signal: %+v", flags)
	}
}
