package coding

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestGitRepoReopenSameATResetsWorktree is the regression for the operator-seat
// branch_fix worktree collision (bug
// operator-seat-branch-fix-readds-same-worktree-branch-git-exit-255). When an AT is
// RE-RUN in place — InterveneEdit resets the failed AT to pending with a corrected
// goal, and branch_fix resets a downstream failed AT — runAT calls repo.Open again
// with the SAME (branch, path). The prior FAILED attempt's worktree is preserved
// (closed only on success), so a naive `git worktree add -f -B` fatals with
// "cannot force update the branch ... used by worktree", aborting the whole organ
// before any capable rung engages. Open must instead reset the stale worktree and
// succeed.
func TestGitRepoReopenSameATResetsWorktree(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "reopen1")
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init: %v", err)
	}
	fork, _ := r.HeadSHA(context.Background())

	ar := &ATRecord{Slug: "duty-fix1", Position: 0, BranchIndex: 1}

	// First open: the attempt writes a file but FAILS the gate, so the orchestrator
	// preserves the worktree (no Close) for operator inspection.
	ws1, err := r.Open(context.Background(), fork, ar)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws1.Dir(), "attempt.txt"), []byte("v1"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// (no Close — the failed worktree lingers, exactly as runAT leaves it)

	// Re-open the SAME AT slot (the edit-retry / branch_fix-reset path). This must NOT
	// collide on the in-use branch — it resets the worktree and returns a fresh one.
	ws2, err := r.Open(context.Background(), fork, ar)
	if err != nil {
		t.Fatalf("re-open same AT must succeed (got the branch_fix collision): %v", err)
	}
	if ws2.Dir() != ws1.Dir() {
		t.Fatalf("re-open should reuse the AT's path; got %q then %q", ws1.Dir(), ws2.Dir())
	}
	// The reset worktree is clean (forked fresh from the fork point), so the prior
	// attempt's uncommitted file is gone — a genuine retry, not a dirty resume.
	if _, err := os.Stat(filepath.Join(ws2.Dir(), "attempt.txt")); !os.IsNotExist(err) {
		t.Fatalf("re-opened worktree should be reset to the fork point (stale file present): err=%v", err)
	}
	// The fresh worktree is usable: a write + commit + fast-forward round-trips.
	if err := os.WriteFile(filepath.Join(ws2.Dir(), "fix.txt"), []byte("real"), 0o600); err != nil {
		t.Fatalf("write after re-open: %v", err)
	}
	sha, ok, err := ws2.Commit(context.Background(), "AT 00: duty-fix1 (retry)")
	if err != nil || !ok || len(sha) < 7 {
		t.Fatalf("commit after re-open: sha=%q ok=%v err=%v", sha, ok, err)
	}
	if err := r.FastForward(context.Background(), sha); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	if err := ws2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, _ := r.Show(context.Background(), "fix.txt"); got != "real" {
		t.Fatalf("Show after retry ff = %q, want real", got)
	}
}
