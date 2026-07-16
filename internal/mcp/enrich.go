package mcp

import (
	"context"
	"sort"
	"strings"

	"corpos/internal/tool"
)

// introspectAction is toolkit-server's dispatch-level enumeration action: every
// surface answers {action:"__actions__"} with {"actions":[…]} — the real,
// registered action names, no error envelope (see toolkit-server dispatch.go
// IntrospectAction). Sourcing names from the live substrate is what keeps the
// specs from drifting away from the canonical surface — the bug's constraint.
const introspectAction = "__actions__"

// describeSurface / describeAction reach the substrate's action-docs corpus:
// admin.action_describe(surface, action) returns the per-action params (name +
// required), which we fold into a compact signature so the model never guesses
// param keys (the confirmed `path` vs `file_path` failure).
const (
	describeSurface = "admin"
	describeAction  = "action_describe"
)

// EnrichedSpecs builds one tool.Spec per abSurface, sourcing the REAL action
// names and a compact per-action param signature from the live substrate so the
// model lands valid calls without guessing action names or param keys. The
// names go into both the human-readable description AND a hard enum on the
// `action` property — the strongest steer for a weak local model.
//
// It is fail-soft at every layer: a surface whose enumeration fails falls back
// to the thin static spec (thinSpec — the envelope schema with no per-action
// detail); an action whose describe call fails contributes a name-only signature.
// So an unreachable substrate degrades to the thin static catalog, never to an
// empty one.
//
// src is the same tool.Provider the loop dispatches through (the mcp.Client),
// reused here so spec-building and tool-dispatch share one transport. Taking the
// Provider seam (not the concrete Client) keeps this sans-IO testable.
func EnrichedSpecs(ctx context.Context, src tool.Provider) []tool.Spec {
	specs := make([]tool.Spec, 0, len(abSurfaces))
	for _, s := range abSurfaces {
		if spec, ok := enrichSurface(ctx, src, s.name, s.description); ok {
			specs = append(specs, spec)
			continue
		}
		// Fallback: the thin static spec for this surface (the single thin-spec path).
		specs = append(specs, thinSpec(s.name, s.description))
	}
	return specs
}

// LazyEnrichedSpecs builds one tool.Spec per surface carrying ONLY the surface
// envelope (corpos #3100): the action ENUM — kept, because profile projection and
// dispatch scoping both key off it — plus a one-line purpose and a name-only list of
// actions, but NOT each action's param signature or purpose clause. The per-action
// param docs are fetched on demand by the model via admin.action_describe, the deepest
// cut to the fixed tool-spec overhead. It is also cheaper to BUILD than EnrichedSpecs:
// one __actions__ enumeration per surface, no per-action describe round-trips.
//
// Fail-soft identically to EnrichedSpecs: a surface that can't be enumerated falls
// back to the thin static spec, so an unreachable substrate degrades gracefully.
func LazyEnrichedSpecs(ctx context.Context, src tool.Provider) []tool.Spec {
	specs := make([]tool.Spec, 0, len(abSurfaces))
	for _, s := range abSurfaces {
		if spec, ok := lazySurface(ctx, src, s.name, s.description); ok {
			specs = append(specs, spec)
			continue
		}
		specs = append(specs, thinSpec(s.name, s.description))
	}
	return specs
}

// lazySurface enumerates one surface's action NAMES (no per-action describe calls)
// and builds a lazy envelope spec. The bool is false when enumeration fails or yields
// no actions, signalling the caller to fall back to the thin spec.
func lazySurface(ctx context.Context, src tool.Provider, surface, purpose string) (tool.Spec, bool) {
	res := src.Dispatch(ctx, tool.Call{Surface: surface, Action: introspectAction})
	if !res.OK {
		return tool.Spec{}, false
	}
	actions := actionsFrom(res.Value)
	if len(actions) == 0 {
		return tool.Spec{}, false
	}
	sort.Strings(actions)
	return LazyEnvelopeSpec(surface, purpose, actions), true
}

