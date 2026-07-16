package coding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Repo is the git seam the orchestrator integrates ATs through: it reports the
// integration branch HEAD (each AT's fork point), gives an AT an isolated working
// directory off that fork point, commits a successful AT's changes, fast-forwards
// the integration branch, and rewinds it (for a branch_fix supersession).
//
// Keeping git behind this seam is what lets the core run loop be git-agnostic and
// sans-IO testable. The default NoopRepo runs every AT directly in the target dir
// with no git — the trivial single-branch path and the test path. A git-backed
// Repo (worktree-per-AT + a real integration branch) layers on without changing
// the orchestrator; it lands with the operator/branch_fix work.
type Repo interface {
	// Init prepares the integration branch for a run (called once by Start).
	Init(ctx context.Context, state *RunState) error
	// HeadSHA returns the integration branch HEAD (the next AT's fork point).
	HeadSHA(ctx context.Context) (string, error)
	// Open gives the AT an isolated workspace forked from forkSHA.
	Open(ctx context.Context, forkSHA string, ar *ATRecord) (Workspace, error)
	// FastForward advances the integration branch to sha.
	FastForward(ctx context.Context, sha string) error
	// ResetTo rewinds the integration branch ref to sha (operator overrides).
	ResetTo(ctx context.Context, sha string) error
	// Promote surfaces a green chain's landed work to the target repo's WORKING
	// TREE: it fast-forwards the repo's checked-out branch to the integration
	// HEAD so a caller's plain filesystem read sees the deliverable (the ATs run
	// in isolated worktrees, so without this the work is stranded on the
	// coding/runs/<id>/integration branch, invisible to the orchestrator). It
	// must be a fast-forward — an error means the tree had diverged or was dirty
	// and the work stays on the integration branch.
	Promote(ctx context.Context) error
}

// Workspace is one AT's isolated working tree.
type Workspace interface {
	// Dir is the absolute path the worker writes into and the gate runs in.
	Dir() string
	// Seed writes files (repo-relative path → content) into the worktree before the
	// worker runs — used to place an AT's authored acceptance oracle. The caller
	// commits afterwards (via Commit) so the seeded files become the diff baseline.
	Seed(files map[string]string) error
	// Commit records the AT's changes, returning the new sha. ok is false when
	// there was nothing to commit (integration HEAD is unchanged).
	Commit(ctx context.Context, msg string) (sha string, ok bool, err error)
	// Close releases the workspace (a worktree is removed on success).
	Close() error
}

// seedFiles writes each (repo-relative path → content) under dir, creating parent
// directories. Shared by the git and noop workspaces. Paths are validated upstream
// (AtomicTask.validate) to be clean + non-escaping.
func seedFiles(dir string, files map[string]string) error {
	for rel, content := range files {
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return fmt.Errorf("seed mkdir %s: %w", rel, err)
		}
		if err := os.WriteFile(dst, []byte(content), 0o600); err != nil {
			return fmt.Errorf("seed write %s: %w", rel, err)
		}
	}
	return nil
}

// NoopRepo is the default git seam: no git at all. Every AT runs in the target
// dir, commits are no-ops, and there is no fork point. It makes a trivial coding
// chain run end-to-end (real gates over real files) and keeps the core testable
// without a git fixture.
type NoopRepo struct {
	// Dir is the working directory every AT runs in (the target repo path).
	Dir string
}

// Init is a no-op (no integration branch under the noop repo).
func (NoopRepo) Init(context.Context, *RunState) error { return nil }

// HeadSHA reports no fork point under the noop repo.
func (NoopRepo) HeadSHA(context.Context) (string, error) { return "", nil }

// Open returns a workspace rooted at the shared target dir.
func (r NoopRepo) Open(context.Context, string, *ATRecord) (Workspace, error) {
	return noopWorkspace{dir: r.Dir}, nil
}

// FastForward is a no-op (no integration branch to move).
func (NoopRepo) FastForward(context.Context, string) error { return nil }

// ResetTo is a no-op (no integration branch to rewind).
func (NoopRepo) ResetTo(context.Context, string) error { return nil }

// Promote is a no-op: every AT already ran in the shared target dir, so the work
// is in the working tree the moment it lands (there is no integration branch to
// surface).
func (NoopRepo) Promote(context.Context) error { return nil }

// noopWorkspace is a shared-dir workspace with no commit.
type noopWorkspace struct{ dir string }

func (w noopWorkspace) Dir() string { return w.dir }

// Seed writes the files into the shared dir (the noop repo has no fork point, so
// the seeded oracle simply pre-exists when the gate runs).
func (w noopWorkspace) Seed(files map[string]string) error { return seedFiles(w.dir, files) }

// Commit reports nothing to commit (the noop repo does not track changes).
func (noopWorkspace) Commit(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (noopWorkspace) Close() error { return nil }
