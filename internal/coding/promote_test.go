package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Promote is the integration→working-tree handoff: after a green chain the landed
// commits live on the coding/runs/<id>/integration branch; Promote fast-forwards
// the target repo's checked-out branch (and working tree) to that HEAD so the
// CALLER's plain filesystem read sees the deliverable. Without it the work is
// stranded on the run branch and the orchestrator reads a stale tree (bug
// corpos-orchestrate-spawn-coding-integration-commit-stranded-on-run-branch-...).

func TestGitRepoPromoteSurfacesIntegrationToWorkingTree(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "runprom")
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init: %v", err)
	}
	fork, _ := r.HeadSHA(context.Background())

	ws, err := r.Open(context.Background(), fork, &ATRecord{Slug: "add-file", Position: 0})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Dir(), "landed.txt"), []byte("fix\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sha, ok, err := ws.Commit(context.Background(), "AT 00: add-file")
	if err != nil || !ok {
		t.Fatalf("commit: sha=%q ok=%v err=%v", sha, ok, err)
	}
	if err := r.FastForward(context.Background(), sha); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	_ = ws.Close()

	// Precondition (the bug): the commit is on the integration branch but the target
	// repo's working tree the caller reads does NOT yet carry the file.
	if _, statErr := os.Stat(filepath.Join(repo, "landed.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: file should be absent from the working tree pre-promote (stat err=%v)", statErr)
	}

	if err := r.Promote(context.Background()); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// After promotion a plain read of the target repo sees the landed work, and the
	// checked-out branch HEAD equals the integration HEAD.
	got, err := os.ReadFile(filepath.Join(repo, "landed.txt"))
	if err != nil || string(got) != "fix\n" {
		t.Fatalf("working tree after promote: got %q err %v, want %q", got, err, "fix\n")
	}
	if head, _ := r.HeadSHA(context.Background()); gitHead(t, repo) != head {
		t.Fatalf("working-tree HEAD %q != integration HEAD %q", gitHead(t, repo), head)
	}
}

func TestGitRepoPromoteNoopWhenUpToDate(t *testing.T) {
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "runprom2")
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Nothing landed (integration == base); promote is a harmless no-op, not an error.
	if err := r.Promote(context.Background()); err != nil {
		t.Fatalf("promote up-to-date should succeed as a no-op: %v", err)
	}
}

// Wiring: on a green chain RunDuty must call Repo.Promote (surfacing the integration
// result to the caller's working tree); on a red chain it must NOT promote a
// non-deliverable. Mirrors TestRunDutySuccessReportsIntegrationCommit's harness.
func TestRunDutyPromotesOnlyOnGreenChain(t *testing.T) {
	green := &fakeRepo{head: "base", ws: &fakeWorkspace{dir: t.TempDir(), sha: "abc1234", ok: true}}
	o := New(WithRunner(&countRunner{passAt: 1}), WithRepo(green))
	o.model = &fakeWorker{}
	res, err := RunDuty(context.Background(), o, nil, BridgeChain("rp1", "duty", "x", codingProfile()), "rp1")
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}
	if !strings.Contains(res.Answer, "succeeded") {
		t.Fatalf("expected a green chain, got %q", res.Answer)
	}
	if !green.promoted {
		t.Fatal("a green chain must promote the integration result to the working tree")
	}

	// A red chain (gate never passes) must not promote.
	red := &fakeRepo{head: "base", ws: &fakeWorkspace{dir: t.TempDir(), sha: "def5678", ok: true}}
	o2 := New(WithRunner(&countRunner{passAt: 99}), WithRepo(red))
	o2.model = &fakeWorker{}
	if _, err := RunDuty(context.Background(), o2, nil, BridgeChain("rp2", "duty", "x", codingProfile()), "rp2"); err != nil {
		t.Fatalf("RunDuty(red): %v", err)
	}
	if red.promoted {
		t.Fatal("a red chain must NOT promote a non-deliverable to the working tree")
	}
}
