package coding

import (
	"strings"
	"testing"
)

func TestAssertionPinsValue(t *testing.T) {
	pinned := []string{
		"Run(\"21\") == 42",
		"Parse returns 42 for \"42\"",
		"list of touched packages matches expected from a sample diff",
		"gate exit 0 even when zero lines covered",
		"Double(21) is 42",
		"returns a non-nil error for a bad input",
	}
	for _, a := range pinned {
		if !assertionPinsValue(a) {
			t.Errorf("assertion should count as pinned: %q", a)
		}
	}
	weak := []string{
		"profile contains coverage data for the expected packages",
		"report generated and displayed without blocking the gate",
		"the parser works",
		"output is generated successfully",
	}
	for _, a := range weak {
		if assertionPinsValue(a) {
			t.Errorf("assertion should be flagged weak (presence-only): %q", a)
		}
	}
}

func TestCoversInvariant(t *testing.T) {
	inv := Invariant{Name: "never-blocks", Keywords: []string{"exit 0"}}
	if !coversInvariant("the gate exit 0 regardless of grade", inv) {
		t.Error("assertion mentioning the keyword should cover the invariant")
	}
	if coversInvariant("the report is generated", inv) {
		t.Error("assertion missing the keyword must not cover the invariant")
	}
	if coversInvariant("anything", Invariant{Name: "x"}) {
		t.Error("an invariant with no keywords cannot be covered")
	}
}

func TestCheckPlanQuality_StructuralProblems(t *testing.T) {
	probs := CheckPlanQuality(Plan{Slug: "f", Tasks: []PlanTask{
		{Slug: "a", Goal: "g", Assertion: "x == 1"},
		{Slug: "a", Goal: "g", Assertion: "y == 2"},                               // dup slug
		{Slug: "b", Goal: "", Assertion: "z == 3"},                                // empty goal
		{Slug: "c", Goal: "g", Assertion: "w == 4", DependsOn: []string{"later"}}, // forward dep
		{Slug: "d", Goal: "g", Assertion: ""},                                     // no assertion
	}})
	kinds := map[string]int{}
	for _, p := range probs {
		kinds[p.Kind]++
	}
	if kinds["structural"] < 4 {
		t.Fatalf("expected >=4 structural problems (dup, empty goal, forward dep, no assertion); got %+v", probs)
	}
}

func TestCheckPlanQuality_CleanPlanPasses(t *testing.T) {
	probs := CheckPlanQuality(Plan{
		Slug: "f",
		Tasks: []PlanTask{
			{Slug: "parse", Goal: "create Parse", Assertion: "Parse(\"42\") == 42 and Parse(\"x\") returns an error"},
			{Slug: "never-blocks", Goal: "keep advisory non-blocking", Assertion: "the gate exit 0 even when zero changed lines are covered"},
		},
		Invariants: []Invariant{{Name: "advisory-never-blocks", Keywords: []string{"exit 0"}}},
	})
	if len(probs) != 0 {
		t.Fatalf("a clean plan should have no problems; got %s", FormatPlanProblems(probs))
	}
}

