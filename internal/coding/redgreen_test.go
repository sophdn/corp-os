package coding

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestParseWorkerTestAdditions_AddedFuncs(t *testing.T) {
	diff := `diff --git a/internal/fs/read_test.go b/internal/fs/read_test.go
new file mode 100644
--- /dev/null
+++ b/internal/fs/read_test.go
@@ -0,0 +1,8 @@
+package fs
+
+import "testing"
+
+func TestRead_SymbolTypeMismatch(t *testing.T) {
+	t.Log("hi")
+}
+func TestRead_Other(t *testing.T){}
diff --git a/internal/fs/read.go b/internal/fs/read.go
--- a/internal/fs/read.go
+++ b/internal/fs/read.go
@@ -1,1 +1,2 @@
+// production change, not a test
`
	got := parseWorkerTestAdditions(diff)
	wantFiles := []string{"internal/fs/read_test.go"}
	if !reflect.DeepEqual(got.Files, wantFiles) {
		t.Fatalf("Files = %v, want %v", got.Files, wantFiles)
	}
	if len(got.Specs) != 1 {
		t.Fatalf("Specs = %+v, want 1 spec", got.Specs)
	}
	if got.Specs[0].Pkg != "internal/fs" {
		t.Fatalf("Pkg = %q", got.Specs[0].Pkg)
	}
	wantFuncs := []string{"TestRead_Other", "TestRead_SymbolTypeMismatch"} // sorted
	if !reflect.DeepEqual(got.Specs[0].Funcs, wantFuncs) {
		t.Fatalf("Funcs = %v, want %v", got.Specs[0].Funcs, wantFuncs)
	}
}

func TestParseWorkerTestAdditions_NoTestFiles(t *testing.T) {
	diff := `diff --git a/internal/fs/read.go b/internal/fs/read.go
--- a/internal/fs/read.go
+++ b/internal/fs/read.go
@@ -1,1 +1,2 @@
+func helper() {}
`
	got := parseWorkerTestAdditions(diff)
	if len(got.Files) != 0 || len(got.Specs) != 0 {
		t.Fatalf("want empty additions, got %+v", got)
	}
}

// A modified test body that adds NO new Test function contributes a changed file but no
// spec (the tautology check only targets newly-authored tests; tampering is T2's job).
func TestParseWorkerTestAdditions_ModifiedBodyNoNewFunc(t *testing.T) {
	diff := `diff --git a/pkg/a_test.go b/pkg/a_test.go
--- a/pkg/a_test.go
+++ b/pkg/a_test.go
@@ -2,3 +2,3 @@
-	want := 1
+	want := 2
`
	got := parseWorkerTestAdditions(diff)
	if len(got.Files) != 1 || got.Files[0] != "pkg/a_test.go" {
		t.Fatalf("Files = %v", got.Files)
	}
	if len(got.Specs) != 0 {
		t.Fatalf("Specs = %+v, want none (no added func)", got.Specs)
	}
}

func TestParseWorkerTestAdditions_MultiplePackages(t *testing.T) {
	diff := `diff --git a/x/x_test.go b/x/x_test.go
+++ b/x/x_test.go
+func TestX(t *testing.T) {}
diff --git a/y/y_test.go b/y/y_test.go
+++ b/y/y_test.go
+func TestY(t *testing.T) {}
`
	got := parseWorkerTestAdditions(diff)
	if len(got.Specs) != 2 || got.Specs[0].Pkg != "x" || got.Specs[1].Pkg != "y" {
		t.Fatalf("Specs = %+v", got.Specs)
	}
}

