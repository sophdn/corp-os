// Package gitenv is the ONE shared definition of the worktree-binding git environment
// scrubbing both verification paths need. corpos runs verification gate commands (build/test)
// on two paths — the agent.spawn loop's VerifyGate (internal/agent) and the coding
// Orchestrator's gate Runner (internal/coding) — and BOTH must run in a clean git context: an
// ambient git hook (corpos's own pre-commit gate) exports GIT_DIR/GIT_WORK_TREE/… into child
// processes, and inheriting them would silently redirect the gate's git/build commands at the
// WRONG repository (reinitializing or corrupting it). The scrub list and the strip function
// lived as verbatim copies in each path; a divergent edit to one (a new GIT_* var, say) would
// quietly leave the other path exposed. Defining them once here means the two gate-execution
// paths cannot drift on the git-context hazard.
package gitenv

import (
	"os"
	"strings"
)

// ContextVars are the worktree-binding git environment variable prefixes stripped before a gate
// command runs. An ambient git hook exports these; inheriting them points git/build at the
// wrong repo. A command that genuinely needs one can still set it explicitly inline in its own
// argv — only the INHERITED process-level vars are scrubbed.
var ContextVars = []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=", "GIT_PREFIX="}

// Clean returns the process environment minus the worktree-binding git vars (ContextVars) — the
// clean git context a verification gate command must run in.
func Clean() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if hasAnyPrefix(kv, ContextVars) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasAnyPrefix reports whether s starts with any of the given prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
