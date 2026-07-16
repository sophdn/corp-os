package agent

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Structural grounding: when the verify gate fails with Go compile errors that name
// undefined symbols or unknown struct fields, the worker has almost always GUESSED an
// internal API. The swap-rehearsal hallucination (decompose-admin-surface runs 2+3): a
// floor model invented Deps{Version}, NewHandlers, ServeHTTP for an admin surface whose
// real API is a dispatch.Table — and never reached for sys.go_doc, even with it scoped
// and the prompt nudging it. Advisory prompting did not change floor-model behavior.
//
// So the loop resolves the implicated symbols ITSELF: it parses the compile errors,
// runs `go doc` on each in the offending file's package, and injects the REAL signatures
// into the revise feedback. The next attempt SEES the API instead of guessing again.
// Grounding is structural — done by the loop on a build failure — not a tool the floor
// model must choose to use. Follow-on to bug 1090 (the go_doc tool alone was insufficient).

const (
	// maxGroundedSymbols caps how many distinct symbols one revise cycle resolves, so a
	// cascade of compile errors can't fan out into a huge go-doc storm.
	maxGroundedSymbols = 5
	// goDocLookupTimeout bounds one `go doc` lookup; doc loading is fast, so a longer run
	// means a building dependency — fail fast and let the worker adapt from the raw error.
	goDocLookupTimeout = 8 * time.Second
	// groundedDocTail caps each resolved doc so a large type's documentation can't
	// dominate the revise prompt.
	groundedDocTail = 1200
)

// groundingTarget is one symbol to resolve, scoped to the package directory of the file
// whose compile error named it (relative to the verify/module root), so a bare type
// resolves against the right package.
type groundingTarget struct {
	pkgDir string // dir of the offending .go file, relative to the verify root ("" = root)
	symbol string // a bare identifier/type (resolved within pkgDir) or a qualified pkg.Sym
}

var (
	// "path/file.go:12:5:" error prefix → the offending file (group 1).
	reErrFile = regexp.MustCompile(`(\S+\.go):\d+:\d+:`)
	// "undefined: X" / "undefined: pkg.X"
	reUndefined = regexp.MustCompile(`undefined: ([\w./]+)`)
	// "unknown field Foo in struct literal of type Bar" → the TYPE (group 1).
	reUnknownField = regexp.MustCompile(`unknown field \w+ in struct literal of type (\w+)`)
	// "type Bar has no field or method Foo" → the TYPE (group 1).
	reNoFieldType = regexp.MustCompile(`type (\w+) has no field or method`)
)

// parseGroundingTargets extracts the symbols/types implicated by Go compile errors in a
// failed gate output. Pure (no IO) so it is table-testable. Each target is scoped to the
// package dir of the file the error names — so a bare type resolves in the right package.
// Returns de-duplicated targets in first-seen order, capped at maxGroundedSymbols.
func parseGroundingTargets(gateOutput string) []groundingTarget {
	var targets []groundingTarget
	seen := map[string]bool{}
	add := func(pkgDir, sym string) {
		sym = strings.TrimSpace(sym)
		if !isGroundableSymbol(sym) {
			return
		}
		key := pkgDir + "\x00" + sym
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, groundingTarget{pkgDir: pkgDir, symbol: sym})
	}
	for _, line := range strings.Split(gateOutput, "\n") {
		pkgDir := ""
		if m := reErrFile.FindStringSubmatch(line); m != nil {
			if d := filepath.Dir(m[1]); d != "." {
				pkgDir = d
			}
		}
		// The TYPE patterns first: resolving the type shows the real fields/methods the
		// worker guessed wrong — the highest-signal grounding.
		if m := reUnknownField.FindStringSubmatch(line); m != nil {
			add(pkgDir, m[1])
		}
		if m := reNoFieldType.FindStringSubmatch(line); m != nil {
			add(pkgDir, m[1])
		}
		if m := reUndefined.FindStringSubmatch(line); m != nil {
			add(pkgDir, m[1])
		}
		if len(targets) >= maxGroundedSymbols {
			break
		}
	}
	return targets
}

