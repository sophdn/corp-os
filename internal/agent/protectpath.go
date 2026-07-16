package agent

import (
	"corpos/internal/hooks"
	"corpos/internal/pathglob"
	"corpos/internal/tool"
)

// protectPathGuard returns a pre_tool_use hook that DENIES a worker's fs.write/edit to
// any path matching one of the protected glob patterns. It keeps a spawned worker's
// auto-verify repair loop honest (bug 1073): the worker must turn the build/test gate
// green by fixing PRODUCTION code, never by editing a *_test.go / acceptance path to
// force a fake green. The denial lands at the DISPATCH boundary (the reason is fed back
// as a tool_error so the worker adapts), so the protected paths are outside the worker's
// writable scope by construction, not convention. An empty pattern set is a no-op.
//
// It mirrors internal/coding.ProtectedPathGuard but lives in package agent so the
// spawner can attach it without importing the coding orchestrator; both match through
// the shared internal/pathglob matcher, so the two cannot drift apart.
func protectPathGuard(protected []string) hooks.Func {
	return func(c *hooks.Context) {
		if c.ToolCall == nil || len(protected) == 0 {
			return
		}
		call := c.ToolCall
		if call.Surface != "fs" || (call.Action != "write" && call.Action != "edit") {
			return
		}
		p := toolCallPath(*call)
		if p != "" && pathglob.IsProtected(p, protected) {
			c.DenyToolCall = true
			// A stronger model cannot lift a protect-path denial — escalating only wastes
			// the frontier rung (bug 1095). Mark it non-escalatable so the loop classifies it
			// ClassUsage; the worker still gets the reason fed back and adapts to fix
			// production, and a genuinely-stuck worker escalates via the RED-gate / no-progress
			// path instead of the denial count.
			c.DenyNonEscalatable = true
			c.DenyReason = "refused: " + p +
				" is a protected path — fix production code to pass the verify gate, do not edit the test/acceptance file to force green"
		}
	}
}

// ToolCallPath extracts the target path from an fs call (the two param spellings the
// substrate uses: path and file_path). It is the ONE shared definition both verification
// paths consume — package agent's guards AND the coding orchestrator's gate-flag/diff checks
// (which call it via agent.ToolCallPath) — so the two paths cannot drift on "what path did
// this call touch".
func ToolCallPath(c tool.Call) string {
	for _, key := range []string{"path", "file_path"} {
		if v, ok := c.Params[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// toolCallPath is the in-package alias for ToolCallPath (kept so existing agent callers and
// tests read unchanged).
func toolCallPath(c tool.Call) string { return ToolCallPath(c) }