// LazyEnvelopeSpec assembles a surface spec carrying the full action enum but only a
// name-only action list in its description (no param signatures). It reuses the same
// enrichedSchema enum as EnvelopeSpec, so Project scopes a lazy spec identically; the
// name-only "- action" lines let Project's filterActionLines narrow the description to
// the granted actions exactly as it does for a full enriched spec.
func LazyEnvelopeSpec(surface, purpose string, actionNames []string) tool.Spec {
	return tool.Spec{
		Name:        surface,
		Description: buildLazyDescription(purpose, actionNames),
		InputSchema: enrichedSchema(actionNames),
	}
}

// buildLazyDescription renders the surface purpose, one name-only "- action" line per
// action (so Project can narrow them under scoping), and a pointer to the canonical
// per-action docs. It deliberately omits param signatures and purpose clauses — that
// detail is fetched on demand via admin.action_describe (the #3100 lazy cut).
func buildLazyDescription(purpose string, actionNames []string) string {
	var b strings.Builder
	b.WriteString(purpose)
	b.WriteString("\n\nActions (params NOT inlined — call admin.action_describe(surface, action) for each one's params, aliases, and examples before using it):")
	for _, a := range actionNames {
		b.WriteString("\n- ")
		b.WriteString(a)
	}
	b.WriteString("\n\nUse these exact action names. admin.action_describe(surface, action) on the substrate is the canonical per-action doc.")
	return b.String()
}

// enrichSurface enumerates one surface's actions and fetches each action's param
// signature, returning a rich spec. The bool is false when enumeration fails (or
// yields no actions), signalling the caller to fall back to the thin spec.
func enrichSurface(ctx context.Context, src tool.Provider, surface, purpose string) (tool.Spec, bool) {
	res := src.Dispatch(ctx, tool.Call{Surface: surface, Action: introspectAction})
	if !res.OK {
		return tool.Spec{}, false
	}
	actions := actionsFrom(res.Value)
	if len(actions) == 0 {
		return tool.Spec{}, false
	}
	sort.Strings(actions)

	entries := make([]string, 0, len(actions))
	for _, a := range actions {
		dr := src.Dispatch(ctx, tool.Call{
			Surface: describeSurface,
			Action:  describeAction,
			Params:  map[string]any{"surface": surface, "action": a},
		})
		if !dr.OK {
			entries = append(entries, a) // name-only fail-soft
			continue
		}
		entry := signature(a, paramsFrom(dr.Value))
		// A compact purpose clause lets a weak/cheap model pick the RIGHT action
		// among valid ones (list vs search), not just a valid name. Fail-soft:
		// no purpose -> signature only.
		if p := shortPurpose(purposeFrom(dr.Value)); p != "" {
			entry += " — " + p
		}
		entries = append(entries, entry)
	}

	return EnvelopeSpec(surface, purpose, actions, entries), true
}

// EnvelopeSpec assembles a surface tool.Spec from a purpose, the surface's action
// names, and one "name(params) — purpose" entry line per action. It is the single
// constructor for the enum-on-`action` schema shape that Project depends on, so a
// surface whose actions are STATIC (the web server — no live __actions__ endpoint
// to enrich from) produces a spec Project narrows exactly like an enriched toolkit
// surface. actionNames are the bare names that populate the enum (and must be the
// tokens projectActions matches); entries are the description lines (see
// buildDescription). Callers pass actionNames sorted so the enum order is stable.
func EnvelopeSpec(surface, purpose string, actionNames, entries []string) tool.Spec {
	return tool.Spec{
		Name:        surface,
		Description: buildDescription(purpose, entries),
		InputSchema: enrichedSchema(actionNames),
	}
}

