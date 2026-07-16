package sysorgan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newTestRunner(t *testing.T, origin string) *runner {
	t.Helper()
	r, err := newRunner(origin)
	if err != nil {
		t.Fatalf("newRunner: %v", err)
	}
	return r
}

func TestRun_BasicCommand(t *testing.T) {
	r := newTestRunner(t, t.TempDir())
	res, err := r.run(context.Background(), "echo hello", runOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Output != "hello" {
		t.Fatalf("output = %q, want %q", res.Output, "hello")
	}
	if res.ExitCode != 0 || res.TimedOut || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	if res.Cwd == "" {
		t.Fatal("cwd should be reported")
	}
}

func TestRun_NonZeroExit(t *testing.T) {
	r := newTestRunner(t, t.TempDir())
	res, err := r.run(context.Background(), "exit 3", runOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit_code = %d, want 3", res.ExitCode)
	}
}

func TestRun_CombinesStdoutStderr(t *testing.T) {
	r := newTestRunner(t, t.TempDir())
	res, _ := r.run(context.Background(), "echo out; echo err 1>&2", runOptions{})
	if !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Fatalf("combined output missing a stream: %q", res.Output)
	}
}

func TestRun_Timeout(t *testing.T) {
	r := newTestRunner(t, t.TempDir())
	res, err := r.run(context.Background(), "sleep 5", runOptions{TimeoutMS: 100})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected timed_out=true")
	}
	if res.ExitCode != timeoutExitCode {
		t.Fatalf("exit_code = %d, want %d", res.ExitCode, timeoutExitCode)
	}
}

func TestRun_OutputTruncation(t *testing.T) {
	r := newTestRunner(t, t.TempDir())
	// Emit ~2000 chars but cap at 100.
	res, _ := r.run(context.Background(), "for i in $(seq 1 200); do echo line$i; done", runOptions{MaxOutputChars: 100})
	if !res.Truncated {
		t.Fatal("expected truncation")
	}
	if !strings.Contains(res.Output, "lines truncated") {
		t.Fatalf("truncation marker missing: %q", res.Output)
	}
}

func TestRun_PersistentCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(t, root)
	// A cd in one call carries forward to the next.
	if _, err := r.run(context.Background(), "cd sub", runOptions{}); err != nil {
		t.Fatal(err)
	}
	res, _ := r.run(context.Background(), "pwd", runOptions{})
	// macOS/Linux may resolve symlinks (/tmp → /private/tmp); compare suffix.
	if !strings.HasSuffix(res.Output, "/sub") {
		t.Fatalf("cwd did not persist: pwd = %q", res.Output)
	}
	if !strings.HasSuffix(r.Cwd(), "/sub") {
		t.Fatalf("runner cwd = %q, want .../sub", r.Cwd())
	}
}

func (r *runner) Cwd() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cwd
}

func TestRun_CwdOverrideIsOneShot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	r := newTestRunner(t, root)
	res, _ := r.run(context.Background(), "pwd", runOptions{Cwd: other})
	if !strings.HasSuffix(res.Output, filepath.Base(other)) {
		t.Fatalf("override cwd not used: %q", res.Output)
	}
	// The persistent cwd is unchanged by a one-shot override.
	if r.Cwd() != root {
		t.Fatalf("persistent cwd mutated by an override: %q want %q", r.Cwd(), root)
	}
}

func TestRun_RecoverCwdWhenVanished(t *testing.T) {
	root := t.TempDir() // origin stays valid
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(t, root)
	if _, err := r.run(context.Background(), "cd sub", runOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sub); err != nil { // the persistent cwd vanishes
		t.Fatal(err)
	}
	// The next run recovers to origin (root, still valid) and executes there.
	res, err := r.run(context.Background(), "echo recovered", runOptions{})
	if err != nil {
		t.Fatalf("run after cwd vanished: %v", err)
	}
	if res.Output != "recovered" {
		t.Fatalf("output = %q", res.Output)
	}
	if r.Cwd() != root {
		t.Fatalf("persistent cwd should reset to origin, got %q", r.Cwd())
	}
}

func TestResolveShell_Branches(t *testing.T) {
	if isExecutableFile("/bin/bash") {
		// $SHELL names an executable bash → used directly (first branch).
		t.Setenv("SHELL", "/bin/bash")
		if got := resolveShell(); got != "/bin/bash" {
			t.Fatalf("SHELL=/bin/bash → %q, want /bin/bash", got)
		}
	}
	// $SHELL is not a bash/zsh executable → fall through to /bin/bash or /bin/sh.
	t.Setenv("SHELL", "/nonexistent/fish")
	if got := resolveShell(); got != "/bin/bash" && got != "/bin/sh" {
		t.Fatalf("fallthrough shell = %q", got)
	}
}

func TestRun_EmptyCommand(t *testing.T) {
	r := newTestRunner(t, t.TempDir())
	if _, err := r.run(context.Background(), "   ", runOptions{}); err == nil {
		t.Fatal("empty command should error")
	}
}

func TestRun_StartError(t *testing.T) {
	r := newTestRunner(t, t.TempDir())
	// Inject a command pointed at a nonexistent binary so Start fails.
	r.execCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command(filepath.Join(t.TempDir(), "does-not-exist-binary"))
	}
	if _, err := r.run(context.Background(), "echo x", runOptions{}); err == nil {
		t.Fatal("expected a start error")
	}
}

