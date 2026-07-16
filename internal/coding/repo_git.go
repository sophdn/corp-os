package coding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitRepo is the git-backed integration seam: each AT runs in an isolated worktree
// forked off the integration branch HEAD, and a green AT's commit fast-forwards the
// branch. A branch_fix rewinds the branch ref to a fork point. All git is run
// through the owned exec Runner (no go-git dependency, CGo-free).
type GitRepo struct {
	runner      Runner
	repoDir     string // the target repo
	worktreeDir string // where per-AT worktrees are created
	integration string // the integration branch name
	runID       string
}

// NewGitRepo builds a git Repo over the owned runner. repoDir is the target repo;
// worktreeDir is a scratch directory the per-AT worktrees live under.
func NewGitRepo(runner Runner, repoDir, worktreeDir string) *GitRepo {
	return &GitRepo{runner: runner, repoDir: repoDir, worktreeDir: worktreeDir}
}

// git runs a git subcommand in dir and returns trimmed stdout, or an error
// carrying the stderr tail.
func (r *GitRepo) git(ctx context.Context, dir string, args ...string) (string, error) {
	run := r.runner.Run(ctx, append([]string{"git", "-C", dir}, args...), dir, 0)
	if run.ExitCode != 0 {
		return "", fmt.Errorf("git %s (exit %d): %s", strings.Join(args, " "), run.ExitCode, tail(run.Stderr, GateTailBytes))
	}
	return strings.TrimSpace(run.Stdout), nil
}

// Init creates the integration branch at the base branch HEAD.
func (r *GitRepo) Init(ctx context.Context, state *RunState) error {
	r.integration = state.IntegrationBranch
	r.runID = state.RunID
	base := state.BaseBranch
	if base == "" {
		// No caller-pinned base: fork off the repo's ACTUAL default/current branch.
		// Hardcoding "main" here failed on master-default (or trunk/develop/detached)
		// repos with "fatal: not a valid object name: 'main'" (bug 1077).
		resolved, err := r.resolveBaseRef(ctx)
		if err != nil {
			return err
		}
		base = resolved
	}
	// Force-create the integration branch at base (idempotent across re-runs).
	if _, err := r.git(ctx, r.repoDir, "branch", "-f", r.integration, base); err != nil {
		return err
	}
	return nil
}

