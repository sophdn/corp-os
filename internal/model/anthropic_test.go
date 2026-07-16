package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"corpos/internal/tool"
)

func TestAnthropicTextResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "k" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version header")
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["system"] != "sys" {
			t.Errorf("system = %v, want sys", req["system"])
		}
		if req["max_tokens"] == nil {
			t.Error("missing max_tokens")
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello from haiku"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":4}}`))
	}))
	defer srv.Close()

	a := NewAnthropic("claude-haiku", WithAnthropicKey("k"), WithAnthropicBaseURL(srv.URL))
	got, err := a.Complete(context.Background(), []ChatMessage{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Text != "hello from haiku" || got.StopReason != StopEndTurn {
		t.Errorf("response = %+v", got)
	}
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

// TestAnthropicCacheUsage checks the adapter folds Anthropic's separately-reported
// cache-read/cache-creation tokens into the full prompt size and breaks them out
// on Usage, so the ledger can bill the cached prefix at the cheaper rate (bug 1046).
func TestAnthropicCacheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":900,"cache_creation_input_tokens":50}}`))
	}))
	defer srv.Close()

	a := NewAnthropic("claude-opus-4-8", WithAnthropicKey("k"), WithAnthropicBaseURL(srv.URL))
	got, err := a.Complete(context.Background(), []ChatMessage{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// InputTokens is the full prompt size: fresh(100) + cache_read(900) + cache_creation(50).
	if got.Usage.InputTokens != 1050 {
		t.Errorf("InputTokens = %d, want 1050 (fresh+cache_read+cache_creation)", got.Usage.InputTokens)
	}
	if got.Usage.CachedInputTokens != 900 || got.Usage.CacheWriteTokens != 50 {
		t.Errorf("cache split = read %d / write %d, want 900 / 50", got.Usage.CachedInputTokens, got.Usage.CacheWriteTokens)
	}
	if got.Usage.CostReported {
		t.Error("Anthropic reports no dollar cost — CostReported must be false")
	}
}

func TestAnthropicToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"tu1","name":"work","input":{"action":"chain_state","params":{"slug":"x"}}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer srv.Close()

	spec := tool.Spec{Name: "work", Description: "d", InputSchema: map[string]any{"type": "object"}}
	got, err := NewAnthropic("claude-haiku", WithAnthropicKey("k"), WithAnthropicBaseURL(srv.URL)).
		Complete(context.Background(), nil, []tool.Spec{spec})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.StopReason != StopToolUse || len(got.ToolCalls) != 1 {
		t.Fatalf("response = %+v", got)
	}
	if got.Text != "let me check" {
		t.Errorf("text = %q", got.Text)
	}
	tc := got.ToolCalls[0]
	if tc.ID != "tu1" || tc.Surface != "work" || tc.Action != "chain_state" || tc.Params["slug"] != "x" {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestAnthropicRoundTripsToolMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer srv.Close()

	msgs := []ChatMessage{
		{Role: RoleAssistant, ToolCalls: []tool.Call{{ID: "c1", Surface: "work", Action: "x", Params: map[string]any{"a": 1}, Rationale: "r"}}},
		{Role: RoleTool, ToolCallID: "c1", Content: `{"ok":true}`},
	}
	got, err := NewAnthropic("claude-haiku", WithAnthropicKey("k"), WithAnthropicBaseURL(srv.URL)).
		Complete(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Text != "done" {
		t.Errorf("text = %q", got.Text)
	}
}

func TestAnthropicErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "non-2xx",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			},
		},
		{
			name: "bad json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("nope"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			a := NewAnthropic("h", WithAnthropicKey("k"), WithAnthropicBaseURL(srv.URL))
			if _, err := a.Complete(context.Background(), nil, nil); err == nil {
				t.Errorf("%s: want error, got nil", tc.name)
			}
		})
	}
}

