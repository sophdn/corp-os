package profile

import (
	"fmt"
	"sort"
	"strings"
)

// Scoring weights for deterministic prompt→profile selection (corpos #3096). A
// keyword signal is the primary evidence; a context-shape affinity is a secondary
// booster that breaks near-ties between profiles whose declared ContextShapes line
// up with the references the parse_context envelope actually found in the prompt.
// signalWeight > shapeWeight so a profile can never be out-voted on shapes alone.
const (
	signalWeight = 2
	shapeWeight  = 1
)

// Selection is the deterministic, explainable outcome of choosing a job-profile for
// a top-level prompt when no -profile was named. It is a pure function of the prompt
// + the (optional) parse_context envelope + the registry, so it is reproducible and
// logged as the labeled dataset for the later ML matcher (#3098).
type Selection struct {
	// Profile is the chosen profile name.
	Profile string
	// Score is the winner's combined signal+shape score. 0 means nothing matched and
	// the safe default was chosen.
	Score int
	// Fallback is true when the safe default was used because confidence was low —
	// either nothing matched (Score 0) or the top two candidates tied (ambiguous).
	Fallback bool
	// Reason is a one-line human explanation of the choice (for the stderr line).
	Reason string
	// Signals are the winner's signal keywords that fired in the prompt.
	Signals []string
	// Shapes are the winner's context-shapes that the envelope referenced.
	Shapes []string
}

// scored is one candidate profile's evidence, retained for the explainable Reason
// and the tie check.
type scored struct {
	name    string
	total   int
	signals []string
	shapes  []string
}

// Select picks the job-profile that best matches prompt, deterministically and
// explainably, for the no-`-profile` daily-driver path. It scores every profile that
// declares Signals: signalWeight per distinct signal keyword present in the prompt,
// plus shapeWeight per profile ContextShape that the parse_context envelope (env, may
// be nil) actually referenced. The unique highest scorer wins. Confidence falls back
// to defaultName — which MUST be a real, tool-bearing profile, never unprojected —
// when nothing matched (Score 0) or the top two tied (ambiguous), so a low-confidence
// prompt lands on the safe default rather than a coin-flip. Being a pure function of
// its inputs it is reproducible by construction.
func Select(prompt string, env map[string]any, reg *Registry, defaultName string) Selection {
	words := wordSet(prompt)
	shapes := envelopeShapes(env)

	ranked := make([]scored, 0, reg.Len())
	for _, name := range reg.Names() { // sorted → deterministic iteration
		p, ok := reg.Get(name)
		if !ok || len(p.Signals) == 0 {
			continue // profiles with no signals are never auto-selected
		}
		// A RequiredShape gates auto-selection on the envelope actually referencing that
		// shape — so a profile whose purpose keys off a specific referent (bug-fix → a filed
		// bug_slug) is not picked on its signal keywords alone. Without the shape it is
		// skipped entirely, so a free-form code-fix prompt ("fix the bug in x.go") falls
		// through to the coding-capable default rather than the flat bug-fix worker (bug 1144).
		if p.RequiredShape != "" && shapes[p.RequiredShape] == 0 {
			continue
		}
		sigN, sigHits := signalHits(words, p.Signals)
		shapeN, shapeHits := shapeAffinity(shapes, p.ContextShapes)
		total := sigN*signalWeight + shapeN*shapeWeight
		if total == 0 {
			continue
		}
		ranked = append(ranked, scored{name: name, total: total, signals: sigHits, shapes: shapeHits})
	}

	// Highest total first; ties stay in Names() (alphabetical) order so the tie test
	// below is deterministic.
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].total > ranked[j].total })

	if len(ranked) == 0 {
		return Selection{
			Profile:  defaultName,
			Score:    0,
			Fallback: true,
			Reason:   fmt.Sprintf("no profile signal matched the prompt → safe default %q", defaultName),
		}
	}
	if len(ranked) > 1 && ranked[0].total == ranked[1].total {
		return Selection{
			Profile:  defaultName,
			Score:    ranked[0].total,
			Fallback: true,
			Reason: fmt.Sprintf("ambiguous: %q and %q tied at %d → safe default %q",
				ranked[0].name, ranked[1].name, ranked[0].total, defaultName),
		}
	}

	w := ranked[0]
	return Selection{
		Profile:  w.name,
		Score:    w.total,
		Fallback: false,
		Signals:  w.signals,
		Shapes:   w.shapes,
		Reason: fmt.Sprintf("auto-selected %q (score %d): signals=[%s] shapes=[%s]",
			w.name, w.total, strings.Join(w.signals, " "), strings.Join(w.shapes, " ")),
	}
}

// FootprintFloor returns the starting model tier for a profile whose declared tier
// is `declared`, raised to mid when the fixed context footprint (projected tool specs
// + injected system prompt/skills, in tokens) would not leave `reserve` tokens of
// conversation headroom inside the local Qwen window. This is the matcher's tier rule
// (corpos #3096 criterion 4): Qwen-first UNLESS the estimated footprint exceeds Qwen's
// window → start mid. A non-local declared tier is returned unchanged (it already
// starts above Qwen); a zero/unknown qwenWindow disables the rule (returns declared)
// so a failed window probe never silently forces every run up a rung.
func FootprintFloor(declared Tier, footprintTokens, reserve, qwenWindow int) Tier {
	if declared != TierLocal || qwenWindow <= 0 {
		return declared
	}
	if footprintTokens+reserve > qwenWindow {
		return TierMid
	}
	return declared
}

// envelopeShapes counts, per reference shape, how many references in a parse_context
// envelope carry it. The envelope is the raw map the knowledge.parse_context probe
// deposits: {"references": [{"shape": "<shape>", ...}, ...], ...}. A nil/!malformed
// envelope yields an empty set (selection then rests on keyword signals alone), so
// the shape booster is purely additive and never required.
func envelopeShapes(env map[string]any) map[string]int {
	out := map[string]int{}
	if env == nil {
		return out
	}
	refs, ok := env["references"].([]any)
	if !ok {
		return out
	}
	for _, r := range refs {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["shape"].(string); ok && s != "" {
			out[s]++
		}
	}
	return out
}

// shapeAffinity counts the profile's ContextShapes that the envelope referenced
// (presence, not multiplicity — a profile that declares a shape is rewarded once for
// it appearing, however many references carry it) and returns that count with the
// matched shapes for explainability.
func shapeAffinity(present map[string]int, contextShapes []string) (int, []string) {
	var matched []string
	seen := make(map[string]struct{}, len(contextShapes))
	for _, sh := range contextShapes {
		if _, dup := seen[sh]; dup {
			continue
		}
		seen[sh] = struct{}{}
		if present[sh] > 0 {
			matched = append(matched, sh)
		}
	}
	return len(matched), matched
}
