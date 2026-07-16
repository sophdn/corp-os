package coding

import (
	"context"
	"fmt"
	"strings"
)

// WorkerForceAdvance marks an AT that an operator force-advanced past its gate.
const WorkerForceAdvance WorkerStatus = "force_advance"

// DefaultMaxBranches caps branch_fix branches per target.
const DefaultMaxBranches = 3

// Edit is an operator's in-place revision of the current (failed) AT's goal, and
// optionally its workspace. It is the cheap, precise lever: a corrected goal that
// pins exact symbols/scope (Finding 0). The slug is unchanged (an edit is a
// revision, not a rename), so it cannot drop downstream consumers.
type Edit struct {
	Goal      string
	Workspace []string // nil = inherit the existing workspace
}

// InterveneEdit replaces the current AT's goal (and optional workspace) and resets
// it so Resume re-runs the worker against the corrected spec.
func (o *Orchestrator) InterveneEdit(state *RunState, e Edit) error {
	if !state.stageable() {
		return notStageable(state)
	}
	ar := state.current()
	if ar == nil {
		return fmt.Errorf("edit: no current AT at position %d", state.CurrentPosition)
	}
	if strings.TrimSpace(e.Goal) == "" {
		return fmt.Errorf("edit requires a non-empty goal")
	}
	ar.Spec.Goal = e.Goal
	if e.Workspace != nil {
		ar.Spec.Workspace = e.Workspace
	}
	resetAT(ar)
	state.Status = ChainPending
	state.FailedATSlug = ""
	o.emitAT(ar)
	o.emitChain(state)
	return nil
}

// InterveneForceAdvance is the operator-authorized gate override: it accepts a
// specific commit as the current AT's success, advances the integration branch to
// it, and resets the next AT so it re-runs against the new state. The justification
// is recorded for audit (force_advance is the ONLY sanctioned gate bypass).
func (o *Orchestrator) InterveneForceAdvance(ctx context.Context, state *RunState, commitSHA, justification string) error {
	if !state.stageable() {
		return notStageable(state)
	}
	if strings.TrimSpace(commitSHA) == "" {
		return fmt.Errorf("force_advance requires a non-empty commit_sha")
	}
	if strings.TrimSpace(justification) == "" {
		return fmt.Errorf("force_advance requires a non-empty justification")
	}
	ar := state.current()
	if ar == nil {
		return fmt.Errorf("force_advance: no current AT at position %d", state.CurrentPosition)
	}
	if err := o.repo.ResetTo(ctx, commitSHA); err != nil {
		return fmt.Errorf("force_advance: advance integration to %q: %w", commitSHA, err)
	}
	ar.CommitSHA = commitSHA
	ar.Status = ATSuccess
	ar.WorkerStatus = WorkerForceAdvance
	ar.Diagnostic = "force_advance: " + justification
	ar.WorktreePath = ""
	o.emitAT(ar)
	state.CurrentPosition++
	if next := state.current(); next != nil {
		resetAT(next)
		o.emitAT(next)
	}
	state.Status = ChainPending
	state.FailedATSlug = ""
	o.emitChain(state)
	return nil
}

