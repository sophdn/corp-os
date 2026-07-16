package agent

import (
	"context"
	"errors"
	"testing"

	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/router"
	"corpos/internal/tool"
)

type fakeProvider struct{ calls int }

func (f *fakeProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	f.calls++
	return tool.Result{Call: c, OK: true, Value: map[string]any{"ok": true}}
}

type errAdapter struct{}

func (errAdapter) Model() string   { return "err" }
func (errAdapter) Available() bool { return true }
func (errAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{}, errors.New("boom")
}

// chanProvider returns a value that cannot be JSON-marshaled (encodeValue fallback).
type chanProvider struct{}

func (chanProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: true, Value: make(chan int)}
}

// recAdapter records the transcript it was asked to complete.
type recAdapter struct{ got []model.ChatMessage }

func (r *recAdapter) Model() string   { return "rec" }
func (r *recAdapter) Available() bool { return true }
func (r *recAdapter) Complete(_ context.Context, msgs []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	r.got = append([]model.ChatMessage(nil), msgs...)
	return model.Response{Model: "rec", Text: "ok", StopReason: model.StopEndTurn}, nil
}

// single wraps one adapter in a single-tier router.
func single(m model.Adapter) *router.Router { return router.New(m, m) }

func TestRunSingleToolThenAnswer(t *testing.T) {
	m := model.NewEcho("qwen",
		model.Response{
			ToolCalls:  []tool.Call{{ID: "c1", Surface: "work", Action: "chain_state"}},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "the answer", StopReason: model.StopEndTurn},
	)
	fp := &fakeProvider{}
	res, err := New(single(m), fp, nil, WithSystemPrompt("sys")).Run(context.Background(), "state?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "the answer" {
		t.Errorf("Text = %q", res.Text)
	}
	if len(res.Dispatches) != 1 || !res.Dispatches[0].OK || fp.calls != 1 {
		t.Errorf("dispatches=%+v calls=%d", res.Dispatches, fp.calls)
	}
}

func TestRunNoToolDirectAnswer(t *testing.T) {
	m := model.NewEcho("qwen", model.Response{Text: "hi", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil).Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "hi" || len(res.Dispatches) != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestRunModelErrorPropagates(t *testing.T) {
	if _, err := New(single(errAdapter{}), &fakeProvider{}, nil).Run(context.Background(), "x"); err == nil {
		t.Error("want error from a failing model")
	}
}

func TestRunExceedsMaxRounds(t *testing.T) {
	toolTurn := model.Response{
		ToolCalls:  []tool.Call{{ID: "c", Surface: "work", Action: "x"}},
		StopReason: model.StopToolUse,
	}
	m := model.NewEcho("qwen", toolTurn, toolTurn, toolTurn)
	if _, err := New(single(m), &fakeProvider{}, nil, WithMaxRounds(2)).Run(context.Background(), "loop"); err == nil {
		t.Error("want error when tool rounds are exceeded")
	}
}

func TestRunEncodesUnmarshalableToolResult(t *testing.T) {
	m := model.NewEcho("qwen",
		model.Response{
			ToolCalls:  []tool.Call{{ID: "c", Surface: "work", Action: "x"}},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "ok", StopReason: model.StopEndTurn},
	)
	res, err := New(single(m), chanProvider{}, nil).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Dispatches) != 1 {
		t.Errorf("dispatches = %d, want 1", len(res.Dispatches))
	}
}

func TestHooksFireAndCostTracked(t *testing.T) {
	m := model.NewEcho("claude-haiku-4-5-20251001",
		model.Response{
			ToolCalls:  []tool.Call{{ID: "c", Surface: "work", Action: "x"}},
			StopReason: model.StopToolUse,
			Usage:      model.Usage{InputTokens: 1000, OutputTokens: 1000},
		},
		model.Response{Text: "done", StopReason: model.StopEndTurn, Usage: model.Usage{InputTokens: 1000}},
	)
	fired := map[hooks.Kind]int{}
	h := hooks.NewSurface()
	for _, k := range hooks.Order {
		k := k
		_ = h.Register(k, "count", func(*hooks.Context) { fired[k]++ })
	}
	loop := New(single(m), &fakeProvider{}, nil, WithHooks(h), WithSession("sess1", "mcp-servers"))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	loop.Close()

	for _, k := range hooks.Order {
		if fired[k] == 0 {
			t.Errorf("hook %s did not fire", k)
		}
	}
	if res.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want > 0 for a priced model", res.CostUSD)
	}
}

