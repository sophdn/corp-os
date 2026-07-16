package mcp

import (
	"context"

	"corpos/internal/tool"
)

// groundTruthStatusFields are the work-ledger fields that describe what the ledger
// BELIEVES about a bug's resolution — not whether the code currently satisfies the
// bug's gate. A reverted fix or a seeded regression leaves these reading "fixed" while
// the repo gate is RED, so an agent that trusts them inverts a "fix the bug" duty.
var groundTruthStatusFields = []string{"status", "resolution_kind", "resolved_commit_sha", "resolved_at"}

// groundTruthDirective is attached to every reconciled bug_read result. It tells a
// decomposing/fixing agent to treat the gate/repo state as authoritative over the
// relocated ledger status, and to frame the duty as a real fix rather than a verify.
const groundTruthDirective = "The fields under `ledger_status` are the work ledger's bookkeeping, NOT proof the code currently works: a reverted or seeded-regression fix leaves status='fixed' while the repo gate is RED. Treat the actual build/test gate + repo state as authoritative. If the gate is RED this bug is NOT fixed regardless of ledger_status — frame the duty as 'fix the failing behavior', never 'verify the existing fix + add a test'."

// GroundTruthReconciler wraps a tool.Provider and rewrites work.bug_read results so a
// decomposing or fixing agent cannot mistake the ledger's resolution STATUS for ground
// truth (bug 1145: an orchestrator handed a status='fixed' bug_read relayed "already
// fixed, just verify it" into the spawned coding duty, inverting "fix the bug" into
// "verify the existing fix + add a test", then thrashed the escalation ladder to the
// cost ceiling without converging on a repo that was actually RED).
//
// The transform is minimal and preserves the bug's SUBSTANCE (problem_statement,
// acceptance_criteria, constraints, …): it only (a) relocates the resolution-status
// fields into a nested `ledger_status` object — so a weak model no longer reads a bare
// top-level status='fixed' as a premise — and (b) adds `ground_truth_directive`. It is
// fail-soft and pass-through for everything else: a non-bug_read call, a failed
// dispatch, a Value that is not a JSON object, or a record carrying none of the status
// fields is returned unchanged. It is idempotent — a second pass finds the fields
// already relocated and makes no further change.
//
// It wraps the per-loop DISPATCH provider (the Dispatch-only seam the loop calls), not
// the spec-building provider, so tool-spec projection is untouched. Corpos-internal
// calls (e.g. the parse_context prober) dispatch through the raw provider and are not
// reconciled.
type GroundTruthReconciler struct{ inner tool.Provider }

// NewGroundTruthReconciler wraps inner so its work.bug_read results carry the
// ledger-status-is-not-ground-truth reconciliation.
func NewGroundTruthReconciler(inner tool.Provider) *GroundTruthReconciler {
	return &GroundTruthReconciler{inner: inner}
}

// Dispatch forwards to the inner provider and, for a successful work.bug_read, folds
// the resolution-status fields under `ledger_status` and attaches the directive.
func (g *GroundTruthReconciler) Dispatch(ctx context.Context, c tool.Call) tool.Result {
	res := g.inner.Dispatch(ctx, c)
	if !res.OK || c.Surface != "work" || c.Action != "bug_read" {
		return res
	}
	m, ok := res.Value.(map[string]any)
	if !ok {
		return res
	}
	status := map[string]any{}
	for _, f := range groundTruthStatusFields {
		if v, present := m[f]; present {
			status[f] = v
			delete(m, f)
		}
	}
	if len(status) == 0 {
		// Not a bug record carrying resolution status (e.g. an ok-shaped non-record
		// body, or an already-reconciled result): leave it untouched.
		return res
	}
	m["ledger_status"] = status
	m["ground_truth_directive"] = groundTruthDirective
	res.Value = m
	return res
}
