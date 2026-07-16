package profile

// Deterministic prompt→profile matching (corpos #3096): Match is the pure, sans-IO keyword
// matcher for job-profile selection — given a prompt and candidate profiles described by signal
// keywords, it picks the best-matching one. Folded from the former standalone internal/profilematch
// package (chain 379 task 3, which removed that orphan); it is NOT yet wired into live profile
// selection — connecting it to the parse_context envelope + the real registry is a separate
// concern. Provenance: first drafted autonomously by the corpos feature pipeline, then corrected
// (highest-scoring not first match, case-insensitive, distinct-keyword scoring) with match_test.go
// pinning the behavior.

import "strings"

// Candidate is a named profile paired with the keywords whose presence in a prompt signal it.
type Candidate struct {
	Name     string
	Keywords []string
}

// Match scores each candidate by the number of its DISTINCT Keywords that appear as whole,
// case-insensitive words in prompt, and returns the Name of the highest-scoring candidate
// together with that score. When no candidate matches any keyword (the best score is 0) it
// returns fallback and 0. Scoring is order-stable: when two candidates tie on score the one
// earlier in candidates wins (the comparison is strictly greater-than, so the first to reach a
// score keeps it). Being a pure function of its inputs, it is deterministic by construction.
func Match(prompt string, candidates []Candidate, fallback string) (string, int) {
	words := wordSet(prompt)
	bestName, bestScore := fallback, 0
	for _, c := range candidates {
		if score, _ := signalHits(words, c.Keywords); score > bestScore {
			// strictly greater → earlier candidate wins ties
			bestName, bestScore = c.Name, score
		}
	}
	return bestName, bestScore
}

// wordSet is the set of whole words in prompt, lower-cased for case-insensitive
// whole-word matching. Shared by Match and the richer Select path so both tokenize
// identically.
func wordSet(prompt string) map[string]struct{} {
	words := make(map[string]struct{})
	for _, w := range strings.Fields(prompt) {
		words[strings.ToLower(w)] = struct{}{}
	}
	return words
}

// signalHits counts the DISTINCT keywords that appear as whole words in words and
// returns that count together with the matched keywords (lower-cased, in first-seen
// order) for explainability. A keyword listed twice is a degenerate signal set, not
// a double weight (a conformance-critic probe surfaced this edge — "number of its
// Keywords that appear" is the count of distinct matching keywords, not list
// entries), so duplicates are collapsed before scoring.
func signalHits(words map[string]struct{}, keywords []string) (int, []string) {
	counted := make(map[string]struct{}, len(keywords))
	var matched []string
	for _, kw := range keywords {
		lk := strings.ToLower(kw)
		if _, dup := counted[lk]; dup {
			continue
		}
		counted[lk] = struct{}{}
		if _, ok := words[lk]; ok {
			matched = append(matched, lk)
		}
	}
	return len(matched), matched
}
