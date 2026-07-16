package coding

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"corpos/internal/model"
)

// PackageTarget locates where a feature task's code + acceptance oracle live in the
// target repo: the repo-relative package directory, the Go package name, and the module
// path the dir lives under. The principal (or the planner) supplies it; the bridge
// derives the oracle path and the gate argv from it. ModulePath is what lets a
// multi-package feature's downstream atom IMPORT an upstream atom's package
// (<ModulePath>/<Dir>); it is optional for a single-package feature that never imports a
// sibling.
type PackageTarget struct {
	Dir         string // repo-relative package dir, e.g. "internal/twotier"
	PackageName string // Go package name, e.g. "twotier"
	ModulePath  string // the target repo's module path, e.g. "corpos" or "example.com/feat"
}

// ImportPath is the Go import path of the package — <ModulePath>/<Dir> — used when a
// downstream atom's oracle imports this (upstream) package. Empty when ModulePath is
// unset (a single-package feature forms no cross-package import).
func (t PackageTarget) ImportPath() string {
	if strings.TrimSpace(t.ModulePath) == "" {
		return ""
	}
	return t.ModulePath + "/" + t.Dir
}

// AuthoredOracle is the CLOSED prose→argv seam for one planned task: an executable,
// protected acceptance oracle (the test source + its path) plus the gate argv that runs
// it and the worker's write-allowlist. ToFeatureTask folds it into a FeatureTask that
// BuildFeatureChain turns into a runnable, oracle-seeded chain.
type AuthoredOracle struct {
	Slug       string
	TestPath   string     // repo-relative, e.g. "internal/twotier/parse_accept_test.go"
	TestFunc   string     // e.g. "TestAccept_Parse"
	TestSource string     // the authored Go test file
	Gate       [][]string // go build ./... ; go test -run ^TestFunc$ ./Dir
	Workspace  []string   // the impl write-allowlist (the package, minus the protected oracle)
}

// ToFeatureTask folds the authored oracle and its planned task into a FeatureTask. The
// oracle is carried (seeded + protected) and the gate runs it; the worker may only write
// the package's non-test files. Cross-task dependency threading (PlanTask.DependsOn →
// InputRef) is the orchestrator's concern and is left to the chain assembler.
func (a AuthoredOracle) ToFeatureTask(task PlanTask) FeatureTask {
	return FeatureTask{
		Slug: task.Slug,
		// Point the worker at the seeded oracle: it is the read-only contract (the worker
		// reads it for the exact API, then implements until it passes — and can never edit
		// it). Without this the worker only learns the API from gate diagnostics on failure.
		Goal: fmt.Sprintf("%s\n\nThe acceptance test %s in %s defines DONE — read it for the exact API and expected values, then write the implementation until it passes. Do NOT modify any _test.go file.",
			task.Goal, a.TestFunc, a.TestPath),
		Gate:      a.Gate,
		Oracles:   map[string]string{a.TestPath: a.TestSource},
		Workspace: a.Workspace,
	}
}

// RedChecker verifies an authored oracle FAILS (is red) on a tree where the feature is
// absent — the anti-vacuous-oracle guarantee that makes a MODEL-authored test trustworthy
// without trusting the model: a test that already passes on unbuilt code asserts nothing.
// A git-backed checker overlays the oracle at the fork ref and runs the gate, requiring a
// non-zero exit (a missing symbol is a compile failure, which is red).
type RedChecker interface {
	OracleIsRed(ctx context.Context, oracle AuthoredOracle) (red bool, detail string, err error)
}

// defaultOracleRounds bounds the author → validate → revise loop.
const defaultOracleRounds = 3

// OracleAuthor turns a planned task's PROSE assertion into an executable acceptance
// oracle: it prompts a model for the test source, validates it is well-formed Go that
// declares the exact acceptance func, and (when a RedChecker is wired) confirms it is red
// on the unbuilt tree — revising against the gate's own feedback until it converges. This
// is the generative half of the gate-authoring bridge; the deterministic validators are
// the trust anchors, so the author tier is a cost/quality choice, not a trust one.
type OracleAuthor struct {
	model     model.Adapter
	checker   RedChecker // optional; nil → parser validation only
	maxRounds int
}