// The Anthropic adapter is the strong rung, so its recoverable rejections must map
// onto the fault taxonomy: a 400 "prompt is too long" → ErrContextOverflow, a 429 →
// ErrRateLimit. An unrelated non-2xx stays a plain (fatal) error.
func TestAnthropicWrapsRecoverableRejections(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"overflow", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`, ErrContextOverflow},
		{"rate limit", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error"}}`, ErrRateLimit},
		{"overloaded", statusOverloaded, `{"error":{"type":"overloaded_error"}}`, ErrRateLimit},
		{"unrelated", http.StatusUnauthorized, `{"error":{"message":"bad key"}}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			a := NewAnthropic("h", WithAnthropicKey("k"), WithAnthropicBaseURL(srv.URL))
			_, err := a.Complete(context.Background(), nil, nil)
			if tc.want == nil {
				if err == nil || errors.Is(err, ErrContextOverflow) || errors.Is(err, ErrRateLimit) {
					t.Fatalf("unrelated %d should be a plain error, got %v", tc.status, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d should wrap %v, got %v", tc.status, tc.want, err)
			}
		})
	}
}

func TestAnthropicTransportError(t *testing.T) {
	a := NewAnthropic("h", WithAnthropicKey("k"),
		WithAnthropicBaseURL("http://127.0.0.1:1"),
		WithAnthropicHTTPClient(&http.Client{Timeout: 2 * time.Second}))
	if _, err := a.Complete(context.Background(), nil, nil); err == nil {
		t.Error("want transport error")
	}
}

func TestAnthropicDefaultTimeoutNotTighterThanOAC(t *testing.T) {
	// Regression guard: Opus is the timeout-fault ESCALATION target, so its per-call
	// bound must be >= the OpenAI-compat rung's — else escalating on a timeout just
	// times out again (swap-rehearsal Run-20, opus bound spent on a timing-out call).
	a := NewAnthropic("h", WithAnthropicKey("k"))
	if a.httpClient.Timeout != anthropicDefaultTimeout {
		t.Fatalf("default client timeout = %v, want anthropicDefaultTimeout %v", a.httpClient.Timeout, anthropicDefaultTimeout)
	}
	if anthropicDefaultTimeout < oacDefaultTimeout {
		t.Errorf("anthropicDefaultTimeout %v < oacDefaultTimeout %v — the escalation target must not have a tighter per-call bound", anthropicDefaultTimeout, oacDefaultTimeout)
	}
}

func TestAnthropicBadURL(t *testing.T) {
	a := NewAnthropic("h", WithAnthropicKey("k"), WithAnthropicBaseURL("http://\x7f"))
	if _, err := a.Complete(context.Background(), nil, nil); err == nil {
		t.Error("want request-build error")
	}
}

func TestAnthropicMarshalError(t *testing.T) {
	tools := []tool.Spec{{Name: "work", InputSchema: map[string]any{"bad": make(chan int)}}}
	if _, err := NewAnthropic("h", WithAnthropicKey("k")).Complete(context.Background(), nil, tools); err == nil {
		t.Error("want marshal error")
	}
}

func TestAnthropicCoalescesToolResults(t *testing.T) {
	// Two tool results after one assistant turn must collapse into ONE Anthropic
	// user message with two tool_result blocks (not two consecutive user messages).
	_, msgs := splitAnthropicMessages([]ChatMessage{
		{Role: RoleAssistant, ToolCalls: []tool.Call{
			{ID: "a", Surface: "work", Action: "x"},
			{ID: "b", Surface: "fs", Action: "read"},
		}},
		{Role: RoleTool, ToolCallID: "a", Content: "ra"},
		{Role: RoleTool, ToolCallID: "b", Content: "rb"},
	})
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (assistant + one coalesced user), got %d: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "user" || len(msgs[1].Content) != 2 {
		t.Fatalf("tool results not coalesced into one user message: %+v", msgs[1])
	}
	if msgs[1].Content[0].Type != "tool_result" || msgs[1].Content[0].ToolUseID != "a" || msgs[1].Content[1].ToolUseID != "b" {
		t.Errorf("tool_result blocks wrong: %+v", msgs[1].Content)
	}
}

func TestAnthropicSkipsUnknownRole(t *testing.T) {
	// An unknown role is skipped (and must not loop forever).
	_, msgs := splitAnthropicMessages([]ChatMessage{
		{Role: "weird", Content: "ignored"},
		{Role: RoleUser, Content: "hi"},
	})
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Errorf("unknown role should be skipped, got %+v", msgs)
	}
}

func TestAnthropicDropsEmptyAssistantTurnOnReplay(t *testing.T) {
	// A prior assistant turn with neither text nor tool calls must be DROPPED
	// from the replayed history, not emitted as an empty text block (which the
	// Anthropic API 400s as "text content blocks must be non-empty"). This is
	// the multi-turn REPL replay hazard.
	_, msgs := splitAnthropicMessages([]ChatMessage{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, Content: ""}, // degenerate empty turn
		{Role: RoleUser, Content: "still there?"},
	})
	if len(msgs) != 2 {
		t.Fatalf("empty assistant turn should be dropped: got %d messages %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Role != "user" {
			t.Errorf("only the two user turns should survive, saw role %q", m.Role)
		}
		for _, b := range m.Content {
			if b.Type == "text" && b.Text == "" {
				t.Errorf("an empty text block leaked into the replay: %+v", m)
			}
		}
	}
}

func TestAnthropicKeepsAssistantTurnWithTextOrTools(t *testing.T) {
	// A text-only assistant turn and a tool-only assistant turn both carry
	// content and must survive the split (guard against over-dropping).
	_, msgs := splitAnthropicMessages([]ChatMessage{
		{Role: RoleAssistant, Content: "an answer"},
		{Role: RoleAssistant, ToolCalls: []tool.Call{{ID: "a", Surface: "work", Action: "x"}}},
	})
	if len(msgs) != 2 {
		t.Fatalf("text-only and tool-only assistant turns must survive: %+v", msgs)
	}
	if msgs[0].Content[0].Type != "text" || msgs[0].Content[0].Text != "an answer" {
		t.Errorf("text turn malformed: %+v", msgs[0])
	}
	if msgs[1].Content[0].Type != "tool_use" {
		t.Errorf("tool turn malformed: %+v", msgs[1])
	}
}

func TestAnthropicModelAndAvailable(t *testing.T) {
	a := NewAnthropic("claude-haiku", WithAnthropicKey("k"), WithAnthropicMaxTokens(1000))
	if a.Model() != "claude-haiku" || !a.Available() {
		t.Errorf("Model/Available with key: %q %v", a.Model(), a.Available())
	}
	if NewAnthropic("claude-haiku").Available() {
		t.Error("no key should be unavailable")
	}
}

func TestAnthropicContextWindow(t *testing.T) {
	t.Parallel()
	// Known id → its static window, no network call (nil context is fine).
	if got, ok := NewAnthropic("claude-opus-4-8").ContextWindow(context.Background()); !ok || got != 200_000 {
		t.Errorf("opus ContextWindow = (%d,%v), want (200000,true)", got, ok)
	}
	// Explicit override wins over the table.
	if got, ok := NewAnthropic("claude-opus-4-8", WithAnthropicWindow(500_000)).ContextWindow(context.Background()); !ok || got != 500_000 {
		t.Errorf("overridden ContextWindow = (%d,%v), want (500000,true)", got, ok)
	}
	// Unrecognized id and no override → fail soft.
	if _, ok := NewAnthropic("mystery-model").ContextWindow(context.Background()); ok {
		t.Error("an unrecognized id with no override should report ok=false")
	}
}
