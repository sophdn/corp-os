package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// alwaysEditAdapter emits an fs.edit tool call every round and NEVER claims done — it
// models the run-6 worker that keeps editing past a passing fix, so the loop only ever
// exits via the opportunistic/exhaustion gate checks, not a done-claim.
type alwaysEditAdapter struct{}

func (alwaysEditAdapter) Model() string   { return "edit" }
func (alwaysEditAdapter) Available() bool { return true }
func (alwaysEditAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{
		Model:      "edit",
		ToolCalls:  []tool.Call{{ID: "e", Surface: "fs", Action: "edit", Params: map[string]any{"path": "x.go"}}},
		StopReason: model.StopToolUse,
	}, nil
}

func greenGate() *VerifyGate {
	return &VerifyGate{Command: []string{"go", "test"},
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 0, "ok" }}
}

func redGate() *VerifyGate {
	return &VerifyGate{Command: []string{"go", "test"},
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 1, "FAIL" }}
}

// Mid-flight stop-when-green: a worker that keeps editing past a green tree is captured
// as success the moment the throttle fires and the gate passes — it does NOT run to the
// round budget. The provider call count proves it stopped early.
func TestOpportunistic_MidFlightStopsEarly(t *testing.T) {
	fp := &fakeProvider{}
	loop := New(single(alwaysEditAdapter{}), fp, nil,
		WithMaxRounds(20), WithVerify(greenGate()), WithOpportunisticVerify(1))
	res, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("a mid-flight green must finish successfully, got err %v", err)
	}
	if res.Fabricated != "" || res.VerifyFailed {
		t.Fatalf("clean opportunistic green should be a plain success; got %+v", res)
	}
	// Stopped at the first throttled check (round 1), not the 20-round budget.
	if fp.calls > 3 {
		t.Fatalf("expected an early stop (a few dispatches), ran %d — did not stop mid-flight", fp.calls)
	}
}

// The exhaustion-time check is ALWAYS on when a verify gate is set (no mid-flight option):
// a worker that runs out the round budget with a green tree is captured as success, not
// discarded as "exceeded max tool rounds".
func TestOpportunistic_ExhaustionBackstopCapturesGreen(t *testing.T) {
	fp := &fakeProvider{}
	loop := New(single(alwaysEditAdapter{}), fp, nil,
		WithMaxRounds(4), WithVerify(greenGate())) // no WithOpportunisticVerify
	res, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("exhaustion with a green gate must be success, got err %v", err)
	}
	if res.VerifyFailed {
		t.Fatalf("backstop green should be a plain success; got %+v", res)
	}
	if fp.calls != 4 {
		t.Fatalf("backstop should run the full budget then succeed; ran %d, want 4", fp.calls)
	}
}

// A RED gate at exhaustion is still an honest failure — the backstop never invents a
// success the gate didn't certify.
func TestOpportunistic_RedGateStillExhausts(t *testing.T) {
	loop := New(single(alwaysEditAdapter{}), &fakeProvider{}, nil,
		WithMaxRounds(3), WithVerify(redGate()))
	if _, err := loop.Run(context.Background(), "fix it"); err == nil {
		t.Fatal("a red gate at exhaustion must still report exceeded-max-rounds, not a false success")
	}
}

// Mid-flight RED reconcile (bug 1103): when the throttled gate runs RED, the failing
// output is fed back as a user turn so the worker reconciles a guessed/output-dependent
// assertion against the REAL test output BEFORE claiming done — and an UNCHANGED failure
// is fed back only ONCE (deduped), not every cadence tick.
func TestOpportunistic_MidFlightRedFeedsBackOnceForReconcile(t *testing.T) {
	loop := New(single(alwaysEditAdapter{}), &fakeProvider{}, nil,
		WithMaxRounds(6), WithVerify(redGate()), WithOpportunisticVerify(1))
	if _, err := loop.Run(context.Background(), "author the test"); err == nil {
		t.Fatal("an always-RED gate must still exhaust, not report a false success")
	}
	var reconciles []string
	for _, m := range loop.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "the verification gate currently FAILS") {
			reconciles = append(reconciles, m.Content)
		}
	}
	if len(reconciles) != 1 {
		t.Fatalf("mid-flight RED should feed the gate output back exactly once (deduped), got %d", len(reconciles))
	}
	if !strings.Contains(reconciles[0], "FAIL") {
		t.Fatalf("reconcile feedback must carry the REAL gate output; got:\n%s", reconciles[0])
	}
}

