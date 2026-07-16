package coding

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// tierEchoWorker records the carried tier each attempt receives and reports a
// scripted highest-tier-reached, so a test can prove the carry threads across
// respawns (chain 392 task 3314).
type tierEchoWorker struct {
	carriedIn []string
	report    []string
	n         int
}

func (w *tierEchoWorker) Attempt(_ context.Context, _ AtomicTask, _ string, fb Feedback) AttemptResult {
	w.carriedIn = append(w.carriedIn, fb.CarriedTierModel)
	out := ""
	if w.n < len(w.report) {
		out = w.report[w.n]
	}
	w.n++
	return AttemptResult{Note: "ok", HighestTierModel: out}
}

// recordingWorker captures the Feedback handed to each attempt so a test can assert
// what cross-attempt state is (and is not) threaded between respawns of one atom.
type recordingWorker struct{ feedbacks []Feedback }

func (w *recordingWorker) Attempt(_ context.Context, _ AtomicTask, _ string, fb Feedback) AttemptResult {
	w.feedbacks = append(w.feedbacks, fb)
	return AttemptResult{Note: "ok"}
}

// TestRunWorkerLoopRespawnsWithoutTierCarry is a CHARACTERIZATION test (chain 392
// task 3313). Across the respawns of one non-converging atom, the ONLY cross-attempt
// state runWorkerLoop threads is PriorGateDiagnostic. No escalated tier is carried —
// each respawn hands the worker a fresh Feedback and (downstream) a fresh router at
// profile.Tier, so the tier a prior attempt reached is discarded. Task 3314 adds a
// carried-tier channel; a later attempt then begins at >= the tier the prior one
// reached. This pins the no-carry baseline.
func TestRunWorkerLoopRespawnsWithoutTierCarry(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}))
	rw := &recordingWorker{}
	o.model = rw
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 3}
	st, err := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	o.RunToCompletion(context.Background(), st)

	if len(rw.feedbacks) != 3 {
		t.Fatalf("want 3 respawns (== MaxIterations), got %d", len(rw.feedbacks))
	}
	// Attempt 1 has no prior diagnostic; later attempts carry the gate's last
	// diagnostic — and that is the ONLY thing carried between respawns today.
	if rw.feedbacks[0].PriorGateDiagnostic != "" {
		t.Fatalf("first attempt should carry no prior diagnostic, got %q", rw.feedbacks[0].PriorGateDiagnostic)
	}
	for i := 1; i < len(rw.feedbacks); i++ {
		if rw.feedbacks[i].PriorGateDiagnostic == "" {
			t.Fatalf("attempt %d should carry the prior gate diagnostic (the sole cross-attempt state)", i+1)
		}
	}
}

// TestSingleAtomRespawnsUnboundedPerDuty is a CHARACTERIZATION test (chain 392 task
// 3313). One non-converging atom re-spawns a fresh worker on EVERY runWorkerLoop
// iteration AND on every operator-seat resume, with no per-duty cap distinct from
// the per-loop maxIter and the seat's maxInterventions. Here: 2 initial iterations +
// 3 seat interventions (mid, mid, strong-impasse) × 2 iterations each = 8 worker
// spawns for ONE atom. In the live path the only hard ceiling is the tree-wide
// -max-spawns budget. Task 3315 adds a per-duty respawn cap; this total then drops to
// the cap with an honest stuck verdict.
func TestSingleAtomRespawnsUnboundedPerDuty(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}))
	fw := &fakeWorker{}
	o.model = fw
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 2}
	st, err := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainFailed {
		t.Fatalf("want initial failure, got %q", st.Status)
	}
	res := seat(o).Run(context.Background(), st)
	if res.FinalStatus != ChainFailed {
		t.Fatalf("want impasse (gate never passes), got %q", res.FinalStatus)
	}
	if fw.attempts != 8 {
		t.Fatalf("characterization: want 8 unbounded per-duty respawns (2 initial + 3 seat interventions × 2), got %d", fw.attempts)
	}
}

// TestRunWorkerLoopCarriesEscalatedTier proves the fix (chain 392 task 3314): the
// tier each respawn reaches is carried into the next respawn's starting floor, and
// the AT records the highest reached — flipping the no-carry characterization.
func TestRunWorkerLoopCarriesEscalatedTier(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}))
	w := &tierEchoWorker{report: []string{"mid", "coder", "coder"}}
	o.model = w
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 3}
	st, err := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	o.RunToCompletion(context.Background(), st)

	// Attempt 1 carries nothing; attempt 2 carries the mid it reached; attempt 3 carries
	// the coder rung it then climbed to — the escalated tier is no longer discarded.
	if got, want := w.carriedIn, []string{"", "mid", "coder"}; !slices.Equal(got, want) {
		t.Fatalf("carried-in tiers = %v, want %v", got, want)
	}
	if st.ATs[0].HighestTierModel != "coder" {
		t.Fatalf("AT should persist the highest tier reached, got %q", st.ATs[0].HighestTierModel)
	}
}