func TestCheckPlanQuality_FoldedInvariants(t *testing.T) {
	probs := CheckPlanQuality(Plan{
		Slug: "f",
		Tasks: []PlanTask{
			{Slug: "everything", Goal: "do it all", Assertion: "exit 0 and the production lines are counted"},
		},
		Invariants: []Invariant{
			{Name: "never-blocks", Keywords: []string{"exit 0"}},
			{Name: "production-only", Keywords: []string{"production"}},
		},
	})
	var folded bool
	for _, p := range probs {
		if p.Kind == "folded-invariants" {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("one atom covering two invariants must be flagged as folded; got %s", FormatPlanProblems(probs))
	}
}

// TestCheckPlanQuality_ReplaysQwen32BDecomposition grounds the gate in the real atomization
// eval: it replays the verbatim 8-task decomposition Qwen2.5-32B produced for the
// two-tier-green feature and asserts the gate catches EXACTLY the weaknesses the scan found
// — the loose presence-only assertions, and the two dropped invariants (advisory-never-
// blocks; production-lines-only) that got no dedicated atom.
func TestCheckPlanQuality_ReplaysQwen32BDecomposition(t *testing.T) {
	plan := Plan{
		Slug: "two-tier-green",
		Tasks: []PlanTask{
			{Slug: "create-diff-parser", Goal: "parse changed production lines", Assertion: "parser correctly identifies all modified lines in a sample diff"},
			{Slug: "extract-touched-packages", Goal: "extract touched packages", Assertion: "list of touched packages matches expected from a sample diff", DependsOn: []string{"create-diff-parser"}},
			{Slug: "generate-coverage-profile", Goal: "generate a coverage profile", Assertion: "profile contains coverage data for the expected packages", DependsOn: []string{"extract-touched-packages"}},
			{Slug: "parse-coverage-profile", Goal: "parse the coverage profile", Assertion: "parsed coverage data matches expected covered lines from a sample profile", DependsOn: []string{"generate-coverage-profile"}},
			{Slug: "intersect-changed-covered-lines", Goal: "intersect changed and covered", Assertion: "intersection correctly identifies lines both changed and covered", DependsOn: []string{"create-diff-parser", "parse-coverage-profile"}},
			{Slug: "determine-green-status", Goal: "grade confirmed/proposed", Assertion: "status correctly determined from sample data", DependsOn: []string{"intersect-changed-covered-lines"}},
			{Slug: "generate-quality-report", Goal: "generate the report", Assertion: "report contains correct green status and is formatted as expected", DependsOn: []string{"determine-green-status"}},
			{Slug: "integrate-report-into-gate", Goal: "wire into the gate", Assertion: "report generated and displayed without blocking the gate", DependsOn: []string{"generate-quality-report"}},
		},
		Invariants: []Invariant{
			{Name: "advisory-never-blocks", Keywords: []string{"exit 0"}},
			{Name: "production-lines-only", Keywords: []string{"production"}},
		},
	}
	probs := CheckPlanQuality(plan)

	weak := map[string]bool{}
	uncovered := 0
	for _, p := range probs {
		switch p.Kind {
		case "weak-assertion":
			weak[p.Task] = true
		case "uncovered-invariant":
			uncovered++
		}
	}
	// The presence-only assertions (3 generate-profile, 7 report-contains, 8 displayed-without-block).
	for _, slug := range []string{"generate-coverage-profile", "generate-quality-report", "integrate-report-into-gate"} {
		if !weak[slug] {
			t.Errorf("expected weak-assertion flag on %q; got problems:\n%s", slug, FormatPlanProblems(probs))
		}
	}
	// The atoms the scan rated crisp+pinned must NOT be flagged weak.
	for _, slug := range []string{"extract-touched-packages", "parse-coverage-profile"} {
		if weak[slug] {
			t.Errorf("%q has a pinned 'matches expected' assertion and must not be flagged weak", slug)
		}
	}
	// Both dropped invariants (no dedicated atom asserts exit-0 or production-only) are caught.
	if uncovered != 2 {
		t.Fatalf("expected both dropped invariants flagged uncovered; got %d:\n%s", uncovered, FormatPlanProblems(probs))
	}
}

func TestFormatPlanProblems(t *testing.T) {
	if FormatPlanProblems(nil) != "" {
		t.Fatal("no problems → empty string")
	}
	out := FormatPlanProblems([]PlanProblem{{Task: "a", Kind: "weak-assertion", Detail: "tighten it"}})
	if !strings.Contains(out, "task a") || !strings.Contains(out, "weak-assertion") {
		t.Fatalf("formatted feedback should name the task + kind; got %q", out)
	}
}

// symbolProbs returns the inconsistent-symbol problems from a plan-quality scan.
func symbolProbs(p Plan) []PlanProblem {
	var out []PlanProblem
	for _, pr := range CheckPlanQuality(p) {
		if pr.Kind == "inconsistent-symbol" {
			out = append(out, pr)
		}
	}
	return out
}

func TestSymbolConsistency_FlagsCasingSplitAcrossAtoms(t *testing.T) {
	// The first end-to-end run's failure: one atom calls GCD, another Gcd (same for LCM/Lcm).
	plan := Plan{Slug: "math", Tasks: []PlanTask{
		{Slug: "gcd", Goal: "add func Gcd", Assertion: "GCD(8,12) == 4"},
		{Slug: "lcm", Goal: "add func Lcm in terms of gcd", Assertion: "Gcd(25,15) == 5 and LCM(4,6) == 12", DependsOn: []string{"gcd"}},
		{Slug: "use", Goal: "wire it", Assertion: "Lcm(2,3) == 6", DependsOn: []string{"lcm"}},
	}}
	probs := symbolProbs(plan)
	// Two split symbols: gcd {GCD,Gcd} and lcm {LCM,Lcm}.
	if len(probs) != 2 {
		t.Fatalf("expected 2 inconsistent-symbol problems (gcd, lcm); got %d:\n%s", len(probs), FormatPlanProblems(probs))
	}
	joined := FormatPlanProblems(probs)
	for _, want := range []string{"GCD", "Gcd", "LCM", "Lcm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problem text should name the conflicting spelling %q; got:\n%s", want, joined)
		}
	}
}

