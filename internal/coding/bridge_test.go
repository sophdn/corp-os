package coding

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/profile"
)

// codingProfile is the resolved atomic-coding-chain profile shape the bridge reads
// the gate / protected paths / revise budget off (already configured there).
func codingProfile() *profile.JobProfile {
	return &profile.JobProfile{
		Name:            "atomic-coding-chain",
		Tier:            profile.TierLocal,
		VerifyCommand:   []string{"sh", "-c", "go build ./... && go test ./..."},
		VerifyMaxRounds: 4,
		ProtectPaths:    []string{"**/*_test.go"},
	}
}

func TestBridgeChainBuildsSingleATFromProfile(t *testing.T) {
	p := codingProfile()
	chain := BridgeChain("run-xyz", "make the parser handle empty input", "/tmp/repo", p)

	// BaseBranch is deliberately empty: the git Repo seam resolves it to the target
	// repo's actual default/current branch at Init time (bug 1077).
	if chain.Slug != "run-xyz" || chain.TargetRepo != "/tmp/repo" || chain.BaseBranch != "" {
		t.Fatalf("chain envelope = %+v", chain)
	}
	if err := chain.Validate(); err != nil {
		t.Fatalf("bridged chain must validate: %v", err)
	}
	if len(chain.Tasks) != 1 {
		t.Fatalf("want a single-AT chain, got %d tasks", len(chain.Tasks))
	}
	at := chain.Tasks[0]
	if at.Goal != "make the parser handle empty input" {
		t.Errorf("AT goal = %q", at.Goal)
	}
	if at.Worker.Kind != WorkerModel {
		t.Errorf("AT worker kind = %q, want model", at.Worker.Kind)
	}
	if at.MaxIterations != 4 {
		t.Errorf("AT max iterations = %d, want the profile's VerifyMaxRounds (4)", at.MaxIterations)
	}
	// Gate is [][]string{p.VerifyCommand} — the profile's single verify command wrapped.
	if len(at.Gate) != 1 || strings.Join(at.Gate[0], " ") != "sh -c go build ./... && go test ./..." {
		t.Errorf("AT gate = %+v, want [][]string{p.VerifyCommand}", at.Gate)
	}
	if len(at.Protected) != 1 || at.Protected[0] != "**/*_test.go" {
		t.Errorf("AT protected = %+v, want the profile's ProtectPaths", at.Protected)
	}
}

func TestBridgeChainWithoutGateOrProtected(t *testing.T) {
	// A profile with no verify command / protect paths yields an AT with no gate /
	// no protected set (not a nil-element-bearing slice).
	chain := BridgeChain("r", "duty", "/repo", &profile.JobProfile{Name: "atomic-coding-chain", Tier: profile.TierLocal})
	at := chain.Tasks[0]
	if at.Gate != nil {
		t.Errorf("no verify command should leave the gate nil, got %+v", at.Gate)
	}
	if at.Protected != nil {
		t.Errorf("no protect paths should leave protected nil, got %+v", at.Protected)
	}
}

// bridgeHarness builds an orchestrator over a NoopRepo + a fake model worker and a
// gate runner that passes on passAt, mirroring seatHarness — sans-IO.
func bridgeHarness(t *testing.T, passAt int) *Orchestrator {
	t.Helper()
	o := New(WithRunner(&countRunner{passAt: passAt}), WithRepo(NoopRepo{Dir: t.TempDir()}))
	o.model = &fakeWorker{}
	return o
}

func bridgeSeat(o *Orchestrator) *OperatorSeat {
	return NewOperatorSeat(o, seatOperator{usage: model.Usage{InputTokens: 200, OutputTokens: 80}},
		decideAdapter{id: "google/gemini-3.1-flash-lite"}, decideAdapter{id: "claude-opus-4-8"}, WithK(2))
}

func TestRunDutyGreenFirstTry(t *testing.T) {
	o := bridgeHarness(t, 1) // gate passes on the first call → no operator needed
	chain := BridgeChain("r1", "do the coding duty", "x", codingProfile())
	res, err := RunDuty(context.Background(), o, bridgeSeat(o), chain, "r1")
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}
	if !strings.Contains(res.Answer, "succeeded") {
		t.Errorf("green answer = %q, want a success line", res.Answer)
	}
	if !res.Success {
		t.Error("a green chain must report Success=true (feeds the cross-invocation respawn-cap reset)")
	}
	// No operator intervention fired, so the seat accrued no operator-decision cost.
	if res.CostUSD != 0 {
		t.Errorf("green-first-try cost = %v, want 0 (no operator intervention)", res.CostUSD)
	}
}

func TestRunDutyEscalatesThenSucceeds(t *testing.T) {
	// The AT's max-iterations is the profile's VerifyMaxRounds (4), so the initial
	// RunToCompletion runs the gate 4 times (all miss), failing the chain; the seat's
	// first resume call (#5) then passes → a mid edit carries it. Exercises the
	// failure→seat→success path + cost mapping.
	o := bridgeHarness(t, 5)
	chain := BridgeChain("r2", "duty", "x", codingProfile())
	res, err := RunDuty(context.Background(), o, bridgeSeat(o), chain, "r2")
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}
	if !strings.Contains(res.Answer, "succeeded") {
		t.Errorf("answer = %q, want success after the seat carried it", res.Answer)
	}
	if res.CostUSD <= 0 {
		t.Errorf("a seat intervention should accrue operator cost, got %v", res.CostUSD)
	}
}

