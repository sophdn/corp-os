package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"corpos/internal/tool"
)

// oacDefaultTimeout is the per-request timeout for the OpenAI-compat transport.
// 300s (raised from 120s): a hosted mid-tier call (Gemini-Flash-Lite via the
// OpenRouter gateway) on a LARGE context — an orchestrator synthesis turn can
// carry 100k+ prompt tokens — spends real wall-clock on prompt processing PLUS
// gateway overhead PLUS generation, and 120s was too tight: swap-rehearsal Run-20
// saw every orchestrate turn die on a "recovered model-call fault (timeout)" at
// 120s (one even after escalating to Opus, whose own bound was tighter still),
// never reaching the verify turn (bug corpos-orchestrate-per-turn-timeout-ends-
// run-before-verify-leaves-unverified-artifact). 300s is well inside a normal
// per-turn budget and still bounds a genuinely hung call. The max-tokens cap
// bounds generation so a healthy call finishes well within this window.
const oacDefaultTimeout = 300 * time.Second

// oacDefaultMaxTokens caps the completion length for the OpenAI-compat transport.
// Unbounded generation let a verbose/looping local model run until the timeout
// (then escalate). A coding tool-call turn's output (the tool-call envelope, or a
// single edit) sits far below this, so the cap is a runaway backstop, not a
// truncator. 0 disables the cap.
const oacDefaultMaxTokens = 4096

// OpenAICompat is an Adapter for any OpenAI-compatible /chat/completions
// endpoint — local Qwen (llama.cpp) today; DeepSeek when pointed at its endpoint
// and key. Tool calls travel as the {action, params, rationale} envelope inside
// the function-call arguments, matching the surface-granular tool specs.
type OpenAICompat struct {
	model           string
	baseURL         string
	apiKey          string
	requireKey      bool
	usageAccounting bool
	maxTokens       int
	staticWindow    int
	httpClient      *http.Client
}

// OACOption configures an OpenAICompat adapter.
type OACOption func(*OpenAICompat)

// WithOACKey sets the bearer API key (empty for a keyless local endpoint).
func WithOACKey(key string) OACOption { return func(o *OpenAICompat) { o.apiKey = key } }

// WithOACRequireKey marks the endpoint as requiring an API key, so Available
// reports false (the router's fallback gate) when none is configured — a remote
// provider (e.g. OpenRouter) the ladder must degrade around when unkeyed, unlike
// a keyless local endpoint.
func WithOACRequireKey() OACOption { return func(o *OpenAICompat) { o.requireKey = true } }

// WithOACUsageAccounting asks the endpoint to return the call's actual cost and
// cache-token breakdown in the response usage object (OpenRouter's
// `usage: {include: true}` extension). Enable it only for providers that honor
// it (OpenRouter) so the ledger can consume the real charge instead of pricing
// from the static table. Lenient endpoints ignore the unknown request field.
func WithOACUsageAccounting() OACOption {
	return func(o *OpenAICompat) { o.usageAccounting = true }
}

// WithOACHTTPClient overrides the HTTP client used for requests.
func WithOACHTTPClient(h *http.Client) OACOption { return func(o *OpenAICompat) { o.httpClient = h } }

// WithOACTimeout overrides the per-call HTTP timeout. The composition root sets a TIGHTER cap
// on the cloud OAC rungs (Gemini/DeepSeek over OpenRouter) than the local floor, so a STALLED
// cloud call is abandoned well before the per-turn budget is spent — leaving the loop's timeout
// recovery room to retry/escalate instead of one stall ending the turn (the orchestrate
// synthesis-turn timeout root cause). A non-positive value leaves the default in place.
func WithOACTimeout(d time.Duration) OACOption {
	return func(o *OpenAICompat) {
		if d > 0 {
			o.httpClient = &http.Client{Timeout: d}
		}
	}
}

// CallTimeout reports the adapter's per-call HTTP timeout (for diagnostics + wiring tests).
func (o *OpenAICompat) CallTimeout() time.Duration { return o.httpClient.Timeout }

// WithOACMaxTokens overrides the completion-length cap (0 disables it).
func WithOACMaxTokens(n int) OACOption { return func(o *OpenAICompat) { o.maxTokens = n } }

// WithOACWindow sets a known static context window (tokens) the adapter reports
// when the live GET /models probe carries no n_ctx — the case for a hosted
// OpenAI-compatible gateway (OpenRouter advertises no window). A local llama.cpp
// endpoint leaves this 0 so its VRAM-fixed n_ctx probe stays authoritative; a
// cloud model passes its published window so a cloud-tier run still sizes
// compaction (bug corpos-no-context-window-knowledge-for-cloud-tiers).
func WithOACWindow(tokens int) OACOption { return func(o *OpenAICompat) { o.staticWindow = tokens } }