// NewOracleAuthor builds an author over a model adapter. checker may be nil (parser-only
// validation); maxRounds<=0 → defaultOracleRounds.
func NewOracleAuthor(m model.Adapter, checker RedChecker, maxRounds int) *OracleAuthor {
	return &OracleAuthor{model: m, checker: checker, maxRounds: maxRounds}
}

func (a *OracleAuthor) rounds() int {
	if a.maxRounds > 0 {
		return a.maxRounds
	}
	return defaultOracleRounds
}

// Author closes the prose→argv seam for one task: it derives the deterministic skeleton
// (test func name, oracle path, gate argv, workspace) and authors + validates the oracle
// source. It returns the AuthoredOracle, or the last validation problem if it never
// converged within the round budget.
// upstream is the set of EARLIER tasks' packages this task's oracle may import (a
// multi-package feature's cross-package edges, resolved from the task's DependsOn by the
// assembler). It is nil for a single-package feature or a task with no upstream deps; the
// author then offers the model no cross-package imports.
func (a *OracleAuthor) Author(ctx context.Context, task PlanTask, target PackageTarget, upstream []PackageTarget) (AuthoredOracle, error) {
	if strings.TrimSpace(task.Slug) == "" || strings.TrimSpace(task.Assertion) == "" {
		return AuthoredOracle{}, fmt.Errorf("oracle author: task needs a slug and a pinned assertion")
	}
	if strings.TrimSpace(target.Dir) == "" || strings.TrimSpace(target.PackageName) == "" {
		return AuthoredOracle{}, fmt.Errorf("oracle author: target needs a package dir and name")
	}
	fn := deriveTestFunc(task.Slug)
	oracle := AuthoredOracle{
		Slug:      task.Slug,
		TestPath:  target.Dir + "/" + task.Slug + "_accept_test.go",
		TestFunc:  fn,
		Gate:      [][]string{{"go", "test", "-count=1", "-run", "^" + fn + "$", "./" + target.Dir}},
		Workspace: []string{target.Dir + "/**"},
	}

	var feedback string
	for round := 1; round <= a.rounds(); round++ {
		src, err := a.draft(ctx, task, target, upstream, fn, feedback)
		if err != nil {
			return AuthoredOracle{}, fmt.Errorf("oracle draft (round %d): %w", round, err)
		}
		if verr := validateOracleSource(src, target.PackageName, fn); verr != nil {
			feedback = verr.Error()
			continue
		}
		oracle.TestSource = src
		if a.checker != nil {
			red, detail, cerr := a.checker.OracleIsRed(ctx, oracle)
			if cerr != nil {
				return AuthoredOracle{}, fmt.Errorf("oracle red-check (round %d): %w", round, cerr)
			}
			if !red {
				feedback = fmt.Sprintf("the oracle PASSES on the current tree, where the feature is NOT yet built — a vacuous test. It MUST fail until the feature exists: call the real API you expect and assert the pinned value. %s", detail)
				continue
			}
		}
		return oracle, nil
	}
	return AuthoredOracle{}, fmt.Errorf("oracle author for %q did not converge in %d rounds: %s", task.Slug, a.rounds(), feedback)
}

func (a *OracleAuthor) draft(ctx context.Context, task PlanTask, target PackageTarget, upstream []PackageTarget, fn, feedback string) (string, error) {
	resp, err := a.model.Complete(ctx, []model.ChatMessage{
		{Role: model.RoleSystem, Content: oracleSystemPrompt},
		{Role: model.RoleUser, Content: oracleUserPrompt(task, target, upstream, fn, feedback)},
	}, nil)
	if err != nil {
		return "", err
	}
	src := extractGoSource(resp.Text)
	if src == "" {
		return "", fmt.Errorf("model response contained no Go source")
	}
	return src, nil
}