// buildDescription mirrors the substrate's meta-tool description shape: a purpose
// sentence, then one line per action — "name(params) — purpose" — then a pointer
// to the canonical per-action docs. One short line per action keeps the catalog
// scannable for the model without bloating the prompt.
func buildDescription(purpose string, entries []string) string {
	var b strings.Builder
	b.WriteString(purpose)
	b.WriteString("\n\nActions — name(params) — purpose; ? marks an optional param:")
	for _, e := range entries {
		b.WriteString("\n- ")
		b.WriteString(e)
	}
	b.WriteString("\n\nUse these exact action names and param keys. For full param docs, aliases, and examples, admin.action_describe(surface, action) on the substrate is canonical.")
	return b.String()
}

// signature renders one action as name(p1, p2?) — required params bare, optional
// params suffixed with `?`. Long param lists are truncated with `…` so a single
// surface's description stays bounded.
func signature(action string, params []specParam) string {
	if len(params) == 0 {
		return action + "()"
	}
	const maxParams = 8
	parts := make([]string, 0, len(params))
	for i, p := range params {
		if i == maxParams {
			parts = append(parts, "…")
			break
		}
		name := p.name
		if !p.required {
			name += "?"
		}
		parts = append(parts, name)
	}
	return action + "(" + strings.Join(parts, ", ") + ")"
}

// enrichedSchema is envelopeSchema with the action property constrained to the
// real action names via a JSON-Schema enum — a hard, decode-time steer that a
// grammar-constrained local model honors directly.
func enrichedSchema(actions []string) map[string]any {
	enum := make([]any, len(actions))
	for i, a := range actions {
		enum[i] = a
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "the action to dispatch on this surface",
				"enum":        enum,
			},
			"params":    map[string]any{"type": "object", "description": "action parameters (keys are listed in the action signature in this tool's description)"},
			"rationale": map[string]any{"type": "string", "description": "required for write actions"},
		},
		"required": []any{"action"},
	}
}

// maxPurposeLen bounds one action's purpose clause so a surface description
// stays compact even when the surface has many actions.
const maxPurposeLen = 80

// shortPurpose reduces an action's purpose to a compact leading clause: the text
// up to the first sentence/clause boundary, hard-capped at maxPurposeLen. The
// gist is what disambiguates intent (e.g. "Search chains by …" vs "Return a
// chain's summary: …"); the rest is detail the model can fetch via
// action_describe. Empty in -> empty out (the entry falls back to name+params).
func shortPurpose(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// Keep only the leading clause — cut at the earliest of these boundaries.
	// Cutting only shortens, so applying each in turn yields the earliest cut.
	for _, sep := range []string{". ", " — ", "; "} {
		if i := strings.Index(p, sep); i > 0 {
			p = p[:i]
		}
	}
	p = strings.TrimRight(p, ".")
	if len(p) > maxPurposeLen {
		p = strings.TrimSpace(p[:maxPurposeLen]) + "…"
	}
	return p
}

// purposeFrom extracts the top-level "purpose" string from an action_describe
// response, tolerating any non-conforming shape by returning "".
func purposeFrom(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	p, _ := m["purpose"].(string)
	return p
}

// specParam is the minimal slice of an action-doc Param the spec builder needs.
type specParam struct {
	name     string
	required bool
}

// actionsFrom extracts the string action names from a __actions__ response
// ({"actions": ["a","b"]}), tolerating any non-conforming shape by returning nil.
func actionsFrom(v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["actions"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		if s, ok := a.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// paramsFrom extracts the param name/required pairs from an action_describe
// response ({"params": [{"name":…, "required":…}, …]}), tolerating any
// non-conforming shape by returning nil.
func paramsFrom(v any) []specParam {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["params"].([]any)
	if !ok {
		return nil
	}
	out := make([]specParam, 0, len(raw))
	for _, p := range raw {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := pm["name"].(string)
		if name == "" {
			continue
		}
		req, _ := pm["required"].(bool)
		out = append(out, specParam{name: name, required: req})
	}
	return out
}
