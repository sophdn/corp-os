package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitTarget creates a temp git repo with one base commit on main and returns
// the repo dir. It skips the test if git is unavailable.
func initGitTarget(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		// Start from a git-context-free env (the test may run under a git hook that
		// exports GIT_DIR) plus deterministic identity.
		cmd.Env = append(cleanGitEnv(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("base\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "base")
	return dir
}

func gitState(repo, runID string) *RunState {
	return &RunState{RunID: runID, BaseBranch: "main", IntegrationBranch: "coding/runs/" + runID + "/integration"}
}

// initGitTargetBranch is initGitTarget parameterized on the initial branch name
// (so a master-default repo can be exercised deterministically across git versions).
func initGitTargetBranch(t *testing.T, defaultBranch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(cleanGitEnv(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q")
	// Force the working branch name regardless of git's compiled-in default, so the
	// test is deterministic (pre-2.28 git defaults to master).
	run("git", "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("base\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "base")
	return dir
}

// TestGitRepoInitMasterDefault is the regression for bug 1077: a target repo whose
// default branch is master (not main) must still initialize the integration branch.
// Before the fix, the base ref defaulted to "main" and Init failed with
// "fatal: not a valid object name: 'main'".
func TestGitRepoInitMasterDefault(t *testing.T) {
	repo := initGitTargetBranch(t, "master")
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	// BaseBranch left empty: the GitRepo seam must resolve the repo's actual branch.
	st := &RunState{RunID: "m1", IntegrationBranch: "coding/runs/m1/integration"}
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init on master-default repo: %v", err)
	}
	if sha, err := r.HeadSHA(context.Background()); err != nil || len(sha) < 7 {
		t.Fatalf("head: %q %v", sha, err)
	}
}

// TestGitRepoInitMainDefaultEmptyBase confirms the resolve path also serves a
// main-default repo when BaseBranch is empty (parity with the master case).
func TestGitRepoInitMainDefaultEmptyBase(t *testing.T) {
	repo := initGitTargetBranch(t, "main")
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := &RunState{RunID: "m2", IntegrationBranch: "coding/runs/m2/integration"}
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init on main-default repo with empty base: %v", err)
	}
	if sha, err := r.HeadSHA(context.Background()); err != nil || len(sha) < 7 {
		t.Fatalf("head: %q %v", sha, err)
	}
}

// TestGitRepoInitExplicitBaseHonored confirms an explicitly-set BaseBranch is still
// honored verbatim (the resolve only kicks in for an empty base).
func TestGitRepoInitExplicitBaseHonored(t *testing.T) {
	repo := initGitTargetBranch(t, "master")
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := &RunState{RunID: "m3", BaseBranch: "master", IntegrationBranch: "coding/runs/m3/integration"}
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init with explicit base=master: %v", err)
	}
	if sha, err := r.HeadSHA(context.Background()); err != nil || len(sha) < 7 {
		t.Fatalf("head: %q %v", sha, err)
	}
}

// TestGitRepoInitDetachedHead covers a detached-HEAD target repo: there is no
// current branch name, so the resolve falls back to the HEAD commit SHA.
func TestGitRepoInitDetachedHead(t *testing.T) {
	repo := initGitTargetBranch(t, "master")
	detach := exec.Command("git", "checkout", "-q", "--detach", "HEAD")
	detach.Dir = repo
	detach.Env = cleanGitEnv()
	if out, err := detach.CombinedOutput(); err != nil {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := &RunState{RunID: "m4", IntegrationBranch: "coding/runs/m4/integration"}
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init on detached-HEAD repo: %v", err)
	}
	if sha, err := r.HeadSHA(context.Background()); err != nil || len(sha) < 7 {
		t.Fatalf("head: %q %v", sha, err)
	}
}

func TestGitRepoInitAndHead(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "run1")
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init: %v", err)
	}
	sha, err := r.HeadSHA(context.Background())
	if err != nil || len(sha) < 7 {
		t.Fatalf("head: %q %v", sha, err)
	}
}

