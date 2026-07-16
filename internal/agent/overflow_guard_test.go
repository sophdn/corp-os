package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/router"
)

func TestFloorFits(t *testing.T) {
	if !floorFits(799, 1000) {
		t.Error("799 (< 80% of 1000) should fit")
	}
	if floorFits(800, 1000) {
		t.Error("800 (== 80% threshold) should NOT fit")
	}
	if floorFits(900, 1000) {
		t.Error("900 (> threshold) should not fit")
	}
	if !floorFits(50000, 0) {
		t.Error("unknown window (0) disables the guard → always fits")
	}
}

// TestProactiveFloorGuardRoutesUpBeforeOverflow: a would-overflow prompt resting on
// the floor is escalated to the larger-window rung BEFORE the call — the floor
// adapter is never invoked (no wasted 400, no bounce-to-death).
func TestProactiveFloorGuardRoutesUpBeforeOverflow(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{resp: answer("qwen")}}}
	strong := &scriptAdapter{id: "opus", steps: []scriptStep{{resp: answer("opus")}}}
	sum := &recSummarizer{out: "[c]"}
	bigPrompt := strings.Repeat("token ", 1200) // thousands of tokens, well over 80% of 1000
	loop := New(router.New(floor, strong), &fakeProvider{}, nil,
		WithCompaction(900, 2, sum),
		WithFloorWindow(1000),
	)
	if _, err := loop.Run(context.Background(), bigPrompt); err != nil {
		t.Fatalf("run: %v", err)
	}
	if floor.calls != 0 {
		t.Errorf("floor adapter called %d time(s); the guard must route a would-overflow prompt UP before the floor sees it", floor.calls)
	}
	if strong.calls == 0 {
		t.Error("the larger-window strong rung was never reached")
	}
}

// A prompt that fits the floor window is served by the floor (the guard does not
// needlessly escalate).
func TestProactiveFloorGuardLetsAFittingPromptStay(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{resp: answer("qwen")}}}
	strong := &scriptAdapter{id: "opus", steps: []scriptStep{{resp: answer("opus")}}}
	sum := &recSummarizer{out: "[c]"}
	loop := New(router.New(floor, strong), &fakeProvider{}, nil,
		WithCompaction(900, 2, sum),
		WithFloorWindow(100000), // huge window → everything fits
	)
	if _, err := loop.Run(context.Background(), "small ask"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if floor.calls == 0 {
		t.Error("a fitting prompt should be served by the floor, not escalated")
	}
}

// On a terminal fault, a GREEN verify gate means the fix landed → report success,
// not a bare error.
func TestCloseOnTerminalFaultVerifyGreenReportsSuccess(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: overflowErr}}}
	green := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 2,
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 0, "ok" }}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), &fakeProvider{}, nil, WithVerify(green))
	res, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("terminal overflow with a GREEN gate must not error (the fix landed): %v", err)
	}
	if res.Stopped != "" {
		t.Errorf("green gate → success expected, got Stopped=%q", res.Stopped)
	}
}

// On a terminal fault with a RED gate, report an honest, gate-evidenced verdict
// (not a bare error) that surfaces the fault class.
func TestCloseOnTerminalFaultVerifyRedReportsHonestVerdict(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: overflowErr}}}
	red := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 2,
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 1, "FAIL boom" }}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), &fakeProvider{}, nil, WithVerify(red))
	res, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("terminal overflow + red gate must report a verdict, not a bare error: %v", err)
	}
	if res.Stopped == "" {
		t.Error("red gate → an honest Stopped verdict expected")
	}
	if res.ModelFault == "" {
		t.Error("the fault class should be surfaced on a terminal fault")
	}
}

// With no verify gate, a terminal fault still surfaces the raw error (prior behavior).
func TestCloseOnTerminalFaultNoVerifyKeepsRawError(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: overflowErr}}}
	loop := New(router.NewLadder([]model.Adapter{floor}, 0), &fakeProvider{}, nil)
	if _, err := loop.Run(context.Background(), "do it"); err == nil {
		t.Error("with no verify gate, a terminal overflow should still surface the raw error")
	}
}
