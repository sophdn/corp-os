package coding

import "strings"

// classifyFailure is a deterministic, LLM-free triage of a failed AT's diagnostic.
// It is advisory: it produces a short hint the operator sees, and a coarse label,
// but the operator owns the actual decision. Mirrors the bench's heuristic (Go-
// focused for now; generalizes when other targets appear).
func classifyFailure(ar *ATRecord) (label, hint string) {
	diag := strings.ToLower(ar.Diagnostic)
	status := ar.WorkerStatus

	compileMarkers := []string{"[build failed]", "syntax error", "undefined:", "imported and not used",
		"declared and not used", "no required module provides package", "missing go.sum entry", "cannot find package"}
	assertionMarkers := []string{"--- fail:", "assertion failed", "expected ", "got ", "not equal:"}

	hasCompile := containsAny(diag, compileMarkers)
	hasAssertion := containsAny(diag, assertionMarkers)

	switch {
	case status == WorkerWorkspaceViolation:
		return "spec_bug", "workspace allowlist may be wrong; suggests edit"
	case hasCompile && strings.Contains(diag, "test"):
		// A failing `go test ...` gate that won't compile is usually a test-shape
		// problem (checked before build: the "[build failed]" marker also appears
		// for a test-compile failure).
		return "test_bug", "tests fail to compile; suggests edit with a tighter spec"
	case hasCompile && (strings.Contains(diag, "build") || strings.Contains(diag, "vet")):
		return "impl_bug", "compile error in build/vet; suggests branch_fix"
	case hasAssertion && !hasCompile:
		return "ambiguous", "tests compiled but assertions failed; could be an impl bug (branch_fix upstream) or a test bug (edit)"
	case status == WorkerMaxIterationsExhausted:
		return "ambiguous", "worker exhausted iterations; branch_fix with diff context is the default"
	default:
		return "ambiguous", "no clear pattern; inspect the diff"
	}
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
