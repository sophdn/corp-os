// Package hooks is the Corp-OS lifecycle hook surface — eight hooks modelled on
// Claude Code's own surface so cross-harness reflexes (parse_context, memory
// load, arc-close filing) port cleanly and become LOOP-fired rather than
// harness-fired (the trigger-migration the design doc calls for). Hooks are
// non-blocking: a panicking hook is recovered and recorded, never aborting the
// turn. Ported from bridge-harness hooks.py.
package hooks

import (
	"fmt"

	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

// Kind is a hook lifecycle point.
type Kind string

// The eight hook kinds, in the order they fire across a session's lifecycle.
// PreUserPrompt and PostTurn fire on every turn.
const (
	SessionStart  Kind = "session_start"
	PreUserPrompt Kind = "pre_user_prompt"
	PreTurn       Kind = "pre_turn"
	PreToolUse    Kind = "pre_tool_use"
	PostToolUse   Kind = "post_tool_use"
	PostTurn      Kind = "post_turn"
	Stop          Kind = "stop"
	SessionEnd    Kind = "session_end"
)

// Order is the declared hook surface, in lifecycle order.
var Order = []Kind{SessionStart, PreUserPrompt, PreTurn, PreToolUse, PostToolUse, PostTurn, Stop, SessionEnd}

// HookError records a hook that panicked during firing.
type HookError struct {
	Hook string
	Err  string
}

// Context is the shared, mutable state passed to every hook. Hooks may rewrite
// the transcript, queue system-prompt additions, or stash metadata.
type Context struct {
	Kind                  Kind
	SessionID             string
	Project               string
	TurnIndex             int
	Transcript            *[]model.ChatMessage // hooks may mutate in place
	SystemPromptAdditions []string
	Metadata              map[string]any
	UserPrompt            string
	ToolCall              *tool.Call
	ToolResult            *tool.Result
	Model                 string
	// Profile is the job-profile this worker runs under, when one is active. It
	// lets profile-aware hooks (skill injection, parse_context pruning) read the
	// worker's envelope without re-plumbing it. Nil when the loop runs unprojected.
	Profile *profile.JobProfile
	// DenyToolCall, when set by a pre_tool_use hook, vetoes the pending tool call:
	// the loop skips the dispatch and feeds DenyReason back to the model as the
	// tool result. This is the seam the risk gate uses (orthogonal to tool scope).
	DenyToolCall bool
	DenyReason   string
	// DenyNonEscalatable marks a veto whose cause no stronger MODEL can lift — a
	// protect-path denial, a policy/config refusal. The loop classifies such a denial
	// ClassUsage (not ClassTool) so it does NOT trip the repeated_tool_error escalation,
	// mirroring the sysorgan/fsorgan usage-error precedent (bug 1095). A plain
	// DenyToolCall (e.g. the risk-gate veto) stays ClassTool — unchanged.
	DenyNonEscalatable bool
	// RequestEscalation, when set by a hook during a turn, feeds the router's
	// explicit_handoff escalation trigger — the "I need the strong tier" signal.
	// Dormant until some hook drives it (e.g. a sentinel-detecting post_tool_use).
	RequestEscalation bool
	// EscalationConfidence, when set non-nil by a hook, feeds the router's
	// low_confidence trigger (the model's confidence in its action, in [0,1]). Nil
	// (unmeasured) never fires low_confidence — dormant until a scorer hook sets it.
	EscalationConfidence *float64
	Errors               []HookError
}

// Func is a hook callback; it receives the shared context and may mutate it.
type Func func(*Context)

type namedHook struct {
	name string
	fn   Func
}

// Surface is a registry of hooks keyed by kind, fired in registration order.
type Surface struct {
	hooks map[Kind][]namedHook
}

// NewSurface returns an empty hook surface.
func NewSurface() *Surface {
	return &Surface{hooks: map[Kind][]namedHook{}}
}

func validKind(k Kind) bool {
	for _, o := range Order {
		if o == k {
			return true
		}
	}
	return false
}

// Register adds a hook for a kind; it errors on an unknown kind. Multiple hooks
// per kind fire in registration order.
func (s *Surface) Register(kind Kind, name string, fn Func) error {
	if !validKind(kind) {
		return fmt.Errorf("unknown hook kind %q", kind)
	}
	s.hooks[kind] = append(s.hooks[kind], namedHook{name: name, fn: fn})
	return nil
}

// Count returns how many hooks are registered for a kind.
func (s *Surface) Count(kind Kind) int { return len(s.hooks[kind]) }

// DeclaredKinds returns the full declared surface (enumerable without a session).
func (s *Surface) DeclaredKinds() []Kind { return Order }

// Fire runs every hook for a kind in registration order and returns the mutated
// context. Non-blocking: a hook that panics is recovered and appended to
// ctx.Errors; firing continues with the next hook.
func (s *Surface) Fire(kind Kind, ctx *Context) *Context {
	ctx.Kind = kind
	for _, h := range s.hooks[kind] {
		s.fireOne(h, ctx)
	}
	return ctx
}

func (s *Surface) fireOne(h namedHook, ctx *Context) {
	defer func() {
		if r := recover(); r != nil {
			ctx.Errors = append(ctx.Errors, HookError{Hook: h.name, Err: fmt.Sprintf("%v", r)})
		}
	}()
	h.fn(ctx)
}