// TestCarriedTierPersistsAcrossSeatResume proves the carried tier survives an operator-
// seat intervention of the same atom (resetAT does NOT clear it): the first resume
// attempt starts at the tier the initial loop reached (chain 392 task 3314).
func TestCarriedTierPersistsAcrossSeatResume(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}))
	w := &tierEchoWorker{report: []string{"mid"}} // reaches mid on the initial attempt only
	o.model = w
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 1}
	st, err := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st) // 1 attempt: carried "", reaches "mid"
	if st.Status != ChainFailed {
		t.Fatalf("want initial failure, got %q", st.Status)
	}
	seat(o).Run(context.Background(), st) // each seat resume re-enters runWorkerLoop

	if len(w.carriedIn) < 2 {
		t.Fatalf("expected at least one seat resume, carriedIn=%v", w.carriedIn)
	}
	if w.carriedIn[0] != "" || w.carriedIn[1] != "mid" {
		t.Fatalf("first seat resume should carry the persisted tier; carriedIn=%v, want [\"\", \"mid\", …]", w.carriedIn)
	}
}

// TestPerDutyRespawnCapStopsWithHonestVerdict proves the fix (chain 392 task 3315): a
// per-duty respawn cap bounds the worker→gate loop ENTRIES for one atom across the whole
// duty. The (cap+1)th re-entry is refused with an honest stuck verdict carrying the last
// real gate diagnostic, and the operator seat halts (even on a cheap rung) instead of
// thrashing — flipping the unbounded-per-duty characterization. With cap=2 + MaxIterations=2:
// entry 1 (initial) + entry 2 (seat resume) run fully (4 worker attempts); the 3rd entry is
// refused while still on the mid rung, so the seat-guard (not onTopRung) is what stops it.
func TestPerDutyRespawnCapStopsWithHonestVerdict(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}), WithPerDutyRespawnCap(2))
	fw := &fakeWorker{}
	o.model = fw
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 2}
	st, err := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	res := seat(o).Run(context.Background(), st)

	if res.FinalStatus != ChainFailed {
		t.Fatalf("want failed (stuck), got %q", res.FinalStatus)
	}
	if fw.attempts != 4 {
		t.Fatalf("cap=2 entries × MaxIterations=2 = 4 worker attempts, got %d", fw.attempts)
	}
	if st.ATs[0].RespawnCount != 2 {
		t.Fatalf("want 2 worker→gate entries (the cap), got %d", st.ATs[0].RespawnCount)
	}
	if st.ATs[0].WorkerStatus != WorkerRespawnCapReached {
		t.Fatalf("want WorkerRespawnCapReached, got %q", st.ATs[0].WorkerStatus)
	}
	if !strings.Contains(st.ATs[0].Diagnostic, "respawn cap") || !strings.Contains(st.ATs[0].Diagnostic, "gate miss") {
		t.Fatalf("verdict should be honest and carry the last real diagnostic, got %q", st.ATs[0].Diagnostic)
	}
	// The seat halted at the cap rather than running to its full intervention budget.
	if len(res.Interventions) >= 30 {
		t.Fatalf("seat should halt at the cap, not exhaust maxInterventions; got %d", len(res.Interventions))
	}
}

// TestT2RehearsalRespawnConvergenceIntegration is the chain-392 end-to-end verification
// (task 3316), driven through the live bridge entry RunDuty with both fixes composed. It
// is the deterministic harness-level equivalent of the REHEARSAL_TARGETS T2 run (fs.read
// non-converging) — no live model/keys, so it is gate-safe where the live re-run is flaky.
// It asserts the respawn tax + thrash are gone: escalation CARRIES across respawns (each
// respawn begins at the tier the prior one reached, never reset to the local floor) AND the
// per-duty cap stops the duty at a bounded number of entries with an honest stuck verdict —
// instead of re-spawning ~12 workers toward the tree budget / $1.5468 cost ceiling.
func TestT2RehearsalRespawnConvergenceIntegration(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}), WithPerDutyRespawnCap(3))
	// A T2-shaped worker that never turns the gate green and reports the tier it climbs to
	// each respawn (local→mid→coder), so the carry is observable.
	w := &tierEchoWorker{report: []string{"mid", "coder", "strong"}}
	o.model = w
	at := AtomicTask{Slug: "fs-read-nonstring", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 1}
	chain := Chain{Slug: "t2", TargetRepo: "x", Tasks: []AtomicTask{at}}

	res, err := RunDuty(context.Background(), o, seat(o), chain, "t2run")
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}
	// Fix 3314: respawn N+1 began at the tier respawn N reached — the escalated tier is no
	// longer discarded and re-climbed from the local floor every attempt.
	if got, want := w.carriedIn, []string{"", "mid", "coder"}; !slices.Equal(got, want) {
		t.Fatalf("escalation must carry across respawns: carried-in = %v, want %v", got, want)
	}
	// Fix 3315: the duty stopped at the cap (3 worker→gate entries), not a 12-spawn thrash.
	if w.n != 3 {
		t.Fatalf("per-duty cap should bound the duty to 3 respawns, got %d worker spawns", w.n)
	}
	// And it stopped HONESTLY: the bridge answer surfaces the stuck verdict, not a fake pass.
	if !strings.Contains(res.Answer, "respawn cap") {
		t.Fatalf("bridge answer should surface the honest stuck verdict, got %q", res.Answer)
	}
}
