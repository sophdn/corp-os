package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/model"
)

// importPathBuildErrs is the rehearsal shape (bug corpos-grounding-does-not-handle-import-path-
// errors): the floor worker wrote a repo-relative import `go/internal/dispatch` instead of the
// module path `toolkit/internal/dispatch`, so the build failed with "is not in std".
const importPathBuildErrs = `# toolkit/internal/admin [toolkit/internal/admin.test]
internal/admin/server_introspection_test.go:12:2: package go/internal/dispatch is not in std (/usr/local/go/src/go/internal/dispatch)
FAIL	toolkit/internal/admin [build failed]`

func TestParseImportPathErrors_Shapes(t *testing.T) {
	cases := []struct {
		name, line, want string
	}{
		{"not-in-std", "x.go:1:2: package go/internal/dispatch is not in std (/usr/.../dispatch)", "go/internal/dispatch"},
		{"not-in-goroot", "x.go:1:2: package go/internal/x is not in GOROOT (/usr/.../x)", "go/internal/x"},
		{"no-required-module", "x.go:1:2: no required module provides package go/internal/y; to add it:\n\tgo get go/internal/y", "go/internal/y"},
		{"cannot-find", `x.go:1:2: cannot find package "go/internal/z" in any of:`, "go/internal/z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseImportPathErrors(c.line)
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("parseImportPathErrors = %v, want [%s]", got, c.want)
			}
		})
	}
	// The real rehearsal fixture yields the one wrong path.
	if got := parseImportPathErrors(importPathBuildErrs); len(got) != 1 || got[0] != "go/internal/dispatch" {
		t.Fatalf("rehearsal fixture parse = %v, want [go/internal/dispatch]", got)
	}
	// Dedup + non-import output → nothing.
	dup := "a.go:1:1: package p/q is not in std\nb.go:2:2: package p/q is not in std"
	if got := parseImportPathErrors(dup); len(got) != 1 {
		t.Fatalf("dup paths should dedup to 1, got %v", got)
	}
	if got := parseImportPathErrors("some assertion failed: X != Y"); len(got) != 0 {
		t.Fatalf("non-import output → no paths, got %v", got)
	}
	// The undefined-symbol shape is NOT an import path (handled by the symbol arm).
	if got := parseImportPathErrors("a.go:1:1: undefined: Foo"); len(got) != 0 {
		t.Fatalf("undefined-symbol must not be parsed as an import path, got %v", got)
	}
}

func TestParseImportPathErrors_Cap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxGroundedImports+4; i++ {
		b.WriteString("x.go:1:1: package pkg/p")
		b.WriteByte('a' + byte(i))
		b.WriteString(" is not in std\n")
	}
	if n := len(parseImportPathErrors(b.String())); n > maxGroundedImports {
		t.Fatalf("paths = %d, want capped at %d", n, maxGroundedImports)
	}
}

