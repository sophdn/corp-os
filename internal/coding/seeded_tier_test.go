package coding

import (
	"context"
	"testing"
)

// Bug 1146 (orchestrate-layer carry): buildCodingPath rebuilds a FRESH coding.Orchestrator
// on every agent-orchestrate re-invocation of the coding path, so the tier a prior coding
// worker escalated to is discarded and the next invocation re-climbs from the local floor.
// WithSeededTier lets the live wiring carry the highest tier any prior coding-path
// invocation in the SAME run reached into the next invocation's atom, so a re-spawned coding
// worker begins there instead of at the Qwen floor. Seeding a tier is monotonic and never
// refuses work (unlike a cross-invocation cap, which lacks a stable per-duty key because the
// orchestrator re-words the duty each respawn), so it is the safe half of the carry.

// TestSeededTierCarriesIntoFirstAttempt: an organ seeded with a tier hands that tier to the
// atom's FIRST worker attempt (the carry the fresh-per-invocation organ otherwise loses).
func TestSeededTierCarriesIntoFirstAttempt(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}), WithSeededTier("coder"))
	w := &tierEchoWorker{report: []string{"coder"}}
	o.model = w
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 1}
	st, err := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	o.RunToCompletion(context.Background(), st)
	if len(w.carriedIn) < 1 || w.carriedIn[0] != "coder" {
		t.Fatalf("seeded tier must be carried into the first attempt; carriedIn=%v, want [\"coder\", …]", w.carriedIn)
	}
	if st.ATs[0].HighestTierModel != "coder" {
		t.Fatalf("seeded atom should start at the seeded tier, got %q", st.ATs[0].HighestTierModel)
	}
}

// TestUnseededFirstAttemptCarriesNothing pins the contrast: with no seed the first attempt
// carries "" (the fresh-organ default) — so seeding is the only thing that bridges the tier
// across a fresh-per-invocation organ.
func TestUnseededFirstAttemptCarriesNothing(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}))
	w := &tierEchoWorker{report: []string{"mid"}}
	o.model = w
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 1}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	o.RunToCompletion(context.Background(), st)
	if w.carriedIn[0] != "" {
		t.Fatalf("unseeded first attempt should carry nothing, got %q", w.carriedIn[0])
	}
}

// TestRunDutyExposesReachedTier: RunDuty surfaces the highest tier the duty's atom reached
// on BridgeResult, so the live wiring can carry it into the next coding-path invocation.
func TestRunDutyExposesReachedTier(t *testing.T) {
	o := New(WithRunner(&countRunner{passAt: 999}), WithRepo(NoopRepo{Dir: t.TempDir()}), WithPerDutyRespawnCap(1))
	w := &tierEchoWorker{report: []string{"coder"}}
	o.model = w
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 1}
	chain := Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}
	res, err := RunDuty(context.Background(), o, seat(o), chain, "r")
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}
	if res.HighestTierModel != "coder" {
		t.Fatalf("BridgeResult should expose the reached tier, got %q", res.HighestTierModel)
	}
}
