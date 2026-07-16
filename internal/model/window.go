package model

import (
	"context"
	"strings"
)

// WindowProber is implemented by adapters that can report their model's context
// window (token capacity). The composition root uses it to size context
// compaction to the window by default (see suggestion
// default-enable-context-compaction-sized-to-window). The report may come from a
// live endpoint probe (llama.cpp's /models n_ctx) or a known static value (a
// cloud model whose endpoint does not advertise its window). An adapter that
// cannot determine its window does not implement it (or returns ok=false).
type WindowProber interface {
	// ContextWindow returns the model's effective context window in tokens, and
	// ok=false when it cannot be determined (the caller then keeps its default).
	ContextWindow(ctx context.Context) (tokens int, ok bool)
}

// KnownContextWindow returns a best-effort STATIC context window (tokens) for a
// known model id, or 0 when the id is unrecognized. It exists because the cloud
// tiers do not advertise their window the way llama.cpp does: OpenRouter's
// GET /models carries no n_ctx, and the Anthropic Messages API has no probe at
// all. Rather than report nothing — which silently disables compaction for a
// cloud-tier run and lets the orchestrator's context grow until the model call
// times out — each adapter falls back to this table so every tier has a window.
//
// The values are the providers' published windows, matched loosely by id
// substring so a dated/suffixed id (e.g. "claude-opus-4-8", "google/gemini-3.1-
// flash-lite") still resolves. They need not be exact: the compaction budget
// reserves headroom below the window and is itself capped for latency, so the
// only hard requirement is not over-reporting a model's real window (which would
// risk a true overflow). When in doubt the caller treats 0 as "unknown".
func KnownContextWindow(modelID string) int {
	id := strings.ToLower(modelID)
	switch {
	case strings.Contains(id, "gemini"):
		// Gemini 2.x/3.x flash/pro families carry a ~1M-token window.
		return 1_000_000
	case strings.Contains(id, "deepseek"):
		// DeepSeek V3.x: 163,840-token window.
		return 163_840
	case isAnthropicModelID(id):
		if strings.Contains(id, "1m") {
			// The explicit 1M-context Claude variant (id carries a "[1m]"/"-1m"
			// marker). Corp-OS's canonical strong rung is plain "claude-opus-4-8"
			// (200k); this branch only fires for an operator-selected 1M id.
			return 1_000_000
		}
		// Standard window shared across the current Claude 4.x families.
		return 200_000
	default:
		return 0
	}
}

// isAnthropicModelID reports whether a (lowercased) model id names a Claude model.
func isAnthropicModelID(id string) bool {
	return strings.Contains(id, "claude") || strings.Contains(id, "opus") ||
		strings.Contains(id, "sonnet") || strings.Contains(id, "haiku")
}
