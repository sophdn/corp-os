package mcp

import (
	"encoding/json"

	"corpos/internal/tool"
)

// SpecFootprint is the approximate prompt cost of one tool spec — the per-turn
// schema tax the model pays before doing any work. It is what capability
// projection shrinks (§4.1.1: the full mount ran ~8.6k tokens/turn).
type SpecFootprint struct {
	Name             string
	DescriptionBytes int
	SchemaBytes      int
	ApproxTokens     int  // (description + schema) / 4 — the rough bytes-per-token ratio
	Enriched         bool // true when this is the runtime EnrichedSpecs shape (action enum + per-action catalog), false when it degraded to the thin static spec
}

// IsEnriched reports whether a spec carries the runtime EnrichedSpecs shape — an
// `action` property constrained by a JSON-Schema enum (the per-action catalog the
// live substrate fills in). A spec that fell back to the thin static envelope (the
// substrate was unreachable at build time) has no enum and is NOT enriched. The
// footprint of a thin spec under-reports the runtime number, so this is what keeps
// -print-tools honest: a window-fit gauge that silently measured thin specs is the
// bug 1066 secondary (the footprint under-reports the enriched runtime spec).
func IsEnriched(s tool.Spec) bool {
	_, ok := enumOf(s)
	return ok
}

// Footprint reports the per-spec and total approximate prompt cost of a spec set,
// plus how many of the specs are enriched (the runtime shape) vs thin (a build-time
// fallback that under-reports the runtime footprint). The byte counts are the
// description text plus the JSON-serialized input schema (what the adapters serialize
// into each request); ApproxTokens uses the ~4 bytes/token rule of thumb. It is a
// comparison instrument (full vs projected), not an exact tokenizer, so the same
// approximation applies to both sides.
func Footprint(specs []tool.Spec) (perSpec []SpecFootprint, totalTokens, enriched int) {
	perSpec = make([]SpecFootprint, 0, len(specs))
	for _, s := range specs {
		descBytes := len(s.Description)
		schemaBytes := 0
		if b, err := json.Marshal(s.InputSchema); err == nil {
			schemaBytes = len(b)
		}
		tokens := (descBytes + schemaBytes + 3) / 4
		isEnriched := IsEnriched(s)
		if isEnriched {
			enriched++
		}
		perSpec = append(perSpec, SpecFootprint{
			Name:             s.Name,
			DescriptionBytes: descBytes,
			SchemaBytes:      schemaBytes,
			ApproxTokens:     tokens,
			Enriched:         isEnriched,
		})
		totalTokens += tokens
	}
	return perSpec, totalTokens, enriched
}
