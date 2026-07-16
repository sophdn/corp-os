package coding

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"corpos/internal/agent"
)

// Red-before-green (the highest-leverage fake-green defense): the orchestrator-owned
// gate runs whatever tests are in the worktree, but until now it never checked that a
// WORKER-AUTHORED regression test actually EXERCISES the bug. A tautological test that
// asserts the buggy behavior passes on the unfixed tree (run-7-redux: a new
// TestRead_SymbolTypeMismatch asserted only err != nil, which the raw crash satisfies),
// so the gate goes green against code that was never fixed — a fake green.
//
// The fix is the canonical red-green discipline ("confirm the test fails before the fix
// exists"): before crediting a worker-added *_test.go, run its NEW test functions against
// the PRE-fix tree (the AT's fork point). A new test that PASSES on unfixed code is a
// tautology and the attempt is rejected as a fake green. The post-fix green is still
// required by the normal gate; this adds the missing pre-fix RED requirement.
//
// It is purely deterministic (git + go test, no model calls): the worker's diff names the
// added tests, and they are replayed on the historical ref with the production fix absent.

// redGreenRepo is the optional Repo capability the red-before-green gate uses: it
// materializes a historical ref in a throwaway worktree, overlays the worker's changed
// test files onto it (the production fix stays absent), and runs the named test functions
// there. GitRepo satisfies it; NoopRepo does not, so the trivial/test path skips the check
// (mirrors the packageReader optional-capability idiom).
type redGreenRepo interface {
	RunTestsAtRefWithOverlay(ctx context.Context, refSHA, srcDir string, overlayFiles []string, specs []testRunSpec, timeout time.Duration) ([]CommandResult, error)
}

// testRunSpec selects the test functions to replay in one package on the pre-fix tree.
type testRunSpec struct {
	// Pkg is the repo-relative package directory, e.g. "internal/fs".
	Pkg string
	// Funcs are the worker-added Test function names to run (the -run filter set).
	Funcs []string
}

// workerTestAdditions groups a worker's changed test files and newly-added Test functions
// by package directory. Only packages with at least one added Test function are included —
// a modified-but-not-added test body is gate-tampering (T2's job), not a tautology check.
type workerTestAdditions struct {
	// Files are all changed *_test.go paths (repo-relative) — the overlay set.
	Files []string
	// Specs are the per-package added-test-function run specs.
	Specs []testRunSpec
}

// testFuncRe matches a Go test function declaration line (added lines, leading + stripped).
// Go requires test functions to be named TestXxx; that is the tautology surface.
var testFuncRe = regexp.MustCompile(`^func (Test[A-Za-z0-9_]*)\s*\(`)

// parseWorkerTestAdditions walks a unified worktree diff and extracts the worker's changed
// *_test.go files plus, per package directory, the newly-ADDED Test function names. It is a
// pure string parser (no IO) so it is exhaustively unit-testable.
func parseWorkerTestAdditions(diff string) workerTestAdditions {
	var files []string
	funcsByDir := map[string][]string{}
	seenFile := map[string]bool{}
	current := "" // the post-image (b/) path of the file currently being scanned
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			current = postImagePath(line)
			if isTestFile(current) && !seenFile[current] {
				files = append(files, current)
				seenFile[current] = true
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if !isTestFile(current) {
				continue
			}
			if m := testFuncRe.FindStringSubmatch(strings.TrimSpace(line[1:])); m != nil {
				dir := path.Dir(current)
				funcsByDir[dir] = append(funcsByDir[dir], m[1])
			}
		}
	}
	return workerTestAdditions{Files: files, Specs: specsFromFuncs(funcsByDir)}
}

// parseModifiedExistingTestFiles returns the *_test.go paths the worker MODIFIED in place —
// i.e. a pre-existing test file (its diff pre-image is a real path, not /dev/null), as opposed
// to a brand-new test file it authored. A pre-existing test is an oracle: authoring a NEW test
// is the test-authoring lane's legitimate deliverable, but rewriting an EXISTING failing test
// to assert the current (possibly buggy) output is oracle-tampering (bug 1161). It is a pure
// string parser (no IO) so it is exhaustively unit-testable.
func parseModifiedExistingTestFiles(diff string) []string {
	var files []string
	seen := map[string]bool{}
	cur := ""      // current section's file path (b/ side)
	isNew := false // this section CREATES the file (new file mode / --- /dev/null)
	flush := func() {
		if cur != "" && !isNew && isTestFile(cur) && !seen[cur] {
			files = append(files, cur)
			seen[cur] = true
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur, isNew = "", false
			if f := strings.Fields(line); len(f) >= 4 {
				cur = strings.TrimPrefix(f[3], "b/")
			}
		case strings.HasPrefix(line, "new file mode "):
			isNew = true
		case line == "--- /dev/null":
			isNew = true
		case strings.HasPrefix(line, "+++ "):
			if p := postImagePath(line); p != "" {
				cur = p // authoritative post-image path (survives a rename/timestamp)
			}
		}
	}
	flush()
	return files
}

// postImagePath parses the post-image path from a `+++ b/<path>` (or `+++ /dev/null`) diff
// header line, returning "" for a deletion (no post-image).
func postImagePath(line string) string {
	p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
	// Strip a trailing tab-delimited timestamp if git emitted one.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(p, "b/")
}

