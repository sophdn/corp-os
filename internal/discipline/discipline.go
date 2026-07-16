// Package discipline applies the intent-discipline FIRING POLICY client-side
// (chain toolkit-decomposition T5). The toolkit keeps the deterministic detect/
// map half: when parse_context is called with discipline_firing="client" it
// returns the raw applicable disciplines in the envelope's candidate_disciplines
// (mapping + entryApplies + opt-out + manifest) and surfaces none inline. corpos
// owns the cadence — the per-envelope cap, the within-envelope dedup, and the
// recent-fire suppression — porting the toolkit's DisciplineFireTracker so corpos
// decides which reminders actually surface.
package discipline

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"corpos/internal/hooks"
	"corpos/internal/profilehooks"
)

const (
	// defaultMaxPerEnvelope caps discipline reminders per turn (mirrors the
	// toolkit's disciplineIntentMaxPerEnvelope) — reminders over-fire and erode
	// trust, so the envelope is noise-budgeted.
	defaultMaxPerEnvelope = 2
	// defaultRecentFireTTL suppresses a (session,intent,discipline) that fired
	// within ~5 turns (the toolkit's disciplineRecentFireTTL wall-clock proxy).
	defaultRecentFireTTL = 5 * time.Minute
)

type fireKey struct{ session, intent, discipline string }

// FireTracker holds per-session recent-fire state and applies the firing policy.
// Thread-safe: one tracker is shared across a session's turns.
type FireTracker struct {
	mu             sync.Mutex
	fired          map[fireKey]time.Time
	ttl            time.Duration
	maxPerEnvelope int
	now            func() time.Time
}

// Option configures a FireTracker.
type Option func(*FireTracker)

// WithTTL overrides the recent-fire suppression window.
func WithTTL(d time.Duration) Option {
	return func(t *FireTracker) {
		if d > 0 {
			t.ttl = d
		}
	}
}

// WithMaxPerEnvelope overrides the per-envelope cap.
func WithMaxPerEnvelope(n int) Option {
	return func(t *FireTracker) {
		if n > 0 {
			t.maxPerEnvelope = n
		}
	}
}

// WithClock injects a clock (tests).
func WithClock(now func() time.Time) Option {
	return func(t *FireTracker) {
		if now != nil {
			t.now = now
		}
	}
}

// NewFireTracker builds a tracker with the default cap (2) + TTL (5m).
func NewFireTracker(opts ...Option) *FireTracker {
	t := &FireTracker{
		fired:          map[fireKey]time.Time{},
		ttl:            defaultRecentFireTTL,
		maxPerEnvelope: defaultMaxPerEnvelope,
		now:            time.Now,
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Candidate is one raw discipline candidate parsed from the envelope.
type Candidate struct {
	Discipline  string
	PresentedAs string
	BodyPath    string
}

// Fire returns the disciplines to surface for one envelope, marking survivors
// fired. It dedups within the envelope, drops disciplines that fired for the
// same (session,intent) inside the TTL, and caps the result. Order is preserved.
func (t *FireTracker) Fire(session, intent string, cands []Candidate) []Candidate {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []Candidate
	seen := map[string]bool{}
	for _, c := range cands {
		if len(out) >= t.maxPerEnvelope {
			break
		}
		if c.Discipline == "" || seen[c.Discipline] {
			continue
		}
		seen[c.Discipline] = true
		k := fireKey{session, intent, c.Discipline}
		if at, ok := t.fired[k]; ok && now.Sub(at) < t.ttl {
			continue
		}
		out = append(out, c)
		t.fired[k] = now
	}
	return out
}

// PreUserPromptHook reads the parse_context envelope's candidate_disciplines,
// applies the firing policy, and injects the surviving discipline reminders into
// the system prompt. Best-effort: a missing/malformed envelope or empty result
// leaves the prompt untouched. Register AFTER the context prober stashes the
// envelope (the prober must call parse_context with discipline_firing="client").
func (t *FireTracker) PreUserPromptHook() hooks.Func {
	return func(c *hooks.Context) {
		if c.Metadata == nil {
			return
		}
		env, ok := c.Metadata[profilehooks.MetadataKeyParseContext].(map[string]any)
		if !ok {
			return
		}
		cands := parseCandidates(env)
		if len(cands) == 0 {
			return
		}
		survivors := t.Fire(c.SessionID, intentShape(env), cands)
		if len(survivors) == 0 {
			return
		}
		c.SystemPromptAdditions = append(c.SystemPromptAdditions, renderDisciplines(survivors))
	}
}

// parseCandidates extracts the raw discipline candidates from the envelope's
// candidate_disciplines array (each a ResolvedReference-shaped map).
func parseCandidates(env map[string]any) []Candidate {
	raw, ok := env["candidate_disciplines"].([]any)
	if !ok {
		return nil
	}
	var out []Candidate
	for _, r := range raw {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		token, _ := rm["token"].(string)
		if token == "" {
			continue
		}
		presented, _ := rm["presented_as"].(string)
		out = append(out, Candidate{
			Discipline:  token,
			PresentedAs: presented,
			BodyPath:    bodyPathOf(rm),
		})
	}
	return out
}

// bodyPathOf pulls the discipline body path off the top candidate's source_ref
// (the toolkit composes it as "skill:<bodyPath>").
func bodyPathOf(rm map[string]any) string {
	tc, ok := rm["top_candidates"].([]any)
	if !ok || len(tc) == 0 {
		return ""
	}
	first, ok := tc[0].(map[string]any)
	if !ok {
		return ""
	}
	src, _ := first["source_ref"].(string)
	return strings.TrimPrefix(src, "skill:")
}

// intentShape pulls the directive-intent shape from the envelope (empty when
// absent — the suppression then keys on an empty intent, which is harmless).
func intentShape(env map[string]any) string {
	intent, ok := env["intent"].(map[string]any)
	if !ok {
		return ""
	}
	shape, _ := intent["shape"].(string)
	return shape
}

// renderDisciplines frames the surviving disciplines as a single system-prompt
// reminder block — disciplines that apply to this turn's intent, with their body
// paths, treated as guidance to consider (not hard instructions).
func renderDisciplines(survivors []Candidate) string {
	var b strings.Builder
	b.WriteString("# Disciplines triggered by this request\n\n")
	b.WriteString("Conventions that apply to what you're about to do — consider them; read the body if relevant:\n")
	for _, s := range survivors {
		line := s.PresentedAs
		if line == "" {
			line = fmt.Sprintf("`%s`", s.Discipline)
			if s.BodyPath != "" {
				line += " — body at " + s.BodyPath
			}
		}
		b.WriteString("\n- ")
		b.WriteString(line)
	}
	return b.String()
}