// InterveneBranchFix inserts a fresh branch AT that re-implements a target AT,
// augmenting its goal with the downstream failure diagnostic and the prior
// attempts' diffs; it supersedes the target (and any prior branches) as SKIPPED,
// rewinds the integration branch to the target's fork point, and resumes from the
// branch. The target must be the failed AT, the immediately-prior AT, or an AT
// whose own branch chain is failing; branches are capped at maxBranches.
func (o *Orchestrator) InterveneBranchFix(ctx context.Context, state *RunState, targetSlug, downstreamDiagnostic string, maxBranches int) error {
	if !state.stageable() {
		return notStageable(state)
	}
	if maxBranches <= 0 {
		maxBranches = DefaultMaxBranches
	}

	targetIdx := -1
	for i := range state.ATs {
		if state.ATs[i].Slug == targetSlug && state.ATs[i].BranchIndex == 0 {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return fmt.Errorf("branch_fix: no original AT with slug %q", targetSlug)
	}
	target := &state.ATs[targetIdx]

	failedIdx := state.CurrentPosition
	var failed *ATRecord
	if failedIdx >= 0 && failedIdx < len(state.ATs) {
		failed = &state.ATs[failedIdx]
	}
	isFailedAT := targetIdx == failedIdx
	isPriorAT := targetIdx == failedIdx-1
	isBranchOfTarget := failed != nil && failed.ParentATSlug == targetSlug
	if !(isFailedAT || isPriorAT || isBranchOfTarget) {
		return fmt.Errorf("branch_fix: target %q (position %d) must be the failed AT (position %d), the one before it, or an AT whose branch chain is failing", targetSlug, targetIdx, failedIdx)
	}

	var existing []*ATRecord
	for i := range state.ATs {
		if state.ATs[i].ParentATSlug == targetSlug {
			existing = append(existing, &state.ATs[i])
		}
	}
	nextBranchIndex := len(existing) + 1
	if nextBranchIndex > maxBranches {
		return fmt.Errorf("branch_fix: max_branches=%d exhausted for target %q", maxBranches, targetSlug)
	}

	if downstreamDiagnostic == "" {
		if failed != nil && failed.Diagnostic != "" {
			downstreamDiagnostic = failed.Diagnostic
		} else {
			downstreamDiagnostic = "(no diagnostic recorded)"
		}
	}

	// Collect prior-attempt diffs (target + each prior branch) BEFORE superseding,
	// while their CommitSHA/WorktreePath are still set.
	var priorDiffs []labeledDiff
	for _, rec := range append([]*ATRecord{target}, existing...) {
		if d := captureDiff(ctx, o.repo, rec); d != "" {
			label := "original target (" + rec.Slug + ")"
			if rec.BranchIndex > 0 {
				label = fmt.Sprintf("branch %d of %s", rec.BranchIndex, rec.Slug)
			}
			priorDiffs = append(priorDiffs, labeledDiff{label, d})
		}
	}

	branchSlug := fmt.Sprintf("%s-fix%d", targetSlug, nextBranchIndex)
	branchSpec := target.Spec
	branchSpec.Slug = branchSlug
	branchSpec.Goal = buildBranchFixGoal(target.Spec.Goal, downstreamDiagnostic, priorDiffs)
	branchParentSHA := target.ParentSHA

	// If the target succeeded but a downstream AT failed, rewind integration to the
	// target's fork point so the branch forks from the same upstream state, and
	// reset the failed downstream AT.
	if targetIdx != failedIdx && target.CommitSHA != "" {
		forkPoint := target.ParentSHA
		if forkPoint == "" {
			if head, err := o.repo.HeadSHA(ctx); err == nil {
				forkPoint = head
			}
		}
		_ = o.repo.ResetTo(ctx, forkPoint)
		if failed != nil {
			resetAT(failed)
			failed.ParentSHA = "" // recaptured when it re-runs
		}
	}

	// Supersede the target and any prior branches.
	target.CommitSHA = ""
	target.Status = ATSkipped
	// Capture the slugs whose records changed (for the log fork) before the slice
	// is reallocated by the insert (which invalidates the pointers).
	supersededSlugs := []string{targetSlug}
	for _, p := range existing {
		p.CommitSHA = ""
		p.Status = ATSkipped
		supersededSlugs = append(supersededSlugs, p.Slug)
	}
	var resetDownstreamSlug string
	if targetIdx != failedIdx && failed != nil {
		resetDownstreamSlug = failed.Slug
	}

	// Insert the branch AT at the target's position (no live pointers used after).
	branchRec := ATRecord{
		Slug:         branchSlug,
		Spec:         branchSpec,
		Position:     targetIdx,
		Status:       ATPending,
		ParentATSlug: targetSlug,
		BranchIndex:  nextBranchIndex,
		ParentSHA:    branchParentSHA,
	}
	tail := append([]ATRecord{branchRec}, state.ATs[targetIdx:]...)
	state.ATs = append(state.ATs[:targetIdx:targetIdx], tail...)
	state.renumber()
	state.CurrentPosition = targetIdx
	state.Status = ChainPending
	state.FailedATSlug = ""

	// Fork the log: the branch is an insert; the superseded/reset records are status
	// deltas. Emit the insert first so Fold creates the branch at its position.
	if br := state.findAT(branchSlug); br != nil {
		o.emitter.Emit(Event{Kind: EvATInserted, AT: *br})
	}
	for _, slug := range supersededSlugs {
		if ar := state.findAT(slug); ar != nil {
			o.emitAT(ar)
		}
	}
	if resetDownstreamSlug != "" {
		if ar := state.findAT(resetDownstreamSlug); ar != nil {
			o.emitAT(ar)
		}
	}
	o.emitChain(state)
	return nil
}

// labeledDiff pairs a prior attempt's label with its diff text.
type labeledDiff struct {
	label string
	diff  string
}

// buildBranchFixGoal composes the augmented goal for a branch AT: the original
// goal, the downstream failure context, and the prior attempts' diffs to learn
// from. It instructs the worker to write the corrected files via its fs tools.
func buildBranchFixGoal(originalGoal, downstreamDiagnostic string, priorDiffs []labeledDiff) string {
	var b strings.Builder
	b.WriteString("You are revising a prior implementation attempt that failed validation.\n\n")
	fmt.Fprintf(&b, "ORIGINAL GOAL:\n%s\n\n", originalGoal)
	fmt.Fprintf(&b, "FAILURE CONTEXT (from the downstream gate or your own gate):\n%s\n", downstreamDiagnostic)
	if len(priorDiffs) > 0 {
		b.WriteString("\nThe following prior attempts failed. Each diff is against the same fork point. Study them, recognize the failure pattern, and avoid repeating it.\n")
		for _, pd := range priorDiffs {
			fmt.Fprintf(&b, "\n--- %s ---\n```diff\n%s\n```\n", pd.label, pd.diff)
		}
	}
	b.WriteString("\nEmit the corrected files using your fs tools.")
	return b.String()
}
