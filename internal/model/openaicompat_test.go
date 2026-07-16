package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"corpos/internal/tool"
)

func TestOpenAICompatTextResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth header = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["model"] != "qwen" {
			t.Errorf("model = %v", req["model"])
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	o := NewOpenAICompat("qwen", srv.URL, WithOACKey("k"))
	got, err := o.Complete(context.Background(), []ChatMessage{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Text != "hi there" || got.StopReason != StopEndTurn {
		t.Errorf("response = %+v", got)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

// TestOpenAICompatUsageAccounting checks that with usage accounting on, the
// adapter sends usage:{include:true} and parses the provider-reported cost +
// cached-token breakdown (the OpenRouter source-of-truth path for bug 1046).
func TestOpenAICompatUsageAccounting(t *testing.T) {
	var sawUsageInclude bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if u, ok := req["usage"].(map[string]any); ok {
			sawUsageInclude, _ = u["include"].(bool)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"cost":0.0123,"prompt_tokens_details":{"cached_tokens":800}}}`))
	}))
	defer srv.Close()

	o := NewOpenAICompat("g", srv.URL, WithOACUsageAccounting())
	got, err := o.Complete(context.Background(), []ChatMessage{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !sawUsageInclude {
		t.Error("usage accounting on → request must carry usage:{include:true}")
	}
	if !got.Usage.CostReported || got.Usage.CostUSD != 0.0123 {
		t.Errorf("provider cost not parsed: %+v", got.Usage)
	}
	if got.Usage.InputTokens != 1000 || got.Usage.CachedInputTokens != 800 {
		t.Errorf("cached-token breakdown not parsed: %+v", got.Usage)
	}
}

// TestOpenAICompatNoUsageAccountingByDefault checks the default omits the usage
// request field (lenient local servers needn't tolerate it) and reports no cost.
func TestOpenAICompatNoUsageAccountingByDefault(t *testing.T) {
	var hasUsageField bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		_, hasUsageField = req["usage"]
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer srv.Close()

	got, err := NewOpenAICompat("qwen", srv.URL).Complete(context.Background(), []ChatMessage{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if hasUsageField {
		t.Error("default (no usage accounting) must not send a usage request field")
	}
	if got.Usage.CostReported {
		t.Error("no provider cost expected without usage accounting")
	}
}

func TestOpenAICompatToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"tc1","type":"function","function":{"name":"work","arguments":"{\"action\":\"chain_state\",\"params\":{\"slug\":\"x\"}}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
	}))
	defer srv.Close()

	spec := tool.Spec{Name: "work", Description: "d", InputSchema: map[string]any{"type": "object"}}
	got, err := NewOpenAICompat("qwen", srv.URL).Complete(context.Background(), nil, []tool.Spec{spec})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.StopReason != StopToolUse || len(got.ToolCalls) != 1 {
		t.Fatalf("response = %+v", got)
	}
	tc := got.ToolCalls[0]
	if tc.ID != "tc1" || tc.Surface != "work" || tc.Action != "chain_state" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Params["slug"] != "x" {
		t.Errorf("params = %+v", tc.Params)
	}
}

func TestOpenAICompatRoundTripsToolMessages(t *testing.T) {
	// An assistant-with-tool-calls message and a tool-result message must
	// serialize without error (covers the toOACMessages tool-call path).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	msgs := []ChatMessage{
		{Role: RoleAssistant, ToolCalls: []tool.Call{{ID: "c1", Surface: "work", Action: "chain_state", Params: map[string]any{"slug": "x"}, Rationale: "r"}}},
		{Role: RoleTool, ToolCallID: "c1", Name: "work", Content: `{"ok":true}`},
	}
	got, err := NewOpenAICompat("qwen", srv.URL).Complete(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Text != "done" {
		t.Errorf("text = %q", got.Text)
	}
}

func TestOpenAICompatErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "non-2xx",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
			},
		},
		{
			name: "bad json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("not json"))
			},
		},
		{
			name: "empty choices",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[]}`))
			},
		},
		{
			name: "bad tool arguments",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"t","type":"function","function":{"name":"work","arguments":"not json"}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if _, err := NewOpenAICompat("qwen", srv.URL).Complete(context.Background(), nil, nil); err == nil {
				t.Errorf("%s: want error, got nil", tc.name)
			}
		})
	}
}