func TestIsGroundableImportPath(t *testing.T) {
	for _, ok := range []string{"go/internal/x", "toolkit/internal/work", "a-b/c_d.e/f"} {
		if !isGroundableImportPath(ok) {
			t.Errorf("%q should be groundable", ok)
		}
	}
	for _, bad := range []string{"", "/abs/path", "a b", "a;rm", "a$b", strings.Repeat("x", 300)} {
		if isGroundableImportPath(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestGroundImportPaths_InjectsCorrectedPath(t *testing.T) {
	resolve := func(_ context.Context, _, bad string) (string, bool) {
		if bad == "go/internal/dispatch" {
			return "toolkit/internal/dispatch", true
		}
		return "", false
	}
	block := groundImportPaths(context.Background(), importPathBuildErrs, "/work/go", resolve)
	if !strings.Contains(block, "Corrected import paths") {
		t.Fatalf("block missing the corrected-imports header:\n%s", block)
	}
	if !strings.Contains(block, "go/internal/dispatch → toolkit/internal/dispatch") {
		t.Fatalf("block should map the wrong path to the real one, got:\n%s", block)
	}
}

func TestGroundImportPaths_EmptyWhenNothingResolves(t *testing.T) {
	none := func(context.Context, string, string) (string, bool) { return "", false }
	if b := groundImportPaths(context.Background(), importPathBuildErrs, "/w", none); b != "" {
		t.Fatalf("no resolution → empty block, got %q", b)
	}
	// A resolver echoing the same (wrong) path back must not be injected.
	echo := func(_ context.Context, _, bad string) (string, bool) { return bad, true }
	if b := groundImportPaths(context.Background(), importPathBuildErrs, "/w", echo); b != "" {
		t.Fatalf("a resolution equal to the wrong path must be dropped, got %q", b)
	}
	if b := groundImportPaths(context.Background(), "no import errors here", "/w", echo); b != "" {
		t.Fatalf("no import errors → empty block, got %q", b)
	}
}

// TestRealGoListImportPath drives a REAL import-path build error through the production
// resolver: a temp module with a real package at internal/dispatch, resolved from the
// repo-relative path the worker would wrongly write.
func TestRealGoListImportPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module toolkit\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "internal", "dispatch")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "dispatch.go"), []byte("package dispatch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The worker wrote "go/internal/dispatch" (repo-relative); the real path is the module one.
	got, ok := realGoListImportPath(context.Background(), root, "go/internal/dispatch")
	if !ok || got != "toolkit/internal/dispatch" {
		t.Fatalf("realGoListImportPath = %q ok=%v, want toolkit/internal/dispatch", got, ok)
	}
	// A path with no real directory under the module resolves to nothing.
	if _, ok := realGoListImportPath(context.Background(), root, "go/internal/nope"); ok {
		t.Fatal("a path with no matching package dir must not resolve")
	}
}

// importCycleBuildErrs is the REAL `go test` output shape for an in-package-test self-import
// (bug corpos-grounding-overcorrects-in-package-test-into-self-import-cycle), captured byte-for-
// byte from the toolchain (cat -A): the "package X" line and the tab-indented "imports X …" line
// are SEPARATE. The chain-378 re-validation caught the original single-line fixture/regex missing
// this — TestParseImportCycleErrors_RealToolchainOutput now pins it to live output.
const importCycleBuildErrs = "# toolkit/internal/admin\n" +
	"package toolkit/internal/admin\n" +
	"\timports toolkit/internal/admin from remote_test.go: import cycle not allowed in test\n" +
	"FAIL\ttoolkit/internal/admin [setup failed]\nFAIL"

func TestParseImportCycleErrors_Shapes(t *testing.T) {
	// The real rehearsal fixture: one self-import cycle, package + file captured.
	got := parseImportCycleErrors(importCycleBuildErrs)
	if len(got) != 1 || got[0].pkg != "toolkit/internal/admin" || got[0].file != "remote_test.go" {
		t.Fatalf("rehearsal fixture parse = %+v, want one {toolkit/internal/admin, remote_test.go}", got)
	}
	// The "from <file>" clause is optional (some toolchain paths omit it).
	noFile := parseImportCycleErrors("package a/b\n\timports a/b: import cycle not allowed in test")
	if len(noFile) != 1 || noFile[0].pkg != "a/b" || noFile[0].file != "" {
		t.Fatalf("no-file shape = %+v, want one {a/b, \"\"}", noFile)
	}
	// A CROSS-package test cycle (importing != imported) is a real design error, not the
	// self-import overcorrection — it must NOT be grounded with the "drop the import" rule.
	cross := parseImportCycleErrors("package a/b\n\timports a/c from b_test.go: import cycle not allowed in test")
	if len(cross) != 0 {
		t.Fatalf("a cross-package cycle must not be parsed as a self-import, got %+v", cross)
	}
	// Dedup by package; non-cycle output yields nothing.
	dup := importCycleBuildErrs + "\n" + importCycleBuildErrs
	if got := parseImportCycleErrors(dup); len(got) != 1 {
		t.Fatalf("dup self-imports should dedup to 1, got %+v", got)
	}
	if got := parseImportCycleErrors("a.go:1:1: undefined: Foo"); len(got) != 0 {
		t.Fatalf("a non-cycle error must yield no cycles, got %+v", got)
	}
}

// TestParseImportCycleErrors_RealToolchainOutput is the anti-drift guard: it drives a REAL
// self-importing in-package test through `go test` and asserts the parser finds the cycle in the
// actual captured output. This is what the hand-authored fixture failed to do — the original fix
// passed against a space-joined transcript but never matched live multi-line output.
func TestParseImportCycleErrors_RealToolchainOutput(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module probe\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "thing")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "thing.go"), []byte("package thing\n\nfunc Foo() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The overcorrection: an in-package test that imports its own package.
	selfImport := "package thing\n\nimport (\n\t\"testing\"\n\t\"probe/thing\"\n)\n\nfunc TestFoo(t *testing.T) {\n\tif thing.Foo() != 1 {\n\t\tt.Fail()\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "thing_test.go"), []byte(selfImport), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "./thing/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the self-import build to fail; output:\n%s", out)
	}
	got := parseImportCycleErrors(string(out))
	if len(got) != 1 || got[0].pkg != "probe/thing" {
		t.Fatalf("parser missed the cycle in REAL toolchain output:\n%s\nparsed=%+v", out, got)
	}
	if got[0].file != "thing_test.go" {
		t.Errorf("want the test file thing_test.go captured, got %q", got[0].file)
	}
}

