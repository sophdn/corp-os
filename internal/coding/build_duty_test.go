package coding

import (
	"strings"
	"testing"
)

// TestBuildDutyTestExistsDirective pins the bug-1163 lever: a production-fix atom (tests protected,
// not a test-authoring AT) tells the worker the acceptance test already exists and to make ONLY the
// production change — counteracting the orchestrator's duty text that says "add a test" for a test
// that is already the protected gate. A test-authoring AT (AllowTestOnlyDiff) must NOT get it.
func TestBuildDutyTestExistsDirective(t *testing.T) {
	const marker = "The acceptance/regression test for this fix ALREADY EXISTS"

	prodFix := AtomicTask{
		Goal:      "fix the failing behavior and add a test case for it",
		Protected: []string{"**/*_test.go"},
		Gate:      [][]string{{"go", "test", "./..."}},
	}
	got := buildDuty(prodFix, "/w", Feedback{Iteration: 1})
	if !strings.Contains(got, marker) {
		t.Errorf("a production-fix atom with tests protected must carry the test-already-exists directive; duty:\n%s", got)
	}
	// It must also tell the worker to ignore a goal that mentions adding a test.
	if !strings.Contains(got, "make ONLY the production-code change") {
		t.Error("the directive must steer the worker to the production change only")
	}

	// A genuine test-authoring atom opts out — it SHOULD author tests.
	authoring := AtomicTask{
		Goal:              "write the regression test",
		Protected:         []string{"**/*_test.go"},
		AllowTestOnlyDiff: true,
		Gate:              [][]string{{"go", "test", "./..."}},
	}
	if got := buildDuty(authoring, "/w", Feedback{Iteration: 1}); strings.Contains(got, marker) {
		t.Error("a test-authoring atom (AllowTestOnlyDiff) must NOT be told the test already exists")
	}

	// No protected tests (e.g. a non-Go or unguarded atom) → no directive.
	unprotected := AtomicTask{Goal: "do the thing", Gate: [][]string{{"make"}}}
	if got := buildDuty(unprotected, "/w", Feedback{Iteration: 1}); strings.Contains(got, marker) {
		t.Error("an atom with no protected test files must NOT carry the directive")
	}
}

func TestProtectsTestFiles(t *testing.T) {
	for _, c := range []struct {
		globs []string
		want  bool
	}{
		{[]string{"**/*_test.go"}, true},
		{[]string{"go/internal/fs/read_symbol_acceptance_test.go"}, true},
		{[]string{"internal/**/*.go"}, false},
		{nil, false},
		{[]string{"docs/*.md"}, false},
	} {
		if got := protectsTestFiles(c.globs); got != c.want {
			t.Errorf("protectsTestFiles(%v) = %v, want %v", c.globs, got, c.want)
		}
	}
}
