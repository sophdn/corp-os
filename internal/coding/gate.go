package coding

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"corpos/internal/gitenv"
)

// Runner runs one command in a working directory and returns its outcome. It is
// the OWNED exec seam: corpos owns command execution for coding chains directly
// (pure-Go os/exec, CGo-free) rather than renting it from a remote surface, so a
// coding chain runs locally without the deployed-container shell constraints. The
// interface keeps the gate sans-IO testable (tests inject a scripted Runner).
type Runner interface {
	Run(ctx context.Context, command []string, dir string, timeout time.Duration) CommandResult
}

// CommandResult records one command invocation.
type CommandResult struct {
	Command  []string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// ExecRunner is the production Runner: it shells out via os/exec. It is pure Go
// (no CGo) so it ships on the same distroless/scratch image as the rest of corpos.
type ExecRunner struct{}

// Run executes command in dir, capturing stdout/stderr. A missing binary maps to
// exit 127 and a timeout to exit 124, matching the bench's command semantics so
// the gate's failure classification is stable across the port.
func (ExecRunner) Run(ctx context.Context, command []string, dir string, timeout time.Duration) CommandResult {
	start := time.Now()
	res := CommandResult{Command: command}
	if len(command) == 0 {
		res.ExitCode = 127
		res.Stderr = "empty command"
		return res
	}
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = cleanGitEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.Duration = time.Since(start)
	res.ExitCode = classifyExit(runCtx, cmd, err)
	if res.ExitCode == 124 {
		res.Stderr = fmt.Sprintf("timeout after %s", timeout)
	}
	return res
}

// classifyExit derives an exit code from a *exec.Cmd run error, distinguishing a
// real non-zero exit, a deadline-exceeded timeout (124), and a binary that could
// not be started (127).
func classifyExit(ctx context.Context, cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	if ctx.Err() == context.DeadlineExceeded {
		return 124
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	// Could not start (binary not found, permission, etc).
	return 127
}

// cleanGitEnv returns the process environment with the worktree-binding git variables removed.
// When corpos is invoked from inside a git hook (e.g. its own pre-commit gate), those vars leak
// into child processes and would silently redirect the gate's git/build commands at the WRONG
// repository. It delegates to gitenv.Clean — the ONE shared definition the agent.spawn
// VerifyGate consumes too — so the two verification paths cannot drift on the git-context scrub.
func cleanGitEnv() []string {
	return gitenv.Clean()
}

// GateTailBytes caps how many trailing bytes of a failing command's stdout/stderr
// the gate retains, keeping diagnostics bounded for the operator + the ledger.
const GateTailBytes = 4096

// Gate is the success oracle for an AT: an ordered list of commands that must all
// exit 0. It is run by the ORCHESTRATOR (never by the worker), which is what makes
// it owned and immutable — a worker cannot run, see, or rewrite its own gate; it
// only receives the gate's diagnostic as revision feedback. An empty gate never
// passes.
type Gate struct {
	Commands [][]string
	Timeout  time.Duration
}

// GateResult is the outcome of one gate evaluation.
type GateResult struct {
	Passed     bool
	Runs       []CommandResult
	Diagnostic string
}

// Run evaluates the gate in dir using runner: it runs each command in order and
// STOPS at the first non-zero exit (fail-fast), returning the runs collected so
// far. Passed is true only if at least one command ran and all exited 0.
func (g Gate) Run(ctx context.Context, runner Runner, dir string) GateResult {
	var res GateResult
	for _, command := range g.Commands {
		run := runner.Run(ctx, command, dir, g.Timeout)
		res.Runs = append(res.Runs, run)
		if run.ExitCode != 0 {
			res.Diagnostic = gateDiagnostic(run)
			return res
		}
	}
	res.Passed = len(res.Runs) > 0
	return res
}

// gateDiagnostic renders a human + operator readable summary of a failing command,
// with stdout/stderr truncated to the tail cap.
func gateDiagnostic(run CommandResult) string {
	return fmt.Sprintf("gate command %q exited %d\nstdout:\n%s\nstderr:\n%s",
		strings.Join(run.Command, " "), run.ExitCode,
		tail(run.Stdout, GateTailBytes), tail(run.Stderr, GateTailBytes))
}

// tail returns the last limit bytes of s, prefixed with a truncation marker when
// content was dropped (UTF-8 safe enough for diagnostics: we cut on a byte
// boundary and let the consumer render replacement runes).
func tail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return "…truncated…\n" + s[len(s)-limit:]
}
