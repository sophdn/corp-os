package coding

import (
	"context"
	"strings"
	"testing"
)

// TestParseModifiedExistingTestFiles pins the new-vs-modified classification the oracle
// guard depends on: only a MODIFIED pre-existing *_test.go is an oracle that can be weakened;
// a newly-created test file, a deletion, and a production file are all excluded.
func TestParseModifiedExistingTestFiles(t *testing.T) {
	modified := "diff --git a/calc/calc_test.go b/calc/calc_test.go\n" +
		"index abc..def 100644\n--- a/calc/calc_test.go\n+++ b/calc/calc_test.go\n@@ -1 +1 @@\n-\tif Add(2,3)!=5\n+\tif Add(2,3)!=-1\n"
	newFile := "diff --git a/calc/more_test.go b/calc/more_test.go\n" +
		"new file mode 100644\nindex 000..abc\n--- /dev/null\n+++ b/calc/more_test.go\n@@ -0,0 +1 @@\n+package calc\n"
	prod := "diff --git a/calc/calc.go b/calc/calc.go\nindex 1..2 100644\n--- a/calc/calc.go\n+++ b/calc/calc.go\n@@ -1 +1 @@\n-a-b\n+a+b\n"

	got := parseModifiedExistingTestFiles(modified + newFile + prod)
	if len(got) != 1 || got[0] != "calc/calc_test.go" {
		t.Fatalf("want only the modified pre-existing test file, got %v", got)
	}
	// A minimal `diff --git`-only section (the gitHeader test idiom) with no new-file marker
	// reads as a modification.
	if g := parseModifiedExistingTestFiles(gitHeader("p/a_test.go")); len(g) != 1 || g[0] != "p/a_test.go" {
		t.Fatalf("bare modified test header should classify as modified, got %v", g)
	}
	// A purely-new test file is not an oracle.
	if g := parseModifiedExistingTestFiles(newFile); len(g) != 0 {
		t.Fatalf("a new test file must not be treated as a modified oracle, got %v", g)
	}
}

// TestRunWorkerLoop_OracleTamperRejected is the bug 1161 regression: a test-authoring AT
// (AllowTestOnlyDiff) whose worker MODIFIED a pre-existing test and turned the gate green is
// rejected as a fake-green when replaying that test's package at the fork point comes back RED
// — i.e. the pre-existing oracle fails against the (protected) production, so the green was
// reached by weakening it, not by a real fix.
func TestRunWorkerLoop_OracleTamperRejected(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:    gitHeader("calc/calc_test.go") + "@@ -1 +1 @@\n-\tif Add(2,3)!=5\n+\tif Add(2,3)!=-1\n",
		results: []CommandResult{{ExitCode: 1}}, // fork-point replay is RED: the original oracle fails
	}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:              "author-test",
		Gate:              [][]string{{"go", "test", "./..."}},
		Worker:            WorkerConfig{Kind: WorkerModel},
		MaxIterations:     1,
		AllowTestOnlyDiff: true,
	}
	status, diag, _, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerFakeGreen {
		t.Fatalf("status = %q, want WorkerFakeGreen (a weakened oracle is a fake-green)", status)
	}
	if !repo.called {
		t.Error("the fork-point oracle replay must have run")
	}
	if diag == "" || !strings.Contains(diag, "oracle-tamper") {
		t.Errorf("diagnostic should name the oracle-tamper, got %q", diag)
	}
}

// TestRunWorkerLoop_OracleStrengthenAllowed: a modified pre-existing test whose fork-point
// replay is GREEN (the original oracle still holds against production) is a legitimate
// strengthening, not a tamper — it succeeds.
func TestRunWorkerLoop_OracleStrengthenAllowed(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:    gitHeader("calc/calc_test.go") + "@@ -1 +2 @@\n \tif Add(2,3)!=5 { t.Fatal() }\n+\tif Sub(5,2)!=3 { t.Fatal() }\n",
		results: []CommandResult{{ExitCode: 0}}, // fork-point replay is GREEN: the oracle holds
	}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:              "strengthen-test",
		Gate:              [][]string{{"go", "test", "./..."}},
		Worker:            WorkerConfig{Kind: WorkerModel},
		MaxIterations:     1,
		AllowTestOnlyDiff: true,
	}
	status, _, _, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerSuccess {
		t.Fatalf("status = %q, want WorkerSuccess (strengthening a still-passing oracle is legit)", status)
	}
}

// TestRunWorkerLoop_NewTestFileSkipsOracleReplay: authoring a NEW test file has no pre-existing
// oracle to weaken, so the replay is not even run — the clean test-authoring deliverable.
func TestRunWorkerLoop_NewTestFileSkipsOracleReplay(t *testing.T) {
	repo := &fakeRedGreenRepo{
		diff:    "diff --git a/calc/more_test.go b/calc/more_test.go\nnew file mode 100644\n--- /dev/null\n+++ b/calc/more_test.go\n@@ -0,0 +1 @@\n+package calc\n",
		results: []CommandResult{{ExitCode: 1}}, // would be RED, but must not be consulted
	}
	o := New(WithRepo(repo), WithRunner(okRunner()), WithModelWorker(&fakeWorker{}))
	spec := AtomicTask{
		Slug:              "new-test",
		Gate:              [][]string{{"go", "test", "./..."}},
		Worker:            WorkerConfig{Kind: WorkerModel},
		MaxIterations:     1,
		AllowTestOnlyDiff: true,
	}
	status, _, _, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec, ParentSHA: "basesha"}, "/work", nil)
	if status != WorkerSuccess {
		t.Fatalf("status = %q, want WorkerSuccess (a new test file is not an oracle)", status)
	}
	if repo.called {
		t.Error("the oracle replay must be SKIPPED when no pre-existing test was modified")
	}
}
