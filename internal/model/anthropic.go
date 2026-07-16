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

// Anthropic is an Adapter for the Anthropic Messages API. It is a SEPARATE
// transport from OpenAICompat because the API shape differs (system prompt is
// top-level; tool calls are tool_use content blocks with an object input, not a
// JSON-string function-call). This mirrors the hermes-agent lesson
// (providers/base.py): a new adapter is warranted only when the API *shape*
// differs — OpenAI-compatible providers (Qwen, DeepSeek, OpenRouter) are one
// OpenAICompat adapter parameterised by config, not N adapters. The Phase-D
// direction is a declarative provider profile/registry like hermes's.
type Anthropic struct {
	model        string
	baseURL      string
	apiKey       string
	maxTokens    int
	staticWindow int
	httpClient   *http.Client
}

const (
	// 300s (raised from 60s): Opus is the timeout-fault ESCALATION target, so it
	// must not have a tighter per-call bound than the OpenAI-compat rung that
	// escalates into it (oacDefaultTimeout, 300s) — a 60s Opus ceiling guaranteed
	// the escalation also timed out on a large prompt (swap-rehearsal Run-20).
	anthropicDefaultTimeout   = 300 * time.Second
	anthropicDefaultBaseURL   = "https://api.anthropic.com"
	anthropicVersion          = "2023-06-01"
	anthropicDefaultMaxTokens = 4096
)

// AnthropicOption configures an Anthropic adapter.
type AnthropicOption func(*Anthropic)

// WithAnthropicKey sets the x-api-key credential.
func WithAnthropicKey(key string) AnthropicOption { return func(a *Anthropic) { a.apiKey = key } }

// WithAnthropicBaseURL overrides the API root (default https://api.anthropic.com).
func WithAnthropicBaseURL(u string) AnthropicOption {
	return func(a *Anthropic) { a.baseURL = strings.TrimRight(u, "/") }
}

// WithAnthropicMaxTokens sets the required max_tokens for responses.
func WithAnthropicMaxTokens(n int) AnthropicOption {
	return func(a *Anthropic) {
		if n > 0 {
			a.maxTokens = n
		}
	}
}

// WithAnthropicHTTPClient overrides the HTTP client.
func WithAnthropicHTTPClient(h *http.Client) AnthropicOption {
	return func(a *Anthropic) { a.httpClient = h }
}

// WithAnthropicWindow overrides the reported context window (tokens). Default is
// the model id's known static window (see ContextWindow); set this for an id the
// KnownContextWindow table does not recognize, or to pin a non-default window.
func WithAnthropicWindow(tokens int) AnthropicOption {
	return func(a *Anthropic) { a.staticWindow = tokens }
}

// NewAnthropic builds an adapter for the given Anthropic model id (e.g.
// "claude-haiku-4-5-20251001").
func NewAnthropic(model string, opts ...AnthropicOption) *Anthropic {
	a := &Anthropic{
		model:      model,
		baseURL:    anthropicDefaultBaseURL,
		maxTokens:  anthropicDefaultMaxTokens,
		httpClient: &http.Client{Timeout: anthropicDefaultTimeout},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

var _ Adapter = (*Anthropic)(nil)

// Model returns the model id.
func (a *Anthropic) Model() string { return a.model }

// Available reports whether an API key is configured (Anthropic requires one);
// this is the router's unavailable-tier fallback gate.
func (a *Anthropic) Available() bool { return a.apiKey != "" }

// ContextWindow reports the model's context window WITHOUT a network call: the
// Messages API has no probe endpoint, so the window is a known static value. It
// returns the configured override (WithAnthropicWindow) when set, else the model
// id's known window (KnownContextWindow — e.g. claude-opus-4-8 → 200k), else
// (0,false) for an unrecognized id. This makes the Anthropic strong rung size
// compaction instead of reporting nothing (bug
// corpos-no-context-window-knowledge-for-cloud-tiers).
func (a *Anthropic) ContextWindow(context.Context) (int, bool) {
	if a.staticWindow > 0 {
		return a.staticWindow, true
	}
	if w := KnownContextWindow(a.model); w > 0 {
		return w, true
	}
	return 0, false
}

var _ WindowProber = (*Anthropic)(nil)

// --- wire types: the Anthropic Messages shape ---

type antBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

type antTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type antRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system,omitempty"`
	Messages  []antMessage `json:"messages"`
	Tools     []antTool    `json:"tools,omitempty"`
}

type antResponse struct {
	Content    []antBlock `json:"content"`
	StopReason string     `json:"stop_reason"`
	Usage      struct {
		// Anthropic's input_tokens EXCLUDES the cache-read and cache-creation
		// tokens, which it reports separately; the full prompt size is their sum.
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// Complete runs one Messages-API turn.
func (a *Anthropic) Complete(ctx context.Context, messages []ChatMessage, tools []tool.Spec) (Response, error) {
	system, msgs := splitAnthropicMessages(messages)
	raw, err := json.Marshal(antRequest{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		System:    system,
		Messages:  msgs,
		Tools:     toAnthropicTools(tools),
	})
	if err != nil {
		return Response{}, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("call model: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Map the recoverable rejections onto the fault taxonomy so the loop absorbs
		// them (the strong rung is Anthropic, so its 400 "prompt is too long" and 429
		// rate_limit_error must be recoverable, not fatal).
		if LooksLikeContextOverflow(resp.StatusCode, string(data)) {
			return Response{}, fmt.Errorf("model %q context overflow (status %d): %s: %w", a.model, resp.StatusCode, data, ErrContextOverflow)
		}
		if LooksLikeRateLimit(resp.StatusCode) {
			return Response{}, fmt.Errorf("model %q rate-limited (status %d): %s: %w", a.model, resp.StatusCode, data, ErrRateLimit)
		}
		return Response{}, fmt.Errorf("model %q returned status %d: %s", a.model, resp.StatusCode, data)
	}

	var parsed antResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}

	var text strings.Builder
	var calls []tool.Call
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, callFromAnthropicBlock(b))
		}
	}
	stop := StopEndTurn
	if parsed.StopReason == "tool_use" {
		stop = StopToolUse
	}
	return Response{
		Model:     a.model,
		Text:      text.String(),
		ToolCalls: calls,
		Usage: Usage{
			// Normalize to the full prompt size so InputTokens has the same meaning
			// across adapters: Anthropic reports cache reads/creations separately.
			InputTokens:       parsed.Usage.InputTokens + parsed.Usage.CacheReadInputTokens + parsed.Usage.CacheCreationInputTokens,
			OutputTokens:      parsed.Usage.OutputTokens,
			CachedInputTokens: parsed.Usage.CacheReadInputTokens,
			CacheWriteTokens:  parsed.Usage.CacheCreationInputTokens,
		},
		StopReason: stop,
	}, nil
}

