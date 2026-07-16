package coding

import (
	"context"
	"strings"
	"testing"
	"time"
)

// funcRunner adapts a func into a Runner for scripted gate/contract tests.
type funcRunner func(command []string, dir string) CommandResult

func (f funcRunner) Run(_ context.Context, command []string, dir string, _ time.Duration) CommandResult {
	return f(command, dir)
}

// okRunner exits 0 for every command. failRunner exits 1.
func okRunner() Runner {
	return funcRunner(func(cmd []string, _ string) CommandResult { return CommandResult{Command: cmd} })
}

func TestGateEmptyNeverPasses(t *testing.T) {
	res := Gate{}.Run(context.Background(), okRunner(), "/tmp")
	if res.Passed {
		t.Fatal("empty gate must not pass")
	}
}

func TestGateAllPass(t *testing.T) {
	g := Gate{Commands: [][]string{{"a"}, {"b"}}}
	res := g.Run(context.Background(), okRunner(), "/tmp")
	if !res.Passed || len(res.Runs) != 2 {
		t.Fatalf("want passed with 2 runs, got passed=%v runs=%d", res.Passed, len(res.Runs))
	}
}

func TestGateFailFast(t *testing.T) {
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		if cmd[0] == "b" {
			return CommandResult{Command: cmd, ExitCode: 2, Stderr: "boom"}
		}
		return CommandResult{Command: cmd}
	})
	g := Gate{Commands: [][]string{{"a"}, {"b"}, {"c"}}}
	res := g.Run(context.Background(), r, "/tmp")
	if res.Passed {
		t.Fatal("gate should fail")
	}
	if len(res.Runs) != 2 {
		t.Fatalf("fail-fast should stop after the failing command, ran %d", len(res.Runs))
	}
	if !strings.Contains(res.Diagnostic, "exited 2") || !strings.Contains(res.Diagnostic, "boom") {
		t.Fatalf("diagnostic missing detail: %q", res.Diagnostic)
	}
}

func TestTailTruncates(t *testing.T) {
	if got := tail("short", GateTailBytes); got != "short" {
		t.Fatalf("short string should pass through, got %q", got)
	}
	big := strings.Repeat("x", GateTailBytes+100)
	got := tail(big, GateTailBytes)
	if !strings.HasPrefix(got, "…truncated…\n") {
		t.Fatalf("want truncation marker, got prefix %q", got[:20])
	}
	if len(got) != len("…truncated…\n")+GateTailBytes {
		t.Fatalf("truncated length wrong: %d", len(got))
	}
}

// --- ExecRunner: real os/exec, CGo-free ---

func TestExecRunnerSuccess(t *testing.T) {
	res := ExecRunner{}.Run(context.Background(), []string{"true"}, t.TempDir(), 0)
	if res.ExitCode != 0 {
		t.Fatalf("`true` should exit 0, got %d (%s)", res.ExitCode, res.Stderr)
	}
}

func TestExecRunnerNonZero(t *testing.T) {
	res := ExecRunner{}.Run(context.Background(), []string{"false"}, t.TempDir(), 0)
	if res.ExitCode != 1 {
		t.Fatalf("`false` should exit 1, got %d", res.ExitCode)
	}
}

func TestExecRunnerCapturesStdout(t *testing.T) {
	res := ExecRunner{}.Run(context.Background(), []string{"sh", "-c", "echo hello"}, t.TempDir(), 0)
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("stdout = %q, want hello", res.Stdout)
	}
}

func TestExecRunnerEmptyCommand(t *testing.T) {
	res := ExecRunner{}.Run(context.Background(), nil, t.TempDir(), 0)
	if res.ExitCode != 127 {
		t.Fatalf("empty command should be 127, got %d", res.ExitCode)
	}
}

func TestExecRunnerNotFound(t *testing.T) {
	res := ExecRunner{}.Run(context.Background(), []string{"corpos-no-such-binary-xyz"}, t.TempDir(), 0)
	if res.ExitCode != 127 {
		t.Fatalf("missing binary should be 127, got %d", res.ExitCode)
	}
}

func TestExecRunnerTimeout(t *testing.T) {
	res := ExecRunner{}.Run(context.Background(), []string{"sleep", "5"}, t.TempDir(), 30*time.Millisecond)
	if res.ExitCode != 124 {
		t.Fatalf("timeout should be 124, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "timeout") {
		t.Fatalf("timeout stderr = %q", res.Stderr)
	}
}

func TestCleanGitEnvStripsWorktreeVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/some/repo/.git")
	t.Setenv("GIT_WORK_TREE", "/some/repo")
	t.Setenv("CORPOS_KEEP", "yes")
	env := cleanGitEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") {
			t.Fatalf("git context var leaked: %q", kv)
		}
	}
	found := false
	for _, kv := range env {
		if kv == "CORPOS_KEEP=yes" {
			found = true
		}
	}
	if !found {
		t.Fatal("non-git env vars must be preserved")
	}
}

func TestExecRunnerRunsInCleanGitContext(t *testing.T) {
	// With GIT_DIR pointing elsewhere, a git command via ExecRunner must operate on
	// its -C target, not the inherited GIT_DIR.
	t.Setenv("GIT_DIR", "/nonexistent/.git")
	res := ExecRunner{}.Run(context.Background(), []string{"sh", "-c", "echo ${GIT_DIR:-clean}"}, t.TempDir(), 0)
	if strings.TrimSpace(res.Stdout) != "clean" {
		t.Fatalf("GIT_DIR should be stripped from the child env, got %q", res.Stdout)
	}
}
