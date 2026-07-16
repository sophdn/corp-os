package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// Bug 1094: a go build/test verify gate must run in the Go module root. When the
// configured dir has no go.mod at/above it but a module lives in a subdirectory (the
// common `go/`-submodule monorepo layout), ResolveGoModuleDir finds it so the gate
// runs there instead of dead-ending on "no go.mod".

func writeMod(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveGoModuleDir_AtOrAboveReturnsDirUnchanged(t *testing.T) {
	root := t.TempDir()
	writeMod(t, root)
	sub := filepath.Join(root, "internal", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// go.mod at root: the dir itself resolves to itself (the `go` tool walks up).
	if got, ok := ResolveGoModuleDir(root); !ok || got != root {
		t.Fatalf("root: got %q ok=%v, want %q true", got, ok, root)
	}
	// A subdir below the module also keeps its dir (go walks up to find the module).
	if got, ok := ResolveGoModuleDir(sub); !ok || got != sub {
		t.Fatalf("subdir: got %q ok=%v, want %q true", got, ok, sub)
	}
}

func TestResolveGoModuleDir_FindsSubdirModule(t *testing.T) {
	root := t.TempDir()
	// No go.mod at root; the module is in go/ (the corpos-toolkit layout).
	writeMod(t, filepath.Join(root, "go"))
	got, ok := ResolveGoModuleDir(root)
	if !ok || got != filepath.Join(root, "go") {
		t.Fatalf("got %q ok=%v, want %q true", got, ok, filepath.Join(root, "go"))
	}
}

func TestResolveGoModuleDir_AmbiguousSubmodulesUnresolved(t *testing.T) {
	root := t.TempDir()
	// Two sibling modules at the same depth: ambiguous — refuse to guess.
	writeMod(t, filepath.Join(root, "a"))
	writeMod(t, filepath.Join(root, "b"))
	if got, ok := ResolveGoModuleDir(root); ok {
		t.Fatalf("ambiguous sibling modules must not resolve, got %q", got)
	}
}

func TestResolveGoModuleDir_NoModule(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := ResolveGoModuleDir(root); ok {
		t.Fatal("a tree with no go.mod must not resolve")
	}
}

// VerifyGateRunnable must treat a subdir-module dir as runnable (the gate will run in
// the resolved module root), not dead-end (bug 1094).
func TestVerifyGateRunnable_SubdirModuleIsRunnable(t *testing.T) {
	root := t.TempDir()
	writeMod(t, filepath.Join(root, "go"))
	if err := VerifyGateRunnable([]string{"go", "build", "./..."}, root); err != nil {
		t.Fatalf("a repo with a go/ submodule must be runnable, got: %v", err)
	}
	// A genuinely module-less tree still fails fast with the actionable error.
	bare := t.TempDir()
	if err := VerifyGateRunnable([]string{"go", "build", "./..."}, bare); err == nil {
		t.Fatal("a tree with no module anywhere must still be non-runnable")
	}
}
