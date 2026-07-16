package coding

import (
	"context"
	"strings"
	"testing"
)

func TestPackageTarget_ImportPath(t *testing.T) {
	withMod := PackageTarget{Dir: "internal/parse", PackageName: "parse", ModulePath: "feat"}
	if got := withMod.ImportPath(); got != "feat/internal/parse" {
		t.Fatalf("ImportPath = %q, want feat/internal/parse", got)
	}
	noMod := PackageTarget{Dir: "internal/parse", PackageName: "parse"}
	if got := noMod.ImportPath(); got != "" {
		t.Fatalf("ImportPath with no module = %q, want empty", got)
	}
}

func TestCheckTaskPackages(t *testing.T) {
	decl := []string{"internal/parse", "internal/pipeline"}
	// Valid placement → no task-package problems.
	clean := Plan{Packages: decl, Tasks: []PlanTask{
		{Slug: "a", Package: "internal/parse"},
		{Slug: "b", Package: "internal/pipeline"},
	}}
	if got := kindOf(checkTaskPackages(clean), "task-package"); got != 0 {
		t.Fatalf("a validly-placed multi-package plan must not flag; got %d", got)
	}
	// Unplaced + undeclared atoms → one problem each.
	bad := Plan{Packages: decl, Tasks: []PlanTask{
		{Slug: "a", Package: ""},                   // unplaced
		{Slug: "b", Package: "internal/elsewhere"}, // undeclared
	}}
	probs := checkTaskPackages(bad)
	if len(probs) != 2 {
		t.Fatalf("want 2 task-package problems, got %d: %s", len(probs), FormatPlanProblems(probs))
	}
	if !strings.Contains(probs[0].Detail, "no `package`") || !strings.Contains(probs[1].Detail, "not a declared package") {
		t.Fatalf("problem details should name the failure mode; got %s", FormatPlanProblems(probs))
	}
	// A single-package (or zero) feature is unconstrained — Package may be empty.
	single := Plan{Packages: []string{"internal/only"}, Tasks: []PlanTask{{Slug: "a", Package: ""}}}
	if got := checkTaskPackages(single); got != nil {
		t.Fatalf("single-package feature must not constrain placement; got %s", FormatPlanProblems(got))
	}
}

// kindOf counts problems of a given kind.
func kindOf(probs []PlanProblem, kind string) int {
	n := 0
	for _, p := range probs {
		if p.Kind == kind {
			n++
		}
	}
	return n
}

// TestFeaturePlanner_AssemblesMultiPackage proves the end-to-end multi-package path: the
// planner places two atoms in two packages, the assembler authors each oracle in its own
// package, and the DOWNSTREAM atom's author prompt is handed the UPSTREAM package's import
// path (so its test can call the upstream API across the package boundary).
func TestFeaturePlanner_AssemblesMultiPackage(t *testing.T) {
	planJSON := `[
		{"slug":"parse","goal":"add Parse","assertion":"Parse(\"42\") == 42","depends_on":[],"package":"internal/parse"},
		{"slug":"pipeline","goal":"add Run wiring Parse","assertion":"Run(\"21\") == 42","depends_on":["parse"],"package":"internal/pipeline"}
	]`
	planner := NewPlanner(&scriptedPlanModel{responses: []string{planJSON}}, 2)
	authorModel := &scriptedPlanModel{responses: []string{
		"package parse\nimport \"testing\"\nfunc TestAccept_Parse(t *testing.T){ if Parse(\"42\")!=42 {t.Fatal(\"x\")} }\n",
		"package pipeline\nimport \"testing\"\nfunc TestAccept_Pipeline(t *testing.T){ if Run(\"21\")!=42 {t.Fatal(\"x\")} }\n",
	}}
	author := NewOracleAuthor(authorModel, &fakeRedChecker{red: true}, 3)
	fp := NewFeaturePlanner(planner, author)

	chain, report, err := fp.Assemble(context.Background(), FeatureSpec{
		Slug: "calc",
		Goal: "parse a number then double it across two packages",
		Packages: []PackageTarget{
			{Dir: "internal/parse", PackageName: "parse", ModulePath: "feat"},
			{Dir: "internal/pipeline", PackageName: "pipeline", ModulePath: "feat"},
		},
	}, "feat-calc", "/repo")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(report.PlanProblems) != 0 {
		t.Fatalf("plan should have converged: %s", FormatPlanProblems(report.PlanProblems))
	}
	// Each oracle is seeded in ITS task's package.
	wantPaths := map[string]string{
		"parse":    "internal/parse/parse_accept_test.go",
		"pipeline": "internal/pipeline/pipeline_accept_test.go",
	}
	for _, task := range chain.Tasks {
		if task.Oracles[wantPaths[task.Slug]] == "" {
			t.Errorf("task %q oracle not at %q: %+v", task.Slug, wantPaths[task.Slug], task.Oracles)
		}
	}
	// The downstream (pipeline) author prompt carries the upstream parse import path; the
	// upstream (parse) prompt carries no UPSTREAM section.
	if len(authorModel.userMsgs) < 2 {
		t.Fatalf("expected an author prompt per task, got %d", len(authorModel.userMsgs))
	}
	if strings.Contains(authorModel.userMsgs[0], "UPSTREAM PACKAGES") {
		t.Errorf("the upstream atom (parse) should have no upstream imports; got:\n%s", authorModel.userMsgs[0])
	}
	if !strings.Contains(authorModel.userMsgs[1], "feat/internal/parse (package parse)") {
		t.Errorf("the downstream atom (pipeline) prompt must offer the parse import path; got:\n%s", authorModel.userMsgs[1])
	}
}

// TestFeaturePlanner_RejectsNoPackage guards the precondition: Assemble needs a declared package.
func TestFeaturePlanner_RejectsNoPackage(t *testing.T) {
	fp := NewFeaturePlanner(NewPlanner(&scriptedPlanModel{responses: []string{"[]"}}, 1),
		NewOracleAuthor(&scriptedPlanModel{responses: []string{"x"}}, &fakeRedChecker{red: true}, 1))
	if _, _, err := fp.Assemble(context.Background(), FeatureSpec{Slug: "f", Goal: "g"}, "feat", "/repo"); err == nil || !strings.Contains(err.Error(), "declares no package") {
		t.Fatalf("want no-package refusal, got %v", err)
	}
}
