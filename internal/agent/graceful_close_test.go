package agent

import (
	"context"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/router"
)

// When the per-turn deadline is spent but the worker's fix already LANDED, the
// graceful close runs the verify gate on the current tree and reports SUCCESS — a
// timed-out-but-green run is never reported as a failure (bug 1102). The gate runs
// under a fresh context even though the turn ctx is spent.
func TestGracefulTimeoutGreenGateReportsLandedFix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the turn deadline is spent before the call returns
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: timeoutErr}}}
	green := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 2,
		run: func(rc context.Context, _ []string, _ string, _ time.Duration) (int, string) {
			if rc.Err() != nil {
				t.Errorf("gate ran under a spent context: %v", rc.Err())
			}
			return 0, "ok"
		}}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), &fakeProvider{}, nil, WithVerify(green))

	res, err := loop.Run(ctx, "slow task")
	if err != nil {
		t.Fatalf("a graceful timeout with a GREEN gate must not error (the fix landed): %v", err)
	}
	if res.Escalate != "" || res.Stopped != "" {
		t.Errorf("a timed-out-but-green tree must report success, got Escalate=%q Stopped=%q", res.Escalate, res.Stopped)
	}
}

// When the per-turn deadline is spent and the gate is RED, the graceful close returns
// an honest unverified/escalate verdict (surfacing the fault class) instead of exiting
// 0 with an empty, unverified answer (bug 1102's core failure).
func TestGracefulTimeoutRedGateReportsUnverifiedEscalate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: timeoutErr}}}
	red := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 2,
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 1, "FAIL boom" }}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), &fakeProvider{}, nil, WithVerify(red))

	res, err := loop.Run(ctx, "slow task")
	if err != nil {
		t.Fatalf("a graceful timeout with a red gate must report a verdict, not a bare error: %v", err)
	}
	if res.Escalate == "" {
		t.Error("a timed-out run with a RED gate must return unverified/escalate, not an empty answer")
	}
	if res.ModelFault != string(model.FaultTimeout) {
		t.Errorf("the timeout fault class should be surfaced, got %q", res.ModelFault)
	}
}

// With NO verify gate the graceful close is a no-op: the turn ends with the fault
// class set and no verdict (prior behavior preserved — the existing
// TestRecoverTimeoutGracefulWhenDeadlineSpent contract).
func TestGracefulTimeoutNoGatePreservesPriorBehavior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: timeoutErr}}}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), &fakeProvider{}, nil)

	res, err := loop.Run(ctx, "slow task")
	if err != nil {
		t.Fatalf("a spent deadline with no gate should end gracefully, not error: %v", err)
	}
	if res.Escalate != "" || res.Stopped != "" {
		t.Errorf("no gate → no verdict, got Escalate=%q Stopped=%q", res.Escalate, res.Stopped)
	}
	if res.ModelFault != string(model.FaultTimeout) {
		t.Errorf("ModelFault = %q, want %q", res.ModelFault, model.FaultTimeout)
	}
}
