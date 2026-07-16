package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/model"
)

func bgSeat(o *Orchestrator) *OperatorSeat {
	return NewOperatorSeat(o, seatOperator{}, decideAdapter{id: "mid"}, decideAdapter{id: "strong"})
}

func TestApplySkipAbortDefault(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	s := bgSeat(o)
	skipSt := &RunState{Status: ChainFailed, ATs: []ATRecord{{Slug: "a", Status: ATFailed}}}
	if err := s.apply(context.Background(), skipSt, "a", OperatorDecision{Op: OpSkip}); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if skipSt.ATs[0].Status != ATSkipped {
		t.Fatalf("skip did not apply: %q", skipSt.ATs[0].Status)
	}
	abortSt := &RunState{Status: ChainFailed, ATs: []ATRecord{{Slug: "a"}}}
	if err := s.apply(context.Background(), abortSt, "a", OperatorDecision{Op: OpAbort}); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if abortSt.Status != ChainAborted {
		t.Fatalf("abort did not apply: %q", abortSt.Status)
	}
	// Unknown op → no-op, no error.
	if err := s.apply(context.Background(), abortSt, "a", OperatorDecision{Op: "weird"}); err != nil {
		t.Fatalf("unknown op should be a no-op, got %v", err)
	}
}

func TestApplyForceAdvanceAndBranchFix(t *testing.T) {
	repo := &resetRepo{}
	o := New(WithRepo(repo))
	s := bgSeat(o)
	faSt := &RunState{Status: ChainFailed, CurrentPosition: 0, ATs: []ATRecord{{Slug: "a", Status: ATFailed}, {Slug: "b"}}}
	if err := s.apply(context.Background(), faSt, "a", OperatorDecision{Op: OpForceAdvance, CommitSHA: "sha", Justification: "j"}); err != nil {
		t.Fatalf("force_advance: %v", err)
	}
	if faSt.ATs[0].Status != ATSuccess {
		t.Fatalf("force_advance did not apply")
	}
	// branch_fix with empty target falls back to the point.
	bfSt := &RunState{Status: ChainFailed, CurrentPosition: 0,
		ATs: []ATRecord{{Slug: "a", Status: ATFailed, Spec: AtomicTask{Slug: "a", Goal: "g"}}}}
	if err := s.apply(context.Background(), bfSt, "a", OperatorDecision{Op: OpBranchFix}); err != nil {
		t.Fatalf("branch_fix: %v", err)
	}
	if bfSt.findAT("a-fix1") == nil {
		t.Fatal("branch_fix via apply did not insert branch")
	}
}

func TestWithMaxInterventionsOption(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	s := NewOperatorSeat(o, seatOperator{}, decideAdapter{id: "mid"}, decideAdapter{id: "strong"}, WithMaxInterventions(1))
	if s.maxInterventions != 1 {
		t.Fatalf("maxInterventions = %d, want 1", s.maxInterventions)
	}
}

func TestGatherContextMissingAT(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	s := bgSeat(o)
	octx := s.gatherContext(context.Background(), &RunState{}, "ghost")
	if octx.FailedATSlug != "ghost" || octx.Goal != "" {
		t.Fatalf("missing AT should yield a bare context, got %+v", octx)
	}
}

// errOperator fails every decision.
type errOperator struct{}

func (errOperator) Decide(context.Context, model.Adapter, OperatorContext) (OperatorDecision, model.Usage, error) {
	return OperatorDecision{}, model.Usage{}, errors.New("operator down")
}

func TestSeatOperatorErrorHalts(t *testing.T) {
	o, st := seatHarness(t, 99)
	s := NewOperatorSeat(o, errOperator{}, decideAdapter{id: "mid"}, decideAdapter{id: "strong"})
	res := s.Run(context.Background(), st)
	if res.FinalStatus != ChainFailed || len(res.Interventions) != 0 {
		t.Fatalf("operator error should halt with no recorded intervention, got %+v", res)
	}
}

// applyErrOperator emits an edit with an empty goal so InterveneEdit errors.
type applyErrOperator struct{}

func (applyErrOperator) Decide(context.Context, model.Adapter, OperatorContext) (OperatorDecision, model.Usage, error) {
	return OperatorDecision{Op: OpEdit, Goal: ""}, model.Usage{}, nil
}

