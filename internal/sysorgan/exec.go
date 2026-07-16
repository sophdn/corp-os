package sysorgan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"corpos/internal/gitenv"
)

// Exec runner contract constants (mirroring the toolkit sys.exec contract, itself
// cannibalized from the harness Bash tool). Model-agnostic: the character cap is
// the only output size guard (no token cap).
const (
	// defaultExecTimeoutMS is the wall-clock timeout when a call sets none.
	defaultExecTimeoutMS = 120_000 // 2 minutes
	// maxExecTimeoutMS is the ceiling a requested timeout is clamped to.
	maxExecTimeoutMS = 600_000 // 10 minutes
	// defaultMaxOutputChars is the combined-output budget when a call sets none.
	defaultMaxOutputChars = 30_000
	// maxOutputCharsUpper is the ceiling the output budget is clamped to.
	maxOutputCharsUpper = 150_000
	// timeoutExitCode is the conventional exit code reported on timeout.
	timeoutExitCode = 124
	// captureCeiling bounds in-memory capture so a runaway command cannot OOM the
	// process. Output beyond this is dropped before truncation accounting.
	captureCeiling = 1 << 20 // 1 MiB
)

// runOptions are the per-call knobs for runner.run.
type runOptions struct {
	Cwd            string
	TimeoutMS      int64
	MaxOutputChars int
}

