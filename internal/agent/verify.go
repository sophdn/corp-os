package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"corpos/internal/gitenv"
)

// VerifyGate is an ORCHESTRATOR-OWNED verification command the loop runs after the
// agent claims it is done — the self-check that closes a write→verify→revise cycle
// without a human relaying the gate. It is SAFE under risk-gate=enforce because the
// LOOP runs a FIXED command itself (not the agent invoking arbitrary sys.exec): the
// model cannot change what runs, only respond to its output. This is capability-floor
// gap #1 (escalate-on-fail must key off an orchestrator-owned gate, not the worker's
// self-report) applied to the general agent loop.
type VerifyGate struct {
	// Command is the fixed gate as an argv vector, e.g. ["go","test","./..."].
	Command []string
	// Dir is the working directory the gate runs in.
	Dir string
	// MaxRounds bounds the verify-fail → revise cycles (<=0 → default 3).
	MaxRounds int
	// Timeout bounds one gate run (<=0 → default 5m).
	Timeout time.Duration
	// run executes the gate and returns (exitCode, combinedOutput); injectable for
	// tests. nil → execVerify (pure-Go os/exec).
	run func(ctx context.Context, command []string, dir string, timeout time.Duration) (int, string)
}

const (
	defaultVerifyMaxRounds = 3
	defaultVerifyTimeout   = 5 * time.Minute
	verifyOutputTail       = 4096
)

// maxRoundsOrDefault returns the configured verify-revise ceiling.
func (g *VerifyGate) maxRoundsOrDefault() int {
	if g.MaxRounds > 0 {
		return g.MaxRounds
	}
	return defaultVerifyMaxRounds
}

// check runs the gate and reports whether it passed plus its tail-capped output.
func (g *VerifyGate) check(ctx context.Context) (bool, string) {
	return g.checkScoped(ctx, nil)
}

// checkScoped runs the gate with `go test ./...` narrowed to pkgs (see ScopeGoTest), so a
// single-package change isn't gated on the whole-repo test suite (which times out on a
// large repo). nil/empty pkgs runs the gate command unchanged.
func (g *VerifyGate) checkScoped(ctx context.Context, pkgs []string) (bool, string) {
	run := g.run
	if run == nil {
		run = execVerify
	}
	to := g.Timeout
	if to <= 0 {
		to = defaultVerifyTimeout
	}
	exit, out := run(ctx, ScopeGoTest(g.Command, pkgs), g.Dir, to)
	return exit == 0, tailString(out, verifyOutputTail)
}

// execVerify runs the gate command via os/exec (pure Go, CGo-free). A missing
// binary maps to 127 and a timeout to 124. It runs in a clean git context (the
// worktree-binding GIT_* vars are stripped) so an ambient git hook env can't
// redirect the gate at the wrong repository.
func execVerify(ctx context.Context, command []string, dir string, timeout time.Duration) (int, string) {
	if len(command) == 0 {
		return 127, "verify: empty command"
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
	var b strings.Builder
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	out := b.String()
	if runCtx.Err() == context.DeadlineExceeded {
		return 124, out + "\n(verify timed out)"
	}
	if err == nil {
		return 0, out
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if c := exitErr.ExitCode(); c >= 0 {
			return c, out
		}
		return 1, out
	}
	return 127, out + "\n" + err.Error()
}

// cleanGitEnv returns the process environment minus the worktree-binding git vars (an ambient
// git hook exports these; inheriting them would point git/build at the wrong repo). It
// delegates to gitenv.Clean — the ONE shared definition the coding gate's Runner consumes too
// — so the agent.spawn and coding verification paths cannot drift on the git-context scrub.
func cleanGitEnv() []string {
	return gitenv.Clean()
}

// tailString returns the last limit bytes of s with a truncation marker when cut.
func tailString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return "…truncated…\n" + s[len(s)-limit:]
}

// isGoBuildGate reports whether command is a go build/test gate — the only verify_command shape
// in use (e.g. ["go","build","./..."], ["go","test","./..."], or ["sh","-c","go build ./... && go
// test ./..."]). The runnable-gate precondition (a reachable go.mod) is specific to this gate, so
// the check keys off it; a non-go gate is left unvalidated (its precondition is its own concern).
func isGoBuildGate(command []string) bool {
	for _, tok := range command {
		f := strings.Fields(tok)
		for i, w := range f {
			if w == "go" && i+1 < len(f) && (f[i+1] == "build" || f[i+1] == "test" || f[i+1] == "vet") {
				return true
			}
		}
		// A bare argv ["go","build",...] has "go" and "build" as adjacent tokens.
		if tok == "go" {
			return true
		}
	}
	if len(command) >= 2 && command[0] == "go" &&
		(command[1] == "build" || command[1] == "test" || command[1] == "vet") {
		return true
	}
	return false
}