const oracleSystemPrompt = `You author ONE Go acceptance test — the executable oracle that defines "done" for a single feature task an automated coding agent will implement next.

Hard rules:
- Output ONLY a complete Go test file: a package clause, imports, and the test function. NO prose, NO markdown fences.
- The file MUST declare EXACTLY the test function name you are given, with signature func(t *testing.T).
- The test must encode the stated ASSERTION as a CONCRETE check that PINS the expected value (compare against an exact value / return / error / exit code). Never a presence-only check.
- The feature does NOT exist yet. Your test MUST call the real API you expect the implementer to build and assert its behavior — so it FAILS on today's tree (a missing symbol is a compile failure, which is correct) and PASSES once the feature is built. A test that passes before the feature exists is wrong.
- Do NOT declare, implement, or stub the feature's functions, types, vars, or consts — a separate implementer builds them, and a stub here both collides with that implementation and makes the test pass for the wrong reason. Your file contains ONLY the package clause, imports, and the Test function(s). Referencing an as-yet-undefined symbol is REQUIRED and correct.
- To build an INPUT value of one of the feature's struct types, write a COMPOSITE LITERAL that references the type — e.g. ` + "`" + `[]Candidate{{Name: "x", Keywords: []string{"a"}}}` + "`" + ` — and NEVER add a ` + "`" + `type Candidate struct{…}` + "`" + ` declaration to make it compile. The type is defined by the implementation; a ` + "`" + `type` + "`" + ` declaration in your test collides with it and is rejected. Reference the feature's types and functions; do not define them.
- Keep it minimal and self-contained: one focused behavior, the package's own API, the standard library. Do not invent unrelated helpers.`

func oracleUserPrompt(task PlanTask, target PackageTarget, upstream []PackageTarget, fn, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PACKAGE: %s (directory %s)\n", target.PackageName, target.Dir)
	fmt.Fprintf(&b, "TEST FUNCTION (declare exactly this): %s\n", fn)
	fmt.Fprintf(&b, "TASK GOAL: %s\n", task.Goal)
	fmt.Fprintf(&b, "ASSERTION the test must encode: %s\n", task.Assertion)
	if imps := importLines(upstream); imps != "" {
		fmt.Fprintf(&b, "\nUPSTREAM PACKAGES available to the implementation (built by earlier tasks):\n%s\nImport one ONLY if your TEST BODY references its API directly (e.g. to construct an input or assert a returned value's type). The function under test may call these packages INTERNALLY without your test importing them — if your test only calls THIS package's own API, do not import an upstream package. Never import a package you do not reference (Go rejects an unused import), and do not invent upstream symbols the task did not describe.\n", imps)
	}
	if feedback != "" {
		fmt.Fprintf(&b, "\nYour previous attempt was REJECTED: %s\nFix it and output the corrected complete test file.\n", feedback)
	}
	return b.String()
}

