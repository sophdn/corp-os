package coding

import (
	"context"
	"strings"
)

// Test-file-edit detection (the literature's cheap reward-hack baseline): "flag any worker
// edit to or deletion of the tests it is graded against, or a change whose only substantive
// content is test files." Both are structured GateFlags raised against the worker's diff —
// surfaced in the run verdict (carried on the ATRecord, emitted with the at_status event),
// never silently swallowed. A flag marks/denies the run and is computed by the orchestrator
// from the worktree diff AFTER the worker is done, so it is not overridable by worker state.

// GateFlagKind discriminates a structured reward-hack signal.
type GateFlagKind string

const (
	// FlagProtectedPathEdit: the worker modified a path the principal marked protected
	// (the gate/acceptance oracle, T4). The gate is orchestrator-owned and immutable, so a
	// tampered gate cannot certify success — this denies the run (WorkerGateIntegrityViolation).
	FlagProtectedPathEdit GateFlagKind = "protected_path_edit"
	// FlagTestOnlyDiff: the worker's diff changes ONLY *_test.go files (>=1 changed .go file,
	// all of them tests) — for a bug-fix AT the production code was never touched, so a green
	// gate is hollow. Denies the run unless the principal's AT sets AllowTestOnlyDiff.
	FlagTestOnlyDiff GateFlagKind = "test_only_diff"
	// FlagCoverageAdvisory: the gate passed (Tier 1) but the Tier-2 quality report graded
	// the green as "proposed" — a changed production line is not exercised by any test, or a
	// touched-package test is skipped (docs/TWO_TIER_GREEN_DESIGN.md). UNLIKE the other flags
	// this is ADVISORY: it never fails the attempt (the status stays WorkerSuccess); it rides
	// on the AT record so the principal sees that the green, while real, is not substantiated.
	FlagCoverageAdvisory GateFlagKind = "coverage_advisory"
)

// GateFlag is one structured reward-hack signal raised against a worker attempt.
type GateFlag struct {
	Kind   GateFlagKind `json:"kind"`
	Detail string       `json:"detail"`
}

// worktreeDiff returns the worker's staged+untracked diff against the fork point, or "" when
// there is no diffable repo (NoopRepo) or no fork point. It is the single diff source the
// gate-integrity and gate-flag checks share, so they cannot drift apart.
func (o *Orchestrator) worktreeDiff(ctx context.Context, dir, parentSHA string) string {
	if parentSHA == "" {
		return ""
	}
	pr, ok := o.repo.(packageReader)
	if !ok {
		return ""
	}
	diff, err := pr.DiffWorktree(ctx, dir, parentSHA)
	if err != nil {
		return ""
	}
	return diff
}

// gateFlags derives the structured reward-hack flags from a worktree diff against the
// principal-owned spec. It is a pure function over the diff text (no IO) so it is
// exhaustively unit-testable. An empty diff or no signals → no flags.
func gateFlags(diff string, spec AtomicTask) []GateFlag {
	if diff == "" {
		return nil
	}
	changed := changedPaths(diff)
	var flags []GateFlag

	// (a) protected gate/acceptance path edit.
	var protectedHits []string
	for _, p := range changed {
		if matchesAny(p, spec.Protected) {
			protectedHits = append(protectedHits, p)
		}
	}
	if len(protectedHits) > 0 {
		flags = append(flags, GateFlag{
			Kind:   FlagProtectedPathEdit,
			Detail: "worker modified protected gate path(s): " + strings.Join(protectedHits, ", "),
		})
	}

	// (b) test-only diff: >=1 changed .go file and every changed .go file is a test.
	var goFiles, testFiles []string
	for _, p := range changed {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		goFiles = append(goFiles, p)
		if isTestFile(p) {
			testFiles = append(testFiles, p)
		}
	}
	if len(goFiles) > 0 && len(goFiles) == len(testFiles) {
		flags = append(flags, GateFlag{
			Kind:   FlagTestOnlyDiff,
			Detail: "implementation diff is test-only (no production .go changed): " + strings.Join(testFiles, ", "),
		})
	}
	return flags
}

// findFlag returns the first flag of kind k, and whether one was present.
func findFlag(flags []GateFlag, k GateFlagKind) (GateFlag, bool) {
	for _, f := range flags {
		if f.Kind == k {
			return f, true
		}
	}
	return GateFlag{}, false
}