// resolveBaseRef returns the repo's current branch name, falling back to the HEAD
// commit SHA for a detached HEAD. This is the single seam that turns a caller's
// empty base into a concrete ref, so any caller (BridgeChain, operator seat) forks
// off whatever the target repo actually checked out.
func (r *GitRepo) resolveBaseRef(ctx context.Context) (string, error) {
	// symbolic-ref --short -q HEAD prints the current branch and exits non-zero
	// (quietly) when HEAD is detached; fall back to the commit SHA in that case.
	if branch, err := r.git(ctx, r.repoDir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && branch != "" {
		return branch, nil
	}
	sha, err := r.git(ctx, r.repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve base ref: %w", err)
	}
	return sha, nil
}

// HeadSHA returns the integration branch HEAD.
func (r *GitRepo) HeadSHA(ctx context.Context) (string, error) {
	return r.git(ctx, r.repoDir, "rev-parse", r.integration)
}

// Open creates an isolated worktree for the AT forked from forkSHA.
func (r *GitRepo) Open(ctx context.Context, forkSHA string, ar *ATRecord) (Workspace, error) {
	suffix := ""
	if ar.BranchIndex > 0 {
		suffix = fmt.Sprintf("-b%d", ar.BranchIndex)
	}
	name := fmt.Sprintf("%02d-%s%s", ar.Position, ar.Slug, suffix)
	path := filepath.Join(r.worktreeDir, name)
	branch := fmt.Sprintf("coding/runs/%s/at%s", r.runID, name)
	from := forkSHA
	if from == "" {
		from = r.integration
	}
	// Re-opening the SAME AT slot must RESET its worktree, not collide. An AT is re-run
	// in place when InterveneEdit resets the failed AT (a corrected-goal retry) or
	// branch_fix resets a downstream failed AT — runAT then calls Open again with the
	// same (branch, path). The prior FAILED attempt's worktree is PRESERVED (closed only
	// on success, for operator diff inspection), so `git worktree add -B` would fatal with
	// "cannot force update the branch ... used by worktree", aborting the organ before any
	// capable rung engages (bug operator-seat-branch-fix-readds-same-worktree-branch-git-
	// exit-255). Best-effort tear down any worktree already at this path first (its diff was
	// captured before the reset), releasing the branch so the add recreates it clean; prune
	// clears a registration whose dir was already removed.
	_, _ = r.git(ctx, r.repoDir, "worktree", "remove", "--force", path)
	_, _ = r.git(ctx, r.repoDir, "worktree", "prune")
	if _, err := r.git(ctx, r.repoDir, "worktree", "add", "-f", "-B", branch, path, from); err != nil {
		return nil, err
	}
	return &gitWorkspace{repo: r, dir: path, branch: branch}, nil
}

// FastForward advances the integration branch ref to sha.
func (r *GitRepo) FastForward(ctx context.Context, sha string) error {
	_, err := r.git(ctx, r.repoDir, "update-ref", "refs/heads/"+r.integration, sha)
	return err
}

// ResetTo rewinds the integration branch ref to sha (a branch_fix supersession).
func (r *GitRepo) ResetTo(ctx context.Context, sha string) error {
	_, err := r.git(ctx, r.repoDir, "update-ref", "refs/heads/"+r.integration, sha)
	return err
}

// Promote surfaces the integration branch to the target repo's working tree by
// fast-forwarding its checked-out branch to the integration HEAD. The ATs commit
// only to the isolated coding/runs/<id>/integration ref (FastForward uses
// update-ref, which never touches the working tree), so without this a green
// chain's deliverable is invisible to a caller's plain fs.read of repoDir — the
// orchestrator reads a stale tree and cannot reconcile the worker's success.
//
// --ff-only is deliberate: the integration branch was forked off this branch's
// HEAD and only adds commits, so a clean tree fast-forwards; a dirty or diverged
// tree makes git refuse rather than create a merge or discard local edits, and
// the work stays safe on the integration branch (the error is surfaced upward).
func (r *GitRepo) Promote(ctx context.Context) error {
	if _, err := r.git(ctx, r.repoDir, "merge", "--ff-only", r.integration); err != nil {
		return fmt.Errorf("promote integration to working tree: %w", err)
	}
	return nil
}

// Show returns the contents of path at the integration branch HEAD (or "" if the
// file does not exist there). Used to assemble current-package context (Finding 0).
func (r *GitRepo) Show(ctx context.Context, path string) (string, error) {
	run := r.runner.Run(ctx, []string{"git", "-C", r.repoDir, "show", r.integration + ":" + path}, r.repoDir, 0)
	if run.ExitCode != 0 {
		return "", nil
	}
	return run.Stdout, nil
}

// ListPackage returns the .go file paths under dir at the integration branch HEAD.
func (r *GitRepo) ListPackage(ctx context.Context, dir string) ([]string, error) {
	out, err := r.git(ctx, r.repoDir, "ls-tree", "-r", "--name-only", r.integration, "--", dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Diff returns the unified diff between two commits on the target repo.
func (r *GitRepo) Diff(ctx context.Context, fromSHA, toSHA string) (string, error) {
	run := r.runner.Run(ctx, []string{"git", "-C", r.repoDir, "diff", fromSHA + ".." + toSHA}, r.repoDir, 0)
	if run.ExitCode != 0 {
		return "", fmt.Errorf("git diff: %s", tail(run.Stderr, GateTailBytes))
	}
	return run.Stdout, nil
}

// DiffWorktree returns the unified diff of a (failed) worktree against fromSHA,
// staging everything first so untracked files the worker wrote are captured.
func (r *GitRepo) DiffWorktree(ctx context.Context, dir, fromSHA string) (string, error) {
	if _, err := r.git(ctx, dir, "add", "-A"); err != nil {
		return "", err
	}
	run := r.runner.Run(ctx, []string{"git", "-C", dir, "diff", "--cached", fromSHA, "--"}, dir, 0)
	if run.ExitCode != 0 {
		return "", fmt.Errorf("git diff worktree: %s", tail(run.Stderr, GateTailBytes))
	}
	return run.Stdout, nil
}

// RunTestsAtRefWithOverlay implements the red-before-green replay (redGreenRepo). It
// materializes refSHA in a throwaway detached worktree (the PRE-fix tree — the production
// fix is absent there), overlays the worker's changed test files from srcDir onto it (so
// the newly-added test functions exist), then runs each spec's functions via `go test -run`.
// The throwaway worktree is removed before returning. Results align 1:1 with specs; a spec
// that exits 0 means its tests pass on unfixed code (a tautology). Overlay sources that no
// longer exist (a deleted test file) are skipped.
func (r *GitRepo) RunTestsAtRefWithOverlay(ctx context.Context, refSHA, srcDir string, overlayFiles []string, specs []testRunSpec, timeout time.Duration) ([]CommandResult, error) {
	scratch := filepath.Join(r.worktreeDir, "redgreen-"+randomRunID())
	if _, err := r.git(ctx, r.repoDir, "worktree", "add", "-f", "--detach", scratch, refSHA); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = r.git(context.Background(), r.repoDir, "worktree", "remove", "--force", scratch)
	}()
	for _, rel := range overlayFiles {
		data, err := os.ReadFile(filepath.Join(srcDir, rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue // a deleted test file has no post-image to overlay
			}
			return nil, fmt.Errorf("overlay read %s: %w", rel, err)
		}
		dst := filepath.Join(scratch, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return nil, fmt.Errorf("overlay mkdir %s: %w", rel, err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, fmt.Errorf("overlay write %s: %w", rel, err)
		}
	}
	results := make([]CommandResult, 0, len(specs))
	for _, spec := range specs {
		cmd := []string{"go", "test", "-count=1", "-run", "^(" + strings.Join(spec.Funcs, "|") + ")$", "./" + spec.Pkg}
		results = append(results, r.runner.Run(ctx, cmd, scratch, timeout))
	}
	return results, nil
}

// gitWorkspace is one AT's worktree.
type gitWorkspace struct {
	repo   *GitRepo
	dir    string
	branch string
}

func (w *gitWorkspace) Dir() string { return w.dir }

// Seed writes the files into the worktree (the caller commits them as the diff
// baseline so the orchestrator's own seeded oracle is not flagged as worker edits).
func (w *gitWorkspace) Seed(files map[string]string) error { return seedFiles(w.dir, files) }

// Commit stages everything and commits. ok is false when there is nothing to
// commit (the worker wrote nothing), leaving the integration HEAD unchanged.
func (w *gitWorkspace) Commit(ctx context.Context, msg string) (string, bool, error) {
	if _, err := w.repo.git(ctx, w.dir, "add", "-A"); err != nil {
		return "", false, err
	}
	// `diff --cached --quiet` exits 1 when there are staged changes.
	check := w.repo.runner.Run(ctx, []string{"git", "-C", w.dir, "diff", "--cached", "--quiet"}, w.dir, 0)
	if check.ExitCode == 0 {
		return "", false, nil // nothing staged
	}
	if _, err := w.repo.git(ctx, w.dir, "commit", "-m", msg); err != nil {
		return "", false, err
	}
	sha, err := w.repo.git(ctx, w.dir, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}

// Close removes the worktree (best-effort; preserved-on-failure is the caller's
// concern — it only closes on the success path).
func (w *gitWorkspace) Close() error {
	_, err := w.repo.git(context.Background(), w.repo.repoDir, "worktree", "remove", "--force", w.dir)
	return err
}