// importLines renders the upstream packages as "  - <import path> (package <name>)" lines,
// skipping any without a formable import path (no module). Empty when there are none.
func importLines(upstream []PackageTarget) string {
	var b strings.Builder
	for _, u := range upstream {
		if ip := u.ImportPath(); ip != "" {
			fmt.Fprintf(&b, "  - %s (package %s)\n", ip, u.PackageName)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// validateOracleSource confirms the authored source is a well-formed acceptance oracle:
// valid Go in the expected package (or its external _test package), declaring the exact
// acceptance func, and — critically — declaring NOTHING ELSE at package level beyond
// imports and Test functions. The last rule closes a live failure mode: the author stubbed
// a placeholder of the function under test (func Gcd ... return 0), which both collides
// with the worker's real implementation (redeclaration) and fools the red-now check (red
// for the wrong reason). An acceptance oracle only CALLS the feature's API; it never
// defines it. This deterministic gate runs before the (heavier) red-now check.
func validateOracleSource(src, pkg, fn string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "oracle_test.go", src, 0)
	if err != nil {
		return fmt.Errorf("authored oracle is not valid Go: %w", err)
	}
	if f.Name.Name != pkg && f.Name.Name != pkg+"_test" {
		return fmt.Errorf("oracle is in package %q but must be %q or %q_test", f.Name.Name, pkg, pkg)
	}
	hasAccept := false
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.GenDecl:
			if decl.Tok != token.IMPORT {
				return fmt.Errorf("oracle must not declare a package-level %s — an acceptance test only CALLS the feature's API, it does not define it", decl.Tok)
			}
		case *ast.FuncDecl:
			if decl.Recv != nil || !isTestFuncName(decl.Name.Name) {
				return fmt.Errorf("oracle must not define %q — an acceptance test contains only its Test function(s); reference the feature's API, do not implement or stub it", decl.Name.Name)
			}
			if decl.Name.Name == fn {
				if decl.Type.Params == nil || len(decl.Type.Params.List) != 1 {
					return fmt.Errorf("acceptance func %s must have signature func(t *testing.T)", fn)
				}
				hasAccept = true
			}
		}
	}
	if !hasAccept {
		return fmt.Errorf("oracle must declare the acceptance test func %s(t *testing.T)", fn)
	}
	return checkNoUnusedImports(f)
}

// checkNoUnusedImports rejects an oracle that imports a package it never references. Go
// treats an unused import as a COMPILE error, so such an oracle is red — but red for the
// WRONG reason (it cannot compile no matter what the implementer writes), and since the
// oracle is protected the worker can never edit the import away: the gate is permanently
// ungreenable and the worker thrashes. This bit the first multi-package run — the author
// imported the upstream package even though the test only called the wrapping function,
// which uses the upstream package INTERNALLY. Blank (_) and dot (.) imports are allowed
// (their use is implicit). The local name defaults to the import path's last element (the
// package-name convention these feature packages follow).
func checkNoUnusedImports(f *ast.File) error {
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := importLocalName(imp, path)
		if name == "_" || name == "." {
			continue
		}
		if !used[name] {
			return fmt.Errorf("oracle imports %q but never references it — an acceptance test must carry NO unused import (Go rejects it as a compile error, making the gate ungreenable). The function under test uses upstream packages internally; your test should import one ONLY if it calls that package's API directly. Remove the unused import", path)
		}
	}
	return nil
}

// importLocalName returns the identifier an import is referenced by: its explicit alias, or
// the path's last element (the package-name convention).
func importLocalName(imp *ast.ImportSpec, path string) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// isTestFuncName reports whether name is a Go testing entry point (Test/Benchmark/Example/
// Fuzz) — the only top-level functions an acceptance oracle may declare.
func isTestFuncName(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// deriveTestFunc maps a kebab/underscore slug to the canonical acceptance-test name
// "TestAccept_<CamelSlug>" (a deterministic, gate-matched identifier).
func deriveTestFunc(slug string) string {
	var b strings.Builder
	b.WriteString("TestAccept_")
	upNext := true
	for _, r := range slug {
		switch {
		case r == '-' || r == '_' || r == ' ':
			upNext = true
		case upNext:
			b.WriteRune(upper(r))
			upNext = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func upper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 32
	}
	return r
}

// extractGoSource pulls the Go file out of a model response: it prefers a fenced block,
// else takes from the first `package ` clause to the end, tolerating prose a local model
// may pad around the code.
func extractGoSource(text string) string {
	if fenced := extractFenced(text); fenced != "" {
		text = fenced
	}
	if i := strings.Index(text, "package "); i >= 0 {
		return strings.TrimSpace(text[i:])
	}
	return ""
}

// extractFenced returns the content of the first ``` ... ``` block (dropping an optional
// language tag on the opening fence), or "" if there is no closed block.
func extractFenced(text string) string {
	start := strings.Index(text, "```")
	if start < 0 {
		return ""
	}
	rest := text[start+3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:] // drop the ```go language tag line
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
