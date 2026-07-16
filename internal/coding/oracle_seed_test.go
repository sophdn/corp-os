package coding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeededOracleIsCommittedBaselineNotWorkerEdit is the load-bearing proof of the
// oracle-seeding mechanism (the structural half of closing the prose→argv seam): an
// AT carrying an authored acceptance oracle has that oracle SEEDED into the worktree
// and committed BEFORE the worker runs, so (a) the gate runs against it, and (b) it
// is part of the diff baseline — NOT mistaken for the worker editing a protected gate
// path. The AT reaching ATSuccess with Protected=**/*_test.go is exactly that proof:
// had the seeded oracle landed in the worker's diff, the gate-integrity check would
// have failed it as WorkerGateIntegrityViolation.
func TestSeededOracleIsCommittedBaselineNotWorkerEdit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	o := New(WithRunner(ExecRunner{}), WithRepo(r))

	at := AtomicTask{
		Slug: "feat",
		// The worker (deterministic) supplies the production file the gate also needs.
		Worker:    WorkerConfig{Kind: WorkerDeterministic, Command: []string{"sh", "-c", "printf 'package x\n' > impl.go"}},
		Oracles:   map[string]string{"feat_test.go": "package x\n\n// authored acceptance oracle\n"},
		Protected: []string{"**/*_test.go"},
		// Gate is green only when BOTH the seeded oracle and the worker's production
		// file are present in the worktree.
		Gate: [][]string{{"test", "-f", "feat_test.go"}, {"test", "-f", "impl.go"}},
	}
	chain := Chain{Slug: "seed-proof", TargetRepo: repo, Tasks: []AtomicTask{at}}

	st, err := o.Start(context.Background(), chain, "seedrun")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("status = %q want success; at0 worker=%q diag=%q flags=%+v",
			st.Status, st.ATs[0].WorkerStatus, st.ATs[0].Diagnostic, st.ATs[0].Flags)
	}
	if st.ATs[0].WorkerStatus == WorkerGateIntegrityViolation {
		t.Fatal("seeded oracle was wrongly flagged as a worker protected-path edit")
	}
	// The integration HEAD must carry BOTH the seeded oracle (committed by the seed
	// step) and the worker's production file (committed by the success step).
	for _, want := range []string{"feat_test.go", "impl.go"} {
		out, err := r.Show(context.Background(), want)
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("integration HEAD missing %q (err=%v, content=%q)", want, err, out)
		}
	}
}

// TestSeedOraclesAdvancesParentSHA exercises the orchestrator seam in isolation: when
// an AT carries oracles, seedOracles writes them through the Workspace and advances the
// diff baseline (ParentSHA) to the seed commit so the worker loop diffs against it.
func TestSeedOraclesAdvancesParentSHA(t *testing.T) {
	ws := &fakeWorkspace{dir: t.TempDir(), sha: "seedsha", ok: true}
	o := New()
	ar := &ATRecord{Position: 3, Slug: "feat", ParentSHA: "forksha",
		Spec: AtomicTask{Slug: "feat", Oracles: map[string]string{"feat_test.go": "package x\n"}}}

	if err := o.seedOracles(context.Background(), ws, ar); err != nil {
		t.Fatalf("seedOracles: %v", err)
	}
	if ws.seeded["feat_test.go"] == "" {
		t.Fatalf("oracle not seeded through the workspace: %+v", ws.seeded)
	}
	if ws.commits != 1 {
		t.Fatalf("seed must commit exactly once, got %d", ws.commits)
	}
	if ar.ParentSHA != "seedsha" {
		t.Fatalf("ParentSHA must advance to the seed commit, got %q", ar.ParentSHA)
	}
}

// A no-oracle AT (every bug-fix task) is left entirely untouched by the seed step.
func TestSeedOraclesNoopWithoutOracles(t *testing.T) {
	ws := &fakeWorkspace{dir: t.TempDir(), sha: "x", ok: true}
	o := New()
	ar := &ATRecord{Slug: "bugfix", ParentSHA: "forksha", Spec: AtomicTask{Slug: "bugfix"}}
	if err := o.seedOracles(context.Background(), ws, ar); err != nil {
		t.Fatalf("seedOracles: %v", err)
	}
	if ws.commits != 0 || ws.seeded != nil || ar.ParentSHA != "forksha" {
		t.Fatalf("no-oracle AT must be untouched: commits=%d seeded=%v parent=%q", ws.commits, ws.seeded, ar.ParentSHA)
	}
}

