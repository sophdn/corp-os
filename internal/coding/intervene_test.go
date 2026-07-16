package coding

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// resetRepo records ResetTo calls and can be made to fail.
type resetRepo struct {
	NoopRepo
	resetTo  string
	resetErr error
}

func (r *resetRepo) ResetTo(_ context.Context, sha string) error {
	r.resetTo = sha
	return r.resetErr
}

func TestInterveneEdit(t *testing.T) {
	o := New()
	st := &RunState{Status: ChainFailed, CurrentPosition: 0, FailedATSlug: "a",
		ATs: []ATRecord{{Slug: "a", Status: ATFailed, Iterations: 2, Spec: AtomicTask{Slug: "a", Goal: "old"}}}}
	if err := o.InterveneEdit(st, Edit{Goal: "new precise goal", Workspace: []string{"x/**"}}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	ar := st.ATs[0]
	if ar.Spec.Goal != "new precise goal" || ar.Status != ATPending || ar.Iterations != 0 || st.Status != ChainPending {
		t.Fatalf("edit did not apply+reset: %+v / %q", ar, st.Status)
	}
	if len(ar.Spec.Workspace) != 1 || ar.Spec.Workspace[0] != "x/**" {
		t.Fatalf("workspace not overridden: %v", ar.Spec.Workspace)
	}
}

func TestInterveneEditErrors(t *testing.T) {
	o := New()
	if err := o.InterveneEdit(&RunState{Status: ChainRunning}, Edit{Goal: "g"}); err == nil {
		t.Fatal("edit on running chain should error")
	}
	st := &RunState{Status: ChainFailed, ATs: []ATRecord{{Slug: "a"}}}
	if err := o.InterveneEdit(st, Edit{Goal: "  "}); err == nil {
		t.Fatal("empty goal should error")
	}
	st2 := &RunState{Status: ChainFailed, CurrentPosition: 9, ATs: []ATRecord{{Slug: "a"}}}
	if err := o.InterveneEdit(st2, Edit{Goal: "g"}); err == nil {
		t.Fatal("no current AT should error")
	}
}

func TestInterveneForceAdvance(t *testing.T) {
	repo := &resetRepo{}
	o := New(WithRepo(repo))
	st := &RunState{Status: ChainFailed, CurrentPosition: 0, FailedATSlug: "a",
		ATs: []ATRecord{{Slug: "a", Status: ATFailed}, {Slug: "b", Status: ATPending}}}
	if err := o.InterveneForceAdvance(context.Background(), st, "cafe1234", "impl correct; tests wrong"); err != nil {
		t.Fatalf("force_advance: %v", err)
	}
	if st.ATs[0].Status != ATSuccess || st.ATs[0].CommitSHA != "cafe1234" || st.ATs[0].WorkerStatus != WorkerForceAdvance {
		t.Fatalf("force_advance did not mark success: %+v", st.ATs[0])
	}
	if repo.resetTo != "cafe1234" || st.CurrentPosition != 1 || st.Status != ChainPending {
		t.Fatalf("force_advance did not advance/reset: reset=%q pos=%d", repo.resetTo, st.CurrentPosition)
	}
}

func TestInterveneForceAdvanceErrors(t *testing.T) {
	o := New()
	st := &RunState{Status: ChainFailed, ATs: []ATRecord{{Slug: "a"}}}
	if err := o.InterveneForceAdvance(context.Background(), st, "", "j"); err == nil {
		t.Fatal("missing commit should error")
	}
	if err := o.InterveneForceAdvance(context.Background(), st, "sha", ""); err == nil {
		t.Fatal("missing justification should error")
	}
	if err := o.InterveneForceAdvance(context.Background(), &RunState{Status: ChainRunning}, "sha", "j"); err == nil {
		t.Fatal("not stageable should error")
	}
	// reset error surfaces.
	bad := &resetRepo{resetErr: errors.New("bad ref")}
	o2 := New(WithRepo(bad))
	st2 := &RunState{Status: ChainFailed, ATs: []ATRecord{{Slug: "a"}}}
	if err := o2.InterveneForceAdvance(context.Background(), st2, "sha", "j"); err == nil {
		t.Fatal("reset error should surface")
	}
}

func TestInterveneBranchFixFailedAT(t *testing.T) {
	o := New() // NoopRepo
	st := &RunState{Status: ChainFailed, CurrentPosition: 0, FailedATSlug: "impl",
		ATs: []ATRecord{{Slug: "impl", Status: ATFailed, Diagnostic: "does not compile",
			Spec: AtomicTask{Slug: "impl", Goal: "build the thing"}}}}
	if err := o.InterveneBranchFix(context.Background(), st, "impl", "", 0); err != nil {
		t.Fatalf("branch_fix: %v", err)
	}
	if len(st.ATs) != 2 {
		t.Fatalf("want 2 ATs after branch insert, got %d", len(st.ATs))
	}
	br := st.ATs[0]
	if br.Slug != "impl-fix1" || br.ParentATSlug != "impl" || br.BranchIndex != 1 || br.Status != ATPending {
		t.Fatalf("branch record wrong: %+v", br)
	}
	if !strings.Contains(br.Spec.Goal, "build the thing") || !strings.Contains(br.Spec.Goal, "does not compile") {
		t.Fatalf("branch goal missing augmentation: %q", br.Spec.Goal)
	}
	if st.ATs[1].Slug != "impl" || st.ATs[1].Status != ATSkipped {
		t.Fatalf("original target not superseded: %+v", st.ATs[1])
	}
	if st.CurrentPosition != 0 || st.Status != ChainPending {
		t.Fatalf("branch_fix should resume from branch: pos=%d status=%q", st.CurrentPosition, st.Status)
	}
}

func TestInterveneBranchFixPriorATRewinds(t *testing.T) {
	repo := &resetRepo{}
	o := New(WithRepo(repo))
	// impl (idx0) succeeded; tests (idx1) failed → branch_fix the prior impl.
	st := &RunState{Status: ChainFailed, CurrentPosition: 1, FailedATSlug: "tests",
		ATs: []ATRecord{
			{Slug: "impl", Status: ATSuccess, CommitSHA: "c1", ParentSHA: "base", Spec: AtomicTask{Slug: "impl", Goal: "impl"}},
			{Slug: "tests", Status: ATFailed, Diagnostic: "assertion failed", ParentSHA: "c1", Spec: AtomicTask{Slug: "tests", Goal: "tests"}},
		}}
	if err := o.InterveneBranchFix(context.Background(), st, "impl", "", 0); err != nil {
		t.Fatalf("branch_fix: %v", err)
	}
	if repo.resetTo != "base" {
		t.Fatalf("should rewind integration to impl's fork point, reset=%q", repo.resetTo)
	}
	// branch inserted at impl's position; failed downstream reset to pending.
	if st.ATs[0].Slug != "impl-fix1" {
		t.Fatalf("branch not at impl position: %+v", st.ATs[0])
	}
	tests := st.findAT("tests")
	if tests.Status != ATPending {
		t.Fatalf("downstream tests should be reset to pending, got %q", tests.Status)
	}
}

func TestInterveneBranchFixGuards(t *testing.T) {
	o := New()
	mk := func() *RunState {
		return &RunState{Status: ChainFailed, CurrentPosition: 0, FailedATSlug: "a",
			ATs: []ATRecord{{Slug: "a", Status: ATFailed, Spec: AtomicTask{Slug: "a"}}}}
	}
	if err := o.InterveneBranchFix(context.Background(), mk(), "nope", "", 0); err == nil {
		t.Fatal("unknown target should error")
	}
	if err := o.InterveneBranchFix(context.Background(), &RunState{Status: ChainRunning}, "a", "", 0); err == nil {
		t.Fatal("not stageable should error")
	}
	// out-of-scope: target far from the failed point.
	far := &RunState{Status: ChainFailed, CurrentPosition: 2, FailedATSlug: "c",
		ATs: []ATRecord{{Slug: "a", Spec: AtomicTask{Slug: "a"}}, {Slug: "b"}, {Slug: "c", Status: ATFailed}}}
	if err := o.InterveneBranchFix(context.Background(), far, "a", "", 0); err == nil {
		t.Fatal("out-of-scope target should error")
	}
	// max_branches exhausted: pre-existing branch + maxBranches=1.
	ex := &RunState{Status: ChainFailed, CurrentPosition: 0, FailedATSlug: "a",
		ATs: []ATRecord{
			{Slug: "a-fix1", Status: ATFailed, ParentATSlug: "a", BranchIndex: 1, Spec: AtomicTask{Slug: "a-fix1"}},
			{Slug: "a", Status: ATSkipped, Spec: AtomicTask{Slug: "a"}},
		}}
	if err := o.InterveneBranchFix(context.Background(), ex, "a", "", 1); err == nil {
		t.Fatal("max_branches exhausted should error")
	}
}
