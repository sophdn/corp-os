package agent

import (
	"context"
	"testing"

	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/tool"
)

// WithExtraHook composes onto the loop: a pre_tool_use hook it adds can veto a dispatch,
// and the veto is fed back to the model as a tool_error (the call never reaches the provider).
func TestWithExtraHook_DeniesDispatch(t *testing.T) {
	m := model.NewEcho("qwen",
		model.Response{
			ToolCalls:  []tool.Call{{ID: "c1", Surface: "fs", Action: "write", Params: map[string]any{"path": "gate/oracle_test.go"}}},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "done", StopReason: model.StopEndTurn},
	)
	fp := &fakeProvider{}
	deny := func(c *hooks.Context) {
		if c.ToolCall != nil && c.ToolCall.Action == "write" {
			c.DenyToolCall = true
			c.DenyReason = "protected"
		}
	}
	res, err := New(single(m), fp, nil, WithExtraHook(hooks.PreToolUse, "deny-write", deny)).
		Run(context.Background(), "write the gate")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fp.calls != 0 {
		t.Fatalf("denied call must not reach the provider; got %d dispatches", fp.calls)
	}
	if len(res.Dispatches) != 1 || res.Dispatches[0].OK {
		t.Fatalf("expected one blocked dispatch, got %+v", res.Dispatches)
	}
}

// A nil fn is ignored (no registration, no panic) and the loop runs normally.
func TestWithExtraHook_NilFnIgnored(t *testing.T) {
	m := model.NewEcho("qwen", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil, WithExtraHook(hooks.PreToolUse, "noop", nil)).
		Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run with a nil extra hook: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("Text = %q", res.Text)
	}
}
