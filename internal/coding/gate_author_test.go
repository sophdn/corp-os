package coding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// errModel always fails Complete (exercises the draft model-error path).
type errModel struct{}

func (errModel) Model() string   { return "err" }
func (errModel) Available() bool { return true }
func (errModel) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{}, errors.New("model down")
}

func TestDeriveTestFunc(t *testing.T) {
	cases := map[string]string{
		"parse-coverage-profile": "TestAccept_ParseCoverageProfile",
		"grade_green":            "TestAccept_GradeGreen",
		"foo":                    "TestAccept_Foo",
	}
	for slug, want := range cases {
		if got := deriveTestFunc(slug); got != want {
			t.Errorf("deriveTestFunc(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestExtractGoSource(t *testing.T) {
	cases := []struct{ in, wantHas string }{
		{"```go\npackage x\nfunc F(){}\n```", "package x"},
		{"here it is:\npackage y\nfunc G(){}", "package y"},
		{"no code here", ""},
		{"```\npackage z\n```", "package z"},
	}
	for _, c := range cases {
		got := extractGoSource(c.in)
		if c.wantHas == "" {
			if got != "" {
				t.Errorf("extractGoSource(%q) = %q, want empty", c.in, got)
			}
			continue
		}
		if !strings.Contains(got, c.wantHas) {
			t.Errorf("extractGoSource(%q) = %q, want it to contain %q", c.in, got, c.wantHas)
		}
	}
}

func TestValidateOracleSource(t *testing.T) {
	good := "package twotier\nimport \"testing\"\nfunc TestAccept_Foo(t *testing.T) { _ = t }\n"
	if err := validateOracleSource(good, "twotier", "TestAccept_Foo"); err != nil {
		t.Fatalf("good source rejected: %v", err)
	}
	// external _test package is allowed
	ext := "package twotier_test\nimport \"testing\"\nfunc TestAccept_Foo(t *testing.T) { _ = t }\n"
	if err := validateOracleSource(ext, "twotier", "TestAccept_Foo"); err != nil {
		t.Fatalf("external test package rejected: %v", err)
	}
	bad := []struct{ name, src, want string }{
		{"not go", "this is not go", "not valid Go"},
		{"wrong package", "package other\nfunc TestAccept_Foo(t *testing.T){}", "must be"},
		{"defines production func", "package twotier\nfunc Gcd(a, b int) int { return 0 }\nfunc TestAccept_Foo(t *testing.T){}", "must not define"},
		{"declares production type", "package twotier\ntype T struct{}\nfunc TestAccept_Foo(t *testing.T){}", "must not declare"},
		{"missing accept func", "package twotier\nimport \"testing\"\nfunc TestOther(t *testing.T){}", "must declare"},
		{"wrong signature", "package twotier\nfunc TestAccept_Foo(){}", "signature"},
		{"unused upstream import", "package twotier\nimport (\n\t\"testing\"\n\t\"example.com/feat/internal/numparse\"\n)\nfunc TestAccept_Foo(t *testing.T){ if Run(\"123\") != 246 { t.Fatal(\"x\") } }", "never references it"},
	}
	for _, c := range bad {
		if err := validateOracleSource(c.src, "twotier", "TestAccept_Foo"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
	// A USED upstream import is fine (the bug was the UNused one): the test references numparse.
	usedImport := "package twotier\nimport (\n\t\"testing\"\n\t\"example.com/feat/internal/numparse\"\n)\nfunc TestAccept_Foo(t *testing.T){ if _, err := numparse.Parse(\"x\"); err == nil { t.Fatal(\"want err\") } }"
	if err := validateOracleSource(usedImport, "twotier", "TestAccept_Foo"); err != nil {
		t.Fatalf("a referenced upstream import must be allowed: %v", err)
	}
	// A blank/aliased import is not flagged as unused.
	blank := "package twotier\nimport (\n\t\"testing\"\n\t_ \"example.com/feat/internal/numparse\"\n)\nfunc TestAccept_Foo(t *testing.T){ _ = t }"
	if err := validateOracleSource(blank, "twotier", "TestAccept_Foo"); err != nil {
		t.Fatalf("a blank import must be allowed: %v", err)
	}
}

// fakeRedChecker reports red/green per a fixed verdict, recording how many oracles it saw.
type fakeRedChecker struct {
	red    bool
	calls  int
	detail string
	err    error
}

func (f *fakeRedChecker) OracleIsRed(_ context.Context, _ AuthoredOracle) (bool, string, error) {
	f.calls++
	return f.red, f.detail, f.err
}

const wellFormedOracle = "package twotier\nimport \"testing\"\nfunc TestAccept_Parse(t *testing.T) { if Parse(\"42\") != 42 { t.Fatal(\"want 42\") } }\n"

func TestOracleAuthor_DerivesSkeletonAndAuthors(t *testing.T) {
	m := &scriptedPlanModel{responses: []string{"```go\n" + wellFormedOracle + "```"}}
	red := &fakeRedChecker{red: true}
	a := NewOracleAuthor(m, red, 3)

	oracle, err := a.Author(context.Background(), PlanTask{Slug: "parse", Goal: "add Parse", Assertion: `Parse("42") == 42`},
		PackageTarget{Dir: "internal/twotier", PackageName: "twotier"}, nil)
	if err != nil {
		t.Fatalf("Author: %v", err)
	}
	if oracle.TestFunc != "TestAccept_Parse" {
		t.Errorf("test func = %q", oracle.TestFunc)
	}
	if oracle.TestPath != "internal/twotier/parse_accept_test.go" {
		t.Errorf("test path = %q", oracle.TestPath)
	}
	wantGate := []string{"go", "test", "-count=1", "-run", "^TestAccept_Parse$", "./internal/twotier"}
	if len(oracle.Gate) != 1 || strings.Join(oracle.Gate[0], " ") != strings.Join(wantGate, " ") {
		t.Errorf("gate = %v", oracle.Gate)
	}
	if red.calls != 1 {
		t.Errorf("red checker should run once, ran %d", red.calls)
	}
	// The authored oracle folds into a FeatureTask that carries + protects it.
	ft := oracle.ToFeatureTask(PlanTask{Slug: "parse", Goal: "add Parse"})
	if ft.Oracles[oracle.TestPath] == "" || len(ft.Gate) != 1 {
		t.Errorf("ToFeatureTask lost the oracle/gate: %+v", ft)
	}
}

// The author REVISES: a first malformed draft is rejected by the parser gate, the
// feedback is fed back, and the second (well-formed) draft converges.
func TestOracleAuthor_RevisesOnInvalidDraft(t *testing.T) {
	m := &scriptedPlanModel{responses: []string{
		"```go\npackage twotier\nfunc Wrong(){}\n```", // missing the acceptance func
		"```go\n" + wellFormedOracle + "```",
	}}
	a := NewOracleAuthor(m, &fakeRedChecker{red: true}, 3)
	oracle, err := a.Author(context.Background(), PlanTask{Slug: "parse", Goal: "g", Assertion: `Parse("42")==42`},
		PackageTarget{Dir: "internal/twotier", PackageName: "twotier"}, nil)
	if err != nil {
		t.Fatalf("Author: %v", err)
	}
	if !strings.Contains(oracle.TestSource, "TestAccept_Parse") {
		t.Fatalf("did not converge on the well-formed draft: %q", oracle.TestSource)
	}
	if len(m.userMsgs) < 2 || !strings.Contains(m.userMsgs[1], "REJECTED") {
		t.Fatalf("revise prompt must feed back the rejection; got %v", m.userMsgs)
	}
}

// A vacuous oracle (green on the unbuilt tree) is rejected by the red checker until the
// budget is spent, then Author errors rather than returning a fake oracle.
func TestOracleAuthor_RejectsVacuousOracle(t *testing.T) {
	m := &scriptedPlanModel{responses: []string{"```go\n" + wellFormedOracle + "```"}} // always well-formed but "green"
	red := &fakeRedChecker{red: false, detail: "passed with feature absent"}
	a := NewOracleAuthor(m, red, 2)
	_, err := a.Author(context.Background(), PlanTask{Slug: "parse", Goal: "g", Assertion: `Parse("42")==42`},
		PackageTarget{Dir: "internal/twotier", PackageName: "twotier"}, nil)
	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("want non-convergence on a vacuous oracle, got %v", err)
	}
	if red.calls != 2 {
		t.Errorf("red checker should run each of the 2 rounds, ran %d", red.calls)
	}
}

// A red-check infrastructure error aborts authoring (it is not a revisable rejection).
func TestOracleAuthor_RedCheckInfraError(t *testing.T) {
	m := &scriptedPlanModel{responses: []string{"```go\n" + wellFormedOracle + "```"}}
	a := NewOracleAuthor(m, &fakeRedChecker{err: errors.New("worktree blew up")}, 2)
	if _, err := a.Author(context.Background(), PlanTask{Slug: "parse", Goal: "g", Assertion: "a"},
		PackageTarget{Dir: "internal/twotier", PackageName: "twotier"}, nil); err == nil || !strings.Contains(err.Error(), "red-check") {
		t.Fatalf("want red-check infra error, got %v", err)
	}
}

func TestDeriveTestFuncNonAlphaAfterSep(t *testing.T) {
	// A non-lowercase char immediately after a separator exercises the upper() passthrough.
	if got := deriveTestFunc("x-2y"); got != "TestAccept_X2y" {
		t.Errorf("deriveTestFunc(x-2y) = %q", got)
	}
}

func TestExtractFencedUnclosed(t *testing.T) {
	if got := extractFenced("```go\npackage x"); got != "" {
		t.Errorf("unclosed fence should yield empty, got %q", got)
	}
	// extractGoSource still recovers via the `package ` fallback when the fence is unclosed.
	if got := extractGoSource("```go\npackage x\nfunc F(){}"); !strings.Contains(got, "package x") {
		t.Errorf("unclosed fence should fall back to package scan, got %q", got)
	}
}

func TestOracleAuthor_DefaultRoundsAndDraftErrors(t *testing.T) {
	// Default rounds when maxRounds<=0.
	a := NewOracleAuthor(&scriptedPlanModel{responses: []string{"```go\n" + wellFormedOracle + "```"}}, &fakeRedChecker{red: true}, 0)
	if a.rounds() != defaultOracleRounds {
		t.Fatalf("default rounds = %d, want %d", a.rounds(), defaultOracleRounds)
	}

	// A model error during draft aborts immediately.
	ae := NewOracleAuthor(errModel{}, nil, 2)
	if _, err := ae.Author(context.Background(), PlanTask{Slug: "s", Assertion: "a"}, PackageTarget{Dir: "d", PackageName: "p"}, nil); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("want draft model error, got %v", err)
	}

	// A response with no Go source is a draft error too.
	an := NewOracleAuthor(&scriptedPlanModel{responses: []string{"sorry, no code"}}, nil, 2)
	if _, err := an.Author(context.Background(), PlanTask{Slug: "s", Assertion: "a"}, PackageTarget{Dir: "d", PackageName: "p"}, nil); err == nil || !strings.Contains(err.Error(), "no Go source") {
		t.Fatalf("want no-source error, got %v", err)
	}
}

func TestOracleAuthor_RejectsBadInput(t *testing.T) {
	a := NewOracleAuthor(&scriptedPlanModel{responses: []string{"x"}}, nil, 1)
	if _, err := a.Author(context.Background(), PlanTask{Slug: "", Assertion: "x"}, PackageTarget{Dir: "d", PackageName: "p"}, nil); err == nil {
		t.Error("empty slug should error")
	}
	if _, err := a.Author(context.Background(), PlanTask{Slug: "s", Assertion: "a"}, PackageTarget{Dir: "", PackageName: ""}, nil); err == nil {
		t.Error("empty target should error")
	}
}