func TestWithProfileThreadsThroughToHookContext(t *testing.T) {
	prof := &profile.JobProfile{Name: "task-lifecycle", Tier: profile.TierLocal}
	var seen *profile.JobProfile
	h := hooks.NewSurface()
	_ = h.Register(hooks.PreTurn, "grab", func(c *hooks.Context) { seen = c.Profile })

	m := model.NewEcho("claude-haiku-4-5-20251001",
		model.Response{Text: "ok", StopReason: model.StopEndTurn})
	loop := New(single(m), &fakeProvider{}, nil, WithHooks(h), WithProfile(prof))
	if loop.Profile() != prof {
		t.Fatalf("Profile() = %v, want the configured profile", loop.Profile())
	}
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if seen != prof {
		t.Errorf("hook context Profile = %v, want the active profile threaded through", seen)
	}
}

func TestNoProfileLeavesHookContextProfileNil(t *testing.T) {
	var seen *profile.JobProfile
	saw := false
	h := hooks.NewSurface()
	_ = h.Register(hooks.PreTurn, "grab", func(c *hooks.Context) { seen = c.Profile; saw = true })
	m := model.NewEcho("claude-haiku-4-5-20251001", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	if _, err := New(single(m), &fakeProvider{}, nil, WithHooks(h)).Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !saw {
		t.Fatal("pre_turn hook did not fire")
	}
	if seen != nil {
		t.Errorf("unprojected loop should leave hook Profile nil, got %v", seen)
	}
}

func TestPreToolUseVetoSkipsDispatchAndFeedsBack(t *testing.T) {
	m := model.NewEcho("claude-haiku-4-5-20251001",
		model.Response{
			ToolCalls:  []tool.Call{{ID: "c", Surface: "sys", Action: "exec"}},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "ok, stopping", StopReason: model.StopEndTurn},
	)
	fp := &fakeProvider{}
	h := hooks.NewSurface()
	_ = h.Register(hooks.PreToolUse, "veto", func(c *hooks.Context) {
		c.DenyToolCall = true
		c.DenyReason = "blocked by test gate"
	})
	res, err := New(single(m), fp, nil, WithHooks(h)).Run(context.Background(), "run something risky")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fp.calls != 0 {
		t.Errorf("vetoed call was still dispatched (%d provider calls)", fp.calls)
	}
	if len(res.Dispatches) != 1 || res.Dispatches[0].OK {
		t.Fatalf("expected one failed (blocked) dispatch, got %+v", res.Dispatches)
	}
	if res.Dispatches[0].ErrorClass != tool.ClassTool {
		t.Errorf("blocked dispatch class = %q, want tool_error", res.Dispatches[0].ErrorClass)
	}
}

func TestContextProberPopulatesHookMetadataBeforePreUserPrompt(t *testing.T) {
	var seen map[string]any
	h := hooks.NewSurface()
	_ = h.Register(hooks.PreUserPrompt, "grab", func(c *hooks.Context) { seen = c.Metadata })
	prober := func(_ context.Context, prompt string) map[string]any {
		return map[string]any{"probed": prompt}
	}
	m := model.NewEcho("claude-haiku-4-5-20251001", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	if _, err := New(single(m), &fakeProvider{}, nil, WithHooks(h), WithContextProber(prober)).Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if seen["probed"] != "hello" {
		t.Errorf("prober result should reach the pre_user_prompt hook Metadata, got %v", seen)
	}
}

func TestNilContextProberLeavesMetadataNil(t *testing.T) {
	var seen map[string]any
	saw := false
	h := hooks.NewSurface()
	_ = h.Register(hooks.PreUserPrompt, "grab", func(c *hooks.Context) { seen = c.Metadata; saw = true })
	m := model.NewEcho("claude-haiku-4-5-20251001", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	if _, err := New(single(m), &fakeProvider{}, nil, WithHooks(h)).Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !saw {
		t.Fatal("hook did not fire")
	}
	if seen != nil {
		t.Errorf("no prober should leave Metadata nil, got %v", seen)
	}
}

func TestPreUserPromptInjectsSystemMessage(t *testing.T) {
	rec := &recAdapter{}
	h := hooks.NewSurface()
	_ = h.Register(hooks.PreUserPrompt, "inject", func(c *hooks.Context) {
		c.SystemPromptAdditions = append(c.SystemPromptAdditions, "reflex note")
	})
	if _, err := New(single(rec), &fakeProvider{}, nil, WithHooks(h)).Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range rec.got {
		if msg.Role == model.RoleSystem && msg.Content == "reflex note" {
			found = true
		}
	}
	if !found {
		t.Error("a pre_user_prompt system-prompt addition should reach the transcript")
	}
}