func TestRunDutyHonestFailure(t *testing.T) {
	// Gate never passes → the seat reaches an impasse; the duty comes back as an
	// honest failure answer (status + diagnostic), NOT a Go error.
	o := bridgeHarness(t, 99)
	chain := BridgeChain("r3", "duty", "x", codingProfile())
	res, err := RunDuty(context.Background(), o, bridgeSeat(o), chain, "r3")
	if err != nil {
		t.Fatalf("an honest organ failure must not be a Go error: %v", err)
	}
	if !strings.Contains(res.Answer, "failed") {
		t.Errorf("failure answer = %q, want a failed-status line", res.Answer)
	}
	if !strings.Contains(res.Answer, "gate miss") {
		t.Errorf("failure answer should carry the gate diagnostic, got %q", res.Answer)
	}
	if res.Success {
		t.Error("an honest failure must report Success=false (extends the cross-invocation respawn-cap streak)")
	}
}

func TestRunDutyNilSeatDrivesOrchestratorAlone(t *testing.T) {
	// A nil seat (no escalation rungs) drives RunToCompletion alone: a green chain
	// still succeeds; a red one returns the orchestrator's own failure verdict.
	o := bridgeHarness(t, 1)
	res, err := RunDuty(context.Background(), o, nil, BridgeChain("r4", "duty", "x", codingProfile()), "r4")
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}
	if !strings.Contains(res.Answer, "succeeded") {
		t.Errorf("nil-seat green = %q", res.Answer)
	}
}

func TestRunDutySuccessReportsIntegrationCommit(t *testing.T) {
	// A repo that commits a real sha surfaces it in the success answer (the landed
	// integration commit), distinguishing the SHA-bearing success branch.
	ws := &fakeWorkspace{dir: t.TempDir(), sha: "abc1234", ok: true}
	o := New(WithRunner(&countRunner{passAt: 1}), WithRepo(&fakeRepo{head: "base", ws: ws}))
	o.model = &fakeWorker{}
	res, err := RunDuty(context.Background(), o, nil, BridgeChain("r5", "duty", "x", codingProfile()), "r5")
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}
	if !strings.Contains(res.Answer, "abc1234") {
		t.Errorf("success answer should carry the integration commit, got %q", res.Answer)
	}
}

func TestRunDutyStartError(t *testing.T) {
	// An invalid chain (no tasks) fails at Start — a genuine infra fault, surfaced
	// as a Go error (distinct from an honest red run).
	o := bridgeHarness(t, 1)
	_, err := RunDuty(context.Background(), o, nil, Chain{Slug: "bad"}, "r6")
	if err == nil {
		t.Fatal("an invalid chain must error at Start")
	}
}

func TestSynthesizeAnswerEdgeCases(t *testing.T) {
	// Success with no commit (NoopRepo) → a success line leading with the verb, no SHA.
	got := synthesizeAnswer(&RunState{Status: ChainSuccess})
	if !strings.HasPrefix(got, "coding chain succeeded") {
		t.Errorf("no-commit success = %q, want it to lead with the success verb", got)
	}
	// Failure with no diagnostic → the placeholder, so the answer is never empty.
	st := &RunState{Status: ChainFailed, FailedATSlug: "a", ATs: []ATRecord{{Slug: "a", Status: ATFailed}}}
	if got := synthesizeAnswer(st); !strings.Contains(got, "(no diagnostic)") {
		t.Errorf("no-diagnostic failure = %q, want the placeholder", got)
	}
}

// A clean success (promoted, no diagnostic) must signal the duty is DONE and that no
// re-verification spawn is needed — the structural counterpart to the orchestrate
// prompt's DECLARE DONE directive (bug run-23: the orchestrator re-spawned a verify
// coding-chain after a confirmed success and strong-bound-halted).
func TestSynthesizeAnswerCleanSuccessSignalsDone(t *testing.T) {
	got := synthesizeAnswer(&RunState{Status: ChainSuccess, ATs: []ATRecord{{Status: ATSuccess, CommitSHA: "abc1234"}}})
	if !strings.Contains(got, "abc1234") {
		t.Errorf("clean success should still carry the integration commit, got %q", got)
	}
	if !strings.Contains(got, "DONE") || !strings.Contains(got, "re-verify") {
		t.Errorf("clean success should signal the duty is DONE and needs no re-verify spawn, got %q", got)
	}
	// A promote failure must NOT carry the done/finish signal — the work is not on the
	// working tree, so the caller still has something to reconcile.
	warned := synthesizeAnswer(&RunState{Status: ChainSuccess, PromoteDiagnostic: "dirty tree", ATs: []ATRecord{{Status: ATSuccess, CommitSHA: "abc1234"}}})
	if !strings.Contains(warned, "WARNING") || strings.Contains(warned, "re-verify") {
		t.Errorf("promote-failure success should warn, not signal done, got %q", warned)
	}
}

func TestNewRunIDUnique(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if a == "" || b == "" || a == b {
		t.Fatalf("NewRunID should mint a non-empty, fresh id each call (got %q, %q)", a, b)
	}
}
