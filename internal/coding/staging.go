package coding

import "fmt"

// --- staging interventions (the "stage" of start/resume/stage) ---------------
//
// Each intervention only STAGES — it mutates run state and sets the chain back to
// PENDING; nothing re-executes until Resume. The richer operator vocabulary
// (edit / branch_fix / force_advance / re_extract), which the router-driven
// operator composes, lands in task 3 on top of these primitives.

// stageable reports whether the chain is in a state an intervention may act on.
func (s *RunState) stageable() bool {
	return s.Status == ChainPaused || s.Status == ChainFailed || s.Status == ChainPending
}

// Retry resets the current AT to PENDING so Resume re-runs it.
func (o *Orchestrator) Retry(state *RunState) error {
	if !state.stageable() {
		return notStageable(state)
	}
	ar := state.current()
	if ar == nil {
		return fmt.Errorf("retry: no current AT at position %d", state.CurrentPosition)
	}
	resetAT(ar)
	state.Status = ChainPending
	state.FailedATSlug = ""
	o.emitAT(ar)
	o.emitChain(state)
	return nil
}

// Skip marks the current AT SKIPPED and advances past it.
func (o *Orchestrator) Skip(state *RunState) error {
	if !state.stageable() {
		return notStageable(state)
	}
	ar := state.current()
	if ar == nil {
		return fmt.Errorf("skip: no current AT at position %d", state.CurrentPosition)
	}
	ar.Status = ATSkipped
	o.emitAT(ar)
	state.CurrentPosition++
	state.Status = ChainPending
	state.FailedATSlug = ""
	o.emitChain(state)
	return nil
}

// Abort marks the chain ABORTED (terminal; workspaces are preserved on disk).
func (o *Orchestrator) Abort(state *RunState) {
	state.Status = ChainAborted
	o.emitChain(state)
}

// notStageable builds the error returned when an intervention is attempted on a
// chain that is not paused/failed/pending.
func notStageable(state *RunState) error {
	return fmt.Errorf("intervention requires the chain to be paused/failed/pending; status is %q", state.Status)
}