// runResult is the outcome of a command — the sys.exec success shape.
type runResult struct {
	Output     string `json:"output"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	Cwd        string `json:"cwd"`
}

// runner executes shell commands host-natively (sandbox=none). Its only
// persistent state is the working directory, which a cd inside a command carries
// forward to the next call. Safe for concurrent use; calls serialize on the cwd.
type runner struct {
	mu     sync.Mutex
	cwd    string
	origin string
	shell  string

	// execCommand is the injectable process seam (defaults to exec.Command) so the
	// run path is testable without spawning real processes where that matters.
	execCommand func(name string, arg ...string) *exec.Cmd
}

// newRunner returns a runner rooted at origin (the process working directory when
// empty), resolved to an absolute path.
func newRunner(origin string) (*runner, error) {
	if strings.TrimSpace(origin) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("sys: resolve working directory: %w", err)
		}
		origin = wd
	}
	abs, err := filepath.Abs(origin)
	if err != nil {
		return nil, fmt.Errorf("sys: absolutize %q: %w", origin, err)
	}
	return &runner{cwd: abs, origin: abs, shell: resolveShell(), execCommand: exec.Command}, nil
}

// run executes command and returns its combined output, exit code, and timing.
func (r *runner) run(ctx context.Context, command string, opts runOptions) (runResult, error) {
	if strings.TrimSpace(command) == "" {
		return runResult{}, errors.New("sys.exec: empty command")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	usedOverride := strings.TrimSpace(opts.Cwd) != ""
	cwd := r.cwd
	if usedOverride {
		cwd = opts.Cwd
	}
	cwd = r.recoverCwd(cwd)

	timeout := time.Duration(effectiveTimeoutMS(opts.TimeoutMS)) * time.Millisecond
	maxChars := effectiveMaxOutputChars(opts.MaxOutputChars)

	// A run in the persistent directory appends a pwd capture so a cd inside the
	// command carries forward. A per-call Cwd override is a one-shot detour that
	// never mutates the persistent directory.
	persists := !usedOverride
	var cwdFile string
	toRun := command
	if persists {
		if f, ferr := os.CreateTemp("", "corpos-sys-cwd-*"); ferr == nil {
			cwdFile = f.Name()
			_ = f.Close()
			defer func() { _ = os.Remove(cwdFile) }()
			toRun = command + "\n__corpos_ec=$?\npwd -P > " + shellSingleQuote(cwdFile) + " 2>/dev/null\nexit $__corpos_ec"
		}
	}

	cap := &cappedBuffer{max: captureCeiling}
	// The command is arbitrary by design; the organ gates it upstream with an
	// allowlist + required rationale (dispatch policy).
	cmd := r.execCommand(r.shell, "-c", toRun)
	cmd.Dir = cwd
	cmd.Env = buildEnv(cwd)
	cmd.Stdout = cap
	cmd.Stderr = cap
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group so a timeout kills the subtree

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return runResult{}, fmt.Errorf("sys.exec: start: %w", err)
	}
	pgid := cmd.Process.Pid

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	var timedOut bool
	select {
	case <-runCtx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done // reap
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
	case waitErr = <-done:
	}
	durationMS := time.Since(start).Milliseconds()

	output, truncated := truncateOutput(stripBlankEdges(cap.String()), maxChars)
	exitCode := exitCodeOf(timedOut, waitErr)

	if persists && !timedOut {
		r.adoptCapturedCwd(cwdFile)
	}

	return runResult{
		Output:     output,
		ExitCode:   exitCode,
		TimedOut:   timedOut,
		Truncated:  truncated,
		DurationMS: durationMS,
		Cwd:        r.cwd,
	}, nil
}

// recoverCwd returns cwd if it exists and is a directory; otherwise it falls back
// to the origin, resetting the persistent cwd when that is what vanished.
func (r *runner) recoverCwd(cwd string) string {
	if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
		return cwd
	}
	if cwd == r.cwd {
		r.cwd = r.origin
	}
	return r.origin
}

// adoptCapturedCwd reads the pwd written by the wrapped command and adopts it as
// the persistent working directory when it names an existing directory.
func (r *runner) adoptCapturedCwd(cwdFile string) {
	if cwdFile == "" {
		return
	}
	raw, err := os.ReadFile(cwdFile)
	if err != nil {
		return
	}
	dir := strings.TrimSpace(string(raw))
	if dir == "" {
		return
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		r.cwd = dir
	}
}

// exitCodeOf maps the wait outcome to the contract's exit code.
func exitCodeOf(timedOut bool, waitErr error) int {
	if timedOut {
		return timeoutExitCode
	}
	if waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode() // -1 when terminated by a signal
	}
	return -1
}

func effectiveTimeoutMS(ms int64) int64 {
	if ms <= 0 {
		return defaultExecTimeoutMS
	}
	if ms > maxExecTimeoutMS {
		return maxExecTimeoutMS
	}
	return ms
}

func effectiveMaxOutputChars(n int) int {
	if n <= 0 {
		return defaultMaxOutputChars
	}
	if n > maxOutputCharsUpper {
		return maxOutputCharsUpper
	}
	return n
}

// stripBlankEdges removes leading and trailing whitespace-only lines; interior
// blank lines and intra-line whitespace are preserved.
func stripBlankEdges(s string) string {
	lines := strings.Split(s, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := len(lines) - 1
	for end >= start && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if start > end {
		return ""
	}
	return strings.Join(lines[start:end+1], "\n")
}

// truncateOutput caps content at max characters, appending a marker counting the
// discarded trailing lines.
func truncateOutput(content string, max int) (string, bool) {
	if len(content) <= max {
		return content, false
	}
	head := content[:max]
	remaining := strings.Count(content[max:], "\n") + 1
	return fmt.Sprintf("%s\n\n... [%d lines truncated] ...", head, remaining), true
}

// resolveShell picks the command shell: $SHELL when it names an executable
// bash/zsh, else /bin/bash, else /bin/sh.
func resolveShell() string {
	if sh := os.Getenv("SHELL"); sh != "" && (strings.Contains(sh, "bash") || strings.Contains(sh, "zsh")) && isExecutableFile(sh) {
		return sh
	}
	if isExecutableFile("/bin/bash") {
		return "/bin/bash"
	}
	return "/bin/sh"
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// buildEnv returns the child environment: a clean git context with PWD pinned to cwd. When
// corpos runs from inside a git hook (its own pre-commit gate), the worktree-binding GIT_* vars
// leak into child processes and would silently redirect a worker's git/build commands at the
// WRONG repo; gitenv.Clean strips them (a command that genuinely needs one can still set it
// inline in its own argv). The scrub list lives once in internal/gitenv, so the exec organ and
// the verification gates cannot drift on the hazard.
func buildEnv(cwd string) []string {
	clean := gitenv.Clean()
	out := make([]string, 0, len(clean)+1)
	for _, kv := range clean {
		if strings.HasPrefix(kv, "PWD=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+cwd)
}

// shellSingleQuote wraps s in single quotes, escaping embedded single quotes, so
// it is safe to interpolate into a /bin/sh command.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cappedBuffer is an io.Writer that retains at most max bytes and silently
// discards the rest (reporting full writes so the producer is never blocked).
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if room >= len(p) {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
