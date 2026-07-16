package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"corpos/internal/tool"
)

func TestClassifyFault(t *testing.T) {
	timeoutNetErr := &net.DNSError{IsTimeout: true} // a net.Error whose Timeout() is true
	cases := []struct {
		name string
		err  error
		want FaultKind
	}{
		{"nil", nil, FaultNone},
		{"wrapped overflow", fmt.Errorf("x: %w", ErrContextOverflow), FaultContextOverflow},
		{"wrapped malformed", fmt.Errorf("x: %w", ErrMalformedToolCall), FaultMalformedToolCall},
		{"wrapped rate limit", fmt.Errorf("x: %w", ErrRateLimit), FaultRateLimit},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), FaultTimeout},
		{"net timeout", timeoutNetErr, FaultTimeout},
		{"plain error", errors.New("boom"), FaultNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyFault(c.err); got != c.want {
				t.Errorf("ClassifyFault(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

func TestLooksLikeContextOverflow(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusBadRequest, `{"error":{"type":"exceed_context_size_error"}}`, true},
		{http.StatusBadRequest, "the prompt is too long for the context window", true},
		{http.StatusRequestEntityTooLarge, "n_ctx exceeded", true},
		{http.StatusBadRequest, "MAXIMUM CONTEXT reached", true}, // case-insensitive
		{http.StatusBadRequest, "some unrelated validation error", false},
		// llama.cpp reports overflow as a 500 server_error too — must be recognised.
		{http.StatusInternalServerError, `{"error":{"code":500,"message":"Context size has been exceeded.","type":"server_error"}}`, true},
		{http.StatusInternalServerError, "internal server error", false}, // generic 500 stays fatal
		{http.StatusOK, "context length", false},                         // a 2xx is never an overflow
	}
	for _, c := range cases {
		if got := LooksLikeContextOverflow(c.status, c.body); got != c.want {
			t.Errorf("LooksLikeContextOverflow(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestLooksLikeRateLimit(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true}, // 429
		{statusOverloaded, true},           // 529 (Anthropic overloaded)
		{http.StatusBadRequest, false},
		{http.StatusInternalServerError, false},
		{http.StatusOK, false},
	}
	for _, c := range cases {
		if got := LooksLikeRateLimit(c.status); got != c.want {
			t.Errorf("LooksLikeRateLimit(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

// A 429 (or 529) is surfaced as a wrapped ErrRateLimit so the loop backs off and
// de-escalates rather than aborting.
func TestOpenAICompatWrapsRateLimit(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, statusOverloaded} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
		}))
		_, err := NewOpenAICompat("qwen", srv.URL).Complete(context.Background(), nil, nil)
		srv.Close()
		if !errors.Is(err, ErrRateLimit) {
			t.Errorf("status %d should wrap ErrRateLimit, got %v", status, err)
		}
	}
}

// A 400 with an overflow body is surfaced as a wrapped ErrContextOverflow so the
// loop can compact-and-retry; an unrelated 400 stays a plain (fatal) error.
func TestOpenAICompatWrapsOverflow(t *testing.T) {
	overflow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"type":"exceed_context_size_error","message":"the request exceeds the available context size"}}`))
	}))
	defer overflow.Close()
	_, err := NewOpenAICompat("qwen", overflow.URL).Complete(context.Background(), nil, nil)
	if !errors.Is(err, ErrContextOverflow) {
		t.Errorf("overflow 400 should wrap ErrContextOverflow, got %v", err)
	}

	// llama.cpp also returns overflow as a 500 server_error — it must wrap too
	// (the run-3 failure mode: an intra-turn overflow came back as 500 and, before
	// this fix, was treated as fatal instead of compact-and-retry).
	overflow500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"Context size has been exceeded.","type":"server_error"}}`))
	}))
	defer overflow500.Close()
	_, err = NewOpenAICompat("qwen", overflow500.URL).Complete(context.Background(), nil, nil)
	if !errors.Is(err, ErrContextOverflow) {
		t.Errorf("overflow 500 should wrap ErrContextOverflow, got %v", err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer plain.Close()
	_, err = NewOpenAICompat("qwen", plain.URL).Complete(context.Background(), nil, nil)
	if errors.Is(err, ErrContextOverflow) || err == nil {
		t.Errorf("an unrelated 400 should be a plain error, got %v", err)
	}
}

// A truncated/invalid tool-call arguments blob is surfaced as a wrapped
// ErrMalformedToolCall so the loop can re-prompt.
func TestOpenAICompatWrapsMalformedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// arguments is a truncated JSON object — unexpected end of input.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"fs","arguments":"{\"action\":\"write\",\"params\":{"}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
	}))
	defer srv.Close()
	_, err := NewOpenAICompat("qwen", srv.URL).Complete(context.Background(), nil, []tool.Spec{{Name: "fs"}})
	if !errors.Is(err, ErrMalformedToolCall) {
		t.Errorf("a malformed tool call should wrap ErrMalformedToolCall, got %v", err)
	}
}

func TestOpenAICompatContextWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("probe path = %q, want /models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"other","meta":{"n_ctx":4096}},{"id":"qwen","meta":{"n_ctx":8192}}]}`))
	}))
	defer srv.Close()
	got, ok := NewOpenAICompat("qwen", srv.URL).ContextWindow(context.Background())
	if !ok || got != 8192 {
		t.Errorf("ContextWindow = (%d,%v), want (8192,true) — the matching model id", got, ok)
	}
}

func TestOpenAICompatContextWindowFallbackAndFailSoft(t *testing.T) {
	// No id match → fall back to the first entry carrying an n_ctx.
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a","meta":{"n_ctx":0}},{"id":"b","meta":{"n_ctx":2048}}]}`))
	}))
	defer fallback.Close()
	if got, ok := NewOpenAICompat("zzz", fallback.URL).ContextWindow(context.Background()); !ok || got != 2048 {
		t.Errorf("fallback ContextWindow = (%d,%v), want (2048,true)", got, ok)
	}

	// Non-2xx → fail soft.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if _, ok := NewOpenAICompat("qwen", bad.URL).ContextWindow(context.Background()); ok {
		t.Error("a non-2xx probe should fail soft (ok=false)")
	}

	// Garbage body → fail soft.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer garbage.Close()
	if _, ok := NewOpenAICompat("qwen", garbage.URL).ContextWindow(context.Background()); ok {
		t.Error("a non-JSON probe should fail soft (ok=false)")
	}

	// No n_ctx anywhere → fail soft.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a","meta":{"n_ctx":0}}]}`))
	}))
	defer empty.Close()
	if _, ok := NewOpenAICompat("qwen", empty.URL).ContextWindow(context.Background()); ok {
		t.Error("no n_ctx should fail soft (ok=false)")
	}
}

func TestOpenAICompatConfiguredWindowFallback(t *testing.T) {
	// Probe carries no n_ctx (a hosted gateway like OpenRouter) → fall back to the
	// configured static window instead of (0,false).
	noNCtx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gem","meta":{}}]}`))
	}))
	defer noNCtx.Close()
	if got, ok := NewOpenAICompat("gem", noNCtx.URL, WithOACWindow(1_000_000)).ContextWindow(context.Background()); !ok || got != 1_000_000 {
		t.Errorf("configured fallback ContextWindow = (%d,%v), want (1000000,true)", got, ok)
	}

	// A live n_ctx still wins over the configured window (probe is authoritative
	// when it carries a value — the local llama.cpp case must stay VRAM-fixed).
	withNCtx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen","meta":{"n_ctx":8192}}]}`))
	}))
	defer withNCtx.Close()
	if got, ok := NewOpenAICompat("qwen", withNCtx.URL, WithOACWindow(1_000_000)).ContextWindow(context.Background()); !ok || got != 8192 {
		t.Errorf("probe should win over configured: ContextWindow = (%d,%v), want (8192,true)", got, ok)
	}

	// No probe value and no configured window → still fail soft.
	if _, ok := NewOpenAICompat("gem", noNCtx.URL).ContextWindow(context.Background()); ok {
		t.Error("no n_ctx and no configured window should fail soft (ok=false)")
	}
}

// The adapter satisfies the WindowProber interface.
var _ WindowProber = (*OpenAICompat)(nil)