func TestGitRepoOpenCommitFastForward(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "run2")
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init: %v", err)
	}
	fork, _ := r.HeadSHA(context.Background())

	ar := &ATRecord{Slug: "make-foo", Position: 0}
	ws, err := r.Open(context.Background(), fork, ar)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Worker writes a file in the worktree.
	if err := os.WriteFile(filepath.Join(ws.Dir(), "foo.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sha, ok, err := ws.Commit(context.Background(), "AT 00: make-foo")
	if err != nil || !ok || len(sha) < 7 {
		t.Fatalf("commit: sha=%q ok=%v err=%v", sha, ok, err)
	}
	if err := r.FastForward(context.Background(), sha); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	if err := ws.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Integration HEAD now equals the AT's commit, and carries the file.
	head, _ := r.HeadSHA(context.Background())
	if head != sha {
		t.Fatalf("head %q != committed %q", head, sha)
	}
	if got, _ := r.Show(context.Background(), "foo.txt"); got != "hi" {
		t.Fatalf("Show after ff = %q, want hi", got)
	}
}

func TestGitRepoCommitNoChanges(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "run3")
	_ = r.Init(context.Background(), st)
	fork, _ := r.HeadSHA(context.Background())
	ws, err := r.Open(context.Background(), fork, &ATRecord{Slug: "noop", Position: 0})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sha, ok, err := ws.Commit(context.Background(), "empty")
	if ok || sha != "" || err != nil {
		t.Fatalf("empty commit should be (\"\",false,nil), got %q %v %v", sha, ok, err)
	}
	_ = ws.Close()
}

func TestGitRepoResetTo(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "run4")
	_ = r.Init(context.Background(), st)
	base, _ := r.HeadSHA(context.Background())

	// Land a commit, then rewind to base.
	ws, _ := r.Open(context.Background(), base, &ATRecord{Slug: "x", Position: 0})
	_ = os.WriteFile(filepath.Join(ws.Dir(), "x.txt"), []byte("x"), 0o600)
	sha, _, _ := ws.Commit(context.Background(), "x")
	_ = r.FastForward(context.Background(), sha)
	_ = ws.Close()
	if h, _ := r.HeadSHA(context.Background()); h != sha {
		t.Fatalf("expected head at %q", sha)
	}
	if err := r.ResetTo(context.Background(), base); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if h, _ := r.HeadSHA(context.Background()); h != base {
		t.Fatalf("after reset head %q != base %q", h, base)
	}
}

func TestGitRepoListPackageAndShow(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "run5")
	_ = r.Init(context.Background(), st)
	fork, _ := r.HeadSHA(context.Background())
	ws, _ := r.Open(context.Background(), fork, &ATRecord{Slug: "pkg", Position: 0})
	pkgDir := filepath.Join(ws.Dir(), "internal", "foo")
	_ = os.MkdirAll(pkgDir, 0o750)
	_ = os.WriteFile(filepath.Join(pkgDir, "foo.go"), []byte("package foo\n"), 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "foo_test.go"), []byte("package foo\n"), 0o600)
	sha, _, _ := ws.Commit(context.Background(), "pkg")
	_ = r.FastForward(context.Background(), sha)
	_ = ws.Close()

	files, err := r.ListPackage(context.Background(), "internal/foo")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %v", files)
	}
	if body, _ := r.Show(context.Background(), "internal/foo/foo.go"); !strings.Contains(body, "package foo") {
		t.Fatalf("Show foo.go = %q", body)
	}
	if missing, _ := r.Show(context.Background(), "internal/foo/nope.go"); missing != "" {
		t.Fatalf("Show missing should be empty, got %q", missing)
	}
}

func TestGitRepoOpenError(t *testing.T) {
	// A non-git dir makes git commands fail.
	r := NewGitRepo(ExecRunner{}, t.TempDir(), t.TempDir())
	r.integration = "nope"
	if _, err := r.HeadSHA(context.Background()); err == nil {
		t.Fatal("HeadSHA in non-git dir should error")
	}
	if _, err := r.Open(context.Background(), "deadbeef", &ATRecord{Slug: "x"}); err == nil {
		t.Fatal("Open in non-git dir should error")
	}
}
