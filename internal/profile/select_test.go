package profile

import (
	"reflect"
	"testing"
)

// regOf builds a validated in-memory registry from profiles for selection tests.
func regOf(t *testing.T, profiles ...JobProfile) *Registry {
	t.Helper()
	r, err := newRegistry(profiles)
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	return r
}

func mkProfile(name string, tier Tier, signals, shapes []string) JobProfile {
	return JobProfile{
		Name:          name,
		Duty:          name + " duty",
		Tier:          tier,
		Signals:       signals,
		ContextShapes: shapes,
		// one harmless tool so Validate passes and the profile is never "toolless"
		Tools: []SurfaceScope{{Surface: "knowledge"}},
	}
}

func envOf(shapes ...string) map[string]any {
	refs := make([]any, 0, len(shapes))
	for _, s := range shapes {
		refs = append(refs, map[string]any{"shape": s, "token": "x"})
	}
	return map[string]any{"references": refs}
}

func TestSelect_KeywordWinnerNoEnvelope(t *testing.T) {
	reg := regOf(t,
		mkProfile("code-review", TierMid, []string{"review", "diff", "PR"}, nil),
		mkProfile("bug-fix", TierLocal, []string{"bug", "fix", "crash"}, nil),
		mkProfile("orchestrate", TierMid, nil, nil), // default; never auto-selected (no signals)
	)
	got := Select("please review this diff", nil, reg, "orchestrate")
	if got.Profile != "code-review" || got.Fallback {
		t.Fatalf("want code-review (no fallback), got %q fallback=%v (%s)", got.Profile, got.Fallback, got.Reason)
	}
	if got.Score != 2*signalWeight { // "review" + "diff"
		t.Fatalf("want score %d, got %d", 2*signalWeight, got.Score)
	}
	if !reflect.DeepEqual(got.Signals, []string{"review", "diff"}) {
		t.Fatalf("want signals [review diff], got %v", got.Signals)
	}
}

func TestSelect_ShapeAffinityBreaksLeanTowardWinner(t *testing.T) {
	// Both score one keyword ("work"); the bug ref shape tips it to bug-fix.
	reg := regOf(t,
		mkProfile("bug-fix", TierLocal, []string{"work"}, []string{"bug_slug"}),
		mkProfile("doc-filing", TierLocal, []string{"work"}, []string{"vault_note"}),
	)
	got := Select("work on this", envOf("bug_slug"), reg, "orchestrate")
	if got.Profile != "bug-fix" || got.Fallback {
		t.Fatalf("want bug-fix via shape affinity, got %q fallback=%v (%s)", got.Profile, got.Fallback, got.Reason)
	}
	if !reflect.DeepEqual(got.Shapes, []string{"bug_slug"}) {
		t.Fatalf("want shapes [bug_slug], got %v", got.Shapes)
	}
}

func TestSelect_NoMatchFallsBackToDefault(t *testing.T) {
	reg := regOf(t,
		mkProfile("code-review", TierMid, []string{"review"}, nil),
		mkProfile("orchestrate", TierMid, nil, nil),
	)
	got := Select("hello there", nil, reg, "orchestrate")
	if got.Profile != "orchestrate" || !got.Fallback || got.Score != 0 {
		t.Fatalf("want orchestrate fallback score 0, got %q fallback=%v score=%d", got.Profile, got.Fallback, got.Score)
	}
}

func TestSelect_TieFallsBackToDefault(t *testing.T) {
	reg := regOf(t,
		mkProfile("code-review", TierMid, []string{"review"}, nil),
		mkProfile("bug-fix", TierLocal, []string{"bug"}, nil),
		mkProfile("orchestrate", TierMid, nil, nil),
	)
	// Both "review" and "bug" present → tie at signalWeight → ambiguous → default.
	got := Select("review the bug", nil, reg, "orchestrate")
	if got.Profile != "orchestrate" || !got.Fallback {
		t.Fatalf("want orchestrate fallback on tie, got %q fallback=%v (%s)", got.Profile, got.Fallback, got.Reason)
	}
	if got.Score != signalWeight {
		t.Fatalf("want tie score %d carried, got %d", signalWeight, got.Score)
	}
}

func TestSelect_SignalsOutweighShapes(t *testing.T) {
	// code-review wins on two keywords even though bug-fix has a matching shape.
	reg := regOf(t,
		mkProfile("code-review", TierMid, []string{"review", "diff"}, nil),
		mkProfile("bug-fix", TierLocal, []string{"review"}, []string{"bug_slug"}),
	)
	got := Select("review the diff", envOf("bug_slug"), reg, "orchestrate")
	if got.Profile != "code-review" {
		t.Fatalf("want code-review (signals outweigh shapes), got %q (%s)", got.Profile, got.Reason)
	}
}

