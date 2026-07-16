package profile

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		name       string
		prompt     string
		candidates []Candidate
		fallback   string
		wantName   string
		wantScore  int
	}{
		{
			name:       "single keyword match scores one",
			prompt:     "a bug here",
			candidates: []Candidate{{Name: "coding", Keywords: []string{"bug"}}},
			fallback:   "default",
			wantName:   "coding", wantScore: 1,
		},
		{
			// The gap the authored oracle missed: the score is the COUNT of matching
			// keywords, not a hard-coded 1.
			name:       "score counts every matching keyword",
			prompt:     "fix the bug and ship the patch",
			candidates: []Candidate{{Name: "coding", Keywords: []string{"bug", "fix", "patch", "absent"}}},
			fallback:   "default",
			wantName:   "coding", wantScore: 3,
		},
		{
			// The gap the authored oracle missed: the HIGHEST scorer wins, not the first
			// candidate that happens to match.
			name:   "highest score wins over an earlier lower scorer",
			prompt: "fix the bug and ship the patch",
			candidates: []Candidate{
				{Name: "low", Keywords: []string{"bug"}},
				{Name: "high", Keywords: []string{"bug", "fix", "patch"}},
			},
			fallback: "default",
			wantName: "high", wantScore: 3,
		},
		{
			name:   "tie is broken by slice order (earlier wins)",
			prompt: "a bug here",
			candidates: []Candidate{
				{Name: "first", Keywords: []string{"bug"}},
				{Name: "second", Keywords: []string{"bug"}},
			},
			fallback: "default",
			wantName: "first", wantScore: 1,
		},
		{
			// A conformance-critic probe surfaced this edge: a duplicate keyword is one signal,
			// not two — distinct keywords are counted, so the score is 1, not 2.
			name:       "duplicate keyword counts once",
			prompt:     "a bug here",
			candidates: []Candidate{{Name: "coding", Keywords: []string{"bug", "bug"}}},
			fallback:   "default",
			wantName:   "coding", wantScore: 1,
		},
		{
			// The gap the authored oracle missed: matching is case-insensitive.
			name:       "matching is case-insensitive",
			prompt:     "A BUG Here",
			candidates: []Candidate{{Name: "coding", Keywords: []string{"Bug"}}},
			fallback:   "default",
			wantName:   "coding", wantScore: 1,
		},
		{
			name:       "whole-word only: a substring does not match",
			prompt:     "run the debugger",
			candidates: []Candidate{{Name: "coding", Keywords: []string{"bug"}}},
			fallback:   "default",
			wantName:   "default", wantScore: 0,
		},
		{
			name:       "no keyword match falls back",
			prompt:     "hello world",
			candidates: []Candidate{{Name: "coding", Keywords: []string{"bug"}}},
			fallback:   "default",
			wantName:   "default", wantScore: 0,
		},
		{
			name:       "no candidates falls back",
			prompt:     "a bug here",
			candidates: nil,
			fallback:   "default",
			wantName:   "default", wantScore: 0,
		},
		{
			name:       "empty prompt falls back",
			prompt:     "",
			candidates: []Candidate{{Name: "coding", Keywords: []string{"bug"}}},
			fallback:   "default",
			wantName:   "default", wantScore: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotScore := Match(c.prompt, c.candidates, c.fallback)
			if gotName != c.wantName || gotScore != c.wantScore {
				t.Fatalf("Match(%q, …, %q) = (%q, %d), want (%q, %d)",
					c.prompt, c.fallback, gotName, gotScore, c.wantName, c.wantScore)
			}
		})
	}
}

// TestMatchDeterministic guards the property the spec names but a behavioral oracle can't
// pin from outside: identical inputs always yield identical output (it is a pure function).
func TestMatchDeterministic(t *testing.T) {
	cands := []Candidate{{Name: "a", Keywords: []string{"bug"}}, {Name: "b", Keywords: []string{"bug"}}}
	n1, s1 := Match("a bug here", cands, "default")
	n2, s2 := Match("a bug here", cands, "default")
	if n1 != n2 || s1 != s2 {
		t.Fatalf("non-deterministic: (%q,%d) vs (%q,%d)", n1, s1, n2, s2)
	}
}
