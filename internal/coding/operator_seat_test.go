package coding

import (
	"context"
	"testing"
	"time"

	"corpos/internal/model"
)

// countRunner fails every gate until the passAt-th call, then passes. A 1-command
// gate + MaxIterations=1 makes one call per chain run/resume.
type countRunner struct {
	calls  int
	passAt int
}

func (r *countRunner) Run(_ context.Context, cmd []string, _ string, _ time.Duration) CommandResult {
	r.calls++
	if r.calls >= r.passAt {
		return CommandResult{Command: cmd}
	}
	return CommandResult{Command: cmd, ExitCode: 1, Stderr: "gate miss"}
}

// seatOperator always emits an edit (diagnosis right); whether it lands depends on
// the gate runner's schedule.
type seatOperator struct{ usage model.Usage }

func (o seatOperator) Decide(_ context.Context, _ model.Adapter, _ OperatorContext) (OperatorDecision, model.Usage, error) {
	return OperatorDecision{Op: OpEdit, Goal: "corrected, API-pinned goal", Reason: "fix"}, o.usage, nil
}

func seatHarness(t *testing.T, passAt int) (*Orchestrator, *RunState) {
	t.Helper()
	o := New(WithRunner(&countRunner{passAt: passAt}), WithRepo(NoopRepo{Dir: t.TempDir()}))
	o.model = &fakeWorker{}
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 1}
	st, err := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st) // first gate miss → FAILED
	if st.Status != ChainFailed {
		t.Fatalf("want initial failure, got %q", st.Status)
	}
	return o, st
}

func seat(o *Orchestrator) *OperatorSeat {
	return NewOperatorSeat(o, seatOperator{usage: model.Usage{InputTokens: 200, OutputTokens: 80}},
		decideAdapter{id: "google/gemini-3.1-flash-lite"}, decideAdapter{id: "claude-opus-4-8"}, WithK(2))
}

func TestSeatMidCarries(t *testing.T) {
	// Gate passes on the 2nd call → one mid edit carries the point, no escalation.
	o, st := seatHarness(t, 2)
	res := seat(o).Run(context.Background(), st)
	if res.FinalStatus != ChainSuccess {
		t.Fatalf("want success, got %q", res.FinalStatus)
	}
	if len(res.Interventions) != 1 || res.Interventions[0].Tier != "mid" || !res.Interventions[0].Carried {
		t.Fatalf("want 1 mid carry, got %+v", res.Interventions)
	}
	if res.CarriedFraction() != 1.0 {
		t.Fatalf("carried_fraction = %v, want 1.0", res.CarriedFraction())
	}
	if res.TotalUSD <= 0 {
		t.Fatalf("mid (gemini) cost should be > 0, got %v", res.TotalUSD)
	}
	if len(res.EscalationCauses) != 0 {
		t.Fatalf("no escalation expected, got %v", res.EscalationCauses)
	}
}

func TestSeatEscalatesToStrongAuthoring(t *testing.T) {
	// Gate passes only on the 4th call: miss(initial), miss(mid#1), miss(mid#2),
	// pass(strong#3). The mid tier diagnosed right (edit) but couldn't author it;
	// strong's edit lands → escalation_cause = authoring.
	o, st := seatHarness(t, 4)
	res := seat(o).Run(context.Background(), st)
	if res.FinalStatus != ChainSuccess {
		t.Fatalf("want success, got %q", res.FinalStatus)
	}
	if len(res.Interventions) != 3 {
		t.Fatalf("want 3 interventions (2 mid + 1 strong), got %d: %+v", len(res.Interventions), res.Interventions)
	}
	last := res.Interventions[2]
	if last.Tier != "strong" || !last.Escalated || !last.Carried {
		t.Fatalf("last intervention should be a carrying strong escalation: %+v", last)
	}
	if res.EscalationCauses["a"] != "authoring" {
		t.Fatalf("escalation cause = %q, want authoring", res.EscalationCauses["a"])
	}
	if res.CarriedFraction() != 0.0 {
		t.Fatalf("escalated point should not count as mid-carried; fraction = %v", res.CarriedFraction())
	}
}

func TestSeatImpasse(t *testing.T) {
	// Gate never passes → strong also fails → impasse, chain stays failed.
	o, st := seatHarness(t, 99)
	res := seat(o).Run(context.Background(), st)
	if res.FinalStatus != ChainFailed {
		t.Fatalf("want failed at impasse, got %q", res.FinalStatus)
	}
	// 2 mid + 1 strong attempt then halt.
	if len(res.Interventions) != 3 {
		t.Fatalf("want 3 attempts before impasse, got %d", len(res.Interventions))
	}
}

func TestSeatCarriedFractionNoInterventions(t *testing.T) {
	if (SeatResult{}).CarriedFraction() != 1.0 {
		t.Fatal("no interventions → carried fraction 1.0")
	}
}

func TestFailedPointAndPointCleared(t *testing.T) {
	st := &RunState{Status: ChainPaused, CurrentPosition: 0, ATs: []ATRecord{{Slug: "a", Status: ATFailed}}}
	if failedPoint(st) != "a" {
		t.Fatal("paused chain should point at current AT")
	}
	if pointCleared(st, "a") {
		t.Fatal("a still-failing point is not cleared")
	}
	st.Status = ChainSuccess
	if !pointCleared(st, "a") {
		t.Fatal("terminal chain clears the point")
	}
	// branch supersession: original skipped → cleared.
	st2 := &RunState{Status: ChainFailed, FailedATSlug: "a-fix1", ATs: []ATRecord{{Slug: "a", Status: ATSkipped}}}
	if !pointCleared(st2, "a") {
		t.Fatal("superseded original should be cleared")
	}
	if failedPoint(&RunState{Status: ChainFailed}) != "" {
		t.Fatal("no AT → empty point")
	}
}