func TestEffectiveTimeoutAndOutput(t *testing.T) {
	if effectiveTimeoutMS(0) != defaultExecTimeoutMS {
		t.Fatal("0 → default timeout")
	}
	if effectiveTimeoutMS(maxExecTimeoutMS+1) != maxExecTimeoutMS {
		t.Fatal("over-ceiling timeout should clamp")
	}
	if effectiveTimeoutMS(5000) != 5000 {
		t.Fatal("in-range timeout passes through")
	}
	if effectiveMaxOutputChars(0) != defaultMaxOutputChars {
		t.Fatal("0 → default output budget")
	}
	if effectiveMaxOutputChars(maxOutputCharsUpper+1) != maxOutputCharsUpper {
		t.Fatal("over-ceiling output should clamp")
	}
	if effectiveMaxOutputChars(500) != 500 {
		t.Fatal("in-range output passes through")
	}
}

func TestStripBlankEdges(t *testing.T) {
	cases := map[string]string{
		"\n\n  hi  \n\n":  "  hi  ",
		"a\n\nb":          "a\n\nb", // interior blanks preserved
		"\n\n\n":          "",
		"":                "",
		"only":            "only",
		"  \nmid\n\t\n  ": "mid",
	}
	for in, want := range cases {
		if got := stripBlankEdges(in); got != want {
			t.Errorf("stripBlankEdges(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateOutput(t *testing.T) {
	out, trunc := truncateOutput("short", 100)
	if trunc || out != "short" {
		t.Fatal("short content should not truncate")
	}
	out, trunc = truncateOutput("a\nb\nc\nd", 3)
	if !trunc || !strings.Contains(out, "lines truncated") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
}

func TestExitCodeOf(t *testing.T) {
	if exitCodeOf(true, nil) != timeoutExitCode {
		t.Fatal("timeout → 124")
	}
	if exitCodeOf(false, nil) != 0 {
		t.Fatal("nil wait → 0")
	}
	// A real non-zero exit yields its code.
	err := exec.Command("/bin/sh", "-c", "exit 7").Run()
	if exitCodeOf(false, err) != 7 {
		t.Fatalf("ExitError → its code, got %d", exitCodeOf(false, err))
	}
	// A non-ExitError failure → -1.
	if exitCodeOf(false, os.ErrInvalid) != -1 {
		t.Fatal("non-ExitError → -1")
	}
}

func TestBuildEnvAndShellQuote(t *testing.T) {
	env := buildEnv("/some/dir")
	var pwd string
	pwdCount := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "PWD=") {
			pwd = kv
			pwdCount++
		}
	}
	if pwd != "PWD=/some/dir" || pwdCount != 1 {
		t.Fatalf("PWD not pinned exactly once: %q (count %d)", pwd, pwdCount)
	}
	if got := shellSingleQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellSingleQuote = %q", got)
	}
}

// TestBuildEnv_StripsInheritedGitContext pins that the worktree-binding git
// location variables inherited from the process environment (e.g. a surrounding
// pre-commit hook that exported GIT_DIR) are scrubbed from the child env, so a
// worker's git invocation cannot be retargeted at the wrong repository.
func TestBuildEnv_StripsInheritedGitContext(t *testing.T) {
	t.Setenv("GIT_DIR", "/some/repo/.git")
	t.Setenv("GIT_WORK_TREE", "/some/repo")
	t.Setenv("GIT_INDEX_FILE", "/some/repo/.git/index")
	t.Setenv("GIT_COMMON_DIR", "/some/repo/.git")
	t.Setenv("GIT_PREFIX", "sub/")

	for _, kv := range buildEnv("/some/dir") {
		for _, prefix := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=", "GIT_PREFIX="} {
			if strings.HasPrefix(kv, prefix) {
				t.Fatalf("inherited git context var leaked into child env: %q", kv)
			}
		}
	}
}

// TestRun_DoesNotInheritGitDir is the end-to-end regression for bug 1016: with
// GIT_DIR set in the organ's own environment, a command executed through run
// must NOT observe it (the organ scrubbed it before exec).
func TestRun_DoesNotInheritGitDir(t *testing.T) {
	t.Setenv("GIT_DIR", "/nonexistent/.git")
	t.Setenv("GIT_WORK_TREE", "/nonexistent")
	r := newTestRunner(t, t.TempDir())
	res, err := r.run(context.Background(), "echo \"${GIT_DIR:-clean}/${GIT_WORK_TREE:-clean}\"", runOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Output != "clean/clean" {
		t.Fatalf("git context leaked into the child command: %q", res.Output)
	}
}

func TestResolveShellAndExecutable(t *testing.T) {
	sh := resolveShell()
	if sh == "" || !isExecutableFile(sh) {
		t.Fatalf("resolveShell returned a non-executable: %q", sh)
	}
	if isExecutableFile(t.TempDir()) {
		t.Fatal("a directory is not an executable file")
	}
	if isExecutableFile(filepath.Join(t.TempDir(), "nope")) {
		t.Fatal("a missing path is not an executable file")
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{max: 4}
	n, _ := c.Write([]byte("ab"))
	if n != 2 {
		t.Fatalf("short write should report full length, got %d", n)
	}
	n, _ = c.Write([]byte("cdef")) // only "cd" fits
	if n != 4 {
		t.Fatalf("write should report full length even when capped, got %d", n)
	}
	if c.String() != "abcd" {
		t.Fatalf("buffer = %q, want abcd", c.String())
	}
	n, _ = c.Write([]byte("more")) // no room
	if n != 4 || c.String() != "abcd" {
		t.Fatalf("write past cap: n=%d buf=%q", n, c.String())
	}
}

func TestNewRunner_DefaultOrigin(t *testing.T) {
	r, err := newRunner("")
	if err != nil {
		t.Fatalf("newRunner(empty): %v", err)
	}
	if r.origin == "" || !filepath.IsAbs(r.origin) {
		t.Fatalf("origin not absolutized: %q", r.origin)
	}
}