func TestGroundImportCycles_InjectsTheInPackageRule(t *testing.T) {
	block := groundImportCycles(importCycleBuildErrs)
	if !strings.Contains(block, "import cycle not allowed in test") {
		t.Fatalf("block should name the error it fixes, got:\n%s", block)
	}
	if !strings.Contains(block, "must NOT import that package") {
		t.Fatalf("block should carry the in-package rule, got:\n%s", block)
	}
	// It names the offending self-import + the package short name so the worker knows what to drop.
	if !strings.Contains(block, `import "toolkit/internal/admin"`) || !strings.Contains(block, "own package admin") {
		t.Fatalf("block should name the self-import to remove and the package, got:\n%s", block)
	}
	// No cycle → empty (so it doesn't add noise to unrelated failures).
	if b := groundImportCycles("a.go:1:1: undefined: Foo"); b != "" {
		t.Fatalf("no cycle → empty block, got %q", b)
	}
}

// TestLoopGroundsImportCycleFailure is the regression: a gate failing with a self-import cycle gets
// the in-package rule injected into the revise feedback (no resolver needed), so the worker drops
// the self-import on the next attempt instead of thrashing to Opus.
func TestLoopGroundsImportCycleFailure(t *testing.T) {
	l, calls := verifyLoop(3, func(n int) (int, string) {
		if n == 1 {
			return 1, importCycleBuildErrs
		}
		return 0, "ok"
	})
	// Both resolver arms off — the import-cycle arm is pure guidance and must fire on its own.
	l.goDocResolve = func(context.Context, string, string) (string, bool) { return "", false }
	l.goListResolve = func(context.Context, string, string) (string, bool) { return "", false }
	if _, err := l.Run(context.Background(), "fix the test"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("want one revise then pass (2 gate checks), got %d", *calls)
	}
	var revise string
	for _, m := range l.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "Automated verification failed") {
			revise = m.Content
		}
	}
	if !strings.Contains(revise, "must NOT import that package") || !strings.Contains(revise, `import "toolkit/internal/admin"`) {
		t.Fatalf("revise feedback should carry the in-package import-cycle rule, got:\n%s", revise)
	}
}

// TestLoopGroundsImportPathFailure is the regression: a gate failing with an import-path error
// gets the corrected module path injected into the revise feedback (the import-path sibling of
// TestLoopGroundsVerifyFailure).
func TestLoopGroundsImportPathFailure(t *testing.T) {
	l, calls := verifyLoop(3, func(n int) (int, string) {
		if n == 1 {
			return 1, importPathBuildErrs
		}
		return 0, "ok"
	})
	// Symbol arm off (no undefined symbols here anyway); import arm resolves deterministically.
	l.goDocResolve = func(context.Context, string, string) (string, bool) { return "", false }
	l.goListResolve = func(_ context.Context, _, bad string) (string, bool) {
		if bad == "go/internal/dispatch" {
			return "toolkit/internal/dispatch", true
		}
		return "", false
	}
	if _, err := l.Run(context.Background(), "fix the import"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("want one revise then pass (2 gate checks), got %d", *calls)
	}
	var revise string
	for _, m := range l.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "Automated verification failed") {
			revise = m.Content
		}
	}
	if !strings.Contains(revise, "go/internal/dispatch → toolkit/internal/dispatch") {
		t.Fatalf("revise feedback should carry the corrected import path, got:\n%s", revise)
	}
}
