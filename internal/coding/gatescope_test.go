package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func allDirs(string) bool { return true }

func TestIsGitRepo(t *testing.T) {
	if (&Orchestrator{repo: NoopRepo{}}).isGitRepo() {
		t.Error("NoopRepo must not count as a git repo (no git diff to scope from)")
	}
	if !(&Orchestrator{repo: &GitRepo{}}).isGitRepo() {
		t.Error("a *GitRepo must count as a git repo")
	}
}

func TestGateHasWholeRepoGoTest(t *testing.T) {
	if !gateHasWholeRepoGoTest([][]string{{"sh", "-c", "go build ./... && go test ./..."}}) {
		t.Error("a sh -c go test ./... gate must be detected")
	}
	if gateHasWholeRepoGoTest([][]string{{"true"}}) {
		t.Error("a non-go gate must not be detected")
	}
}

// goTestScopes with the real osDirExists: a present package dir scopes; an absent one bails.
func TestGoTestScopes_RealOSDirExists(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := goTestScopes([]string{"internal/work/chain.go"}, root, root, osDirExists); len(got) != 1 || got[0] != "./internal/work/..." {
		t.Fatalf("present pkg dir should scope, got %v", got)
	}
	if got := goTestScopes([]string{"internal/absent/x.go"}, root, root, osDirExists); got != nil {
		t.Fatalf("absent pkg dir must bail, got %v", got)
	}
}

func TestGoTestScopes_SubdirModule(t *testing.T) {
	// Worktree-relative edits in a go/-submodule repo map to module-relative package scopes.
	changed := []string{"go/internal/work/chain.go", "go/internal/work/task.go"}
	got := goTestScopes(changed, "/w", "/w/go", allDirs)
	if len(got) != 1 || got[0] != "./internal/work/..." {
		t.Fatalf("got %v, want [./internal/work/...] (deduped)", got)
	}
}

func TestGoTestScopes_ModuleAtRoot(t *testing.T) {
	got := goTestScopes([]string{"internal/cost/cost.go"}, "/w", "/w", allDirs)
	if len(got) != 1 || got[0] != "./internal/cost/..." {
		t.Fatalf("got %v, want [./internal/cost/...]", got)
	}
}

func TestGoTestScopes_BailsWhenUnsafe(t *testing.T) {
	// No Go edits → no scope.
	if got := goTestScopes(nil, "/w", "/w", allDirs); got != nil {
		t.Fatalf("no edits should not scope, got %v", got)
	}
	// An edit outside the gate's module (above gateDir) → bail (run whole module).
	if got := goTestScopes([]string{"other.go"}, "/w", "/w/go", allDirs); got != nil {
		t.Fatalf("an edit outside the module must bail, got %v", got)
	}
	// A derived package dir that doesn't exist → bail rather than risk a false failure.
	if got := goTestScopes([]string{"internal/work/x.go"}, "/w", "/w", func(string) bool { return false }); got != nil {
		t.Fatalf("a nonexistent package dir must bail, got %v", got)
	}
}

func TestScopeOrSkipGate_RewritesShellGoTest(t *testing.T) {
	cmds := [][]string{{"sh", "-c", "go build ./... && go test ./..."}}
	out, skip := scopeOrSkipGate(cmds, []string{"go/internal/work/chain.go"}, "/w", "/w/go", allDirs)
	if skip != "" {
		t.Fatalf("a mappable edit must scope, not skip: %q", skip)
	}
	if len(out) != 1 || !strings.Contains(out[0][2], "go test ./internal/work/...") || strings.Contains(out[0][2], "go test ./...") {
		t.Fatalf("gate not scoped: %v", out)
	}
	// build stays whole-module.
	if !strings.Contains(out[0][2], "go build ./...") {
		t.Fatalf("go build should stay whole-module: %v", out)
	}
}

// No Go edits → the organ must NOT run the whole-module suite (it only times out on a
// large module); scopeOrSkipGate returns a "no edits" diagnostic instead.
func TestScopeOrSkipGate_NoEditsSkips(t *testing.T) {
	cmds := [][]string{{"sh", "-c", "go build ./... && go test ./..."}}
	out, skip := scopeOrSkipGate(cmds, nil, "/w", "/w/go", allDirs)
	if out != nil {
		t.Fatalf("no-edits must not return runnable commands, got %v", out)
	}
	if !strings.Contains(skip, "no Go edits detected") {
		t.Fatalf("skipDiag = %q, want the no-edits diagnostic", skip)
	}
}

// Edits that don't map to a package under the module root → skip (not whole-module), with
// a distinct "couldn't map" diagnostic that names the files.
func TestScopeOrSkipGate_UnmappableEditsSkip(t *testing.T) {
	cmds := [][]string{{"sh", "-c", "go build ./... && go test ./..."}}
	out, skip := scopeOrSkipGate(cmds, []string{"other.go"}, "/w", "/w/go", allDirs)
	if out != nil {
		t.Fatalf("unmappable edits must not return runnable commands, got %v", out)
	}
	if !strings.Contains(skip, "could not be mapped") || !strings.Contains(skip, "other.go") {
		t.Fatalf("skipDiag = %q, want the unmappable-edit diagnostic naming the file", skip)
	}
}

// A gate without a whole-module `go test ./...` is returned unchanged and never skipped.
func TestScopeOrSkipGate_NonWholeModuleGateUnchanged(t *testing.T) {
	cmds := [][]string{{"go", "build", "./..."}}
	out, skip := scopeOrSkipGate(cmds, nil, "/w", "/w/go", allDirs)
	if skip != "" {
		t.Fatalf("a non-whole-module gate must not be skipped: %q", skip)
	}
	if len(out) != 1 || out[0][1] != "build" {
		t.Fatalf("gate should be returned unchanged, got %v", out)
	}
}
