package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// okWrite is a successful fs.write dispatch on path p (the worker's file mutation that LANDED).
func okWrite(p string) tool.Result {
	return tool.Result{
		Call: tool.Call{Surface: "fs", Action: "write", Params: map[string]any{"path": p}},
		OK:   true,
	}
}

// TestScaffoldFabricationVerdict drives the guard's pure verdict over the dispatch-record /
// verify-dir equivalence classes: a go.mod (or sibling manifest) written INTO the verify-dir is
// the fabrication signature (refuse); a manifest written elsewhere, or a non-manifest write, or no
// write, is a clean green (pass).
func TestScaffoldFabricationVerdict(t *testing.T) {
	cases := []struct {
		name    string
		dir     string
		writes  []string
		fab     bool
		wantSub string
	}{
		{
			name:    "go.mod written into the verify-dir (the 2026-06-11 dogfood)",
			dir:     "/work/go",
			writes:  []string{"/work/go/go.mod"},
			fab:     true,
			wantSub: "fabricated a build-scaffold (go.mod) in the verify-dir",
		},
		{
			name:   "go.sum sibling manifest into the verify-dir also fabrication",
			dir:    "/work/go",
			writes: []string{"/work/go/go.sum"},
			fab:    true,
		},
		{
			name:   "go.mod in a SUBDIR of the verify-dir is still inside it",
			dir:    "/work",
			writes: []string{"/work/nested/go.mod"},
			fab:    true,
		},
		{
			name:   "relative go.mod with an explicit dir resolves into the verify-dir",
			dir:    "/work/go",
			writes: []string{"go.mod"},
			fab:    true,
		},
		{
			name:   "relative go.mod with NO dir (process CWD) is fabrication",
			dir:    "",
			writes: []string{"go.mod"},
			fab:    true,
		},
		{
			name:   "go.mod OUTSIDE the verify-dir is legitimate dependency work, not fabrication",
			dir:    "/work/go",
			writes: []string{"/other/repo/go.mod"},
			fab:    false,
		},
		{
			name:   "a production .go write is not a build-scaffold",
			dir:    "/work/go",
			writes: []string{"/work/go/internal/foo.go"},
			fab:    false,
		},
		{
			name:   "no writes at all is a clean green",
			dir:    "/work/go",
			writes: nil,
			fab:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var disp []tool.Result
			for _, w := range tc.writes {
				disp = append(disp, okWrite(w))
			}
			got := scaffoldFabricationVerdict(disp, tc.dir)
			if tc.fab && got == "" {
				t.Fatalf("expected a fabrication verdict, got pass")
			}
			if !tc.fab && got != "" {
				t.Fatalf("expected a clean pass, got verdict %q", got)
			}
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Fatalf("verdict %q should contain %q", got, tc.wantSub)
			}
		})
	}
}

// TestScaffoldFabricationGuardAssess confirms the Guard wrapper: a scaffold write into the
// verify-dir fails Assess; a clean dispatch passes. Stage/Name/Describe are the catalog metadata.
func TestScaffoldFabricationGuardAssess(t *testing.T) {
	g := ScaffoldFabricationGuard{VerifyDir: "/work/go"}
	if g.Stage() != StageFakeGreen {
		t.Fatalf("scaffold-fab must run at StageFakeGreen, got %v", g.Stage())
	}
	if g.Name() != "scaffold-fab" {
		t.Fatalf("name = %q", g.Name())
	}
	refuse := g.Assess(context.Background(), GuardInput{Dispatches: []tool.Result{okWrite("/work/go/go.mod")}})
	if refuse.ok() {
		t.Fatal("a go.mod written into the verify-dir must refuse the green")
	}
	clean := g.Assess(context.Background(), GuardInput{Dispatches: []tool.Result{okWrite("/work/go/internal/foo.go")}})
	if !clean.ok() {
		t.Fatalf("a production write must pass, got %q", clean.Reason)
	}
}

// TestScaffoldFabricationDeniedWriteIsNotCounted confirms only a LANDED scaffold write counts: a
// failed/denied fs.write of a go.mod (isMutatingWrite false) does not trip the guard, mirroring
// the fake-green guard's "denied write is not authored" rule.
func TestScaffoldFabricationDeniedWriteIsNotCounted(t *testing.T) {
	denied := tool.Result{
		Call: tool.Call{Surface: "fs", Action: "write", Params: map[string]any{"path": "/work/go/go.mod"}},
		OK:   false,
	}
	if v := scaffoldFabricationVerdict([]tool.Result{denied}, "/work/go"); v != "" {
		t.Fatalf("a denied go.mod write must not trip the guard, got %q", v)
	}
}

// TestVerifyGateRunnable covers Part A's pure precondition: a go build/test gate needs a reachable
// go.mod; a non-go gate or an empty command is always runnable.
func TestVerifyGateRunnable(t *testing.T) {
	// A temp dir WITH a go.mod is runnable; a sibling WITHOUT one fails fast.
	withMod := t.TempDir()
	if err := os.WriteFile(filepath.Join(withMod, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noMod := t.TempDir()

	if err := VerifyGateRunnable([]string{"go", "build", "./..."}, withMod); err != nil {
		t.Fatalf("a dir with go.mod must be runnable, got %v", err)
	}
	err := VerifyGateRunnable([]string{"sh", "-c", "go build ./... && go test ./..."}, noMod)
	if err == nil {
		t.Fatal("a go gate with no reachable go.mod must fail fast")
	}
	if !strings.Contains(err.Error(), "has no go.mod") || !strings.Contains(err.Error(), "module root") {
		t.Fatalf("the message must be actionable (name go.mod + the fix), got %q", err.Error())
	}
	// A go.mod in a PARENT dir is reachable (the module root the go tool walks up to).
	child := filepath.Join(withMod, "internal", "deep")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyGateRunnable([]string{"go", "test", "./..."}, child); err != nil {
		t.Fatalf("a go.mod in an ancestor dir must count as reachable, got %v", err)
	}
	// A non-go gate is not prechecked (its precondition is its own concern).
	if err := VerifyGateRunnable([]string{"make", "check"}, noMod); err != nil {
		t.Fatalf("a non-go gate must not be prechecked, got %v", err)
	}
	// An empty command is a no-op.
	if err := VerifyGateRunnable(nil, noMod); err != nil {
		t.Fatalf("an empty command must be runnable, got %v", err)
	}
}
