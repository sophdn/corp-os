// Package model defines the Corp-OS model layer: the Adapter interface a model
// provider implements, the value types that cross it, and concrete adapters
// (Echo for tests, OpenAICompat for local Qwen / DeepSeek; Anthropic later). An
// adapter turns a transcript plus the offered tools into one Response; the
// router (a later increment) selects among adapters per turn. Tool requests use
// the shared tool.Call type so the loop dispatches them with no translation.
package model

import (
	"context"

	"corpos/internal/tool"
)

// Transcript roles.
const (
	// RoleSystem is the system-prompt role.
	RoleSystem = "system"
	// RoleUser is a user message.
	RoleUser = "user"
	// RoleAssistant is a model message (text and/or tool calls).
	RoleAssistant = "assistant"
	// RoleTool is a tool-result message.
	RoleTool = "tool"
)

// Stop reasons a turn can end with.
const (
	// StopEndTurn means the turn is final (no further tool calls).
	StopEndTurn = "end_turn"
	// StopToolUse means the model is awaiting tool results.
	StopToolUse = "tool_use"
)

// ChatMessage is one transcript message.
type ChatMessage struct {
	// Role is one of the Role* constants.
	Role string
	// Content is the message text (a tool result's payload for RoleTool).
	Content string
	// ToolCallID, for a RoleTool message, is the id of the call it answers.
	ToolCallID string
	// Name, for a RoleTool message, is the tool name (surface) that produced it.
	Name string
	// ToolCalls, for a RoleAssistant message, are the calls it requested.
	ToolCalls []tool.Call
}

// Usage is the token + cost usage for one model call. InputTokens is the FULL
// prompt token count; CachedInputTokens (prompt-cache reads) and CacheWriteTokens
// (Anthropic cache creation) are subsets of it, broken out so the ledger can bill
// the re-sent cached prefix at the cheaper cache rate instead of the fresh rate.
// When CostReported is true the provider returned an authoritative dollar cost
// (CostUSD) for the call, which the ledger uses as the source of truth.
type Usage struct {
	// InputTokens is the full prompt token count (incl. cached + cache-write).
	InputTokens int
	// OutputTokens is the completion token count.
	OutputTokens int
	// CachedInputTokens is the prompt-cache-read portion of InputTokens.
	CachedInputTokens int
	// CacheWriteTokens is the cache-creation portion of InputTokens (Anthropic).
	CacheWriteTokens int
	// CostUSD is the provider-reported cost for this call (valid iff CostReported).
	CostUSD float64
	// CostReported is true when the provider returned an authoritative cost.
	CostReported bool
}

// Response is one model turn's output.
type Response struct {
	// Model is the identifier that produced the response.
	Model string
	// Text is the assistant prose (may be empty on a pure tool-call turn).
	Text string
	// ToolCalls are the requested invocations (empty when the turn is final).
	ToolCalls []tool.Call
	// Usage is the token usage for cost accounting.
	Usage Usage
	// StopReason is StopEndTurn or StopToolUse.
	StopReason string
}

// Adapter drives one model identifier, turning a transcript (plus the offered
// tools) into one Response.
type Adapter interface {
	// Model returns the model identifier this adapter drives.
	Model() string
	// Available reports whether the adapter can serve a turn now (the router's
	// cold-start / unavailable fallback gate).
	Available() bool
	// Complete runs one turn over the transcript, optionally offering tools.
	Complete(ctx context.Context, messages []ChatMessage, tools []tool.Spec) (Response, error)
}
