package agent

import (
	"context"
	"strings"

	"corpos/internal/tool"
)

// Fake-green guard for the agent-SPAWN coding path (bug 1050). The 1050 run took the
// agent.spawn path (orchestrate → Spawner), NOT the internal/coding.Orchestrator path —
// and on that path the loop-owned VerifyGate only checks the gate's exit code. So a worker
// that AUTHORS its own *_test.go (a hollow test asserting only the buggy behavior) and
// leaves production code unchanged can still drive the gate GREEN: the gate IS the worker's
// self-report in disguise (the structural root cause named in 1050).
//
// The primary defense is the protect-paths dispatch denial (bug 1073): on a profile that
// declares ProtectPaths covering "**/*_test.go", a worker's fs.write/edit to a test path is
// DENIED at the boundary, so the worker cannot create OR edit the file the gate runs. This
// detector is the independent second layer the bug also asks for ("detect a worker-ADDED
// test and refuse to count it as the gate"): even if a test-file write LANDS — a coding
// profile with a verify gate but no/incomplete ProtectPaths, or a glob that misses a path —
// a passing gate that the worker's own newly-written test file could be certifying is NOT
// trusted as a clean green. It is computed by the loop from the dispatch record AFTER the
// gate runs, so the worker cannot revise it (the same unforgeable-footer idiom as workaudit).
//
// It fires only when BOTH hold this turn: (a) the worker landed a successful fs.write/edit to
// a *_test.go path, AND (b) it also mutated production code is NOT required — a green gate
// that depends on a worker-authored test is suspect regardless. The verdict makes the turn
// report a fake-green escalation instead of a clean done.

// workerAuthoredTestPaths returns the *_test.go paths the worker SUCCESSFULLY wrote or
// edited across a turn's dispatch record. A denied protect-paths write is not OK, so it is
// not counted — only a test-file mutation that actually landed surfaces here.
func workerAuthoredTestPaths(dispatches []tool.Result) []string {
	var paths []string
	seen := map[string]bool{}
	for _, d := range dispatches {
		if !isMutatingWrite(d) {
			continue
		}
		p := toolCallPath(d.Call)
		if isTestFilePath(p) && !seen[p] {
			paths = append(paths, p)
			seen[p] = true
		}
	}
	return paths
}

// IsTestFile reports whether a path is a Go test file. It is the ONE shared definition both
// verification paths consume — package agent's fake-green guard AND the coding orchestrator's
// red-green/test-only-diff checks (which call it via agent.IsTestFile) — so the two paths
// cannot drift on "what is a test file".
func IsTestFile(p string) bool {
	return strings.HasSuffix(p, "_test.go")
}

// isTestFilePath is the in-package alias for IsTestFile (kept so existing agent callers and
// tests read unchanged).
func isTestFilePath(p string) bool { return IsTestFile(p) }

// fakeGreenVerdict returns a non-empty fake-green verdict when a GREEN gate cannot be trusted
// because the worker AUTHORED (wrote/edited) one or more of the test files the gate runs. An
// empty verdict means the green is clean (no worker-authored test landed). It is only
// consulted on a passing gate at a done-claim — a worker-authored test among the dispatches
// means the gate may be the worker's self-report, so the done-claim is refused.
func fakeGreenVerdict(dispatches []tool.Result) string {
	authored := workerAuthoredTestPaths(dispatches)
	if len(authored) == 0 {
		return ""
	}
	return "fake-green: the verify gate passed but the worker authored the test file(s) it runs (" +
		strings.Join(authored, ", ") +
		") — a worker-added test cannot be the gate (fix production code; the acceptance test must be principal-authored and immutable)"
}

// FakeGreenGuard is a fake-green-stage Guard: consulted ONLY after a verify gate PASSED, it
// refuses a green that the worker's own authored test could be certifying. The zero value is
// the guard (it carries no config). Assess wraps the pure fakeGreenVerdict so the verdict is
// byte-identical to the bespoke wiring.
type FakeGreenGuard struct{}

func (FakeGreenGuard) Name() string      { return "fake-green" }
func (FakeGreenGuard) Stage() GuardStage { return StageFakeGreen }
func (FakeGreenGuard) Describe() string {
	return "refuses a PASSED verify gate when the worker authored a *_test.go the gate runs (the green may be the worker's self-report)"
}
func (FakeGreenGuard) Assess(_ context.Context, in GuardInput) GuardVerdict {
	if v := fakeGreenVerdict(in.Dispatches); v != "" {
		return fail(v)
	}
	return pass()
}
