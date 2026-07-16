package mcp

import (
	"strings"

	"corpos/internal/tool"
)

// Scope is an action-level tool allow-list: surface name -> allowed actions.
// A surface ABSENT from the map is fully denied. A surface present with an EMPTY
// action list is fully allowed (every action it offers). A non-empty list
// restricts the worker to exactly those actions.
//
// A profile maps to a Scope at spawn time (one entry per profile.SurfaceScope);
// keeping Scope a plain map here decouples the mcp client from the profile
// package — the composition root (main) does the trivial profile→Scope mapping.
type Scope map[string][]string

// Union returns a new Scope granting every call that sc OR other grants — the
// monotonic widening used by a profile re-scope (corpos #3097). A whole-surface
// grant (empty action list) on either side wins for that surface (it is the widest
// form); otherwise the surface's allowed actions are the de-duplicated union of
// both lists. Neither input is mutated.
func (sc Scope) Union(other Scope) Scope {
	out := make(Scope, len(sc)+len(other))
	for s, acts := range sc {
		out[s] = append([]string(nil), acts...)
	}
	for s, acts := range other {
		existing, ok := out[s]
		if !ok {
			out[s] = append([]string(nil), acts...)
			continue
		}
		if len(existing) == 0 || len(acts) == 0 {
			out[s] = nil // either side grants the whole surface → whole surface
			continue
		}
		seen := make(map[string]bool, len(existing))
		for _, a := range existing {
			seen[a] = true
		}
		for _, a := range acts {
			if !seen[a] {
				seen[a] = true
				out[s] = append(out[s], a)
			}
		}
	}
	return out
}

// Project filters a full enriched spec set down to a profile's scope. It is the
// capability projection at the heart of role-scoping (§4.2/§4.4.4): corpos
// exposes EXACTLY the projected subset, so a worker cannot invoke a surface or
// action its profile did not declare — the projected tools ARE the allow-list.
//
// For an action-scoped surface it narrows BOTH the action enum (the hard
// decode-time steer) and the per-action description lines to the allowed set.
//
// Fail-closed on degraded specs: if a scoped surface fell back to a thin
// (enum-less) spec because the substrate was unreachable at spec-build time, an
// action-level scope cannot be honored, so the surface is DROPPED rather than
// exposed whole. A whole-surface scope (empty action list) keeps the thin spec —
// the profile asked for the entire surface, which the thin spec already is.
func Project(specs []tool.Spec, scope Scope) []tool.Spec {
	out := make([]tool.Spec, 0, len(scope))
	for _, s := range specs {
		allowed, ok := scope[s.Name]
		if !ok {
			continue // surface not in scope → fully denied
		}
		if len(allowed) == 0 {
			out = append(out, s) // whole-surface scope
			continue
		}
		if p, ok := projectActions(s, allowed); ok {
			out = append(out, p)
		}
	}
	return out
}

// projectActions narrows one enriched spec to the allowed action set, rebuilding
// the enum and description from the intersection. The bool is false (drop the
// surface) when the spec carries no enum (thin fallback — cannot honor an
// action-level scope) or when none of the requested actions exist on the surface.
func projectActions(s tool.Spec, allowed []string) (tool.Spec, bool) {
	enum, ok := enumOf(s)
	if !ok {
		return tool.Spec{}, false // thin fallback spec → fail closed on action scope
	}
	allow := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allow[a] = true
	}
	kept := make([]string, 0, len(enum))
	for _, a := range enum { // preserve the enum's (sorted) order
		if allow[a] {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return tool.Spec{}, false // requested actions don't exist on this surface
	}
	return tool.Spec{
		Name:        s.Name,
		Description: filterActionLines(s.Description, allow),
		InputSchema: enrichedSchema(kept),
	}, true
}

// enumOf returns the action-property enum of a spec as an ordered string slice,
// and false when the spec has no enum (a thin fallback spec).
func enumOf(s tool.Spec) ([]string, bool) {
	props, ok := s.InputSchema["properties"].(map[string]any)
	if !ok {
		return nil, false
	}
	action, ok := props["action"].(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok := action["enum"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if str, ok := v.(string); ok {
			out = append(out, str)
		}
	}
	return out, true
}

// filterActionLines drops the per-action description lines ("- name(params) — …")
// whose action is not allowed, keeping the purpose header, the action-list intro,
// and the trailing canonical-docs pointer untouched. The action name is the token
// up to the first "(" or space — exactly the buildDescription line shape.
func filterActionLines(desc string, allow map[string]bool) string {
	lines := strings.Split(desc, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.HasPrefix(ln, "- ") {
			if !allow[actionOfLine(ln)] {
				continue
			}
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// actionOfLine extracts the action name from a "- name(params) — purpose" line.
func actionOfLine(line string) string {
	s := strings.TrimPrefix(line, "- ")
	if i := strings.IndexAny(s, "( "); i >= 0 {
		return s[:i]
	}
	return s
}
