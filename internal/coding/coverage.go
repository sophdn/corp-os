package coding

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CoverageGrade is the Tier-2 quality report (docs/TWO_TIER_GREEN_DESIGN.md): an ADVISORY
// grade layered on the hard build/test gate. It never fails an attempt — it colors a
// gate-green as confirmed (the changed production lines are exercised by a test) or
// proposed (some changed line is uncovered, or a touched-package test is skipped). The
// zero value (Verdict == "") means "not applicable / not measured" — Tier 2 is a no-op.
type CoverageGrade struct {
	Verdict        string           // "confirmed" | "proposed" | "" (n/a)
	ChangedLines   int              // executable changed PRODUCTION lines considered
	ExercisedLines int              // of those, how many a test executed
	Uncovered      map[string][]int // production file -> changed line numbers with count==0
	SkippedTests   []string         // t.Skip'd tests in touched packages (secondary signal)
	Advisory       string           // one-line summary; "" when confirmed/na
}

// coverBlock is one row of a Go coverprofile: a [startLine,endLine] span of file with a
// hit count (covered == count>0).
type coverBlock struct {
	file      string
	startLine int
	endLine   int
	covered   bool
}

// productionChangedLines returns the POST-image line numbers of added/changed lines per
// PRODUCTION Go file in the unified diff (test files and non-Go files dropped — test files
// are the fake-green blockers' concern, not coverage's).
func productionChangedLines(diff string) map[string][]int {
	all := parseDiffHunks(diff)
	out := make(map[string][]int, len(all))
	for f, lines := range all {
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		out[f] = lines
	}
	return out
}

// parseDiffHunks walks a unified diff and returns, per file, the POST-image line numbers
// of its '+' (added/changed) content lines. It tracks the current file from `+++ b/<path>`
// headers and the running new-line counter from each `@@ -a,b +c,d @@` hunk header,
// advancing it on context (' ') and added ('+') lines and holding it on removed ('-').
func parseDiffHunks(diff string) map[string][]int {
	out := map[string][]int{}
	var file string
	var newLine int
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "+++ "):
			// "+++ b/path" (or "+++ /dev/null" for a deletion).
			p := strings.TrimPrefix(ln, "+++ ")
			p = strings.TrimPrefix(p, "b/")
			if p == "/dev/null" {
				file = ""
			} else {
				file = p
			}
		case strings.HasPrefix(ln, "@@"):
			newLine = parseHunkNewStart(ln)
		case file == "":
			// outside a known file (e.g. the `diff --git`/`---` preamble) — ignore.
		case strings.HasPrefix(ln, "+"):
			if !strings.HasPrefix(ln, "+++") {
				out[file] = append(out[file], newLine)
				newLine++
			}
		case strings.HasPrefix(ln, "-"):
			// removed line: present in the old image only, does not advance the new counter.
		case strings.HasPrefix(ln, "\\"):
			// "\ No newline at end of file" — not a content line.
		default:
			// context line (' ' prefix, or an empty line inside a hunk): advances new.
			newLine++
		}
	}
	return out
}

// parseHunkNewStart extracts c from a hunk header "@@ -a,b +c,d @@" (the post-image start
// line). Returns 0 if it cannot parse (callers then attribute nothing until the next hunk).
func parseHunkNewStart(header string) int {
	plus := strings.Index(header, "+")
	if plus < 0 {
		return 0
	}
	rest := header[plus+1:]
	// rest looks like "c,d @@ ..." or "c @@ ...".
	end := strings.IndexAny(rest, ", ")
	if end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0
	}
	return n
}

// parseCoverProfile parses a Go coverprofile (the `go test -coverprofile` output) into
// blocks. The first line is `mode: <mode>`; each subsequent row is
// `<import-path-file>:startLine.startCol,endLine.endCol numStmt count`. Malformed rows are
// skipped (best-effort: a parse hiccup degrades to "less coverage data", never a crash).
func parseCoverProfile(profile string) []coverBlock {
	var blocks []coverBlock
	for _, ln := range strings.Split(profile, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "mode:") {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) != 3 {
			continue
		}
		colon := strings.LastIndex(fields[0], ":")
		if colon < 0 {
			continue
		}
		file := fields[0][:colon]
		span := fields[0][colon+1:] // startLine.startCol,endLine.endCol
		comma := strings.Index(span, ",")
		if comma < 0 {
			continue
		}
		start := beforeDot(span[:comma])
		end := beforeDot(span[comma+1:])
		count, errc := strconv.Atoi(fields[2])
		if start <= 0 || end <= 0 || errc != nil {
			continue
		}
		blocks = append(blocks, coverBlock{file: file, startLine: start, endLine: end, covered: count > 0})
	}
	return blocks
}

