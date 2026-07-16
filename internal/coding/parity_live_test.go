package coding

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fileWorker is a deterministic "worker" that writes a fixed set of files per AT
// slug into the worktree — a stand-in for a model worker, used to exercise the
// ORGAN's mechanics (worktree-per-AT, owned gate, integration branch, commit,
// forward parity) end-to-end on REAL Go. (The local-Qwen worker's authoring
// quality is measured separately in the operator/coder live tests + the spike.)
type fileWorker struct {
	files map[string]map[string]string // slug → {relpath: content}
}

func (w fileWorker) Attempt(_ context.Context, at AtomicTask, dir string, _ Feedback) AttemptResult {
	for rel, content := range w.files[at.Slug] {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		_ = os.WriteFile(p, []byte(content), 0o600)
	}
	return AttemptResult{Note: "wrote files for " + at.Slug}
}

// initGoTarget creates a temp git repo holding a Go module (go.mod on main) so the
// chain's worktrees can build/test. Skips if git/go are unavailable.
func initGoTarget(t *testing.T) string {
	t.Helper()
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 to run the heavy real-Go parity audit (nested go build/test)")
	}
	for _, bin := range []string{"git", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(cleanGitEnv(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module calctarget\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "base: go.mod")
	return dir
}

// TestParityAuditRealGoSuccess: the organ runs a real Go coding chain end-to-end
// (worktree → owned go gate → integration commit) and the forward-parity guard
// confirms the reference suite passes on the final impl — real correctness, no
// false pass. This is the parity audit's green path on a real target.
func TestParityAuditRealGoSuccess(t *testing.T) {
	repo := initGoTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	o := New(WithRunner(ExecRunner{}), WithRepo(r))
	o.model = fileWorker{files: map[string]map[string]string{
		"calc-impl": {"internal/calc/calc.go": "package calc\n\nfunc Add(a, b int) int { return a + b }\n"},
	}}
	chain := Chain{Slug: "calc", TargetRepo: repo, BaseBranch: "main", Tasks: []AtomicTask{
		{Slug: "calc-impl", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"go", "build", "./..."}}},
	}}
	st, err := o.Start(context.Background(), chain, "calc-ok")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("organ should run the chain to SUCCESS on real Go, got %q (%s)", st.Status, st.ATs[0].Diagnostic)
	}

	// Forward parity against the reference suite.
	ref := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ref, "internal", "calc"), 0o750)
	_ = os.WriteFile(filepath.Join(ref, "internal", "calc", "calc_test.go"),
		[]byte("package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fatal(\"Add wrong\") } }\n"), 0o600)

	// Materialize the final integration tree, then run forward parity over it.
	srcTree := materialize(t, repo, st.IntegrationBranch)
	res, err := ForwardParity(context.Background(), ExecRunner{}, srcTree, ref, []string{"internal/calc"}, nil, nil)
	if err != nil {
		t.Fatalf("forward parity: %v", err)
	}
	if !res.Compiled || !res.Passed || res.FalsePass {
		t.Fatalf("correct impl should pass the reference suite: %+v", res)
	}
}

// TestParityAuditRealGoCatchesFalsePass: a WRONG impl passes the chain's own (weak)
// build gate so the chain goes green — but the forward-parity guard catches that the
// reference suite fails. This is the load-bearing false-pass signal on real Go.
func TestParityAuditRealGoCatchesFalsePass(t *testing.T) {
	repo := initGoTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	o := New(WithRunner(ExecRunner{}), WithRepo(r))
	o.model = fileWorker{files: map[string]map[string]string{
		"calc-impl": {"internal/calc/calc.go": "package calc\n\nfunc Add(a, b int) int { return a - b }\n"}, // WRONG
	}}
	chain := Chain{Slug: "calc", TargetRepo: repo, BaseBranch: "main", Tasks: []AtomicTask{
		{Slug: "calc-impl", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"go", "build", "./..."}}},
	}}
	st, _ := o.Start(context.Background(), chain, "calc-fp")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("a compiling (wrong) impl passes the build gate, so the chain goes green; got %q", st.Status)
	}

	ref := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ref, "internal", "calc"), 0o750)
	_ = os.WriteFile(filepath.Join(ref, "internal", "calc", "calc_test.go"),
		[]byte("package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fatalf(\"Add(2,3)=%d want 5\", Add(2, 3)) } }\n"), 0o600)

	srcTree := materialize(t, repo, st.IntegrationBranch)
	res, err := ForwardParity(context.Background(), ExecRunner{}, srcTree, ref, []string{"internal/calc"}, nil, nil)
	if err != nil {
		t.Fatalf("forward parity: %v", err)
	}
	if !res.Compiled || res.Passed || !res.FalsePass {
		t.Fatalf("forward-parity guard should flag the false pass: %+v", res)
	}
}

// materialize checks out the integration branch's tree into a fresh dir via git
// archive (the parity audit runs against the final integrated state).
func materialize(t *testing.T, repo, branch string) string {
	t.Helper()
	dst := t.TempDir()
	// git archive <branch> | tar -x -C dst
	archive := exec.Command("git", "-C", repo, "archive", branch)
	archive.Env = cleanGitEnv()
	out, err := archive.Output()
	if err != nil {
		t.Fatalf("git archive: %v", err)
	}
	tar := exec.Command("tar", "-x", "-C", dst)
	tar.Stdin = bytes.NewReader(out)
	if err := tar.Run(); err != nil {
		t.Fatalf("tar extract: %v", err)
	}
	return dst
}