func TestSelect_DeterministicAndCaseInsensitive(t *testing.T) {
	reg := regOf(t, mkProfile("code-review", TierMid, []string{"Review"}, nil))
	a := Select("REVIEW this", nil, reg, "orchestrate")
	b := Select("REVIEW this", nil, reg, "orchestrate")
	if a.Profile != "code-review" || a.Profile != b.Profile || a.Score != b.Score {
		t.Fatalf("want stable case-insensitive selection, got %+v / %+v", a, b)
	}
}

func TestSelect_MalformedEnvelopeIgnored(t *testing.T) {
	reg := regOf(t, mkProfile("code-review", TierMid, []string{"review"}, []string{"bug_slug"}))
	for _, env := range []map[string]any{
		nil,
		{"references": "not-a-list"},
		{"references": []any{"not-a-map", map[string]any{"noshape": 1}}},
	} {
		got := Select("review", env, reg, "orchestrate")
		if got.Profile != "code-review" {
			t.Fatalf("malformed env %v should still keyword-match, got %q", env, got.Profile)
		}
	}
}

func TestFootprintFloor(t *testing.T) {
	cases := []struct {
		name                           string
		declared                       Tier
		footprint, reserve, qwenWindow int
		want                           Tier
	}{
		{"fits stays local", TierLocal, 1000, 4000, 32000, TierLocal},
		{"overflows bumps to mid", TierLocal, 30000, 4000, 32000, TierMid},
		{"exactly fits stays local", TierLocal, 28000, 4000, 32000, TierLocal},
		{"one over bumps", TierLocal, 28001, 4000, 32000, TierMid},
		{"non-local unchanged", TierMid, 99999, 4000, 32000, TierMid},
		{"strong unchanged", TierStrong, 99999, 4000, 32000, TierStrong},
		{"zero window disables rule", TierLocal, 99999, 4000, 0, TierLocal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FootprintFloor(c.declared, c.footprint, c.reserve, c.qwenWindow); got != c.want {
				t.Fatalf("FootprintFloor(%s,%d,%d,%d) = %s, want %s",
					c.declared, c.footprint, c.reserve, c.qwenWindow, got, c.want)
			}
		})
	}
}

// Bug 1144: a RequiredShape gates auto-selection on the envelope referencing that shape,
// so bug-fix ("fix ONE FILED bug") is picked only when a filed bug is referenced. A
// free-form code-fix prompt (a path, no bug_slug) falls through to the coding-capable
// default (orchestrate → atomic-coding-chain → DeepSeek coding rung) instead of the flat
// single-worker bug-fix profile.
func TestSelect_RequiredShapeGatesAutoSelect(t *testing.T) {
	prompt := "fix the bug in calc.go so the failing test passes"

	// Characterization: WITHOUT the gate, the code-fix prompt wins bug-fix on signals alone.
	plain := JobProfile{Name: "bug-fix", Tier: TierMid, Signals: []string{"fix", "bug", "failing"}, ContextShapes: []string{"path", "bug_slug"}}
	if got := Select(prompt, envOf("path"), regOf(t, plain), "orchestrate"); got.Profile != "bug-fix" {
		t.Fatalf("characterization: an ungated bug-fix should win a code-fix prompt on signals, got %q (%s)", got.Profile, got.Reason)
	}

	// WITH the gate (RequiredShape=bug_slug): the same prompt with only a path (no filed
	// bug) is gated out → falls through to the coding default.
	gated := plain
	gated.RequiredShape = "bug_slug"
	reg := regOf(t, gated)
	if got := Select(prompt, envOf("path"), reg, "orchestrate"); got.Profile != "orchestrate" || !got.Fallback {
		t.Fatalf("code-fix (no bug_slug) must route to the coding default, got %q fallback=%v (%s)", got.Profile, got.Fallback, got.Reason)
	}

	// Constraint: a GENUINE bug-fix prompt (a bug_slug IS referenced) still selects bug-fix.
	if got := Select(prompt, envOf("bug_slug", "path"), reg, "orchestrate"); got.Profile != "bug-fix" || got.Fallback {
		t.Fatalf("a filed-bug prompt (bug_slug present) must keep bug-fix, got %q fallback=%v (%s)", got.Profile, got.Fallback, got.Reason)
	}
}

// With no envelope (parse_context unavailable), a RequiredShape profile is skipped — the
// run degrades to the coding default rather than the flat worker.
func TestSelect_RequiredShapeSkippedWhenEnvelopeNil(t *testing.T) {
	gated := JobProfile{Name: "bug-fix", Tier: TierMid, Signals: []string{"fix", "bug"}, RequiredShape: "bug_slug"}
	if got := Select("fix the bug", nil, regOf(t, gated), "orchestrate"); got.Profile != "orchestrate" {
		t.Fatalf("no envelope → RequiredShape profile skipped → coding default; got %q (%s)", got.Profile, got.Reason)
	}
}
