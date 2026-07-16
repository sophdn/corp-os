package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"corpos/internal/model"
)

// WithVerifyDir (the orchestrator's per-attempt worktree) overrides the spawner's fixed
// WithSpawnVerifyDir, so the worker's self-verify gate runs WHERE the worker edits — not
// against the unedited target repo. Without this the gate reads RED on a tree that never
// carries the attempt's fix and the opportunistic stop-when-green check can never fire.
func TestWithVerifyDir_OverridesSpawnDir(t *testing.T) {
	fs := &recordingWriteFS{}
	coder := &scriptedCoder{turns: []model.Response{{Text: "done", StopReason: model.StopEndTurn}}}

	target := t.TempDir()   // the spawner's fixed dir (the "wrong" one)
	worktree := t.TempDir() // the per-attempt worktree (where edits actually live)
	for _, d := range []string{target, worktree} {
		if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte("module x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var sawDir string
	sp := NewSpawner(fs, nil, nil, coder, WithSpawnVerifyDir(target))
	sp.verifyRun = func(_ context.Context, _ []string, dir string, _ time.Duration) (int, string) {
		sawDir = dir
		return 0, "ok"
	}
	// The per-spawn override is passed as a Run option (as the coding ModelWorker does).
	if _, err := sp.Run(context.Background(), codingProfile(), "build it", WithVerifyDir(worktree)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawDir != worktree {
		t.Fatalf("gate ran in %q, want the worktree override %q (not the fixed spawn dir %q)", sawDir, worktree, target)
	}
}

// An empty override or a gate-less loop is a no-op (the override never panics or blanks a dir).
func TestWithVerifyDir_EmptyAndGatelessNoop(t *testing.T) {
	l := New(single(model.NewEcho("q", model.Response{})), &fakeProvider{}, nil, WithVerifyDir(""))
	if l.verify != nil {
		t.Fatal("no gate configured — override must not create one")
	}
	g := &VerifyGate{Command: []string{"go", "test"}, Dir: "keep"}
	l2 := New(single(model.NewEcho("q", model.Response{})), &fakeProvider{}, nil, WithVerify(g), WithVerifyDir(""))
	if l2.verify.Dir != "keep" {
		t.Fatalf("empty override must leave the gate dir; got %q", l2.verify.Dir)
	}
}
