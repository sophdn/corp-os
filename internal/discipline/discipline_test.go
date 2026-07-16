package discipline

import (
	"strings"
	"testing"
	"time"

	"corpos/internal/hooks"
	"corpos/internal/profilehooks"
)

func cand(name, presented, body string) Candidate {
	return Candidate{Discipline: name, PresentedAs: presented, BodyPath: body}
}

func TestFire_CapDedupAndSuppression(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewFireTracker(WithClock(func() time.Time { return now }), WithMaxPerEnvelope(2))

	cands := []Candidate{cand("a", "", ""), cand("a", "", ""), cand("b", "", ""), cand("c", "", "")}
	// dedup drops the second "a"; cap stops at 2 → [a, b].
	got := tr.Fire("s", "verify", cands)
	if len(got) != 2 || got[0].Discipline != "a" || got[1].Discipline != "b" {
		t.Fatalf("cap/dedup wrong: %+v", got)
	}
	// Same session+intent within TTL → a and b suppressed; c now surfaces.
	got2 := tr.Fire("s", "verify", cands)
	if len(got2) != 1 || got2[0].Discipline != "c" {
		t.Fatalf("recent-fire suppression wrong: %+v", got2)
	}
	// A different intent is a different key → a fires again.
	if got3 := tr.Fire("s", "fix", []Candidate{cand("a", "", "")}); len(got3) != 1 {
		t.Errorf("different intent should not be suppressed: %+v", got3)
	}
}

func TestFire_TTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewFireTracker(WithClock(func() time.Time { return now }), WithTTL(5*time.Minute))
	if got := tr.Fire("s", "verify", []Candidate{cand("a", "", "")}); len(got) != 1 {
		t.Fatalf("first fire should surface")
	}
	now = now.Add(6 * time.Minute) // past TTL
	if got := tr.Fire("s", "verify", []Candidate{cand("a", "", "")}); len(got) != 1 {
		t.Errorf("after TTL expiry the discipline should re-fire: %+v", got)
	}
}

func TestOptionsIgnoreZeroValues(t *testing.T) {
	tr := NewFireTracker(WithTTL(0), WithMaxPerEnvelope(0), WithClock(nil))
	if tr.ttl != defaultRecentFireTTL || tr.maxPerEnvelope != defaultMaxPerEnvelope || tr.now == nil {
		t.Errorf("zero/nil options should be ignored: ttl=%v max=%d nowNil=%v", tr.ttl, tr.maxPerEnvelope, tr.now == nil)
	}
}

func envWith(cands []any, intent string) map[string]any {
	env := map[string]any{"candidate_disciplines": cands}
	if intent != "" {
		env["intent"] = map[string]any{"shape": intent}
	}
	return env
}

func refMap(token, presented, sourceRef string) map[string]any {
	m := map[string]any{"token": token, "presented_as": presented}
	if sourceRef != "" {
		m["top_candidates"] = []any{map[string]any{"source_ref": sourceRef}}
	}
	return m
}

func TestPreUserPromptHook_Injects(t *testing.T) {
	tr := NewFireTracker()
	env := envWith([]any{
		refMap("bug-filing-discipline", "[intent-mapped] bug-filing-discipline — body at skills/bug-filing/SKILL.md", "skill:skills/bug-filing/SKILL.md"),
		refMap("code-review", "", "skill:skills/code-review/SKILL.md"),
	}, "verify")
	c := &hooks.Context{Kind: hooks.PreUserPrompt, SessionID: "s1", Metadata: map[string]any{profilehooks.MetadataKeyParseContext: env}}
	tr.PreUserPromptHook()(c)

	if len(c.SystemPromptAdditions) != 1 {
		t.Fatalf("want 1 injection, got %d", len(c.SystemPromptAdditions))
	}
	out := c.SystemPromptAdditions[0]
	for _, want := range []string{"# Disciplines triggered", "bug-filing-discipline", "code-review", "skills/code-review/SKILL.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("injection missing %q:\n%s", want, out)
		}
	}
}

func TestPreUserPromptHook_NoopPaths(t *testing.T) {
	tr := NewFireTracker()
	cases := []struct {
		name string
		c    *hooks.Context
	}{
		{"nil metadata", &hooks.Context{}},
		{"no parse_context", &hooks.Context{Metadata: map[string]any{"other": 1}}},
		{"wrong type", &hooks.Context{Metadata: map[string]any{profilehooks.MetadataKeyParseContext: "nope"}}},
		{"no candidates", &hooks.Context{Metadata: map[string]any{profilehooks.MetadataKeyParseContext: map[string]any{"references": []any{}}}}},
		{"candidate without token", &hooks.Context{Metadata: map[string]any{profilehooks.MetadataKeyParseContext: envWith([]any{map[string]any{"presented_as": "x"}, "notamap"}, "")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr.PreUserPromptHook()(tc.c)
			if len(tc.c.SystemPromptAdditions) != 0 {
				t.Errorf("expected no injection, got %v", tc.c.SystemPromptAdditions)
			}
		})
	}
}