func TestSymbolConsistency_GoalVsAssertionSplitFlagged(t *testing.T) {
	plan := Plan{Slug: "f", Tasks: []PlanTask{
		{Slug: "a", Goal: "create func Parse(s string)", Assertion: "parse(\"42\") == 42"},
	}}
	if got := symbolProbs(plan); len(got) != 1 {
		t.Fatalf("a goal-declared Parse exercised as parse in the assertion must flag; got %d", len(got))
	}
}

func TestSymbolConsistency_ConsistentPlanClean(t *testing.T) {
	// One spelling everywhere, plus an internally-consistent helper (strconv.Atoi) and
	// ordinary prose capitalization ("Parse the input") that is NOT call-shaped — none flag.
	plan := Plan{Slug: "f", Tasks: []PlanTask{
		{Slug: "parse", Goal: "Parse the input: add func Parse(s string) using strconv.Atoi(s)", Assertion: "Parse(\"42\") == 42"},
		{Slug: "double", Goal: "add func Double", Assertion: "Double(21) == 42 after Parse(\"21\")", DependsOn: []string{"parse"}},
	}}
	if got := symbolProbs(plan); len(got) != 0 {
		t.Fatalf("a consistently-spelled plan must not flag; got:\n%s", FormatPlanProblems(got))
	}
}

func TestCheckDuplicateAssertions(t *testing.T) {
	// Two atoms with identical assertions (modulo case/space) → the later is flagged.
	dup := Plan{Tasks: []PlanTask{
		{Slug: "a", Assertion: `Match("x", c, "d") returns ("bug", 1)`},
		{Slug: "b", Assertion: `match("x", c, "d")   RETURNS ("bug", 1)`},
		{Slug: "c", Assertion: `Match("y", c, "d") returns ("fix", 0)`},
	}}
	probs := kindOf2(CheckPlanQuality(dup), "duplicate-assertion")
	if len(probs) != 1 || probs[0].Task != "b" {
		t.Fatalf("want one duplicate-assertion flag on task b; got %s", FormatPlanProblems(probs))
	}
	// All-distinct assertions → no flag.
	if got := kindOf2(CheckPlanQuality(Plan{Tasks: []PlanTask{
		{Slug: "a", Assertion: "x == 1"}, {Slug: "b", Assertion: "y == 2"},
	}}), "duplicate-assertion"); len(got) != 0 {
		t.Fatalf("distinct assertions must not flag; got %s", FormatPlanProblems(got))
	}
}

func kindOf2(probs []PlanProblem, kind string) []PlanProblem {
	var out []PlanProblem
	for _, p := range probs {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}
