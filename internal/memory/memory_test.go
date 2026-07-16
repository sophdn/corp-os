package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"corpos/internal/hooks"
	"corpos/internal/tool"
)

// fakeDispatcher returns a canned result and records the call it received.
type fakeDispatcher struct {
	res  tool.Result
	got  tool.Call
	went bool
}

func (f *fakeDispatcher) Dispatch(_ context.Context, call tool.Call) tool.Result {
	f.got = call
	f.went = true
	return f.res
}

func okResult(value any) tool.Result {
	return tool.Result{OK: true, Value: value}
}

func runHook(in *Injector) *hooks.Context {
	ctx := &hooks.Context{Kind: hooks.SessionStart}
	in.SessionStartHook()(ctx)
	return ctx
}

func TestSessionStart_InjectsDigest(t *testing.T) {
	f := &fakeDispatcher{res: okResult(map[string]any{
		"memory_markdown": "- [a](memory/user/a.md) — fact a\n",
		"entry_count":     1,
	})}
	ctx := runHook(New(f, "mcp-servers"))

	if !f.went || f.got.Surface != "knowledge" || f.got.Action != "memory_read" {
		t.Fatalf("expected knowledge.memory_read dispatch, got %+v", f.got)
	}
	if f.got.Params["project"] != "mcp-servers" {
		t.Errorf("project param = %v", f.got.Params["project"])
	}
	if len(ctx.SystemPromptAdditions) != 1 {
		t.Fatalf("want 1 system prompt addition, got %d", len(ctx.SystemPromptAdditions))
	}
	got := ctx.SystemPromptAdditions[0]
	for _, want := range []string{"# Memory", "project mcp-servers", "1 entries", "fact a"} {
		if !strings.Contains(got, want) {
			t.Errorf("injection missing %q:\n%s", want, got)
		}
	}
}

func TestSessionStart_NoopPaths(t *testing.T) {
	cases := []struct {
		name string
		in   *Injector
	}{
		{"empty project", New(&fakeDispatcher{res: okResult(map[string]any{"memory_markdown": "x"})}, "")},
		{"dispatch not ok", New(&fakeDispatcher{res: tool.Result{OK: false, Value: map[string]any{"error": "boom"}}}, "p")},
		{"result error", New(&fakeDispatcher{res: okResult(map[string]any{"error": "params.project is required"})}, "p")},
		{"empty digest", New(&fakeDispatcher{res: okResult(map[string]any{"memory_markdown": "", "entry_count": 0})}, "p")},
		{"undecodable value", New(&fakeDispatcher{res: okResult(make(chan int))}, "p")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := runHook(tc.in)
			if len(ctx.SystemPromptAdditions) != 0 {
				t.Errorf("expected no injection, got %v", ctx.SystemPromptAdditions)
			}
		})
	}
}

func TestWithTimeout(t *testing.T) {
	f := &fakeDispatcher{res: okResult(map[string]any{"memory_markdown": "- [a](memory/user/a.md) — a\n", "entry_count": 1})}
	in := New(f, "p", WithTimeout(2*time.Second), WithTimeout(0)) // 0 is ignored
	if in.timeout != 2*time.Second {
		t.Errorf("timeout = %v, want 2s (0 override ignored)", in.timeout)
	}
	if ctx := runHook(in); len(ctx.SystemPromptAdditions) != 1 {
		t.Errorf("expected injection with custom timeout")
	}
}
