package coding

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ParityResult is the outcome of a forward-parity check: the chain went green, but
// do the REFERENCE tests pass on its final implementation?
type ParityResult struct {
	// Compiled is false when the reference tests do not even compile against the
	// impl (an API divergence — the impl named things differently than ground truth).
	Compiled bool
	// Passed is true when the reference tests pass — real correctness.
	Passed bool
	// Failures are the failing test lines (compiled && !passed).
	Failures []string
	// FalsePass is the load-bearing signal: compiled but not passing, AND not fully
	// explained by the SPEC-ambiguity allowlist. A true value means the chain's
	// green is not real (wrong impl, or a test was mutated to match a wrong impl).
	FalsePass bool
	// AllowlistedFailures are failing lines suppressed by the SPEC-ambiguity
	// allowlist (recorded for audit — suppression is never silent).
	AllowlistedFailures []string
	// OutputTail is the tail of the test output for inspection.
	OutputTail string
}

// ForwardParity swaps the reference *_test.go for each package into a copy of the
// final integration tree and runs the test suite, classifying the result. The
// SPEC-ambiguity allowlist suppresses known-defensible divergences (e.g. an
// underspecified no-args behavior both sides implement reasonably) so the guard can
// gate autonomously without false alarms — but it records what it suppressed.
func ForwardParity(ctx context.Context, runner Runner, srcDir, referenceDir string, packages []string, testCmd []string, allowlist []string) (ParityResult, error) {
	work, err := os.MkdirTemp("", "coding-parity-")
	if err != nil {
		return ParityResult{}, fmt.Errorf("parity workdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	if err := copyTree(srcDir, work); err != nil {
		return ParityResult{}, fmt.Errorf("copy src tree: %w", err)
	}
	for _, pkg := range packages {
		if err := swapReferenceTests(referenceDir, work, pkg); err != nil {
			return ParityResult{}, fmt.Errorf("swap reference tests for %q: %w", pkg, err)
		}
	}

	cmd := testCmd
	if len(cmd) == 0 {
		cmd = []string{"go", "test", "./..."}
	}
	run := runner.Run(ctx, cmd, work, 0)
	out := run.Stdout + run.Stderr

	res := ParityResult{
		Compiled:   !strings.Contains(out, "[build failed]"),
		Passed:     run.ExitCode == 0,
		OutputTail: tail(out, GateTailBytes),
	}
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "--- FAIL") || strings.HasPrefix(t, "FAIL") || strings.HasPrefix(t, "# ") {
			res.Failures = append(res.Failures, t)
		}
	}
	res.FalsePass = classifyFalsePass(&res, allowlist)
	return res, nil
}

// classifyFalsePass sets the false-pass verdict: a compiled-but-failing result is a
// real false pass UNLESS every failing line is covered by the SPEC-ambiguity
// allowlist. It records the allowlisted failures on the result. A non-compiling
// result is its own (API-divergence) signal, not a false pass.
func classifyFalsePass(res *ParityResult, allowlist []string) bool {
	if !res.Compiled || res.Passed {
		return false
	}
	unexplained := false
	for _, f := range res.Failures {
		if matchesAllowlist(f, allowlist) {
			res.AllowlistedFailures = append(res.AllowlistedFailures, f)
			continue
		}
		// A bare "FAIL\tpkg" summary line is implied by a specific failure already
		// classified; only count substantive "--- FAIL"/"# pkg" lines as unexplained.
		if strings.HasPrefix(f, "FAIL") {
			continue
		}
		unexplained = true
	}
	return unexplained
}

// matchesAllowlist reports whether a failing line matches any SPEC-ambiguity
// allowlist entry (substring match).
func matchesAllowlist(line string, allowlist []string) bool {
	for _, a := range allowlist {
		if a != "" && strings.Contains(line, a) {
			return true
		}
	}
	return false
}

// swapReferenceTests removes the existing *_test.go in work/<pkg> and copies in the
// reference package's *_test.go (the ground-truth oracle). A missing reference or
// target package directory is skipped (not an error — not every package has a
// reference suite).
func swapReferenceTests(referenceDir, work, pkg string) error {
	target := filepath.Join(work, pkg)
	refPkg := filepath.Join(referenceDir, pkg)
	if !isDir(target) || !isDir(refPkg) {
		return nil
	}
	existing, err := filepath.Glob(filepath.Join(target, "*_test.go"))
	if err != nil {
		return err
	}
	for _, f := range existing {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	refTests, err := filepath.Glob(filepath.Join(refPkg, "*_test.go"))
	if err != nil {
		return err
	}
	for _, rt := range refTests {
		if err := copyFile(rt, filepath.Join(target, filepath.Base(rt))); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies src into dst recursively, skipping any .git directory.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

// copyFile copies one file, creating parent directories.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // paths are repo-internal, under temp dirs
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // dst is under a temp work dir
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// isDir reports whether p is an existing directory.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