// beforeDot parses the line number out of a "line.col" token (returns 0 on error).
func beforeDot(s string) int {
	if dot := strings.Index(s, "."); dot >= 0 {
		s = s[:dot]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// parseSkippedTests returns the names of tests that emitted "--- SKIP" in `go test -v`
// output (a secondary proposed-green signal: a worker may skip a test to pass the gate).
func parseSkippedTests(testOutput string) []string {
	var out []string
	for _, ln := range strings.Split(testOutput, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "--- SKIP:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(ln, "--- SKIP:"))
		if sp := strings.IndexAny(rest, " \t("); sp >= 0 {
			rest = rest[:sp]
		}
		if rest != "" {
			out = append(out, rest)
		}
	}
	return out
}

// gradeCoverage intersects the changed production lines with the coverage blocks and the
// skipped-test list to produce the Tier-2 verdict. A changed line is COUNTED only when it
// falls inside some coverage block (an executable statement); changed lines in no block
// (braces, blanks, declarations, comments) are non-executable and ignored. A counted line
// is exercised when its block was covered, uncovered otherwise. A coverprofile file path
// (`corpos/internal/x/y.go`) matches a diff path (`internal/x/y.go`) by suffix.
func gradeCoverage(changed map[string][]int, blocks []coverBlock, skipped []string) CoverageGrade {
	g := CoverageGrade{Uncovered: map[string][]int{}, SkippedTests: skipped}
	files := make([]string, 0, len(changed))
	for f := range changed {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		fileBlocks := blocksForFile(f, blocks)
		for _, line := range changed[f] {
			b, ok := blockContaining(line, fileBlocks)
			if !ok {
				continue // non-executable changed line
			}
			g.ChangedLines++
			if b.covered {
				g.ExercisedLines++
			} else {
				g.Uncovered[f] = append(g.Uncovered[f], line)
			}
		}
	}
	switch {
	case g.ChangedLines == 0 && len(skipped) == 0:
		g.Verdict = "" // nothing executable to grade
	case len(g.Uncovered) == 0 && len(skipped) == 0:
		g.Verdict = "confirmed"
	default:
		g.Verdict = "proposed"
		g.Advisory = buildAdvisory(g)
	}
	return g
}

// blocksForFile returns the coverage blocks whose file matches the diff path by suffix.
// The "/"+diffPath guard rejects basename-prefix false matches (x/y.go vs xx/y.go). A
// residual narrow case — a repo-ROOT file sharing a basename with a nested file when BOTH
// packages are in the coverage scope — is left as a benign mis-attribution on an advisory
// signal; -coverpkg scopes instrumentation to the touched packages, so it rarely arises.
func blocksForFile(diffPath string, blocks []coverBlock) []coverBlock {
	var out []coverBlock
	for _, b := range blocks {
		if b.file == diffPath || strings.HasSuffix(b.file, "/"+diffPath) {
			out = append(out, b)
		}
	}
	return out
}

// blockContaining returns the block whose [startLine,endLine] span contains line.
func blockContaining(line int, blocks []coverBlock) (coverBlock, bool) {
	for _, b := range blocks {
		if line >= b.startLine && line <= b.endLine {
			return b, true
		}
	}
	return coverBlock{}, false
}

// buildAdvisory renders the one-line proposed-green advisory.
func buildAdvisory(g CoverageGrade) string {
	var parts []string
	if len(g.Uncovered) > 0 {
		files := make([]string, 0, len(g.Uncovered))
		for f := range g.Uncovered {
			files = append(files, f)
		}
		sort.Strings(files)
		var locs []string
		for _, f := range files {
			ls := g.Uncovered[f]
			strs := make([]string, len(ls))
			for i, l := range ls {
				strs[i] = strconv.Itoa(l)
			}
			locs = append(locs, fmt.Sprintf("%s:%s", f, strings.Join(strs, ",")))
		}
		parts = append(parts, "changed lines not exercised by any test: "+strings.Join(locs, "; "))
	}
	if len(g.SkippedTests) > 0 {
		parts = append(parts, "skipped tests in touched packages: "+strings.Join(g.SkippedTests, ", "))
	}
	return "gate green, but " + strings.Join(parts, "; ")
}

// touchedPackages maps the changed production files to their `./`-relative package import
// paths (deduped, sorted) — the scope for the coverage run.
func touchedPackages(changed map[string][]int) []string {
	set := map[string]struct{}{}
	for f := range changed {
		dir := path.Dir(f)
		if dir == "." || dir == "" {
			set["."] = struct{}{}
		} else {
			set["./"+dir] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// computeCoverageGrade runs a scoped coverage measurement in the worktree and grades it.
// It is invoked only AFTER the hard gate passed, so the touched packages build+test green;
// this second run adds the coverage profile, scoped to just the touched packages (cheap —
// a narrow warm-cache `go test` is sub-second). Any tooling failure degrades to the zero
// grade (Tier 2 is advisory: a measurement hiccup must never block a green). diff is the
// worker's unified diff against the AT fork point.
func computeCoverageGrade(ctx context.Context, runner Runner, dir, diff string, timeout time.Duration) CoverageGrade {
	changed := productionChangedLines(diff)
	if len(changed) == 0 {
		return CoverageGrade{}
	}
	pkgs := touchedPackages(changed)
	if len(pkgs) == 0 {
		return CoverageGrade{}
	}
	profPath := filepath.Join(dir, ".corpos-coverage.out")
	// Clean slate BEFORE the run: a leftover profile from a prior abnormally-terminated run
	// (or a run that exits non-zero without overwriting) must never be graded as if it were
	// this attempt's tree (the advisory must degrade to the zero grade on any hiccup).
	_ = os.Remove(profPath)
	defer func() { _ = os.Remove(profPath) }()

	cmd := []string{"go", "test", "-count=1", "-covermode=atomic",
		"-coverprofile=" + profPath, "-coverpkg=" + strings.Join(pkgs, ","), "-v"}
	cmd = append(cmd, pkgs...)
	run := runner.Run(ctx, cmd, dir, timeout)
	if run.ExitCode != 0 {
		// The scoped coverage run failed (it shouldn't — the hard gate already proved these
		// packages green — but a flake / coverpkg hiccup must not grade a partial profile).
		return CoverageGrade{}
	}

	content, err := os.ReadFile(profPath)
	if err != nil {
		return CoverageGrade{} // could not measure → advisory no-op
	}
	blocks := parseCoverProfile(string(content))
	skipped := parseSkippedTests(run.Stdout)
	return gradeCoverage(changed, blocks, skipped)
}
