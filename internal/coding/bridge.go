package coding

import (
	"context"
	"fmt"

	"corpos/internal/profile"
)

// This file is the duty→organ bridge: it activates the operator-seat organ as the
// live coding path. The orchestrate agent decomposes a goal into duties and
// delegates a CODING duty (free text) here; the bridge wraps the duty as a
// single-AT coding.Chain, drives the orchestrator's worker→gate→revise loop, and on
// failure hands the run to the OperatorSeat (branch_fix carryover + K→strong
// escalation). It is COMPOSITION over the existing organ — no organ mechanics are
// re-created here, only seeded and driven — so the organ's characterization net
// stays the authority for its behavior.

// NewRunID mints a fresh run id for a coding run (the same crypto-random id the
// orchestrator uses internally), exported so the live wiring can derive the chain
// slug + worktree dir from it before Start.
func NewRunID() string { return randomRunID() }

// BridgeChain wraps a free-text coding duty as a single-AT coding.Chain. The AT's
// gate, protected paths, and revise budget are read off the resolved coding profile
// (atomic-coding-chain) — they are ALREADY configured there (the orchestrator-owned
// build/test gate), so the bridge does not re-derive them. Gate is a list of argv
// vectors, so the profile's single VerifyCommand wraps as [][]string{cmd}.
func BridgeChain(slug, duty, targetRepo string, p *profile.JobProfile) Chain {
	at := AtomicTask{
		Slug:          "duty",
		Goal:          duty,
		Worker:        WorkerConfig{Kind: WorkerModel},
		MaxIterations: p.VerifyMaxRounds,
	}
	if len(p.VerifyCommand) > 0 {
		at.Gate = [][]string{p.VerifyCommand}
	}
	if len(p.ProtectPaths) > 0 {
		at.Protected = append([]string(nil), p.ProtectPaths...)
	}
	return Chain{
		Slug:       slug,
		TargetRepo: targetRepo,
		// BaseBranch left empty: the git Repo seam forks off the target repo's actual
		// default/current branch (bug 1077). A hardcoded "main" broke master-default repos.
		Tasks: []AtomicTask{at},
	}
}

// BridgeResult is the organ outcome the spawn path maps back into its tool result.
// Answer is a synthesized status line (the integration commit on success, the final
// status + last diagnostic on failure); CostUSD is the operator-decision spend the
// seat accrued (the worker spawns accrue separately on the spawner's own telemetry).
type BridgeResult struct {
	Answer  string
	CostUSD float64
	// HighestTierModel is the model id of the highest tier this duty's atom escalated to
	// (across its within-organ respawns + any seat resume). The live coding-path wiring
	// carries it into the NEXT coding-path invocation's WithSeededTier so a re-spawned
	// coding worker starts there instead of the local floor (bug 1146, orchestrate layer).
	HighestTierModel string
	// Success reports whether the coding chain reached a verified green (ChainSuccess). The
	// live wiring uses it to bound cross-invocation RESPAWN thrash: a run of consecutive
	// non-success invocations against the same goal is capped so the orchestrator cannot
	// fan one stuck bug out into many escalating workers (Run-53: 5–6 workers → Opus, $3.65,
	// no fix). A success resets the counter, so a legitimate multi-atom feature is unaffected.
	Success bool
}

// RunDuty drives one coding duty through the organ: Start seeds the run + the
// integration branch, RunToCompletion runs the single AT through the gate, and on a
// non-success the OperatorSeat intervenes (carryover + escalation) until terminal.
// It never returns a Go error — an honest organ failure comes back as a BridgeResult
// with a failure Answer, the same contract a bare worker reporting failure has, so
// the orchestrator can read the verdict and decide a follow-up.
//
// A nil seat (no escalation rungs wired) drives the orchestrator alone; the slice
// always wires a seat, but RunDuty stays seat-optional so it is drivable sans the
// adapters in a test.
func RunDuty(ctx context.Context, orch *Orchestrator, seat *OperatorSeat, chain Chain, runID string) (BridgeResult, error) {
	state, err := orch.Start(ctx, chain, runID)
	if err != nil {
		return BridgeResult{}, fmt.Errorf("coding bridge start: %w", err)
	}
	state = orch.RunToCompletion(ctx, state)
	var cost float64
	if state.Status != ChainSuccess && seat != nil {
		res := seat.Run(ctx, state)
		cost = res.TotalUSD
	}
	// On any green chain (direct or seat-recovered), surface the landed integration
	// commit to the target repo's working tree so the spawning orchestrator's next
	// fs.read sees the deliverable. Without this the work is stranded on the
	// coding/runs/<id>/integration branch and the orchestrator reads a stale tree,
	// cannot reconcile the worker's success, and burns the frontier rung to a halt.
	if state.Status == ChainSuccess {
		if err := orch.repo.Promote(ctx); err != nil {
			state.PromoteDiagnostic = err.Error()
		}
	}
	// Surface the highest tier the duty's atom reached so the live wiring can carry it into
	// the next coding-path invocation (bug 1146). The bridge builds a single-AT chain, so the
	// first (only) AT holds the duty's escalation state.
	var reachedTier string
	if len(state.ATs) > 0 {
		reachedTier = state.ATs[0].HighestTierModel
	}
	return BridgeResult{Answer: synthesizeAnswer(state), CostUSD: cost, HighestTierModel: reachedTier, Success: state.Status == ChainSuccess}, nil
}

// synthesizeAnswer renders the run's terminal verdict for the spawn result: on a
// green chain, the integration commit (the landed work); otherwise the final status
// plus the failed AT's diagnostic, so the orchestrator sees WHY it stuck.
func synthesizeAnswer(state *RunState) string {
	if state.Status == ChainSuccess {
		sha := integrationCommit(state)
		base := "coding chain succeeded"
		if sha != "" {
			base = fmt.Sprintf("coding chain succeeded; integration commit %s", sha)
		}
		if state.PromoteDiagnostic != "" {
			// The work landed on the integration branch but could not be surfaced to
			// the working tree; tell the caller so it doesn't read a stale tree and
			// conclude nothing happened.
			return fmt.Sprintf("%s (WARNING: not promoted to working tree: %s)", base, state.PromoteDiagnostic)
		}
		// Clean success: the gate ran and the change is on the working tree. Tell the
		// spawning orchestrator the duty is DONE so it finishes instead of spawning a
		// redundant coding-chain to re-verify (run-23: that burned the bounded frontier
		// rung to a strong-bound halt). Confirming with a read is fine; re-running the
		// work is not.
		return base + " — gate-verified and landed on the working tree; this duty is DONE, synthesize and finish (a confirming read is enough — do NOT spawn another coding run to re-verify)."
	}
	point := failedPoint(state)
	diag := ""
	if ar := state.findAT(point); ar != nil {
		diag = ar.Diagnostic
	}
	if diag == "" {
		diag = "(no diagnostic)"
	}
	return fmt.Sprintf("coding chain %s on %q: %s", state.Status, point, diag)
}

// integrationCommit returns the SHA of the last AT that landed on the integration
// branch (the run's final integrated state), or "" if none committed.
func integrationCommit(state *RunState) string {
	for i := len(state.ATs) - 1; i >= 0; i-- {
		if state.ATs[i].CommitSHA != "" {
			return state.ATs[i].CommitSHA
		}
	}
	return ""
}
