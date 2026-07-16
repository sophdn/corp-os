package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageDirs(t *testing.T) {
	dirs := packageDirs([]string{"internal/foo/foo.go", "internal/foo/bar.go", "internal/baz/baz.go", "README.md", "*.go"})
	if len(dirs) != 2 || dirs[0] != "internal/baz" || dirs[1] != "internal/foo" {
		t.Fatalf("packageDirs = %v, want sorted [internal/baz internal/foo]", dirs)
	}
	if len(packageDirs([]string{"README.md"})) != 0 {
		t.Fatal("no .go patterns → no dirs")
	}
}

func TestCurrentPackageFilesNoopRepo(t *testing.T) {
	// NoopRepo is not a packageReader → empty (degrades, does not fail).
	if got := currentPackageFiles(context.Background(), NoopRepo{}, AtomicTask{Workspace: []string{"internal/foo/x.go"}}); got != "" {
		t.Fatalf("noop repo should yield empty package files, got %q", got)
	}
}

func TestCurrentPackageFilesGit(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "ctx1")
	_ = r.Init(context.Background(), st)
	fork, _ := r.HeadSHA(context.Background())
	ws, _ := r.Open(context.Background(), fork, &ATRecord{Slug: "seed", Position: 0})
	pkg := filepath.Join(ws.Dir(), "internal", "store")
	_ = os.MkdirAll(pkg, 0o750)
	_ = os.WriteFile(filepath.Join(pkg, "store.go"), []byte("package store\nfunc Open() {}\n"), 0o600)
	_ = os.WriteFile(filepath.Join(pkg, "store_test.go"), []byte("package store\n// test only\n"), 0o600)
	sha, _, _ := ws.Commit(context.Background(), "seed")
	_ = r.FastForward(context.Background(), sha)
	_ = ws.Close()

	got := currentPackageFiles(context.Background(), r, AtomicTask{Workspace: []string{"internal/store/store.go"}})
	if !strings.Contains(got, "func Open()") {
		t.Fatalf("current package files should include store.go source, got %q", got)
	}
	if strings.Contains(got, "test only") {
		t.Fatal("test files must be excluded from current-package context")
	}
}

func TestCaptureDiff(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "diff1")
	_ = r.Init(context.Background(), st)
	base, _ := r.HeadSHA(context.Background())

	// Succeeded AT: diff parent..commit.
	ws, _ := r.Open(context.Background(), base, &ATRecord{Slug: "impl", Position: 0})
	_ = os.WriteFile(filepath.Join(ws.Dir(), "new.go"), []byte("package p\n"), 0o600)
	sha, _, _ := ws.Commit(context.Background(), "impl")
	_ = r.FastForward(context.Background(), sha)
	_ = ws.Close()
	ar := &ATRecord{Slug: "impl", ParentSHA: base, CommitSHA: sha}
	if d := captureDiff(context.Background(), r, ar); !strings.Contains(d, "new.go") {
		t.Fatalf("succeeded-AT diff should mention new.go, got %q", d)
	}

	// Failed AT with a preserved worktree: diff worktree against parent.
	ws2, _ := r.Open(context.Background(), base, &ATRecord{Slug: "broken", Position: 1})
	_ = os.WriteFile(filepath.Join(ws2.Dir(), "broken.go"), []byte("package p // wip\n"), 0o600)
	failed := &ATRecord{Slug: "broken", ParentSHA: base, WorktreePath: ws2.Dir()}
	if d := captureDiff(context.Background(), r, failed); !strings.Contains(d, "broken.go") {
		t.Fatalf("failed-AT diff should mention broken.go, got %q", d)
	}

	// No fork point → empty.
	if d := captureDiff(context.Background(), r, &ATRecord{Slug: "x"}); d != "" {
		t.Fatalf("no parent_sha → empty diff, got %q", d)
	}
	// NoopRepo is not a packageReader → empty.
	if d := captureDiff(context.Background(), NoopRepo{}, ar); d != "" {
		t.Fatalf("noop repo → empty diff, got %q", d)
	}
}

func TestCapBytes(t *testing.T) {
	if capBytes("abc", 10) != "abc" {
		t.Fatal("short string unchanged")
	}
	if capBytes("abcdef", 3) != "abc" {
		t.Fatal("long string capped")
	}
}