// The 1103 happy path: a gate that is RED on the first mid-flight check then GREEN (the
// worker reconciled its assertion) feeds the real output back and then stops the run as a
// plain success — the in-loop test-execution signal closes without a principal review-reject.
func TestOpportunistic_MidFlightReconcileThenGreenSucceeds(t *testing.T) {
	var n int
	flip := &VerifyGate{Command: []string{"go", "test"},
		run: func(context.Context, []string, string, time.Duration) (int, string) {
			n++
			if n == 1 {
				return 1, "FAIL: want {action:'',params:null,rationale:''} got {action:''}"
			}
			return 0, "ok"
		}}
	loop := New(single(alwaysEditAdapter{}), &fakeProvider{}, nil,
		WithMaxRounds(20), WithVerify(flip), WithOpportunisticVerify(1))
	res, err := loop.Run(context.Background(), "author the test")
	if err != nil {
		t.Fatalf("a worker that reconciles to green mid-flight must succeed, got err %v", err)
	}
	if res.VerifyFailed {
		t.Fatalf("reconciled green should be a plain success; got %+v", res)
	}
	found := false
	for _, m := range loop.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "the verification gate currently FAILS") {
			found = true
		}
	}
	if !found {
		t.Fatal("the mid-flight RED must be fed back as a reconcile turn before the green stop")
	}
}

// Mid-flight reconcile feedback carries the SAME structural grounding as the post-done
// revise loop: a RED gate naming a guessed API resolves the real signatures into the
// reconcile turn, so the worker stops guessing internal APIs mid-flight too.
func TestOpportunistic_MidFlightReconcileCarriesGrounding(t *testing.T) {
	gate := &VerifyGate{Command: []string{"go", "test"},
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 1, adminBuildErrs }}
	loop := New(single(alwaysEditAdapter{}), &fakeProvider{}, nil,
		WithMaxRounds(4), WithVerify(gate), WithOpportunisticVerify(1))
	loop.goDocResolve = (&fakeResolver{}).resolve
	_, _ = loop.Run(context.Background(), "author the test")
	var reconcile string
	for _, m := range loop.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "the verification gate currently FAILS") {
			reconcile = m.Content
		}
	}
	if reconcile == "" {
		t.Fatal("no mid-flight reconcile feedback found")
	}
	if !strings.Contains(reconcile, "Grounded signatures") {
		t.Fatalf("mid-flight reconcile should carry grounded signatures resolved from the build error; got:\n%s", reconcile)
	}
}

// refuseFakeGreen always refuses at the fake-green stage (a stand-in for the
// scaffold/test-tamper guards).
type refuseFakeGreen struct{}

func (refuseFakeGreen) Name() string                                    { return "test-refuse" }
func (refuseFakeGreen) Stage() GuardStage                               { return StageFakeGreen }
func (refuseFakeGreen) Describe() string                                { return "always refuses" }
func (refuseFakeGreen) Assess(context.Context, GuardInput) GuardVerdict { return fail("suspect green") }

// A gate that passes but a fake-green guard refuses is NOT a clean done: the opportunistic
// path declines it (keeps the honest exhaustion failure), holding an opportunistic green
// to the same anti-fake-green bar as a done-claim green.
func TestOpportunistic_FakeGreenRefusalDeclinesSuccess(t *testing.T) {
	loop := New(single(alwaysEditAdapter{}), &fakeProvider{}, nil,
		WithMaxRounds(3), WithVerify(greenGate()))
	loop.guards.register(refuseFakeGreen{})
	if _, err := loop.Run(context.Background(), "fix it"); err == nil {
		t.Fatal("a fake-green-refused green must not be captured as opportunistic success")
	}
}