// NewOpenAICompat builds an adapter for model served at baseURL (e.g.
// "http://localhost:8081/v1").
func NewOpenAICompat(model, baseURL string, opts ...OACOption) *OpenAICompat {
	o := &OpenAICompat{
		model:      model,
		baseURL:    strings.TrimRight(baseURL, "/"),
		maxTokens:  oacDefaultMaxTokens,
		httpClient: &http.Client{Timeout: oacDefaultTimeout},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

var _ Adapter = (*OpenAICompat)(nil)

// Model returns the model id.
func (o *OpenAICompat) Model() string { return o.model }

// Available reports whether a backend endpoint is configured (and a key is
// present when the endpoint requires one).
func (o *OpenAICompat) Available() bool {
	if o.baseURL == "" {
		return false
	}
	return !o.requireKey || o.apiKey != ""
}

// ContextWindow probes the OpenAI-compatible endpoint for the model's effective
// context window. llama.cpp's GET {baseURL}/models reports it per model under
// data[].meta.n_ctx (the loaded n_ctx, not n_ctx_train); the matching model id
// is preferred, falling back to any entry that carries an n_ctx. When the probe
// yields no n_ctx (a hosted gateway like OpenRouter advertises none) it falls
// back to the configured static window (WithOACWindow), so a cloud tier still
// reports a window instead of (0,false). With neither, returns (0,false) — a
// fail-soft probe, never fatal.
func (o *OpenAICompat) ContextWindow(ctx context.Context) (int, bool) {
	if w, ok := o.probeWindow(ctx); ok {
		return w, true
	}
	if o.staticWindow > 0 {
		return o.staticWindow, true
	}
	return 0, false
}

// probeWindow performs the live GET /models n_ctx probe, returning (0,false) on
// any failure or when no entry carries an n_ctx.
func (o *OpenAICompat) probeWindow(ctx context.Context) (int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return 0, false
	}
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, false
	}
	var parsed struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return 0, false
	}
	fallback := 0
	for _, m := range parsed.Data {
		if m.Meta.NCtx <= 0 {
			continue
		}
		if m.ID == o.model {
			return m.Meta.NCtx, true
		}
		if fallback == 0 {
			fallback = m.Meta.NCtx
		}
	}
	if fallback > 0 {
		return fallback, true
	}
	return 0, false
}

// --- wire types: the OpenAI chat-completions shape ---

type oacFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oacToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oacFunctionCall `json:"function"`
}

type oacMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []oacToolCall `json:"tool_calls,omitempty"`
}

type oacToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oacToolDef struct {
	Type     string          `json:"type"`
	Function oacToolFunction `json:"function"`
}

type oacRequest struct {
	Model     string        `json:"model"`
	Messages  []oacMessage  `json:"messages"`
	Tools     []oacToolDef  `json:"tools,omitempty"`
	Usage     *oacUsageOpts `json:"usage,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

// oacUsageOpts is OpenRouter's request-side usage-accounting toggle; when present
// the response carries the call's actual cost + cache-token details.
type oacUsageOpts struct {
	Include bool `json:"include"`
}

type oacResponse struct {
	Choices []struct {
		Message      oacMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int      `json:"prompt_tokens"`
		CompletionTokens    int      `json:"completion_tokens"`
		Cost                *float64 `json:"cost"` // OpenRouter: actual USD charge (pointer → distinguish absent from 0)
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"` // prompt-cache reads (subset of prompt_tokens)
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Complete runs one chat-completions turn.
func (o *OpenAICompat) Complete(ctx context.Context, messages []ChatMessage, tools []tool.Spec) (Response, error) {
	reqBody := oacRequest{
		Model:     o.model,
		Messages:  toOACMessages(messages),
		Tools:     toOACTools(tools),
		MaxTokens: o.maxTokens,
	}
	if o.usageAccounting {
		reqBody.Usage = &oacUsageOpts{Include: true}
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return Response{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("call model: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if LooksLikeContextOverflow(resp.StatusCode, string(data)) {
			// Wrap the sentinel so the loop can compact-and-retry rather than abort.
			return Response{}, fmt.Errorf("model %q context overflow (status %d): %s: %w", o.model, resp.StatusCode, data, ErrContextOverflow)
		}
		if LooksLikeRateLimit(resp.StatusCode) {
			// Wrap the sentinel so the loop backs off / de-escalates rather than abort.
			return Response{}, fmt.Errorf("model %q rate-limited (status %d): %s: %w", o.model, resp.StatusCode, data, ErrRateLimit)
		}
		return Response{}, fmt.Errorf("model %q returned status %d: %s", o.model, resp.StatusCode, data)
	}

	var parsed oacResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("model %q returned no choices", o.model)
	}
	choice := parsed.Choices[0]
	calls, err := toToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return Response{}, err
	}
	usage := Usage{
		// prompt_tokens already includes the cached portion (OpenAI convention),
		// so InputTokens is the full prompt size and CachedInputTokens is its subset.
		InputTokens:       parsed.Usage.PromptTokens,
		OutputTokens:      parsed.Usage.CompletionTokens,
		CachedInputTokens: parsed.Usage.PromptTokensDetails.CachedTokens,
	}
	if parsed.Usage.Cost != nil {
		usage.CostUSD = *parsed.Usage.Cost
		usage.CostReported = true
	}
	return Response{
		Model:      o.model,
		Text:       choice.Message.Content,
		ToolCalls:  calls,
		Usage:      usage,
		StopReason: stopReason(choice.FinishReason),
	}, nil
}

