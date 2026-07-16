package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

func fsEdit(path string) tool.Result {
	return tool.Result{OK: true, Call: tool.Call{Surface: "fs", Action: "write", Params: map[string]any{"path": path}}}
}

func alwaysExist(string) bool { return true }

func TestScopeGoTest(t *testing.T) {
	gate := []string{"sh", "-c", "go build ./... && go test ./..."}
	// sh -c form: go test scoped, go build left whole-repo.
	got := ScopeGoTest(gate, []string{"./internal/admin/...", "./internal/tool/..."})
	want := "go build ./... && go test ./internal/admin/... ./internal/tool/..."
	if len(got) != 3 || got[2] != want {
		t.Fatalf("sh -c scope = %v, want test rewritten to %q", got, want)
	}
	// Bare argv form.
	bare := ScopeGoTest([]string{"go", "test", "./...", "-count=1"}, []string{"./internal/admin/..."})
	if strings.Join(bare, " ") != "go test ./internal/admin/... -count=1" {
		t.Fatalf("bare scope = %v", bare)
	}
	// Empty pkgs / unrecognized → unchanged.
	if got := ScopeGoTest(gate, nil); got[2] != "go build ./... && go test ./..." {
		t.Fatalf("empty pkgs must not rewrite: %v", got)
	}
	if got := ScopeGoTest([]string{"make", "test"}, []string{"./x/..."}); got[1] != "test" {
		t.Fatalf("unrecognized gate must be unchanged: %v", got)
	}
}

func TestGoPackagesFromEdits(t *testing.T) {
	// Module-relative path under module root /work/go (base "go", no prefix to strip).
	got := goPackagesFromEdits([]tool.Result{fsEdit("internal/admin/x_test.go")}, "/work/go", alwaysExist)
	if len(got) != 1 || got[0] != "./internal/admin/..." {
		t.Fatalf("module-relative scope = %v, want [./internal/admin/...]", got)
	}
	// Subdir-module: repo-relative path "go/internal/admin/..." with verifyDir "/work/go".
	sub := goPackagesFromEdits([]tool.Result{fsEdit("go/internal/admin/x_test.go")}, "/work/go", alwaysExist)
	if len(sub) != 1 || sub[0] != "./internal/admin/..." {
		t.Fatalf("subdir-module strip = %v, want [./internal/admin/...]", sub)
	}
	// Dedup across two files in the same package.
	dd := goPackagesFromEdits([]tool.Result{fsEdit("internal/admin/a.go"), fsEdit("internal/admin/b_test.go")}, "/work", alwaysExist)
	if len(dd) != 1 {
		t.Fatalf("same-package edits should dedupe to one scope, got %v", dd)
	}
	// A root-package edit can't be narrowed → nil (whole-repo).
	if got := goPackagesFromEdits([]tool.Result{fsEdit("main.go")}, "/work", alwaysExist); got != nil {
		t.Fatalf("root edit must bail to whole-repo, got %v", got)
	}
	// A derived dir that doesn't exist → nil (safety: never a false-failure scope).
	if got := goPackagesFromEdits([]tool.Result{fsEdit("internal/ghost/x.go")}, "/work", func(string) bool { return false }); got != nil {
		t.Fatalf("non-existent package dir must bail to whole-repo, got %v", got)
	}
	// Non-Go writes are ignored; no Go edits → nil.
	if got := goPackagesFromEdits([]tool.Result{fsEdit("README.md")}, "/work", alwaysExist); got != nil {
		t.Fatalf("non-Go edits must not scope, got %v", got)
	}
}

// editThenDone makes one fs.write, then claims done — so the verify gate runs with one
// edited .go file in the dispatch record.
type editThenDone struct {
	n    int
	path string
}

func (a *editThenDone) Model() string   { return "m" }
func (a *editThenDone) Available() bool { return true }
func (a *editThenDone) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	a.n++
	if a.n == 1 {
		return model.Response{Model: "m", ToolCalls: []tool.Call{{ID: "1", Surface: "fs", Action: "write", Params: map[string]any{"path": a.path}}}}, nil
	}
	return model.Response{Model: "m", Text: "done", StopReason: model.StopEndTurn}, nil
}

// TestLoopScopesVerifyGateToEditedPackage is the bug regression: when the worker edits a
// single package, the verify gate's whole-repo `go test ./...` is narrowed to that package
// (so a one-package change isn't gated on — and timed out by — the whole repo's suite).
func TestLoopScopesVerifyGateToEditedPackage(t *testing.T) {
	var gotCmd []string
	g := &VerifyGate{
		Command:   []string{"sh", "-c", "go build ./... && go test ./..."},
		MaxRounds: 2,
		run: func(_ context.Context, cmd []string, _ string, _ time.Duration) (int, string) {
			gotCmd = cmd
			return 0, "ok"
		},
	}
	adapter := &editThenDone{path: "internal/admin/server_introspection_test.go"}
	l := New(router.New(adapter, adapter), vProvider{}, []tool.Spec{{Name: "fs"}}, WithVerify(g))
	l.dirExists = alwaysExist // the edited package "exists" → scoping proceeds

	if _, err := l.Run(context.Background(), "author the admin test"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(gotCmd) != 3 {
		t.Fatalf("gate ran with %v, want the sh -c form", gotCmd)
	}
	if !strings.Contains(gotCmd[2], "go test ./internal/admin/...") {
		t.Fatalf("gate test was not scoped to the edited package: %q", gotCmd[2])
	}
	if !strings.Contains(gotCmd[2], "go build ./...") {
		t.Fatalf("go build must stay whole-repo for compile safety: %q", gotCmd[2])
	}
}
