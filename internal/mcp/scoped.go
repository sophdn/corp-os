package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"corpos/internal/tool"
)

// Allows reports whether a Scope grants a call to surface.action. A surface
// ABSENT from the scope is fully denied; a surface present with an EMPTY action
// list is fully allowed (every action); a non-empty list grants exactly its
// actions. This is the dispatch-time counterpart to Project (which narrows the
// SPECS the model sees): together they make a profile a hard capability boundary
// rather than an advisory spec filter.
func (sc Scope) Allows(surface, action string) bool {
	allowed, ok := sc[surface]
	if !ok {
		return false
	}
	if len(allowed) == 0 {
		return true // whole-surface scope
	}
	for _, a := range allowed {
		if a == action {
			return true
		}
	}
	return false
}

// surfaces returns the scope's granted surface names, sorted, for an actionable
// denial message ("the tools you may use are: …").
func (sc Scope) surfaces() []string {
	out := make([]string, 0, len(sc))
	for s := range sc {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// RescopeRung is one widening step the dispatch boundary may take when a call is
// denied — a named profile whose scope is unioned into the current one. It is the
// profile analog of a model-ladder rung (corpos #3097).
type RescopeRung struct {
	Name  string
	Scope Scope
}

// ScopedOption configures a *Scoped at construction.
type ScopedOption func(*Scoped)

// WithRescopeLadder arms the profile-rescope ladder (corpos #3097): name labels the
// starting profile; ladder is the ordered set of widening rungs; budget bounds how
// many widenings the boundary may take in this run (no-op at <= 0). On a denied call
// the boundary widens (union) to the first rung that grants it, within budget, and
// transparently re-dispatches — so a worker mis-classified into too-narrow a profile
// recovers instead of failing, the profile analog of model-tier escalation.
func WithRescopeLadder(name string, ladder []RescopeRung, budget int) ScopedOption {
	return func(s *Scoped) { s.name, s.ladder, s.budget = name, ladder, budget }
}

// WithRescopeLog registers a callback fired once per widening with the from/to
// profile names and the surface/action that triggered it — the logged half of
// "logged + bounded".
func WithRescopeLog(fn func(from, to, surface, action string)) ScopedOption {
	return func(s *Scoped) { s.onRescope = fn }
}

// Scoped wraps a tool.Provider and ENFORCES a Scope at the dispatch boundary: a
// call whose surface (or scoped action) the active profile did not grant is
// denied here — before it reaches the inner provider — and folded back to the
// model as a tool_error it can adapt to. This closes the gap bug 1044 names: the
// aggregator routes by surface name with no profile check, so projecting the spec
// set (mcp.Project) only HIDES un-granted surfaces; a model that emits a call for
// one anyway (from training or prior context) would otherwise have it dispatched.
// A profile is a capability boundary, and this is where the boundary is made hard.
//
// Enforcement lives in a decorator, not by unmounting surfaces from the
// aggregator, because other workers/rungs on the same aggregator still need the
// full surface set (the constraint on task enforce-profile-scope-at-dispatch).
// The denial is a tool_error, never a fatal abort, so the loop keeps the turn.
// When a rescope ladder is armed (corpos #3097) the boundary can widen itself
// rather than only deny — see Dispatch.
type Scoped struct {
	inner tool.Provider

	mu      sync.Mutex // guards scope/budget/name mutated by a rescope
	scope   Scope
	name    string        // current profile name (for rescope logging)
	ladder  []RescopeRung // ordered widening rungs (corpos #3097)
	budget  int           // remaining widenings
	applied int           // rungs consumed so far (ladder is traversed in order)

	onRescope func(from, to, surface, action string)
}

// ensure *Scoped satisfies the provider seam at compile time.
var _ tool.Provider = (*Scoped)(nil)

// NewScoped wraps inner so every dispatch is checked against scope first. A nil
// scope denies everything (a profile that granted no tools cannot act); callers
// that want the unscoped provider simply pass it through without wrapping. Options
// arm the rescope ladder; with none, the boundary is fixed (the prior behavior).
func NewScoped(inner tool.Provider, scope Scope, opts ...ScopedOption) *Scoped {
	s := &Scoped{inner: inner, scope: scope}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Dispatch enforces the scope, then delegates. A denied call never reaches the
// inner provider — so a scoped run cannot mutate (or even read) an un-granted
// surface. When a rescope ladder is armed, a denied call first tries to widen the
// boundary (corpos #3097) and, on success, is transparently re-dispatched; only an
// un-widenable denial is folded back to the model as a tool_error naming what was
// denied and what is in scope, so the model can still recover by choosing an
// offered tool.
func (s *Scoped) Dispatch(ctx context.Context, call tool.Call) tool.Result {
	if s.allows(call) {
		return s.inner.Dispatch(ctx, call)
	}
	if s.tryRescope(call) {
		return s.inner.Dispatch(ctx, call) // transparent retry on the widened boundary
	}
	return tool.Fail(call, tool.ClassTool, s.denyReason(call), 0)
}

// allows reports whether the current (possibly widened) scope grants the call.
func (s *Scoped) allows(call tool.Call) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope.Allows(call.Surface, call.Action)
}

// tryRescope widens the boundary to the first not-yet-consumed ladder rung that
// grants call, within budget, unioning that rung's scope into the current one
// (monotonic — a widening never drops a grant). It reports whether it widened.
// Traversal is in declared order and each success consumes the rungs up to and
// including the chosen one, so the ladder climbs strictly upward and is bounded by
// budget. Concurrency-safe: the whole check-and-widen is under the mutex.
func (s *Scoped) tryRescope(call tool.Call) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.budget <= 0 {
		return false
	}
	for i := s.applied; i < len(s.ladder); i++ {
		rung := s.ladder[i]
		if !rung.Scope.Allows(call.Surface, call.Action) {
			continue
		}
		from := s.name
		s.scope = s.scope.Union(rung.Scope)
		s.name = rung.Name
		s.applied = i + 1
		s.budget--
		if s.onRescope != nil {
			s.onRescope(from, rung.Name, call.Surface, call.Action)
		}
		return true
	}
	return false
}

// denyReason builds the actionable tool_error for a denied call: it distinguishes
// a surface that is entirely out of scope from a surface that is granted but only
// for other actions, and always lists the surfaces the run may use.
func (s *Scoped) denyReason(call tool.Call) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	have := strings.Join(s.scope.surfaces(), ", ")
	if have == "" {
		have = "(none)"
	}
	if _, surfaceGranted := s.scope[call.Surface]; surfaceGranted {
		return fmt.Sprintf("action %q on surface %q is not in this run's profile scope — denied at the dispatch boundary; the surfaces you may use are: %s",
			call.Action, call.Surface, have)
	}
	return fmt.Sprintf("surface %q is not in this run's profile scope — denied at the dispatch boundary; the surfaces you may use are: %s",
		call.Surface, have)
}