func TestSeatApplyErrorHalts(t *testing.T) {
	o, st := seatHarness(t, 99)
	s := NewOperatorSeat(o, applyErrOperator{}, decideAdapter{id: "mid"}, decideAdapter{id: "strong"})
	res := s.Run(context.Background(), st)
	if res.FinalStatus != ChainFailed {
		t.Fatalf("apply error should halt, status=%q", res.FinalStatus)
	}
}

func TestBranchFixWithGitDiffs(t *testing.T) {
	// A git-backed branch_fix: impl succeeded (committed) but tests failed; the
	// branch_fix captures impl's real diff and augments the branch goal with it.
	repo := initGitTarget(t)
	r := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	st := gitState(repo, "bf1")
	if err := r.Init(context.Background(), st); err != nil {
		t.Fatalf("init: %v", err)
	}
	base, _ := r.HeadSHA(context.Background())
	ws, _ := r.Open(context.Background(), base, &ATRecord{Slug: "impl", Position: 0})
	_ = os.WriteFile(filepath.Join(ws.Dir(), "impl.go"), []byte("package p\nfunc F() int { return 0 }\n"), 0o600)
	sha, _, _ := ws.Commit(context.Background(), "impl")
	_ = r.FastForward(context.Background(), sha)
	_ = ws.Close()

	o := New(WithRepo(r))
	run := &RunState{Status: ChainFailed, CurrentPosition: 1, IntegrationBranch: st.IntegrationBranch, RunID: st.RunID,
		FailedATSlug: "tests",
		ATs: []ATRecord{
			{Slug: "impl", Status: ATSuccess, ParentSHA: base, CommitSHA: sha, Spec: AtomicTask{Slug: "impl", Goal: "implement F"}},
			{Slug: "tests", Status: ATFailed, ParentSHA: sha, Diagnostic: "F should return 1", Spec: AtomicTask{Slug: "tests", Goal: "test F"}},
		}}
	if err := o.InterveneBranchFix(context.Background(), run, "impl", "", 0); err != nil {
		t.Fatalf("branch_fix: %v", err)
	}
	br := run.findAT("impl-fix1")
	if br == nil || !strings.Contains(br.Spec.Goal, "impl.go") {
		t.Fatalf("branch goal should include impl's prior diff, got %q", brGoal(br))
	}
}

func brGoal(ar *ATRecord) string {
	if ar == nil {
		return "(nil)"
	}
	return ar.Spec.Goal
}

func TestGitRepoErrorPaths(t *testing.T) {
	bad := NewGitRepo(ExecRunner{}, t.TempDir(), t.TempDir()) // not a git repo
	bad.integration = "x"
	if _, err := bad.Diff(context.Background(), "a", "b"); err == nil {
		t.Fatal("Diff in non-git dir should error")
	}
	if _, err := bad.DiffWorktree(context.Background(), t.TempDir(), "a"); err == nil {
		t.Fatal("DiffWorktree in non-git dir should error")
	}
	if _, err := bad.ListPackage(context.Background(), "x"); err == nil {
		t.Fatal("ListPackage in non-git dir should error")
	}
	st := &RunState{BaseBranch: "nonexistent-base", IntegrationBranch: "i", RunID: "z"}
	if err := bad.Init(context.Background(), st); err == nil {
		t.Fatal("Init off a missing base branch should error")
	}
	ws := &gitWorkspace{repo: bad, dir: t.TempDir()}
	if _, _, err := ws.Commit(context.Background(), "m"); err == nil {
		t.Fatal("Commit in non-git dir should error")
	}
}

func TestStartRepoInitError(t *testing.T) {
	o := New(WithRepo(initErrRepo{}))
	if _, err := o.Start(context.Background(), Chain{Slug: "c", Tasks: []AtomicTask{detTask("a")}}, "r"); err == nil {
		t.Fatal("Start should surface a repo Init error")
	}
}

// initErrRepo fails Init.
type initErrRepo struct{ NoopRepo }

func (initErrRepo) Init(context.Context, *RunState) error { return errors.New("init boom") }
