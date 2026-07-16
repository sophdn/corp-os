package coding

import (
	"context"
	"strings"
	"testing"
)

// TestFeaturePlanner_AssemblesPlanToChain proves the capstone wiring: a converged plan's
// atoms each get an authored, protected oracle, folded into a runnable feature chain. The
// planner + author run on scripted models; the red-now anchor is a stub (the per-half live
// proofs cover the model behaviour).
func TestFeaturePlanner_AssemblesPlanToChain(t *testing.T) {
	planJSON := `[{"slug":"reverse","goal":"add Reverse","assertion":"Reverse(\"ab\") == \"ba\"","depends_on":[]},
	              {"slug":"upper","goal":"add Upper","assertion":"Upper(\"ab\") == \"AB\"","depends_on":[]}]`
	planner := NewPlanner(&scriptedPlanModel{responses: []string{planJSON}}, 3)

	authorModel := &scriptedPlanModel{responses: []string{
		"package strutil\nimport \"testing\"\nfunc TestAccept_Reverse(t *testing.T){ if Reverse(\"ab\")!=\"ba\"{t.Fatal(\"x\")} }\n",
		"package strutil\nimport \"testing\"\nfunc TestAccept_Upper(t *testing.T){ if Upper(\"ab\")!=\"AB\"{t.Fatal(\"x\")} }\n",
	}}
	author := NewOracleAuthor(authorModel, &fakeRedChecker{red: true}, 3)

	fp := NewFeaturePlanner(planner, author)
	chain, report, err := fp.Assemble(context.Background(),
		FeatureSpec{Slug: "strutil", Goal: "add small string utilities",
			Packages: []PackageTarget{{Dir: "internal/strutil", PackageName: "strutil"}}},
		"feat-strutil", "/repo")
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(report.PlanProblems) != 0 {
		t.Fatalf("plan should have converged: %v", report.PlanProblems)
	}
	if len(chain.Tasks) != 2 || len(report.Oracles) != 2 {
		t.Fatalf("want 2 tasks + 2 oracles, got %d / %d", len(chain.Tasks), len(report.Oracles))
	}
	// Each atom carries its seeded oracle at the derived path, gated on its acceptance func.
	want := map[string]string{
		"reverse": "internal/strutil/reverse_accept_test.go",
		"upper":   "internal/strutil/upper_accept_test.go",
	}
	for _, task := range chain.Tasks {
		path := want[task.Slug]
		if task.Oracles[path] == "" {
			t.Errorf("task %q missing seeded oracle at %q: %+v", task.Slug, path, task.Oracles)
		}
		if !matchesAny(path, task.Protected) {
			t.Errorf("task %q oracle %q not protected by %v", task.Slug, path, task.Protected)
		}
	}
	// The chain validates (BuildFeatureChain ran Validate inside Assemble).
	if err := chain.Validate(); err != nil {
		t.Fatalf("assembled chain invalid: %v", err)
	}
}

// A plan that never reaches gate-worthy must abort assembly (no gates can be authored from
// an ungate-worthy plan) rather than build a chain on loose assertions.
func TestFeaturePlanner_RefusesUngateworthyPlan(t *testing.T) {
	weak := `[{"slug":"foo","goal":"g","assertion":"foo works","depends_on":[]}]` // presence-only assertion
	planner := NewPlanner(&scriptedPlanModel{responses: []string{weak}}, 2)
	author := NewOracleAuthor(&scriptedPlanModel{responses: []string{"x"}}, &fakeRedChecker{red: true}, 1)
	fp := NewFeaturePlanner(planner, author)

	_, report, err := fp.Assemble(context.Background(),
		FeatureSpec{Slug: "f", Goal: "g", Packages: []PackageTarget{{Dir: "internal/f", PackageName: "f"}}}, "feat-f", "/repo")
	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("want non-convergence refusal, got %v", err)
	}
	if len(report.PlanProblems) == 0 {
		t.Fatal("report should carry the remaining plan problems for inspection")
	}
}

// A planner model error aborts assembly before any authoring.
func TestFeaturePlanner_PropagatesPlanError(t *testing.T) {
	planner := NewPlanner(errModel{}, 2)
	author := NewOracleAuthor(&scriptedPlanModel{responses: []string{"x"}}, &fakeRedChecker{red: true}, 1)
	fp := NewFeaturePlanner(planner, author)
	if _, _, err := fp.Assemble(context.Background(),
		FeatureSpec{Slug: "f", Goal: "g", Packages: []PackageTarget{{Dir: "internal/f", PackageName: "f"}}}, "feat", "/repo"); err == nil || !strings.Contains(err.Error(), "plan:") {
		t.Fatalf("want plan error, got %v", err)
	}
}

// An oracle the author cannot validate aborts assembly with a task-scoped error.
func TestFeaturePlanner_PropagatesAuthorFailure(t *testing.T) {
	planJSON := `[{"slug":"reverse","goal":"add Reverse","assertion":"Reverse(\"ab\") == \"ba\"","depends_on":[]}]`
	planner := NewPlanner(&scriptedPlanModel{responses: []string{planJSON}}, 2)
	// The author model never produces a well-formed oracle declaring the acceptance func.
	author := NewOracleAuthor(&scriptedPlanModel{responses: []string{"package strutil\nfunc Nope(){}"}}, &fakeRedChecker{red: true}, 2)
	fp := NewFeaturePlanner(planner, author)

	_, _, err := fp.Assemble(context.Background(),
		FeatureSpec{Slug: "strutil", Goal: "g", Packages: []PackageTarget{{Dir: "internal/strutil", PackageName: "strutil"}}}, "feat", "/repo")
	if err == nil || !strings.Contains(err.Error(), `author oracle for task "reverse"`) {
		t.Fatalf("want task-scoped author failure, got %v", err)
	}
}
