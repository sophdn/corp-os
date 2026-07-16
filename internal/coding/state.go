package coding

// ChainStatus is the lifecycle state of a chain run.
type ChainStatus string

const (
	ChainPending ChainStatus = "pending"
	ChainRunning ChainStatus = "running"
	ChainPaused  ChainStatus = "paused"
	ChainSuccess ChainStatus = "success"
	ChainFailed  ChainStatus = "failed"
	ChainAborted ChainStatus = "aborted"
)

// ATStatus is the lifecycle state of one AT within a run.
type ATStatus string

const (
	ATPending ATStatus = "pending"
	ATRunning ATStatus = "running"
	ATSuccess ATStatus = "success"
	ATFailed  ATStatus = "failed"
	ATSkipped ATStatus = "skipped"
)

// ATRecord is the per-AT run state. Unlike the bench (which kept spec and run
// state in two parallel arrays that had to be renumbered in lockstep — a bug
// class), the record CARRIES its spec, so the run is a single authority: an edit
// mutates Spec in place, an inject/branch_fix inserts a record, and there is no
// parallel array to drift. It is also the projection unit: in task 6 these records
// become a fold over the toolkit event ledger rather than a stored struct, but the
// shape is the contract either way.
type ATRecord struct {
	// Slug is the stable identity (an edit may not change it).
	Slug string
	// Spec is the AT's (possibly intervention-mutated) configuration.
	Spec         AtomicTask
	Position     int
	Status       ATStatus
	Iterations   int
	Outputs      map[string]any
	CommitSHA    string
	WorkerStatus WorkerStatus
	Diagnostic   string
	// WorktreePath is the AT's working tree while it runs; it is cleared on
	// success and PRESERVED on failure so the operator can inspect the diff.
	WorktreePath string
	// Flags are the structured reward-hack signals raised against the worker's diff
	// (test-file tampering, test-only diff). They are surfaced in the run verdict and
	// carried on the at_status event — never silently swallowed.
	Flags []GateFlag
	// HighestTierModel is the model id of the highest tier any respawn of THIS atom has
	// escalated to so far. runWorkerLoop reads it to start the next respawn there
	// (Feedback.CarriedTierModel) and updates it from each attempt's reached tier, so
	// the escalated tier is carried across both revise iterations and operator-seat
	// resumes instead of being discarded (chain 392 task 3314). Deliberately NOT cleared
	// by resetAT: the tier an atom needed is a durable difficulty signal that should
	// survive a retry/intervention of the same atom.
	HighestTierModel string
	// RespawnCount is how many times the worker→gate loop has been (re-)ENTERED for this
	// atom across the whole duty (the initial run plus every operator-seat resume). The
	// per-duty respawn cap bounds it so one non-converging atom stops honestly instead of
	// thrashing to the cost ceiling (chain 392 task 3315). Like HighestTierModel it
	// persists across interventions (NOT cleared by resetAT) — it counts attempts at the
	// SAME duty, which a reset does not undo.
	RespawnCount int
	// LastRealDiagnostic is the last actual gate diagnostic any attempt on this atom
	// produced. It persists across resetAT (which an edit intervention calls, clearing
	// Diagnostic) so the per-duty respawn-cap stuck verdict can still surface WHY the atom
	// never converged, not just that it stopped (chain 392 task 3315).
	LastRealDiagnostic string

	// Branch-fix tracking (the supersession + fork semantics):
	// ParentATSlug is empty for original ATs and set to the target's slug for a
	// branch created by a branch_fix intervention; BranchIndex is 0 for originals
	// and 1+ for successive branches of the same target; ParentSHA is the
	// integration HEAD captured before the AT ran (its worktree fork point), preset
	// for a branch so it forks from the same upstream state the target saw.
	ParentATSlug string
	BranchIndex  int
	ParentSHA    string
}

// RunState is the full state of one chain run. It carries the resolved chain so
// interventions that mutate the chain (edit/inject/branch_fix) and resume operate
// over a single authority.
type RunState struct {
	RunID             string
	ChainSlug         string
	TargetRepo        string
	BaseBranch        string
	IntegrationBranch string
	Status            ChainStatus
	CurrentPosition   int
	ATs               []ATRecord
	FailedATSlug      string
	// PromoteDiagnostic is set when a green chain's integration branch could not be
	// fast-forwarded into the target repo's working tree (dirty/diverged tree). The
	// work stays on the integration branch; the answer surfaces this so the caller
	// is not left reading a stale tree believing nothing happened.
	PromoteDiagnostic string
}

// atAt returns a pointer to the AT record at the current position, or nil when the
// position is past the end of the chain.
func (s *RunState) current() *ATRecord {
	if s.CurrentPosition < 0 || s.CurrentPosition >= len(s.ATs) {
		return nil
	}
	return &s.ATs[s.CurrentPosition]
}

// findAT returns a pointer to the first AT record with the given slug, or nil.
func (s *RunState) findAT(slug string) *ATRecord {
	for i := range s.ATs {
		if s.ATs[i].Slug == slug {
			return &s.ATs[i]
		}
	}
	return nil
}

// renumber re-indexes AT positions after an insertion so Position matches slice
// order (kept consistent for projection + telemetry).
func (s *RunState) renumber() {
	for i := range s.ATs {
		s.ATs[i].Position = i
	}
}

// resetAT clears an AT's run fields back to a fresh PENDING state. Used by retry,
// edit (which retries), and branch_fix (resetting the superseded downstream AT).
func resetAT(r *ATRecord) {
	r.Status = ATPending
	r.Iterations = 0
	r.Outputs = nil
	r.CommitSHA = ""
	r.WorkerStatus = ""
	r.Diagnostic = ""
	r.WorktreePath = ""
	r.Flags = nil
}

// isResolved reports whether an AT no longer needs to run (succeeded or was
// skipped/superseded), so the run loop advances past it.
func (r ATRecord) isResolved() bool {
	return r.Status == ATSuccess || r.Status == ATSkipped
}