// specsFromFuncs renders the per-package run specs from the dir→funcs map, sorted (stable
// for deterministic diagnostics and tests) and de-duplicated.
func specsFromFuncs(funcsByDir map[string][]string) []testRunSpec {
	if len(funcsByDir) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(funcsByDir))
	for d := range funcsByDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	specs := make([]testRunSpec, 0, len(dirs))
	for _, d := range dirs {
		specs = append(specs, testRunSpec{Pkg: d, Funcs: dedupeSorted(funcsByDir[d])})
	}
	return specs
}

// dedupeSorted returns the unique elements of in, sorted.
func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// isTestFile reports whether a path is a Go test file. It delegates to agent.IsTestFile — the
// ONE shared definition both verification paths consume — so the coding path's red-green /
// test-only-diff checks and the spawn-path fake-green guard cannot drift on the test-file
// classification.
func isTestFile(p string) bool {
	return agent.IsTestFile(p)
}

// tautologyVerdict runs the red-before-green check for a passing attempt: it identifies the
// worker's newly-added test functions, replays them on the pre-fix tree (the fork point,
// production fix absent), and returns a non-empty verdict when any of them PASS there — a
// tautological test that certifies a fake green. An empty verdict means the check found no
// tautology (no added tests, no pre-fix tree available, or every added test correctly fails
// on unfixed code). An error is an infrastructure failure running the check.
func (o *Orchestrator) tautologyVerdict(ctx context.Context, dir, parentSHA string) (string, error) {
	rg, ok := o.repo.(redGreenRepo)
	if !ok || parentSHA == "" {
		return "", nil // NoopRepo / trivial path: no historical ref to replay against.
	}
	pr, ok := o.repo.(packageReader)
	if !ok {
		return "", nil
	}
	diff, err := pr.DiffWorktree(ctx, dir, parentSHA)
	if err != nil || diff == "" {
		return "", err
	}
	adds := parseWorkerTestAdditions(diff)
	if len(adds.Specs) == 0 {
		return "", nil // No worker-added tests → nothing for the red-green gate to certify.
	}
	results, err := rg.RunTestsAtRefWithOverlay(ctx, parentSHA, dir, adds.Files, adds.Specs, o.gateTimeout)
	if err != nil {
		return "", fmt.Errorf("red-before-green replay: %w", err)
	}
	for i, res := range results {
		if res.ExitCode == 0 {
			// The added test(s) passed with the fix absent: they assert the buggy behavior.
			return fmt.Sprintf("tautological-test (passes on unfixed code): %s [%s]",
				adds.Specs[i].Pkg, strings.Join(adds.Specs[i].Funcs, ",")), nil
		}
	}
	return "", nil
}

// oracleTamperVerdict runs the oracle-preservation check for a passing attempt whose diff is
// test-only (bug 1161). The red-before-green check above only guards NEWLY-ADDED tests; this
// guards a MODIFIED pre-existing test. In the test-authoring lane production is protected, so a
// worker unable to fix buggy production could otherwise reach a green gate by rewriting a
// failing pre-existing test to assert the current (buggy) output — a fake-green with a tampered
// oracle. Because production is protected (unchanged since the fork point), the fork-point tree
// carries the ORIGINAL tests against the SAME production the gate just passed; replaying each
// modified test file's package there tells us whether that oracle actually holds. A RED replay
// means the pre-existing test fails against production — the green was reached by weakening it.
//
// An empty verdict means clean (no modified pre-existing test, no replay repo, or the original
// oracles still pass). A worker that only AUTHORS new test files or ADDS cases never trips it.
// Trade-off: a pre-existing test that does not even COMPILE at the fork point also replays RED,
// so legitimately repairing a broken (non-compiling) pre-existing test is conservatively
// flagged — the honest path there is to report the breakage, not silently green it.
func (o *Orchestrator) oracleTamperVerdict(ctx context.Context, dir, parentSHA, diff string) (string, error) {
	rg, ok := o.repo.(redGreenRepo)
	if !ok || parentSHA == "" {
		return "", nil // NoopRepo / trivial path: no historical ref to replay against.
	}
	modified := parseModifiedExistingTestFiles(diff)
	if len(modified) == 0 {
		return "", nil // Only new/added test files: legitimate authoring, no oracle to weaken.
	}
	seenDir := map[string]bool{}
	var specs []testRunSpec
	for _, f := range modified {
		d := path.Dir(f)
		if seenDir[d] {
			continue
		}
		seenDir[d] = true
		specs = append(specs, testRunSpec{Pkg: d, Funcs: []string{".+"}}) // run every test in the package
	}
	// No overlay: run the fork-point's OWN (pre-tamper) test files against fork-point production.
	results, err := rg.RunTestsAtRefWithOverlay(ctx, parentSHA, dir, nil, specs, o.gateTimeout)
	if err != nil {
		return "", fmt.Errorf("oracle-preservation replay: %w", err)
	}
	for i, res := range results {
		if res.ExitCode != 0 {
			return fmt.Sprintf("oracle-tamper: the pre-existing test(s) in %s FAIL against production at the fork point, so making the gate green by editing %s weakened a failing oracle instead of fixing production — report the production bug and stop; never rewrite a failing test to assert the current (buggy) output",
				specs[i].Pkg, strings.Join(modified, ", ")), nil
		}
	}
	return "", nil
}
