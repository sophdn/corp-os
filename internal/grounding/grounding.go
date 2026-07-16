// Package grounding closes corpos's RAG implicit-feedback loop (chain
// toolkit-decomposition T5, piece 3). The toolkit records a grounding_events row
// server-side for every search corpos runs over MCP; this package detects, from
// corpos's OWN session transcript, which of those hits the agent actually used —
// followed / cited / mentioned — and records the click signal back via
// knowledge.record_query_interaction (resolved to the grounding_event by the
// search call's span_id). It ports what the toolkit's grounding-events-processor
// did for Claude Code from the session JSONL.
//
// Scope (T5 piece 3): the followed / cited / mentioned tiers, which corpos's
// persisted tool_calls + transcript fully support. The resolved-from tier
// (terminal work-resolution → search linkage) is deferred — corpos has no
// terminal-rationale hook yet.
package grounding

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/session"
	"corpos/internal/tool"
)

const defaultTimeout = 15 * time.Second

// searchActions are the knowledge searches whose hits get grounded.
var searchActions = map[string]bool{
	"vault_search":     true,
	"knowledge_search": true,
	"kiwix_search":     true,
}

// readActions are the actions that count as "following" a hit (reading its body
// at the exact source_ref).
var readActions = map[string]bool{
	"vault_read":  true,
	"read":        true,
	"kiwix_fetch": true,
}

// Dispatcher is the subset of the MCP client this package needs.
type Dispatcher interface {
	Dispatch(ctx context.Context, call tool.Call) tool.Result
}

// SessionReader is the subset of *session.Store the recorder reads.
type SessionReader interface {
	ToolCalls() ([]session.ToolCallRow, error)
	Messages() ([]session.Message, error)
}

// Recorder detects + records click signals for one session.
type Recorder struct {
	disp      Dispatcher
	store     SessionReader
	sessionID string
	timeout   time.Duration
}

// Option configures a Recorder.
type Option func(*Recorder)

// WithTimeout overrides the per-emit timeout.
func WithTimeout(d time.Duration) Option {
	return func(r *Recorder) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// New builds a Recorder over a Dispatcher + the session store.
func New(disp Dispatcher, store SessionReader, sessionID string, opts ...Option) *Recorder {
	r := &Recorder{disp: disp, store: store, sessionID: sessionID, timeout: defaultTimeout}
	for _, o := range opts {
		o(r)
	}
	return r
}

// SessionEndHook returns the hook that scans the finished session and records its
// click signals. Fires at SessionEnd, when every turn's tool_calls are persisted
// and the store is still open. Best-effort throughout — a read/emit failure
// drops that signal, never blocks shutdown.
func (r *Recorder) SessionEndHook() hooks.Func {
	return func(_ *hooks.Context) {
		r.scanAndEmit(context.Background())
	}
}

// scanAndEmit walks the session's searches and emits one interaction per used hit.
func (r *Recorder) scanAndEmit(ctx context.Context) {
	tcs, err := r.store.ToolCalls()
	if err != nil {
		return
	}
	msgs, err := r.store.Messages()
	if err != nil {
		return
	}
	for _, tc := range tcs {
		if !tc.OK || tc.SpanID == "" || !searchActions[tc.Action] {
			continue
		}
		for i, ref := range extractSourceRefs(tc.ResultJSON) {
			kind := detectClick(ref, tc.TurnIndex, tcs, msgs)
			if kind == "" {
				continue
			}
			r.emit(ctx, tc.SpanID, ref, kind, i+1)
		}
	}
}

// emit records one click signal via knowledge.record_query_interaction.
func (r *Recorder) emit(ctx context.Context, spanID, sourceRef, clickKind string, position int) {
	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	r.disp.Dispatch(callCtx, tool.Call{
		Surface: "knowledge",
		Action:  "record_query_interaction",
		Params: map[string]any{
			"span_id":    spanID,
			"source_ref": sourceRef,
			"click_kind": clickKind,
			"session_id": r.sessionID,
			"position":   position,
		},
	})
}

// detectClick returns the strongest click signal for ref appearing AFTER the
// search's turn: followed (a later read of the ref) > cited (a markdown link or
// path:line reference) > mentioned (the ref string in later assistant text).
// Returns "" when ref was never used.
func detectClick(ref string, searchTurn int, tcs []session.ToolCallRow, msgs []session.Message) string {
	if ref == "" {
		return ""
	}
	for _, tc := range tcs {
		if tc.TurnIndex > searchTurn && readActions[tc.Action] && strings.Contains(tc.ParamsJSON, ref) {
			return "followed"
		}
	}
	cited := false
	mentioned := false
	for _, m := range msgs {
		if m.TurnIndex <= searchTurn || m.Role != string(model.RoleAssistant) {
			continue
		}
		if strings.Contains(m.Content, "]("+ref) || strings.Contains(m.Content, ref+":") {
			cited = true
		} else if strings.Contains(m.Content, ref) {
			mentioned = true
		}
	}
	switch {
	case cited:
		return "cited"
	case mentioned:
		return "mentioned"
	default:
		return ""
	}
}

// extractSourceRefs pulls the hit source refs from a search result body, in
// result order, deduped. It looks at the common result arrays ("results",
// "hits") and reads each element's "path", "source_ref", or "url" — covering
// vault_search (results[].path), knowledge_search (results[].source_ref/path),
// and kiwix_search (hits[].path/url).
func extractSourceRefs(resultJSON string) []string {
	if resultJSON == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &obj); err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, key := range []string{"results", "hits"} {
		arr, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			ref := firstString(em, "path", "source_ref", "url")
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

// firstString returns the first non-empty string value among keys.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
