// Package pathglob is the shared forward-slash path-glob matcher used wherever
// corpos decides whether a file path falls under a protected/allow pattern set —
// the coding orchestrator's protected-gate-path guards and the spawner's
// repair-loop test-file protection both match through it, so the two cannot drift
// apart. ** matches zero or more path segments; a single segment is matched with
// path.Match semantics (* and ? within a segment).
package pathglob

import (
	"path"
	"strings"
)

// Matches reports whether a forward-slash path matches a glob pattern where ** matches
// zero or more path segments and a single segment is matched with path.Match semantics
// (* and ? within a segment). Patterns and paths are split on "/" so ** spans arbitrary
// depth (which path.Match alone does not).
func Matches(p, pattern string) bool {
	return matchSegments(strings.Split(strings.Trim(p, "/"), "/"), strings.Split(strings.Trim(pattern, "/"), "/"))
}

func matchSegments(segs, pats []string) bool {
	if len(pats) == 0 {
		return len(segs) == 0
	}
	if pats[0] == "**" {
		if len(pats) == 1 {
			return true
		}
		for i := 0; i <= len(segs); i++ {
			if matchSegments(segs[i:], pats[1:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if ok, _ := path.Match(pats[0], segs[0]); !ok {
		return false
	}
	return matchSegments(segs[1:], pats[1:])
}

// MatchesAny reports whether p matches any of the patterns.
func MatchesAny(p string, patterns []string) bool {
	for _, pat := range patterns {
		if Matches(p, pat) {
			return true
		}
	}
	return false
}

// IsProtected reports whether path p falls under a protected pattern set that may
// include "!"-prefixed EXCEPTIONS. p is protected when it matches at least one positive
// pattern and NO exception pattern, evaluated order-independently (any exception match
// wins). This lets a test-authoring profile protect all production source yet still
// permit its own *_test.go deliverable with ["**/*.go", "!**/*_test.go"]. With no
// exception patterns it is exactly MatchesAny, so existing protected sets are unchanged.
func IsProtected(p string, patterns []string) bool {
	matchedPositive := false
	for _, pat := range patterns {
		if ex, ok := strings.CutPrefix(pat, "!"); ok {
			if Matches(p, ex) {
				return false
			}
			continue
		}
		if Matches(p, pat) {
			matchedPositive = true
		}
	}
	return matchedPositive
}