// TestSeedOraclesPropagatesErrors covers the two failure branches: a Seed write error
// and a Commit error both abort the seed step (the AT then fails in runOne).
func TestSeedOraclesPropagatesErrors(t *testing.T) {
	o := New()
	spec := AtomicTask{Slug: "feat", Oracles: map[string]string{"feat_test.go": "package x\n"}}

	wsSeedErr := &fakeWorkspace{dir: t.TempDir(), seedErr: errors.New("disk full")}
	if err := o.seedOracles(context.Background(), wsSeedErr, &ATRecord{Spec: spec}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("want seed error, got %v", err)
	}
	wsCommitErr := &fakeWorkspace{dir: t.TempDir(), commitErr: errors.New("commit boom")}
	if err := o.seedOracles(context.Background(), wsCommitErr, &ATRecord{Spec: spec}); err == nil || !strings.Contains(err.Error(), "commit boom") {
		t.Fatalf("want commit error, got %v", err)
	}
}

// TestNoopWorkspaceSeed covers the noop seam: seeding writes the file into the shared
// dir (no commit), so the gate sees a pre-existing oracle on the trivial path.
func TestNoopWorkspaceSeed(t *testing.T) {
	dir := t.TempDir()
	ws, err := NoopRepo{Dir: dir}.Open(context.Background(), "", &ATRecord{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ws.Seed(map[string]string{"sub/foo_test.go": "package x\n"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "sub", "foo_test.go")); err != nil || !strings.Contains(string(b), "package x") {
		t.Fatalf("seeded file missing/wrong: %v %q", err, b)
	}
}

// TestSeedFilesIOErrors covers the defensive mkdir/write failure branches.
func TestSeedFilesIOErrors(t *testing.T) {
	dir := t.TempDir()
	// mkdir failure: a parent path component is a regular file.
	if err := os.WriteFile(filepath.Join(dir, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedFiles(dir, map[string]string{"blocker/x_test.go": "y"}); err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("want mkdir error, got %v", err)
	}
	// write failure: the destination path is itself a directory.
	if err := os.Mkdir(filepath.Join(dir, "y_test.go"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := seedFiles(dir, map[string]string{"y_test.go": "z"}); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("want write error, got %v", err)
	}
}

func TestAtomicTaskValidateOracleRules(t *testing.T) {
	cases := []struct {
		name    string
		oracles map[string]string
		protect []string
		wantErr string
	}{
		{"unprotected oracle is a fake-green hole", map[string]string{"a_test.go": "x"}, nil, "not covered by protected"},
		{"escaping path rejected", map[string]string{"../evil_test.go": "x"}, []string{"**/*_test.go"}, "clean repo-relative"},
		{"absolute path rejected", map[string]string{"/etc/x_test.go": "x"}, []string{"**/*_test.go"}, "clean repo-relative"},
		{"protected oracle is accepted", map[string]string{"pkg/a_test.go": "x"}, []string{"**/*_test.go"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := AtomicTask{Slug: "t", Oracles: c.oracles, Protected: c.protect,
				Worker: WorkerConfig{Kind: WorkerModel}}
			err := task.validate(map[string]bool{})
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("want ok, got %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestBuildFeatureChainThreadsAndProtectsOracle(t *testing.T) {
	tasks := []FeatureTask{{
		Slug:    "parse",
		Goal:    "add Parse",
		Gate:    [][]string{{"go", "test", "-run", "^TestAccept_Parse$", "./internal/x/"}},
		Oracles: map[string]string{"internal/x/parse_accept_test.go": "package x\n"},
	}}
	chain, err := BuildFeatureChain("feat", "/repo", tasks)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := chain.Tasks[0]
	if got.Oracles["internal/x/parse_accept_test.go"] == "" {
		t.Fatalf("oracle not threaded into the AT: %+v", got.Oracles)
	}
	// The default Protected set must cover the threaded oracle (else it would be a
	// fake-green hole) — and Validate (run inside BuildFeatureChain) already enforced it.
	if !matchesAny("internal/x/parse_accept_test.go", got.Protected) {
		t.Fatalf("threaded oracle not covered by Protected %v", got.Protected)
	}

	// A non-test oracle path is rejected: the feature's acceptance oracle is a Go test.
	_, err = BuildFeatureChain("feat", "/repo", []FeatureTask{{
		Slug:    "x",
		Goal:    "g",
		Gate:    [][]string{{"true"}},
		Oracles: map[string]string{"internal/x/notatest.go": "package x\n"},
	}})
	if err == nil || !strings.Contains(err.Error(), "_test.go") {
		t.Fatalf("want non-test-oracle rejection, got %v", err)
	}
}