// isGroundableSymbol restricts a parsed symbol to a Go identifier / qualified path, so
// the value handed to `go doc` is a clean doc token. The symbols come from compiler
// output, but validate anyway — defense in depth, mirroring sysorgan.validateGoDocSymbol.
func isGroundableSymbol(sym string) bool {
	if len(sym) == 0 || len(sym) > 128 {
		return false
	}
	for _, r := range sym {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '/' || r == '_':
		default:
			return false
		}
	}
	return true
}

// goDocResolver resolves a symbol to its `go doc` text, run in pkgDir. Injectable so the
// loop wiring is testable without the go toolchain. ok=false when the symbol doesn't
// resolve (the worker then adapts from the raw compile error alone).
type goDocResolver func(ctx context.Context, pkgDir, symbol string) (string, bool)

// groundFromGateOutput parses a failed gate output, resolves each implicated symbol via
// `go doc` (run in the offending file's package under verifyRoot), and returns a block of
// the REAL signatures to inject into the revise feedback. Empty when nothing resolves —
// in which case the feedback is just the raw gate output, unchanged.
func groundFromGateOutput(ctx context.Context, gateOutput, verifyRoot string, resolve goDocResolver) string {
	if resolve == nil {
		return ""
	}
	targets := parseGroundingTargets(gateOutput)
	var b strings.Builder
	for _, t := range targets {
		doc, ok := resolve(ctx, filepath.Join(verifyRoot, t.pkgDir), t.symbol)
		if !ok {
			continue
		}
		if doc = strings.TrimSpace(doc); doc == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("Grounded signatures (resolved by `go doc` — these are the REAL declarations your code referenced but got wrong; use these exact names, fields, and signatures rather than guessing):\n")
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", t.symbol, tailString(doc, groundedDocTail))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Import-path grounding (the sibling arm, bug corpos-grounding-does-not-handle-import-path-
// errors): a floor worker frequently writes a REPO-RELATIVE import (`import "go/internal/x"`)
// instead of the module path (`toolkit/internal/x`), producing "is not in std" / "cannot find
// package" / "no required module provides package" — error shapes the symbol parser above
// ignores, so grounding fired 0 times and the test never compiled despite otherwise-correct
// code. This arm parses those shapes and resolves the REAL module import path via `go list`.

const (
	// maxGroundedImports caps how many distinct wrong import paths one revise cycle resolves.
	maxGroundedImports = 5
	// goListLookupTimeout bounds one `go list` lookup (loading is fast; a longer run means a
	// building dependency — fail fast and let the worker adapt from the raw error).
	goListLookupTimeout = 8 * time.Second
)

var (
	// "package <path> is not in std (…)" — the module-era shape for an unresolvable import.
	reImportNotInStd = regexp.MustCompile(`package ([\w./-]+) is not in std`)
	// "package <path> is not in GOROOT (…)".
	reImportNotInGOROOT = regexp.MustCompile(`package ([\w./-]+) is not in GOROOT`)
	// "no required module provides package <path>; to add it: …".
	reNoRequiredModule = regexp.MustCompile(`no required module provides package ([\w./-]+)`)
	// `cannot find package "<path>" in any of:` (GOPATH-era shape, still emitted in cases).
	reCannotFindPackage = regexp.MustCompile(`cannot find package "([^"]+)"`)
)

// parseImportPathErrors extracts the WRONG import paths implicated by Go import-path compile
// errors in a failed gate output. Pure (no IO) so it is table-testable. De-duplicated in
// first-seen order, capped at maxGroundedImports. These are the paths the worker WROTE that
// the toolchain can't resolve — almost always a repo-relative path needing the module prefix.
func parseImportPathErrors(gateOutput string) []string {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if !isGroundableImportPath(p) || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	for _, line := range strings.Split(gateOutput, "\n") {
		for _, re := range []*regexp.Regexp{reImportNotInStd, reImportNotInGOROOT, reNoRequiredModule, reCannotFindPackage} {
			if m := re.FindStringSubmatch(line); m != nil {
				add(m[1])
			}
		}
		if len(paths) >= maxGroundedImports {
			break
		}
	}
	if len(paths) > maxGroundedImports {
		paths = paths[:maxGroundedImports]
	}
	return paths
}

// isGroundableImportPath restricts a parsed import path to a clean Go import-path token (the
// symbol validator's character set plus '-', which import paths allow). Defense in depth — the
// paths come from compiler output, but a `go list` argument is still validated.
func isGroundableImportPath(p string) bool {
	if len(p) == 0 || len(p) > 256 || strings.HasPrefix(p, "/") {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '/' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// goListResolver resolves a WRONG import path to the correct module import path for the
// directory it was trying to name. Injectable so the loop wiring is testable without the go
// toolchain. ok=false when no candidate directory resolves (the worker then adapts from the
// raw compile error alone).
type goListResolver func(ctx context.Context, moduleRoot, badImportPath string) (string, bool)

// groundImportPaths parses import-path errors from a failed gate output, resolves each wrong
// path to the correct module path (via resolve, run under moduleRoot), and returns a block of
// the corrections to inject into the revise feedback. Empty when nothing resolves — the
// feedback is then just the raw gate output (plus any symbol grounding).
func groundImportPaths(ctx context.Context, gateOutput, moduleRoot string, resolve goListResolver) string {
	if resolve == nil {
		return ""
	}
	var b strings.Builder
	for _, bad := range parseImportPathErrors(gateOutput) {
		correct, ok := resolve(ctx, moduleRoot, bad)
		if !ok {
			continue
		}
		if correct = strings.TrimSpace(correct); correct == "" || correct == bad {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("Corrected import paths (the import path you wrote does not resolve — it is almost always a repo-relative path; use the REAL module path on the right, not the one you guessed):\n")
		}
		fmt.Fprintf(&b, "\n--- %s → %s ---\n", bad, correct)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Import-cycle grounding (the third arm, bug corpos-grounding-overcorrects-in-package-test-into-
// self-import-cycle): after the import-path arm corrects a repo-relative import to the module
// path, a floor worker writing an IN-PACKAGE (white-box) test — `package X`, not `package X_test`
// — over-applies the correction and IMPORTS X from X's own *_test.go. The toolchain then emits
// "import cycle not allowed in test", a shape the symbol and import-path arms both ignore, so the
// chain-378 capstone worker thrashed to Opus to rediscover the in-package rule. This arm parses
// the shape and injects the rule directly: an in-package test references its package's identifiers
// without importing it. No go-toolchain lookup — the fix IS the rule, so there is no resolver.

// The toolchain emits the in-package-test import cycle across TWO lines (captured from real
// `go test` output — see TestParseImportCycleErrors_RealToolchainOutput):
//
//	# pkg/path
//	package pkg/path
//		imports pkg/path from foo_test.go: import cycle not allowed in test
//
// reCyclePackageLine captures the importing package from the "package <path>" line; the imported
// package + test file come from the following tab-indented "imports … : import cycle not allowed
// in test" line. (An earlier single-line regex never matched the real multi-line output — the
// chain-378 re-validation rehearsal caught it firing 0 times despite a textbook self-import.)
var (
	reCyclePackageLine = regexp.MustCompile(`^package ([\w./-]+)\s*$`)
	reCycleImportsLine = regexp.MustCompile(`imports ([\w./-]+)(?: from ([\w./-]+))?: import cycle not allowed in test`)
)

// importCycle is one parsed self-import-cycle: the package an in-package test wrongly imported and
// the *_test.go that imported it ("" when the toolchain didn't name a file).
type importCycle struct {
	pkg  string
	file string
}

// parseImportCycleErrors extracts in-package-test SELF-import cycles from a failed gate output.
// Pure (no IO) so it is table-testable. It tracks the package named by the most recent
// "package <path>" line and pairs it with the following "imports <path> …: import cycle not
// allowed in test" line; a cycle is grounded only when the imported package equals that importing
// package — the floor-worker overcorrection (an in-package test importing itself). A genuine
// cross-package cycle is a real design error the worker must rethink, not a mechanical "drop the
// import" fix, so it is skipped. De-duplicated by package in first-seen order, capped at
// maxGroundedImports.
func parseImportCycleErrors(gateOutput string) []importCycle {
	var cycles []importCycle
	seen := map[string]bool{}
	curPkg := ""
	for _, line := range strings.Split(gateOutput, "\n") {
		if m := reCyclePackageLine.FindStringSubmatch(line); m != nil {
			curPkg = m[1]
			continue
		}
		m := reCycleImportsLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		imported, file := m[1], m[2]
		if curPkg == "" || curPkg != imported { // only the self-import overcorrection
			continue
		}
		if !isGroundableImportPath(imported) || seen[imported] {
			continue
		}
		seen[imported] = true
		cycles = append(cycles, importCycle{pkg: imported, file: file})
		if len(cycles) >= maxGroundedImports {
			break
		}
	}
	return cycles
}

// groundImportCycles parses self-import-cycle errors and returns the structural correction to
// inject into the revise feedback. Pure — no go-toolchain lookup, because the fix is the rule
// itself. Empty when no self-import cycle is present (the feedback is then the raw gate output
// plus any symbol/import-path grounding).
func groundImportCycles(gateOutput string) string {
	cycles := parseImportCycleErrors(gateOutput)
	if len(cycles) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Import-cycle fix — your *_test.go is an IN-PACKAGE (white-box) test: it declares the SAME `package` as the code under test, so it must NOT import that package; importing your own package is exactly what \"import cycle not allowed in test\" means. Remove the self-import below and reference the package's identifiers DIRECTLY (write `Foo(x)`, not `pkg.Foo(x)`). To write an external black-box test instead, change the file's clause to `package <name>_test` and import the package — but to reach UNEXPORTED identifiers, keep the in-package clause and just drop the import.")
	for _, c := range cycles {
		shortName := c.pkg
		if i := strings.LastIndex(shortName, "/"); i >= 0 {
			shortName = shortName[i+1:]
		}
		where := ""
		if c.file != "" {
			where = " in " + c.file
		}
		fmt.Fprintf(&b, "\n--- remove `import \"%s\"`%s — it is this test's own package %s ---\n", c.pkg, where, shortName)
	}
	return strings.TrimRight(b.String(), "\n")
}

// realGoListImportPath resolves a wrong import path to the correct module path by `go list`-ing
// candidate module-relative dirs under moduleRoot, stripping leading path segments (the worker
// usually PREPENDED a wrong prefix, e.g. the repo subdir "go/", so the trailing segments name a
// real package under the module). Bounded timeout + clean git env (mirroring realGoDoc). It is
// the production goListResolver. ok=false when no candidate resolves.
func realGoListImportPath(ctx context.Context, moduleRoot, badImportPath string) (string, bool) {
	segs := strings.Split(strings.Trim(badImportPath, "/"), "/")
	for i := 0; i < len(segs); i++ {
		rel := strings.Join(segs[i:], "/")
		if rel == "" {
			continue
		}
		if p, ok := goListDir(ctx, moduleRoot, rel); ok && p != badImportPath {
			return p, true
		}
	}
	return "", false
}

// goListDir runs `go list -f {{.ImportPath}} ./<relDir>` in moduleRoot and returns the
// resolved module import path. ok=false on any non-zero exit, timeout, or empty output.
func goListDir(ctx context.Context, moduleRoot, relDir string) (string, bool) {
	runCtx, cancel := context.WithTimeout(ctx, goListLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "go", "list", "-f", "{{.ImportPath}}", "./"+relDir)
	cmd.Dir = moduleRoot
	cmd.Env = cleanGitEnv()
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil || runCtx.Err() != nil {
		return "", false
	}
	s := strings.TrimSpace(out.String())
	return s, s != ""
}

// realGoDoc runs `go doc <symbol>` in pkgDir with a bounded timeout and a clean git env
// (mirroring execVerify). It is the production goDocResolver. ok=false on any non-zero
// exit, timeout, or empty output — the symbol didn't resolve, so the worker falls back to
// the raw compile error.
func realGoDoc(ctx context.Context, pkgDir, symbol string) (string, bool) {
	runCtx, cancel := context.WithTimeout(ctx, goDocLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "go", "doc", symbol)
	cmd.Dir = pkgDir
	cmd.Env = cleanGitEnv()
	var b strings.Builder
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	if err != nil || runCtx.Err() != nil {
		return "", false
	}
	out := strings.TrimSpace(b.String())
	return out, out != ""
}
