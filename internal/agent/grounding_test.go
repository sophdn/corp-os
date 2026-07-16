package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
)

// adminBuildErrs is the REAL gate output from the swap-rehearsal (decompose-admin-surface
// run 2) — the floor worker hallucinated the admin API and the build failed naming the
// guessed symbols. The grounding reflex must turn these into go-doc lookups.
const adminBuildErrs = `# toolkit/internal/admin [toolkit/internal/admin.test]
internal/admin/server_introspection_test.go:12:12: undefined: health
internal/admin/server_introspection_test.go:15:3: unknown field Version in struct literal of type Deps
internal/admin/server_introspection_test.go:16:3: unknown field SchemaVersion in struct literal of type Deps
internal/admin/server_introspection_test.go:34:16: undefined: NewHandlers
internal/admin/server_introspection_test.go:36:17: undefined: http
internal/admin/server_introspection_test.go:37:44: undefined: http
FAIL	toolkit/internal/admin [build failed]`

func hasTarget(targets []groundingTarget, pkgDir, sym string) bool {
	for _, t := range targets {
		if t.pkgDir == pkgDir && t.symbol == sym {
			return true
		}
	}
	return false
}

func TestParseGroundingTargets_RealRehearsalErrors(t *testing.T) {
	got := parseGroundingTargets(adminBuildErrs)
	// The implicated symbols, each scoped to the offending file's package dir. The
	// `unknown field ... type Deps` lines both resolve to the TYPE Deps (deduped).
	for _, want := range []struct{ dir, sym string }{
		{"internal/admin", "Deps"},        // the money one — shows the real fields
		{"internal/admin", "health"},      // undefined identifier
		{"internal/admin", "NewHandlers"}, // hallucinated function
		{"internal/admin", "http"},        // forgotten import
	} {
		if !hasTarget(got, want.dir, want.sym) {
			t.Errorf("missing grounding target {%s %s} in %+v", want.dir, want.sym, got)
		}
	}
	// Deps appears once despite two `unknown field … type Deps` lines.
	depsCount := 0
	for _, tg := range got {
		if tg.symbol == "Deps" {
			depsCount++
		}
	}
	if depsCount != 1 {
		t.Errorf("Deps should be deduped to one target, got %d", depsCount)
	}
}

func TestParseGroundingTargets_PatternsAndCap(t *testing.T) {
	// Root-package file → empty pkgDir; "has no field or method" type extraction.
	root := parseGroundingTargets("main.go:5:2: undefined: Foo\nmain.go:6:3: d.X undefined (type Cfg has no field or method X)")
	if !hasTarget(root, "", "Foo") || !hasTarget(root, "", "Cfg") {
		t.Fatalf("root-package extraction wrong: %+v", root)
	}
	// Cap at maxGroundedSymbols.
	var b strings.Builder
	for i := 0; i < maxGroundedSymbols+4; i++ {
		b.WriteString("x.go:1:1: undefined: Sym")
		b.WriteByte('A' + byte(i))
		b.WriteByte('\n')
	}
	if n := len(parseGroundingTargets(b.String())); n > maxGroundedSymbols {
		t.Fatalf("targets = %d, want capped at %d", n, maxGroundedSymbols)
	}
	// No Go errors → no targets.
	if n := len(parseGroundingTargets("some unrelated test failure: assertion X != Y")); n != 0 {
		t.Fatalf("non-compile output should yield no targets, got %d", n)
	}
}

