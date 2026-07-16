package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/router"
	"corpos/internal/tool"
)

// Bug 1148: a spawn-only orchestrator owns no verify gate of its own, yet its workers
// land a passing fix in the shared repo. When the orchestrator then thrashes onto the
// bounded frontier and the strong-bound halt fires, the run must report the achieved
// GREEN (via the terminal green backstop) instead of the false-negative "stuck / no
// final answer". These tests pin that a WithTerminalGreen gate converts a green-at-halt
// into success, and — critically — that a RED gate still halts (the backstop never masks
// a genuine non-convergence).

// TestTerminalGreen_StrongBoundHaltReportsGreen: same runaway signature as
// TestStrongBoundHalts, but with a terminal green backstop configured (and NO full verify
// gate, mirroring the orchestrator). The strong-bound point must capture the green and
// finish as success rather than emitting the strong-bound verdict.
func TestTerminalGreen_StrongBoundHaltReportsGreen(t *testing.T) {
	const bound = 1
	floor := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	strong := &varyingRunaway{}
	rt := router.New(floor, strong, router.WithBoundedTop(bound))
	loop := New(rt, &fakeProvider{}, []tool.Spec{{Name: "fs"}},
		WithMaxRounds(6), WithTerminalGreen(greenGate()))

	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("a captured terminal green is a clean success, not an error: %v", err)
	}
	if strings.Contains(res.Stopped, "strong-bound reached") {
		t.Fatalf("repo green at halt must NOT report a strong-bound stall; Stopped = %q", res.Stopped)
	}
	if res.VerifyFailed || res.Fabricated != "" {
		t.Fatalf("a clean terminal green should be a plain success; got %+v", res)
	}
}

// TestTerminalGreen_RedGateStillHalts: the backstop must never mask a genuine non-green
// halt. With a RED terminal gate the run halts exactly as it would with no backstop.
func TestTerminalGreen_RedGateStillHalts(t *testing.T) {
	const bound = 1
	floor := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	strong := &varyingRunaway{}
	rt := router.New(floor, strong, router.WithBoundedTop(bound))
	loop := New(rt, &fakeProvider{}, []tool.Spec{{Name: "fs"}},
		WithMaxRounds(6), WithTerminalGreen(redGate()))

	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("the strong-bound halt is a clean honest stop, not an error: %v", err)
	}
	if !strings.Contains(res.Stopped, "strong-bound reached") {
		t.Fatalf("a RED repo at halt must still report the strong-bound stall; Stopped = %q", res.Stopped)
	}
}

// TestTerminalGreen_FullVerifyGateWins: when a full verify gate is present it supersedes
// the backstop (they never both apply). A green full gate captures success as before; the
// terminal gate is simply not consulted.
func TestTerminalGreen_FullVerifyGateWins(t *testing.T) {
	const bound = 1
	floor := alwaysCalls{call: tool.Call{ID: "c", Surface: "fs", Action: "grep"}}
	strong := &varyingRunaway{}
	rt := router.New(floor, strong, router.WithBoundedTop(bound))
	// Full gate green, terminal gate RED — the full gate must win, so the run succeeds.
	loop := New(rt, &fakeProvider{}, []tool.Spec{{Name: "fs"}},
		WithMaxRounds(6), WithVerify(greenGate()), WithTerminalGreen(redGate()))

	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("a green full gate captures success at halt: %v", err)
	}
	if strings.Contains(res.Stopped, "strong-bound reached") {
		t.Fatalf("a green full verify gate must capture success, not halt; Stopped = %q", res.Stopped)
	}
}
