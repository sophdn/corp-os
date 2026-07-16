package coding

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseDiffHunks_AddedLinesPostImage(t *testing.T) {
	diff := "diff --git a/internal/x/y.go b/internal/x/y.go\n" +
		"--- a/internal/x/y.go\n" +
		"+++ b/internal/x/y.go\n" +
		"@@ -10,3 +10,5 @@ func F() {\n" +
		" \ta := 1\n" + // context: line 10, advance
		"+\tb := 2\n" + // added: line 11
		"+\tc := 3\n" + // added: line 12
		" \treturn\n" // context: line 13
	got := parseDiffHunks(diff)
	if want := []int{11, 12}; !reflect.DeepEqual(got["internal/x/y.go"], want) {
		t.Fatalf("added lines = %v, want %v", got["internal/x/y.go"], want)
	}
}

func TestParseDiffHunks_RemovalsDoNotAdvanceNewCounter(t *testing.T) {
	diff := "+++ b/f.go\n" +
		"@@ -5,4 +5,3 @@\n" +
		" keep\n" + // line 5
		"-gone1\n" + // removal: no new-line advance
		"-gone2\n" + // removal
		"+added\n" + // added: line 6
		" tail\n" // line 7
	got := parseDiffHunks(diff)
	if want := []int{6}; !reflect.DeepEqual(got["f.go"], want) {
		t.Fatalf("added lines = %v, want %v (removals must not advance the new counter)", got["f.go"], want)
	}
}

func TestProductionChangedLines_DropsTestAndNonGo(t *testing.T) {
	diff := "+++ b/a.go\n@@ -1,0 +1,1 @@\n+x\n" +
		"+++ b/a_test.go\n@@ -1,0 +1,1 @@\n+y\n" +
		"+++ b/README.md\n@@ -1,0 +1,1 @@\n+z\n"
	got := productionChangedLines(diff)
	if _, ok := got["a.go"]; !ok {
		t.Fatal("production a.go must be kept")
	}
	if _, ok := got["a_test.go"]; ok {
		t.Fatal("a_test.go must be dropped")
	}
	if _, ok := got["README.md"]; ok {
		t.Fatal("non-Go README.md must be dropped")
	}
}

func TestParseCoverProfile(t *testing.T) {
	prof := "mode: atomic\n" +
		"corpos/internal/x/y.go:10.2,12.3 2 1\n" + // covered
		"corpos/internal/x/y.go:20.2,22.3 1 0\n" + // uncovered
		"garbage line\n" +
		"corpos/internal/x/z.go:5.1,5.10 1 4\n"
	blocks := parseCoverProfile(prof)
	if len(blocks) != 3 {
		t.Fatalf("parsed %d blocks, want 3 (malformed row skipped)", len(blocks))
	}
	if blocks[0].startLine != 10 || blocks[0].endLine != 12 || !blocks[0].covered {
		t.Errorf("block0 = %+v, want lines 10-12 covered", blocks[0])
	}
	if blocks[1].covered {
		t.Errorf("block1 (count 0) must be uncovered")
	}
}

func TestParseSkippedTests(t *testing.T) {
	out := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestB\n--- SKIP: TestB (0.00s)\n    f_test.go:9: needs net\n"
	got := parseSkippedTests(out)
	if want := []string{"TestB"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped = %v, want %v", got, want)
	}
}

func TestTouchedPackages(t *testing.T) {
	changed := map[string][]int{"internal/x/y.go": {1}, "internal/x/z.go": {2}, "main.go": {3}}
	got := touchedPackages(changed)
	want := []string{"./internal/x", "."}
	// sorted: "." < "./internal/x"
	if !reflect.DeepEqual(got, []string{".", "./internal/x"}) {
		t.Fatalf("touched = %v, want %v", got, want)
	}
}

func TestGradeCoverage_ConfirmedWhenAllChangedExercised(t *testing.T) {
	changed := map[string][]int{"internal/x/y.go": {11}}
	blocks := []coverBlock{{file: "corpos/internal/x/y.go", startLine: 10, endLine: 12, covered: true}}
	g := gradeCoverage(changed, blocks, nil)
	if g.Verdict != "confirmed" {
		t.Fatalf("verdict = %q, want confirmed (%+v)", g.Verdict, g)
	}
	if g.ChangedLines != 1 || g.ExercisedLines != 1 {
		t.Fatalf("changed/exercised = %d/%d, want 1/1", g.ChangedLines, g.ExercisedLines)
	}
}

func TestGradeCoverage_ProposedWhenChangedLineUncovered(t *testing.T) {
	changed := map[string][]int{"internal/x/y.go": {21}}
	blocks := []coverBlock{{file: "corpos/internal/x/y.go", startLine: 20, endLine: 22, covered: false}}
	g := gradeCoverage(changed, blocks, nil)
	if g.Verdict != "proposed" {
		t.Fatalf("verdict = %q, want proposed", g.Verdict)
	}
	if !strings.Contains(g.Advisory, "not exercised") || !strings.Contains(g.Advisory, "y.go:21") {
		t.Fatalf("advisory should name the uncovered line; got %q", g.Advisory)
	}
}