// goModReachable reports whether a go.mod is reachable from dir — present in dir or any ancestor
// (the module root the `go` tool walks up to find). An empty dir means the process CWD. It is the
// runnable precondition for a go build/test gate: without it `go build ./...` can only error
// (no module), which is structurally distinct from a RED test result.
func goModReachable(dir string) bool {
	d := dir
	if d == "" {
		if cwd, err := os.Getwd(); err == nil {
			d = cwd
		} else {
			return false
		}
	}
	d = filepath.Clean(d)
	for {
		if fi, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !fi.IsDir() {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// moduleSearchMaxDepth bounds the downward go.mod search so resolution stays cheap on a
// real repo (a go/-submodule sits at depth 1; depth 2 covers the odd extra nesting).
const moduleSearchMaxDepth = 2

// ResolveGoModuleDir returns the directory a go build/test gate should run in, given a
// configured working dir, plus whether a module was found. If a go.mod is reachable at
// dir or any ANCESTOR (the layout the `go` tool itself handles), dir is returned
// unchanged. Otherwise it searches the subtree under dir (bounded depth, skipping
// vendor/.git/node_modules/testdata) for the SHALLOWEST go.mod and returns that module
// root — the common subdir-module monorepo layout (e.g. a repo whose module is in go/).
// Multiple modules at the same shallowest depth are ambiguous and a tree with none both
// return ("", false), so the caller keeps the actionable not-runnable error rather than
// guessing (bug 1094). An empty dir means the process CWD.
func ResolveGoModuleDir(dir string) (string, bool) {
	d := dir
	if d == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		d = cwd
	}
	d = filepath.Clean(d)
	if goModReachable(d) {
		return d, true
	}
	// Search down for the shallowest go.mod; a unique one at the shallowest depth wins.
	var found []string
	foundDepth := -1
	_ = filepath.WalkDir(d, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // tolerate a per-entry error; keep searching the rest
		}
		depth := depthUnder(d, path)
		if de.IsDir() {
			name := de.Name()
			if path != d && (name == "vendor" || name == ".git" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			if depth > moduleSearchMaxDepth {
				return filepath.SkipDir // past the search cap
			}
			if foundDepth >= 0 && depth > foundDepth {
				return filepath.SkipDir // a deeper module can't be the shallowest
			}
			return nil
		}
		if de.Name() != "go.mod" {
			return nil
		}
		modDir := filepath.Dir(path)
		switch md := depthUnder(d, modDir); {
		case foundDepth < 0 || md < foundDepth:
			found, foundDepth = []string{modDir}, md // a new shallowest
		case md == foundDepth:
			found = append(found, modDir) // a tie at the shallowest depth → ambiguous
		}
		return nil
	})
	if len(found) == 1 {
		return found[0], true
	}
	return "", false
}

// depthUnder returns how many path segments p is below base (base itself = 0).
func depthUnder(base, p string) int {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

// VerifyGateRunnable validates AT SPAWN that a configured verify gate can actually run in its
// working dir — distinguishing "the gate is misconfigured / cannot run here" from a normal RED
// result, so a non-functional gate fails fast with an actionable message instead of being handed
// to the worker (who, in the 2026-06-11 dogfood, gamed it by fabricating the module it checks).
// It validates the only verify_command in use, the go build/test gate: the verify-dir must have a
// reachable go.mod. A nil/empty command or a non-go gate is runnable (nothing to precheck). The
// returned error is the operator-actionable next step. Both the spawner (Spawner.Run) and the
// direct -verify CLI path call it, so the two cannot drift on the runnable precondition.
func VerifyGateRunnable(command []string, dir string) error {
	if len(command) == 0 {
		return nil
	}
	if !isGoBuildGate(command) {
		return nil
	}
	if _, ok := ResolveGoModuleDir(dir); !ok {
		where := dir
		if where == "" {
			where = "the process working directory"
		}
		return fmt.Errorf("verify-dir %s has no go.mod reachable at/above it and no unique module in its subtree — the gate cannot run here; point -verify-dir at the module root: %w", where, ErrVerifyGateUnrunnable)
	}
	return nil
}

// ErrVerifyGateUnrunnable marks a verify-gate PRECONDITION failure: the configured go
// build/test gate cannot run in its working dir (no reachable or unique go.mod). It is a
// worker-config error no stronger MODEL can fix, so the spawn boundary classifies a Run
// error wrapping it as ClassUsage (non-escalatable, bug 1095) — distinct from a genuine
// worker/organ runtime fault, which stays ClassTool. Callers test it with errors.Is.
var ErrVerifyGateUnrunnable = errors.New("verify gate not runnable")
