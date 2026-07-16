package coding

import "corpos/internal/pathglob"

// pathMatches reports whether a forward-slash path matches a glob pattern. It is a
// thin alias onto the shared internal/pathglob matcher so the coding orchestrator's
// protected-path guards and the spawner's repair-loop test-file protection match
// through one implementation and cannot drift apart.
func pathMatches(p, pattern string) bool { return pathglob.Matches(p, pattern) }

// matchesAny reports whether a path falls under a protected pattern set. It delegates to
// the shared pathglob.IsProtected, so a "!"-prefixed exception (e.g. a test-authoring
// profile's "!**/*_test.go") un-protects a path consistently across the spawner guard,
// the coding orchestrator's protected-path guard, and the gate-integrity diff check —
// they match through one implementation and cannot drift apart. With no "!" patterns it
// is exactly the any-match it has always been.
func matchesAny(p string, patterns []string) bool { return pathglob.IsProtected(p, patterns) }