func TestGradeCoverage_NonExecutableChangedLinesIgnored(t *testing.T) {
	// Changed line 99 is in NO coverage block (a brace/blank/decl) → ignored, not uncovered.
	changed := map[string][]int{"internal/x/y.go": {99}}
	blocks := []coverBlock{{file: "corpos/internal/x/y.go", startLine: 10, endLine: 12, covered: true}}
	g := gradeCoverage(changed, blocks, nil)
	if g.ChangedLines != 0 || g.Verdict != "" {
		t.Fatalf("a non-executable changed line must be ignored (n/a); got %+v", g)
	}
}

func TestGradeCoverage_SkippedTestIsProposed(t *testing.T) {
	changed := map[string][]int{"internal/x/y.go": {11}}
	blocks := []coverBlock{{file: "corpos/internal/x/y.go", startLine: 10, endLine: 12, covered: true}}
	g := gradeCoverage(changed, blocks, []string{"TestThing"})
	if g.Verdict != "proposed" {
		t.Fatalf("a skipped touched-package test must propose (not confirm); got %q", g.Verdict)
	}
	if !strings.Contains(g.Advisory, "skipped tests") {
		t.Fatalf("advisory should mention skipped tests; got %q", g.Advisory)
	}
}

// scriptedCovRunner writes a fixture coverprofile to the -coverprofile path the command
// carries (mimicking `go test -coverprofile`), and returns the scripted -v stdout.
type scriptedCovRunner struct {
	profile  string
	stdout   string
	exitCode int
	gotCmd   []string
}

func (s *scriptedCovRunner) Run(_ context.Context, cmd []string, _ string, _ time.Duration) CommandResult {
	s.gotCmd = cmd
	for _, a := range cmd {
		if p, ok := strings.CutPrefix(a, "-coverprofile="); ok {
			_ = os.WriteFile(p, []byte(s.profile), 0o600)
		}
	}
	return CommandResult{Command: cmd, ExitCode: s.exitCode, Stdout: s.stdout}
}

// A non-zero coverage run degrades to the zero grade even though a (possibly partial/stale)
// profile was written — the advisory must never grade a tree the run didn't certify.
func TestComputeCoverageGrade_NonZeroRunDegradesToNoop(t *testing.T) {
	runner := &scriptedCovRunner{
		profile:  "mode: atomic\ncorpos/internal/x/y.go:20.2,22.3 1 0\n",
		exitCode: 1,
	}
	g := computeCoverageGrade(context.Background(), runner, t.TempDir(),
		"+++ b/internal/x/y.go\n@@ -20,1 +20,2 @@\n old\n+\tnewline\n", time.Minute)
	if g.Verdict != "" {
		t.Fatalf("a non-zero coverage run must degrade to no-op; got %+v", g)
	}
}

func TestComputeCoverageGrade_EndToEndProposed(t *testing.T) {
	dir := t.TempDir()
	diff := "+++ b/internal/x/y.go\n@@ -20,1 +20,2 @@\n old\n+\tnewline\n" // added post-image line 21
	runner := &scriptedCovRunner{
		profile: "mode: atomic\ncorpos/internal/x/y.go:20.2,22.3 1 0\n", // line 21 uncovered
		stdout:  "=== RUN TestX\n--- PASS: TestX (0.00s)\n",
	}
	g := computeCoverageGrade(context.Background(), runner, dir, diff, time.Minute)
	if g.Verdict != "proposed" {
		t.Fatalf("verdict = %q, want proposed (%+v)", g.Verdict, g)
	}
	// The coverage run was scoped to the touched package.
	joined := strings.Join(runner.gotCmd, " ")
	if !strings.Contains(joined, "-coverpkg=./internal/x") || !strings.Contains(joined, "./internal/x") {
		t.Fatalf("coverage run should be scoped to ./internal/x; got %q", joined)
	}
	// The temp profile is cleaned up.
	if _, err := os.Stat(filepath.Join(dir, ".corpos-coverage.out")); !os.IsNotExist(err) {
		t.Fatal("the temp coverprofile should be removed")
	}
}

func TestComputeCoverageGrade_NoProductionChangeIsNoop(t *testing.T) {
	runner := &scriptedCovRunner{}
	g := computeCoverageGrade(context.Background(), runner, t.TempDir(), "+++ b/a_test.go\n@@ -1,0 +1,1 @@\n+x\n", time.Minute)
	if g.Verdict != "" {
		t.Fatalf("a test-only diff must yield no Tier-2 grade; got %+v", g)
	}
	if runner.gotCmd != nil {
		t.Fatal("no coverage run should happen when there is no production change")
	}
}

func TestComputeCoverageGrade_ProfileReadFailureDegradesToNoop(t *testing.T) {
	// A runner that does NOT write the profile → ReadFile fails → advisory no-op (never blocks).
	runner := &scriptedCovRunner{stdout: "ok"}
	g := computeCoverageGrade(context.Background(), runner, t.TempDir(), "+++ b/a.go\n@@ -1,1 +1,2 @@\n x\n+y\n", time.Minute)
	if g.Verdict != "" {
		t.Fatalf("a missing profile must degrade to no-op, not a verdict; got %+v", g)
	}
}
