package risk

import "testing"

// TestBuildTestGateApprovesGitRecovery is the run-9 fix: a coding worker that
// botched an edit can restore/inspect via git (recovery + read-only subcommands),
// where previously every `git` call was blocked and it could not revert.
func TestBuildTestGateApprovesGitRecovery(t *testing.T) {
	g := BuildTestGate()
	for _, cmd := range []string{
		"git restore go/internal/fs/read.go",
		"git checkout -- read.go",
		"git checkout .",
		"git stash",
		"git status",
		"git diff",
		"git show HEAD:read.go",
		"git -C /home/user/dev/repo restore read.go", // global -C <dir> skipped → restore
		"go build ./... && git restore broken.go",     // compound: both segments safe
	} {
		if ok, reason := g.Approve(execCall(cmd), Classify(execCall(cmd))); !ok {
			t.Errorf("build-test gate should approve git recovery %q, denied: %s", cmd, reason)
		}
	}
}

// TestBuildTestGateDeniesUnsafeGit keeps the boundary tight: history, remote, and
// destructive-clean git ops stay denied even though recovery git is now allowed.
func TestBuildTestGateDeniesUnsafeGit(t *testing.T) {
	g := BuildTestGate()
	for _, cmd := range []string{
		"git push origin main",
		"git commit -am wip",
		"git reset --hard HEAD~1",
		"git clean -fd", // would delete untracked files (e.g. the acceptance test)
		"git rebase main",
		"git",                       // bare git: no subcommand
		"go test ./... && git push", // compound: the git push segment is unsafe
		"git -C /repo reset --hard", // global -C skipped → reset (unsafe)
	} {
		ok, reason := g.Approve(execCall(cmd), Classify(execCall(cmd)))
		if ok {
			t.Errorf("build-test gate must deny unsafe git %q", cmd)
		}
		if reason == "" {
			t.Errorf("denial of %q must carry an actionable reason", cmd)
		}
	}
}
