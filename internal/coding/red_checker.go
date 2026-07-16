package coding

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// GitRedChecker is the production RedChecker: it overlays an authored oracle at a fork ref
// in a throwaway detached worktree and runs the oracle's acceptance test there, reporting
// RED iff the run exits non-zero. This is the anti-vacuous-oracle trust anchor — a model-
// authored test referencing a not-yet-built feature symbol fails to COMPILE on the fork
// tree, which is a non-zero exit and therefore correctly red. A test that already passes
// on the unbuilt tree asserts nothing, and the author is told to revise it.
type GitRedChecker struct {
	repo    *GitRepo
	forkSHA string
	timeout time.Duration
}

// NewGitRedChecker builds a checker over a target GitRepo and the fork ref the oracle must
// be red against (typically the repo HEAD before the feature is built).
func NewGitRedChecker(repo *GitRepo, forkSHA string, timeout time.Duration) *GitRedChecker {
	return &GitRedChecker{repo: repo, forkSHA: forkSHA, timeout: timeout}
}

// OracleIsRed stages the in-memory oracle source into a throwaway dir and runs its
// acceptance func at the fork ref via the red-before-green overlay machinery, returning
// red (non-zero exit), a short diagnostic tail, and any infrastructure error.
func (c *GitRedChecker) OracleIsRed(ctx context.Context, oracle AuthoredOracle) (bool, string, error) {
	srcDir, err := os.MkdirTemp("", "oracle-redcheck-")
	if err != nil {
		return false, "", err
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	dst := filepath.Join(srcDir, oracle.TestPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, "", err
	}
	if err := os.WriteFile(dst, []byte(oracle.TestSource), 0o600); err != nil {
		return false, "", err
	}
	specs := []testRunSpec{{Pkg: filepath.Dir(oracle.TestPath), Funcs: []string{oracle.TestFunc}}}
	results, err := c.repo.RunTestsAtRefWithOverlay(ctx, c.forkSHA, srcDir, []string{oracle.TestPath}, specs, c.timeout)
	if err != nil {
		return false, "", err
	}
	if len(results) == 0 {
		return false, "no result", nil
	}
	return results[0].ExitCode != 0, tail(results[0].Stderr+results[0].Stdout, 400), nil
}
