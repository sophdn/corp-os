// Package profilehooks wires a worker's job-profile into the loop's hook surface:
// it injects the profile's governing skills into the system prompt and prunes the
// parse_context envelope to the profile's declared shapes. These are the two
// context seams §5#2 calls for — the hook surface already exists (internal/hooks),
// it was just unwired.
//
// Both hooks read the active profile off the mutable hooks.Context (threaded there
// by agent.WithProfile). When no profile is active they are no-ops, so registering
// them unconditionally is safe.
package profilehooks

import (
	"strings"
	"sync"

	"corpos/internal/hooks"
	"corpos/internal/skills"
)

// MetadataKeyParseContext is the hooks.Context.Metadata key under which the per-turn
// parse_context probe deposits its raw envelope for ContextPruner to prune. The cmd/corpos
// prober (parseContextProber, wired by -context-probe) writes it and the discipline-fire hook
// (internal/discipline) reads it, so the key is defined here as the one name the prober, the
// pruner, and the discipline hook agree on.
const MetadataKeyParseContext = "parse_context"

// SkillInjector returns a pre_user_prompt hook that injects the active profile's
// governing skills into the session's system prompt exactly once. It loads the
// named skills from loader (unknown names are skipped — skills.Select's contract)
// and appends the assembled discipline block as a SystemPromptAddition, which the
// loop folds into the transcript (loop.go). A mechanical profile that names one
// tiny discipline gets one tiny block; a profile with no skills injects nothing.
//
// skillBudgetTokens caps the assembled block to the worker's window (full bodies
// while they fit, terse tiers beyond it — see skills.SystemPromptWithin). It is
// the single fix for the floor-overflow keystone: on a narrow local window the
// full discipline corpus (~8.7k tok for the coding profile) alone exceeds the
// compaction budget, forcing every coding turn to route up to the strong rung. A
// value <= 0 means no cap (large-window models get full bodies).
func SkillInjector(loader *skills.Loader, skillBudgetTokens int) hooks.Func {
	var once sync.Once
	return func(c *hooks.Context) {
		if c.Profile == nil || len(c.Profile.Skills) == 0 || loader == nil {
			return
		}
		once.Do(func() {
			selected, err := loader.Select(c.Profile.Skills)
			if err != nil || len(selected) == 0 {
				return
			}
			c.SystemPromptAdditions = append(c.SystemPromptAdditions, skills.SystemPromptWithin(selected, skillBudgetTokens))
		})
	}
}

// ContextPruner returns a pre_user_prompt hook that prunes a parse_context
// envelope (when one has been deposited in Context.Metadata) down to the active
// profile's declared shapes, so a mechanical worker carries a tiny context payload
// instead of the full reference sweep. It acts when the -context-probe prober has
// deposited an envelope this turn, and is a no-op otherwise (no prober, or the probe
// returned nothing) — so registering it unconditionally is harmless.
func ContextPruner() hooks.Func {
	return func(c *hooks.Context) {
		if c.Profile == nil || c.Metadata == nil {
			return
		}
		env, ok := c.Metadata[MetadataKeyParseContext].(map[string]any)
		if !ok {
			return
		}
		c.Metadata[MetadataKeyParseContext] = PruneEnvelope(env, c.Profile.ContextShapes)
	}
}

// ContextInjector returns a pre_user_prompt hook that renders the parse_context
// envelope in Context.Metadata (already pruned to the profile's shapes by
// ContextPruner, which must be registered BEFORE this) into a compact
// system-prompt addition — so the worker is oriented by exactly the references its
// duty cares about. A missing/empty payload is a no-op. Register order:
// ContextPruner then ContextInjector.
func ContextInjector() hooks.Func {
	return func(c *hooks.Context) {
		if c.Metadata == nil {
			return
		}
		env, ok := c.Metadata[MetadataKeyParseContext].(map[string]any)
		if !ok {
			return
		}
		refs, ok := env["references"].([]any)
		if !ok || len(refs) == 0 {
			return
		}
		var b strings.Builder
		b.WriteString("Resolved references in your request (scoped to this profile):")
		wrote := 0
		for _, r := range refs {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			token, _ := rm["token"].(string)
			if token == "" {
				continue
			}
			b.WriteString("\n- ")
			b.WriteString(token)
			if shape, _ := rm["shape"].(string); shape != "" {
				b.WriteString(" [" + shape + "]")
			}
			if s := referenceSummary(rm); s != "" {
				b.WriteString(": " + s)
			}
			wrote++
		}
		if wrote == 0 {
			return
		}
		c.SystemPromptAdditions = append(c.SystemPromptAdditions, b.String())
	}
}

// referenceSummary picks a short human label for one parse_context reference:
// the top candidate's Title or ID, else the trimmed presented_as line.
func referenceSummary(rm map[string]any) string {
	if tc, ok := rm["top_candidates"].([]any); ok && len(tc) > 0 {
		if first, ok := tc[0].(map[string]any); ok {
			if t, _ := first["Title"].(string); t != "" {
				return truncate(t, 100)
			}
			if id, _ := first["ID"].(string); id != "" {
				return truncate(id, 100)
			}
		}
	}
	if pa, _ := rm["presented_as"].(string); pa != "" {
		return truncate(pa, 100)
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}

// PruneEnvelope filters a parse_context envelope's "references" to those whose
// "shape" is in shapes, leaving the rest of the envelope intact. It is pure (it
// returns a new top-level map; the kept references are shared, not copied) so the
// firing step and tests can call it directly.
//
// An empty shapes list prunes ALL references (the profile declared no shapes →
// the leanest possible payload). A malformed envelope (no references array) is
// returned unchanged.
func PruneEnvelope(env map[string]any, shapes []string) map[string]any {
	refs, ok := env["references"].([]any)
	if !ok {
		return env
	}
	allow := make(map[string]bool, len(shapes))
	for _, s := range shapes {
		allow[s] = true
	}
	kept := make([]any, 0, len(refs))
	for _, r := range refs {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if shape, _ := rm["shape"].(string); allow[shape] {
			kept = append(kept, r)
		}
	}
	out := make(map[string]any, len(env))
	for k, v := range env {
		out[k] = v
	}
	out["references"] = kept
	return out
}