// splitAnthropicMessages separates system messages (Anthropic carries the system
// prompt top-level) from the user/assistant/tool turns, converting each to the
// Messages wire shape.
func splitAnthropicMessages(messages []ChatMessage) (system string, out []antMessage) {
	var sys []string
	for i := 0; i < len(messages); {
		m := messages[i]
		switch m.Role {
		case RoleSystem:
			sys = append(sys, m.Content)
			i++
		case RoleUser:
			out = append(out, antMessage{Role: "user", Content: []antBlock{{Type: "text", Text: m.Content}}})
			i++
		case RoleAssistant:
			blocks := make([]antBlock, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, antBlock{Type: "tool_use", ID: tc.ID, Name: tc.Surface, Input: toolEnvelope(tc)})
			}
			// An assistant turn with neither text nor tool calls carries no
			// content. The Anthropic Messages API rejects a message whose only
			// block is an empty text block ("text content blocks must be
			// non-empty"). A single-turn CLI never replayed that message, but in
			// a multi-turn REPL it sits in the carried history and 400s the next
			// turn. Drop it rather than emit an empty block — it has no tool_use
			// blocks, so no following tool_result is orphaned by the omission.
			if len(blocks) == 0 {
				i++
				continue
			}
			out = append(out, antMessage{Role: "assistant", Content: blocks})
			i++
		case RoleTool:
			// Coalesce a run of consecutive tool results into ONE user message:
			// the Anthropic Messages API requires every tool_result for an
			// assistant's tool_use blocks in a single following user turn (one
			// tool_result block per call), not separate consecutive user messages.
			var blocks []antBlock
			for i < len(messages) && messages[i].Role == RoleTool {
				blocks = append(blocks, antBlock{Type: "tool_result", ToolUseID: messages[i].ToolCallID, Content: messages[i].Content})
				i++
			}
			out = append(out, antMessage{Role: "user", Content: blocks})
		default:
			i++
		}
	}
	return strings.Join(sys, "\n\n"), out
}

// toAnthropicTools converts tool specs to the Messages tool shape.
func toAnthropicTools(tools []tool.Spec) []antTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]antTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, antTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// callFromAnthropicBlock maps a tool_use content block to a tool.Call, reading
// the {action, params, rationale} envelope out of the block input.
func callFromAnthropicBlock(b antBlock) tool.Call {
	c := tool.Call{ID: b.ID, Surface: b.Name}
	if s, ok := b.Input["action"].(string); ok {
		c.Action = s
	}
	if p, ok := b.Input["params"].(map[string]any); ok {
		c.Params = p
	}
	if r, ok := b.Input["rationale"].(string); ok {
		c.Rationale = r
	}
	return c
}