func TestOpenAICompatTransportError(t *testing.T) {
	o := NewOpenAICompat("qwen", "http://127.0.0.1:1", WithOACHTTPClient(&http.Client{Timeout: 2 * time.Second}))
	if _, err := o.Complete(context.Background(), nil, nil); err == nil {
		t.Error("want transport error")
	}
}

func TestOpenAICompatDefaultTimeoutIsGenerous(t *testing.T) {
	// Regression guard (bug corpos-orchestrate-per-turn-timeout-…): the default
	// per-call timeout must stay generous enough for a large hosted mid-tier
	// synthesis call. Run-20 saw 120s trip every orchestrate turn; don't regress.
	o := NewOpenAICompat("qwen", "http://x")
	if o.httpClient.Timeout != oacDefaultTimeout {
		t.Fatalf("default client timeout = %v, want oacDefaultTimeout %v", o.httpClient.Timeout, oacDefaultTimeout)
	}
	if oacDefaultTimeout < 300*time.Second {
		t.Errorf("oacDefaultTimeout = %v, want >= 300s (a large hosted call must not trip the per-call timeout)", oacDefaultTimeout)
	}
}

func TestOpenAICompatBadURL(t *testing.T) {
	if _, err := NewOpenAICompat("qwen", "http://\x7f").Complete(context.Background(), nil, nil); err == nil {
		t.Error("want request-build error")
	}
}

func TestOpenAICompatMarshalError(t *testing.T) {
	// A channel in a tool spec's schema can't be JSON-marshaled → request marshal fails.
	tools := []tool.Spec{{Name: "work", InputSchema: map[string]any{"bad": make(chan int)}}}
	if _, err := NewOpenAICompat("qwen", "http://x").Complete(context.Background(), nil, tools); err == nil {
		t.Error("want marshal error")
	}
}

func TestOpenAICompatModelAndAvailable(t *testing.T) {
	o := NewOpenAICompat("qwen", "http://x")
	if o.Model() != "qwen" || !o.Available() {
		t.Errorf("Model/Available: %q %v", o.Model(), o.Available())
	}
	if NewOpenAICompat("qwen", "").Available() {
		t.Error("empty baseURL should be unavailable")
	}
}

func TestOpenAICompatRequireKey(t *testing.T) {
	// A key-requiring endpoint is unavailable without a key, available with one.
	if NewOpenAICompat("gem", "http://x", WithOACRequireKey()).Available() {
		t.Error("a key-requiring endpoint with no key should be unavailable")
	}
	if !NewOpenAICompat("gem", "http://x", WithOACRequireKey(), WithOACKey("k")).Available() {
		t.Error("a key-requiring endpoint with a key should be available")
	}
	// Without WithOACRequireKey a keyless endpoint stays available (local Qwen).
	if !NewOpenAICompat("qwen", "http://x").Available() {
		t.Error("a keyless local endpoint should stay available")
	}
}

// TestOpenAICompatMaxTokens checks the adapter sends a default max_tokens cap and
// that WithOACMaxTokens overrides it — the runaway/timeout backstop (a verbose
// local model used to generate until the request timed out, then escalate).
func TestOpenAICompatMaxTokens(t *testing.T) {
	var sawMax float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		sawMax, _ = req["max_tokens"].(float64)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	// Default cap applied.
	o := NewOpenAICompat("qwen", srv.URL)
	if _, err := o.Complete(context.Background(), []ChatMessage{{Role: RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if int(sawMax) != oacDefaultMaxTokens {
		t.Errorf("default max_tokens = %d, want %d", int(sawMax), oacDefaultMaxTokens)
	}

	// Override.
	o2 := NewOpenAICompat("qwen", srv.URL, WithOACMaxTokens(123))
	if _, err := o2.Complete(context.Background(), []ChatMessage{{Role: RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if int(sawMax) != 123 {
		t.Errorf("overridden max_tokens = %d, want 123", int(sawMax))
	}
}

func TestWithOACTimeout(t *testing.T) {
	if got := NewOpenAICompat("m", "http://x", WithOACTimeout(45*time.Second)).CallTimeout(); got != 45*time.Second {
		t.Errorf("CallTimeout = %v, want 45s", got)
	}
	// A non-positive duration leaves the constructor default in place.
	def := NewOpenAICompat("m", "http://x").CallTimeout()
	if got := NewOpenAICompat("m", "http://x", WithOACTimeout(0)).CallTimeout(); got != def {
		t.Errorf("WithOACTimeout(0) CallTimeout = %v, want the default %v", got, def)
	}
}
