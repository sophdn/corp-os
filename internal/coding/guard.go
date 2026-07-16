package coding

import (
	"context"
	"strings"

	"corpos/internal/agent"
	"corpos/internal/hooks"
	"corpos/internal/risk"
	"corpos/internal/tool"
)

// Gate integrity (capability-floor gap #1): the gate a coding worker runs must be
// orchestrator-owned and IMMUTABLE. A local worker once rewrote its own gate test
// against an explicit keep-it instruction, so escalate-on-fail must be judged on an
// external gate the worker cannot tamper with. Three enforcement points (defense in depth):
//
//   - ProtectedPathGate: a risk.Gate that DENIES a worker's fs.write/edit to a protected
//     path. The fs-mutation class risk.Classify leaves UNGATED, so it is wired via
//     ProtectedPathGuard (a pre_tool_use hook) — not risk.Guard, which fires only on
//     gated verdicts.
//   - ProtectedPathGuard: the pre_tool_use hook the coding worker injects per-attempt
//     (agent.WithExtraHook), enforcing the protected-acceptance-path denial at the
//     DISPATCH boundary so the principal-owned gate is outside the worker's writable
//     scope by construction, not convention (T4).
//   - gateIntegrityViolations: an orchestrator-owned POST-attempt check over the
//     worker's diff that rejects an attempt which modified a protected path even if it
//     made the gate pass (belt-and-suspenders behind the dispatch guard).

// ProtectedPathGate is a risk.Gate that blocks worker writes to protected paths.
// It approves everything else (it is a scope-of-mutation gate, not a general deny).
type ProtectedPathGate struct {
	Protected []string
}

// Approve denies a mutating fs call whose target path matches a protected pattern.
func (g ProtectedPathGate) Approve(c tool.Call, v risk.Verdict) (bool, string) {
	if v.Class != risk.ClassMutating {
		return true, ""
	}
	if c.Surface != "fs" || (c.Action != "write" && c.Action != "edit") {
		return true, ""
	}
	p := callPath(c)
	if p != "" && matchesAny(p, g.Protected) {
		return false, "refused: " + p + " is a protected gate path (the gate is orchestrator-owned and immutable)"
	}
	return true, ""
}

// ProtectedPathGuard returns a pre_tool_use hook that DENIES a worker's fs.write/edit to a
// protected path at the dispatch boundary. Unlike risk.Guard (which consults its gate only
// for GATED verdicts, and fs-mutation is ungated) this fires for the ungated mutating-write
// class — exactly the protected-path policy risk.Classify defers. It is how the principal-
// owned acceptance test is kept outside the worker's writable scope by construction (T4): a
// worker attempt to write a protected path never lands. An empty protected set is a no-op.
func ProtectedPathGuard(protected []string) hooks.Func {
	g := ProtectedPathGate{Protected: protected}
	return func(c *hooks.Context) {
		if c.ToolCall == nil || len(protected) == 0 {
			return
		}
		if ok, reason := g.Approve(*c.ToolCall, risk.Classify(*c.ToolCall)); !ok {
			c.DenyToolCall = true
			c.DenyReason = reason
		}
	}
}

// callPath extracts the target path from an fs call (the two param spellings the substrate
// uses: path and file_path). It delegates to agent.ToolCallPath — the ONE shared definition
// both verification paths consume — so the coding orchestrator's gate-flag/diff path-matching
// and the spawn-path guards cannot drift on path extraction.
func callPath(c tool.Call) string {
	return agent.ToolCallPath(c)
}

// gateIntegrityViolations returns the protected paths a worker's in-progress
// attempt modified, by diffing the worktree against the fork point and matching
// changed files against the AT's Protected globs. Empty when there are no protected
// paths, the repo can't diff (NoopRepo), or nothing protected was touched. It shares
// the worktreeDiff source with gateFlags so the two cannot drift apart.
func (o *Orchestrator) gateIntegrityViolations(ctx context.Context, spec AtomicTask, dir, parentSHA string) []string {
	if len(spec.Protected) == 0 {
		return nil
	}
	diff := o.worktreeDiff(ctx, dir, parentSHA)
	if diff == "" {
		return nil
	}
	var hits []string
	for _, p := range changedPaths(diff) {
		if matchesAny(p, spec.Protected) {
			hits = append(hits, p)
		}
	}
	return hits
}

// changedPaths parses the file paths from a unified `git diff` ("diff --git a/X
// b/X" header lines).
func changedPaths(diff string) []string {
	var paths []string
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		paths = append(paths, strings.TrimPrefix(fields[3], "b/"))
	}
	return paths
}