func TestIsGroundableSymbol(t *testing.T) {
	for _, ok := range []string{"Deps", "pkg.Sym", "corpos/internal/tool.Spec", "a_b"} {
		if !isGroundableSymbol(ok) {
			t.Errorf("%q should be groundable", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a;rm", "a$b", "a`b`", strings.Repeat("x", 200)} {
		if isGroundableSymbol(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// fakeResolver resolves only the REAL symbols (mimicking go doc: hallucinated names fail),
// and records every symbol it was asked for.
type fakeResolver struct{ asked []string }

func (f *fakeResolver) resolve(_ context.Context, _ /*pkgDir*/, symbol string) (string, bool) {
	f.asked = append(f.asked, symbol)
	if symbol == "Deps" {
		return "type Deps struct {\n\tPool *db.Pool\n\tStartedAt time.Time\n\tGitSHA string\n\tBuiltAtUnix int64\n\tPackageVer string\n}", true
	}
	return "", false // health / NewHandlers / http don't exist → no resolution
}

func TestGroundFromGateOutput_InjectsRealSignatures(t *testing.T) {
	f := &fakeResolver{}
	block := groundFromGateOutput(context.Background(), adminBuildErrs, "/work/go", f.resolve)
	if block == "" {
		t.Fatal("expected a grounded block for a build failure naming a real type")
	}
	if !strings.Contains(block, "Grounded signatures") || !strings.Contains(block, "GitSHA") {
		t.Fatalf("block should surface the REAL Deps fields (GitSHA), got:\n%s", block)
	}
	// The hallucinated symbols were attempted but did not resolve, so they don't appear.
	if strings.Contains(block, "NewHandlers") || strings.Contains(block, "--- health ---") {
		t.Fatalf("unresolved hallucinated symbols must not appear in the block:\n%s", block)
	}
	// It actually exercised go-doc on the real type.
	asked := strings.Join(f.asked, ",")
	if !strings.Contains(asked, "Deps") {
		t.Fatalf("resolver was not asked for Deps; asked=%v", f.asked)
	}
}

func TestGroundFromGateOutput_EmptyWhenNothingResolves(t *testing.T) {
	none := func(context.Context, string, string) (string, bool) { return "", false }
	if b := groundFromGateOutput(context.Background(), adminBuildErrs, "", none); b != "" {
		t.Fatalf("no resolutions → empty block, got %q", b)
	}
	if b := groundFromGateOutput(context.Background(), "clean output, no errors", "", (&fakeResolver{}).resolve); b != "" {
		t.Fatalf("no targets → empty block, got %q", b)
	}
}

// TestLoopGroundsVerifyFailure is the bug regression: when the verify gate fails with Go
// compile errors naming a guessed API, the loop resolves the real signatures and injects
// them into the revise feedback — the worker no longer has to choose to call go_doc.
func TestLoopGroundsVerifyFailure(t *testing.T) {
	l, calls := verifyLoop(3, func(n int) (int, string) {
		if n == 1 {
			return 1, adminBuildErrs // first verify: the hallucinated test won't compile
		}
		return 0, "ok"
	})
	f := &fakeResolver{}
	l.goDocResolve = f.resolve

	if _, err := l.Run(context.Background(), "write the admin characterization test"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("want one revise then pass (2 gate checks), got %d", *calls)
	}
	// The reflex resolved the real type from the compile error.
	if !strings.Contains(strings.Join(f.asked, ","), "Deps") {
		t.Fatalf("grounding reflex did not resolve Deps; asked=%v", f.asked)
	}
	// The revise feedback the worker saw carried the REAL signatures, not just the raw error.
	var revise string
	for _, m := range l.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "Automated verification failed") {
			revise = m.Content
		}
	}
	if revise == "" {
		t.Fatal("no revise feedback message found")
	}
	if !strings.Contains(revise, "Grounded signatures") || !strings.Contains(revise, "GitSHA") {
		t.Fatalf("revise feedback should carry the grounded real signatures, got:\n%s", revise)
	}
}

// TestRealGoDoc covers the production resolver against a real, stable symbol in this
// package (the test runs with cwd = the package dir, so a bare symbol resolves here).
func TestRealGoDoc(t *testing.T) {
	out, ok := realGoDoc(context.Background(), "", "VerifyGate")
	if !ok || !strings.Contains(out, "VerifyGate") {
		t.Fatalf("realGoDoc(VerifyGate) = %q ok=%v, want the real type doc", out, ok)
	}
	if _, ok := realGoDoc(context.Background(), "", "NoSuchSymbolZZZ"); ok {
		t.Fatal("a nonexistent symbol must not resolve")
	}
}