func TestPostImagePath(t *testing.T) {
	cases := map[string]string{
		"+++ b/internal/fs/read_test.go":      "internal/fs/read_test.go",
		"+++ b/a.go\t2026-01-01 00:00:00.000": "a.go",
		"+++ /dev/null":                       "",
	}
	for in, want := range cases {
		if got := postImagePath(in); got != want {
			t.Fatalf("postImagePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDedupeSorted(t *testing.T) {
	got := dedupeSorted([]string{"b", "a", "b", "c", "a"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupeSorted = %v, want %v", got, want)
	}
}

func TestIsTestFile(t *testing.T) {
	if !isTestFile("a/b_test.go") || isTestFile("a/b.go") || isTestFile("readme.md") {
		t.Fatal("isTestFile classification wrong")
	}
}

// fakeRedGreenRepo satisfies Repo (via NoopRepo) + packageReader + redGreenRepo so the
// red-before-green check can be unit-tested without a real git repo.
type fakeRedGreenRepo struct {
	NoopRepo
	diff     string
	diffErr  error
	results  []CommandResult
	runErr   error
	gotRef   string
	gotFiles []string
	gotSpecs []testRunSpec
	called   bool
}

func (r *fakeRedGreenRepo) ListPackage(context.Context, string) ([]string, error) { return nil, nil }
func (r *fakeRedGreenRepo) Show(context.Context, string) (string, error)          { return "", nil }
func (r *fakeRedGreenRepo) Diff(context.Context, string, string) (string, error)  { return "", nil }
func (r *fakeRedGreenRepo) DiffWorktree(context.Context, string, string) (string, error) {
	return r.diff, r.diffErr
}
func (r *fakeRedGreenRepo) RunTestsAtRefWithOverlay(_ context.Context, ref, _ string, files []string, specs []testRunSpec, _ time.Duration) ([]CommandResult, error) {
	r.called = true
	r.gotRef, r.gotFiles, r.gotSpecs = ref, files, specs
	return r.results, r.runErr
}

func newOrchWithRepo(repo Repo) *Orchestrator { return New(WithRepo(repo)) }

func TestTautologyVerdict_PassesOnUnfixed(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:    "+++ b/p/a_test.go\n+func TestBug(t *testing.T) {}\n",
		results: []CommandResult{{ExitCode: 0}}, // green on pre-fix → tautology
	}
	o := newOrchWithRepo(repo)
	verdict, err := o.tautologyVerdict(context.Background(), "/work", "basesha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if verdict == "" {
		t.Fatal("want a tautology verdict, got none")
	}
	if repo.gotRef != "basesha" || len(repo.gotSpecs) != 1 || repo.gotSpecs[0].Pkg != "p" {
		t.Fatalf("overlay called with ref=%q specs=%+v files=%v", repo.gotRef, repo.gotSpecs, repo.gotFiles)
	}
}

func TestTautologyVerdict_FailsOnUnfixed_Legit(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:    "+++ b/p/a_test.go\n+func TestBug(t *testing.T) {}\n",
		results: []CommandResult{{ExitCode: 1}}, // red on pre-fix → legit
	}
	o := newOrchWithRepo(repo)
	verdict, err := o.tautologyVerdict(context.Background(), "/work", "basesha")
	if err != nil || verdict != "" {
		t.Fatalf("legit red→green should yield no verdict; got verdict=%q err=%v", verdict, err)
	}
}

func TestTautologyVerdict_NoAddedTests(t *testing.T) {
	repo := &fakeRedGreenRepo{diff: "+++ b/p/a.go\n+func helper() {}\n"}
	o := newOrchWithRepo(repo)
	verdict, err := o.tautologyVerdict(context.Background(), "/work", "basesha")
	if err != nil || verdict != "" {
		t.Fatalf("no added tests → no verdict; got %q %v", verdict, err)
	}
	if repo.called {
		t.Fatal("overlay should not run when there are no added tests")
	}
}

func TestTautologyVerdict_NoParentSHA(t *testing.T) {
	repo := &fakeRedGreenRepo{diff: "+++ b/p/a_test.go\n+func TestBug(t *testing.T) {}\n"}
	o := newOrchWithRepo(repo)
	verdict, err := o.tautologyVerdict(context.Background(), "/work", "")
	if err != nil || verdict != "" {
		t.Fatalf("empty parentSHA → skip; got %q %v", verdict, err)
	}
}

func TestTautologyVerdict_NoopRepoSkips(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	verdict, err := o.tautologyVerdict(context.Background(), "/work", "basesha")
	if err != nil || verdict != "" {
		t.Fatalf("NoopRepo (no redGreenRepo) → skip; got %q %v", verdict, err)
	}
}

func TestTautologyVerdict_DiffError(t *testing.T) {
	repo := &fakeRedGreenRepo{diffErr: errors.New("diff boom")}
	o := newOrchWithRepo(repo)
	if _, err := o.tautologyVerdict(context.Background(), "/work", "basesha"); err == nil {
		t.Fatal("want diff error propagated")
	}
}

func TestTautologyVerdict_EmptyDiff(t *testing.T) {
	repo := &fakeRedGreenRepo{diff: ""}
	o := newOrchWithRepo(repo)
	verdict, err := o.tautologyVerdict(context.Background(), "/work", "basesha")
	if err != nil || verdict != "" {
		t.Fatalf("empty diff → no verdict; got %q %v", verdict, err)
	}
}

func TestTautologyVerdict_ReplayError(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:   "+++ b/p/a_test.go\n+func TestBug(t *testing.T) {}\n",
		runErr: errors.New("worktree add failed"),
	}
	o := newOrchWithRepo(repo)
	if _, err := o.tautologyVerdict(context.Background(), "/work", "basesha"); err == nil {
		t.Fatal("want replay error propagated")
	}
}

// runWorkerLoop wiring: a passing gate plus a tautological worker test is rejected as a
// fake green rather than credited as success.
func TestRunWorkerLoop_FakeGreenRejected(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:    "+++ b/p/a_test.go\n+func TestBug(t *testing.T) {}\n",
		results: []CommandResult{{ExitCode: 0}},
	}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:          "fix",
		Gate:          [][]string{{"go", "test", "./..."}},
		Worker:        WorkerConfig{Kind: WorkerModel},
		MaxIterations: 1,
	}
	status, diag, _, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerFakeGreen {
		t.Fatalf("status = %q (%s), want WorkerFakeGreen", status, diag)
	}
}

func TestRunWorkerLoop_LegitGreenCredited(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:    "+++ b/p/a_test.go\n+func TestBug(t *testing.T) {}\n",
		results: []CommandResult{{ExitCode: 1}}, // red on pre-fix → legit
	}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:          "fix",
		Gate:          [][]string{{"go", "test", "./..."}},
		Worker:        WorkerConfig{Kind: WorkerModel},
		MaxIterations: 1,
	}
	status, diag, _, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerSuccess {
		t.Fatalf("status = %q (%s), want WorkerSuccess", status, diag)
	}
}

func TestRunWorkerLoop_ReplayErrorIsCommandError(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:   "+++ b/p/a_test.go\n+func TestBug(t *testing.T) {}\n",
		runErr: errors.New("boom"),
	}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:          "fix",
		Gate:          [][]string{{"go", "test", "./..."}},
		Worker:        WorkerConfig{Kind: WorkerModel},
		MaxIterations: 1,
	}
	status, _, _, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerCommandError {
		t.Fatalf("status = %q, want WorkerCommandError", status)
	}
}
