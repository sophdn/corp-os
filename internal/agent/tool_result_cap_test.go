package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

func capEcho() model.Adapter {
	return model.NewEcho("m", model.Response{Text: "done", StopReason: model.StopEndTurn})
}

func TestToolResultCap_Sizing(t *testing.T) {
	// No compactor → the safe default.
	l := New(router.New(capEcho(), capEcho()), vProvider{}, nil)
	if got := l.toolResultCap(); got != defaultToolResultChars {
		t.Fatalf("no-compactor cap = %d, want %d", got, defaultToolResultChars)
	}
	// With a compaction budget → ~budget chars (a quarter of the budget in tokens).
	lc := New(router.New(capEcho(), capEcho()), vProvider{}, nil, WithCompaction(8000, 2, capEcho()))
	if got := lc.toolResultCap(); got != (8000/4)*approxCharsPerToken {
		t.Fatalf("budget-derived cap = %d, want %d", got, (8000/4)*approxCharsPerToken)
	}
	// A tiny budget is floored.
	lf := New(router.New(capEcho(), capEcho()), vProvider{}, nil, WithCompaction(100, 2, capEcho()))
	if got := lf.toolResultCap(); got != minToolResultChars {
		t.Fatalf("tiny-budget cap = %d, want the floor %d", got, minToolResultChars)
	}
	// An explicit override wins.
	lo := New(router.New(capEcho(), capEcho()), vProvider{}, nil, WithCompaction(8000, 2, capEcho()), WithToolResultCap(321))
	if got := lo.toolResultCap(); got != 321 {
		t.Fatalf("override cap = %d, want 321", got)
	}
	// A negative override clamps to 0 → derives (here, default).
	ln := New(router.New(capEcho(), capEcho()), vProvider{}, nil, WithToolResultCap(-5))
	if got := ln.toolResultCap(); got != defaultToolResultChars {
		t.Fatalf("negative override should derive, got %d", got)
	}
}

func TestCapToolResult_TruncatesWithMarker(t *testing.T) {
	l := New(router.New(capEcho(), capEcho()), vProvider{}, nil, WithToolResultCap(50))
	if got := l.capToolResult("short"); got != "short" {
		t.Fatalf("short content must pass through unchanged, got %q", got)
	}
	long := strings.Repeat("x", 200)
	out := l.capToolResult(long)
	if !strings.HasPrefix(out, strings.Repeat("x", 50)) {
		t.Fatalf("truncated head should be the first 50 chars")
	}
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "200 chars") {
		t.Fatalf("marker missing the truncation notice + original size: %q", out)
	}
	// The body that reaches the model is bounded (head + a short marker), not 200.
	if len(out) > 50+400 {
		t.Fatalf("capped result too large: %d chars", len(out))
	}
}

func TestCapToolResult_UTF8Boundary(t *testing.T) {
	l := New(router.New(capEcho(), capEcho()), vProvider{}, nil, WithToolResultCap(10))
	// '€' is 3 bytes; place it straddling the 10-byte cut so a naive slice splits it.
	content := strings.Repeat("a", 9) + "€" + "tail"
	out := l.capToolResult(content)
	if !utf8.ValidString(out) {
		t.Fatalf("truncation split a multi-byte rune: %q", out)
	}
}

// oneToolThenDone makes exactly one tool call, then claims done.
type oneToolThenDone struct{ n int }

func (a *oneToolThenDone) Model() string   { return "m" }
func (a *oneToolThenDone) Available() bool { return true }
func (a *oneToolThenDone) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	a.n++
	if a.n == 1 {
		return model.Response{Model: "m", ToolCalls: []tool.Call{{ID: "1", Surface: "fs", Action: "read"}}}, nil
	}
	return model.Response{Model: "m", Text: "done", StopReason: model.StopEndTurn}, nil
}

// hugeProvider returns an oversized result — a full bug_list dump / unscoped grep.
type hugeProvider struct{ payload string }

func (p hugeProvider) Dispatch(context.Context, tool.Call) tool.Result {
	return tool.Result{OK: true, Value: map[string]any{"data": p.payload}}
}

// TestLoopCapsHugeToolResult is the run-6 regression: a single oversized tool
// result must be truncated before it enters the transcript, never blowing the
// window.
func TestLoopCapsHugeToolResult(t *testing.T) {
	adapter := &oneToolThenDone{}
	huge := strings.Repeat("Z", 100_000) // a 100k-char result, like the run-6 overflow
	l := New(router.New(adapter, adapter), hugeProvider{payload: huge}, []tool.Spec{{Name: "fs"}}, WithToolResultCap(2000))

	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	var toolMsg string
	for _, m := range l.transcript {
		if m.Role == model.RoleTool {
			toolMsg = m.Content
		}
	}
	if toolMsg == "" {
		t.Fatal("expected a RoleTool message in the transcript")
	}
	if len(toolMsg) > 2000+500 {
		t.Fatalf("oversized tool result was NOT capped: %d chars entered the transcript", len(toolMsg))
	}
	if !strings.Contains(toolMsg, "truncated") {
		t.Fatalf("capped result missing the truncation marker: %q", toolMsg[:min(200, len(toolMsg))])
	}
	if strings.Contains(toolMsg, strings.Repeat("Z", 5000)) {
		t.Fatal("the huge payload leaked into the transcript uncapped")
	}
}
