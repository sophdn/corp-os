package agent

import (
	"context"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// errProvider fails every dispatch with a tool error, so a turn that makes a
// tool call reports ToolErrors=1 — the escalation trigger.
type errProvider struct{}

func (errProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: false, Value: map[string]any{"error": "boom"}, ErrorClass: tool.ClassTool}
}

// usageProvider fails every dispatch with a WORKER-RECOVERABLE usage error (the
// class the fs organ now emits): a failed call the worker self-corrects, which
// must NOT count toward the repeated_tool_error escalation trigger.
type usageProvider struct{}

func (usageProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: false, Value: map[string]any{"error": "fs.edit requires file_path"}, ErrorClass: tool.ClassUsage}
}

// TestLoop_UsageErrorDoesNotEscalate is the fix for bug
// escalate-after-1-sends-recoverable-param-error-turns-to-opus-…: a turn whose
// only failures are worker-recoverable usage errors must NOT flip the router to
// the strong tier, even with escalate-after-1 — climbing the model ladder cannot
// fix a deterministic fs usage slip, and the floor self-corrects. (Contrast
// TestLoop_TwoTierEscalatesAfterToolErrorTurn, where a genuine tool error does
// escalate.)
func TestLoop_UsageErrorDoesNotEscalate(t *testing.T) {
	cheap := model.NewEcho("cheap",
		model.Response{ToolCalls: []tool.Call{{ID: "c", Surface: "fs", Action: "edit"}}, StopReason: model.StopToolUse},
		model.Response{Text: "cheap-answer", StopReason: model.StopEndTurn},
	)
	strong := model.NewEcho("strong", model.Response{Text: "strong-answer", StopReason: model.StopEndTurn})
	rt := router.New(cheap, strong, router.WithEscalation(1, 2))
	loop := New(rt, usageProvider{}, nil)

	r1, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if r1.Text != "cheap-answer" {
		t.Errorf("turn 1 should finish on the cheap tier: %q", r1.Text)
	}
	if rt.State() == router.StateEscalated {
		t.Fatal("a usage-error-only turn must NOT escalate — the floor self-corrects a recoverable slip")
	}
}

// TestLoop_TwoTierEscalatesAfterToolErrorTurn pins the cost-routing path end to
// end through the loop: a tool-error turn on the cheap tier flips the router to
// the strong tier for the next turn (free/cheap bulk, escalate only on trouble).
func TestLoop_TwoTierEscalatesAfterToolErrorTurn(t *testing.T) {
	cheap := model.NewEcho("cheap",
		model.Response{ToolCalls: []tool.Call{{ID: "c", Surface: "work", Action: "x"}}, StopReason: model.StopToolUse},
		model.Response{Text: "cheap-answer", StopReason: model.StopEndTurn},
	)
	strong := model.NewEcho("strong", model.Response{Text: "strong-answer", StopReason: model.StopEndTurn})
	rt := router.New(cheap, strong, router.WithEscalation(1, 2))
	loop := New(rt, errProvider{}, nil)

	// Turn 1 runs on the cheap tier; its tool call errors (ToolErrors=1).
	r1, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if r1.Text != "cheap-answer" {
		t.Errorf("turn 1 should finish on the cheap tier: %q", r1.Text)
	}
	if rt.State() != router.StateEscalated {
		t.Fatalf("router should have escalated after the tool-error turn, state=%s", rt.State())
	}

	// Turn 2 is now driven by the strong tier.
	r2, err := loop.Run(context.Background(), "again")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if r2.Text != "strong-answer" {
		t.Errorf("turn 2 should be driven by the strong tier, got %q", r2.Text)
	}
}