// stopReason maps an OpenAI finish_reason to a Corp-OS stop reason.
func stopReason(finish string) string {
	if finish == "tool_calls" {
		return StopToolUse
	}
	return StopEndTurn
}

// toOACMessages converts transcript messages to the wire shape.
func toOACMessages(messages []ChatMessage) []oacMessage {
	out := make([]oacMessage, 0, len(messages))
	for _, m := range messages {
		om := oacMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, oacToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: oacFunctionCall{Name: tc.Surface, Arguments: encodeArgs(tc)},
			})
		}
		out = append(out, om)
	}
	return out
}

// encodeArgs serializes a tool call's {action, params, rationale} envelope into
// the OpenAI function-call arguments string.
func encodeArgs(tc tool.Call) string {
	b, err := json.Marshal(toolEnvelope(tc))
	if err != nil {
		return "{}"
	}
	return string(b)
}

// toOACTools converts tool specs to the wire shape.
func toOACTools(tools []tool.Spec) []oacToolDef {
	if len(tools) == 0 {
		return nil
	}
	out := make([]oacToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, oacToolDef{
			Type:     "function",
			Function: oacToolFunction{Name: t.Name, Description: t.Description, Parameters: t.InputSchema},
		})
	}
	return out
}

// toToolCalls maps wire tool calls to tool.Call, decoding the arguments envelope.
func toToolCalls(wire []oacToolCall) ([]tool.Call, error) {
	if len(wire) == 0 {
		return nil, nil
	}
	calls := make([]tool.Call, 0, len(wire))
	for _, w := range wire {
		var args struct {
			Action string `json:"action"`
			// RawMessage (not map[string]any) so the args decode SUCCEEDS even when a weak
			// model sends params as a string; the tolerant decodeParamsField unwraps it.
			Params    json.RawMessage `json:"params"`
			Rationale string          `json:"rationale"`
		}
		if w.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(w.Function.Arguments), &args); err != nil {
				// Before declaring the call malformed, try a bounded conservative salvage of
				// the dominant weak-model malformations (native-format-wrapped JSON, a
				// single-quoted Python-repr object) so a recoverable intent is executed
				// instead of burning the reprompt budget + a frontier escalation (Run-33:
				// deepseek-v3.2 leaked a single-quoted `…function_calls>` blob → parse_failure).
				salvaged, ok := salvageToolArgs(w.Function.Arguments)
				if ok {
					err = json.Unmarshal(salvaged, &args)
				}
				if !ok || err != nil {
					// A local model emitting truncated/invalid JSON args is a recoverable
					// malformed-tool-call fault, not a fatal error: wrap the sentinel so the
					// loop can re-prompt. The decode error is preserved for diagnostics.
					debugLogMalformedArgs(w.Function.Name, w.Function.Arguments, err.Error())
					return nil, fmt.Errorf("decode tool arguments for %q (%s): %w", w.Function.Name, err.Error(), ErrMalformedToolCall)
				}
			}
		}
		params, perr := decodeParamsField(args.Params)
		if perr != nil {
			// params was neither an object nor an unwrappable string — a genuine
			// malformation; keep the reprompt/escalate backstop.
			debugLogMalformedArgs(w.Function.Name, w.Function.Arguments, perr.Error())
			return nil, fmt.Errorf("decode tool params for %q (%s): %w", w.Function.Name, perr.Error(), ErrMalformedToolCall)
		}
		calls = append(calls, tool.Call{
			ID:        w.ID,
			Surface:   w.Function.Name,
			Action:    args.Action,
			Params:    params,
			Rationale: args.Rationale,
		})
	}
	return calls, nil
}
