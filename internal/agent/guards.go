package agent

import (
	"context"

	"corpos/internal/tool"
)

// Post-turn audit guards — ONE pipeline.
//
// The loop runs a set of structurally-unforgeable audits at a done-claim (a turn the
// model ended with NO real tool call). Each audit cross-checks the claim against the
// REAL dispatch record the loop computed AFTER generation — the hermes unforgeable-footer
// idiom — so a worker cannot revise its verdict by narrating success. Historically each
// audit was a bespoke file + a WithX option + a hand-wired branch in the loop's done-claim
// path; that is accretive (fine at a few, a pile at twenty). This factors the shared shape
// into ONE light Guard interface + a registry the loop runs uniformly, so adding a guard is
// implement + register, NOT another edit to the done-claim branch.
//
// It is deliberately a LIGHT seam (interface + ordered registry + a stage tag), NOT a policy
// engine: the guards stay small pure verdict functions; the registry just runs them in order
// and returns the first non-empty verdict per stage.

// GuardStage marks WHERE in the done-claim path a guard runs — the two decision points the
// loop already had. Keeping the stage explicit preserves the exact pre-refactor ordering and
// the Result-field mapping (a fabrication audit fails the claim as fabrication; a fake-green
// audit fails a PASSED gate as an escalation).
type GuardStage int

const (
	// StageFabrication runs BEFORE the verify gate: a done-claim not backed by real work
	// (no-work, prose-narrated tool call, an unsatisfied required read). A non-empty verdict
	// refuses the claim as fabrication (surfaced on Result.Fabricated).
	StageFabrication GuardStage = iota
	// StageFakeGreen runs AFTER the verify gate PASSED: a green that may be the worker's own
	// self-report (a worker-authored test the gate runs). A non-empty verdict refuses the
	// green as a fake pass (surfaced on Result.Escalate / VerifyFailed).
	StageFakeGreen
)

// GuardInput carries the unforgeable-footer inputs every post-turn audit reads: the final
// done-claim text and the turn's REAL dispatch record. It is a superset — a given guard reads
// only the fields it needs (a no-work audit reads Dispatches; a prose-tool-call audit reads
// FinalText) — so one input type serves the whole registry without each guard taking a bespoke
// signature.
type GuardInput struct {
	// FinalText is the assistant's done-claim text (the turn it ended with no tool call).
	FinalText string
	// Dispatches is the turn's ordered tool-dispatch record (the unforgeable work evidence).
	Dispatches []tool.Result
	// LoopGated reports whether the loop has its OWN authoritative verify gate that will run
	// right after this audit (onDeclaredDone). It suppresses the text-based verification-
	// fabrication backstop — redundant when a real gate follows — while leaving the no-work
	// and prose-narration signals armed. False for a bare/ungated worker (the backstop's
	// original target: a worker that pastes fake "PASSED" output with no gate behind it).
	LoopGated bool
}

// GuardVerdict is a guard's finding: an empty Reason means the claim is sound (the guard
// passes); a non-empty Reason is the ACTIONABLE next-step message surfaced to the operator /
// escalation reader (the fsorgan "identical strings" standard — name the problem AND the fix).
type GuardVerdict struct {
	// Reason is the actionable verdict message, or "" when the guard passes.
	Reason string
}

// ok reports whether the verdict is a pass (no finding).
func (v GuardVerdict) ok() bool { return v.Reason == "" }

// pass is the sound verdict (no finding).
func pass() GuardVerdict { return GuardVerdict{} }

// fail builds a non-empty (refusing) verdict with an actionable message.
func fail(reason string) GuardVerdict { return GuardVerdict{Reason: reason} }

// Guard is one post-turn audit. It is a light interface: a stable Name (for -print-guards and
// diagnostics), a Stage (which done-claim decision point it runs at), and Assess (the pure
// verdict over the unforgeable-footer inputs). Implementations are small value types holding
// only their config — the verdict logic stays a pure function so it is exhaustively unit-testable.
type Guard interface {
	// Name is the stable guard id (kebab-case), shown by -print-guards.
	Name() string
	// Describe is a one-line human description of what the guard refuses, shown by -print-guards.
	Describe() string
	// Stage is the done-claim decision point this guard runs at.
	Stage() GuardStage
	// Assess returns the guard's verdict over the done-claim inputs (empty Reason = sound).
	Assess(ctx context.Context, in GuardInput) GuardVerdict
}

// guardRegistry is the ordered set of post-turn audits the loop runs at a done-claim. Order is
// significant and preserved from the pre-refactor hand-wiring (first non-empty verdict at a
// stage wins), so the consolidation is behavior-preserving. The zero value is an empty registry
// (no guards), which is the read-only-duty default.
type guardRegistry struct {
	guards []Guard
}

// register appends a guard (nil-safe). Registration order IS run order within a stage.
func (r *guardRegistry) register(g Guard) {
	if g != nil {
		r.guards = append(r.guards, g)
	}
}

// assess runs every guard at the given stage in registration order and returns the FIRST
// non-empty verdict (and true), or a sound verdict (and false) when all pass. First-match-wins
// preserves the pre-refactor short-circuit ordering.
func (r *guardRegistry) assess(ctx context.Context, stage GuardStage, in GuardInput) (GuardVerdict, bool) {
	for _, g := range r.guards {
		if g.Stage() != stage {
			continue
		}
		if v := g.Assess(ctx, in); !v.ok() {
			return v, true
		}
	}
	return pass(), false
}

// names returns the registered guards in order (for -print-guards). It is read-only.
func (r *guardRegistry) all() []Guard {
	out := make([]Guard, len(r.guards))
	copy(out, r.guards)
	return out
}

// GuardCatalog returns one instance of every post-turn audit guard corpos knows about, in run
// order — the declarative, enumerable guard set the `-print-guards` view renders (sibling to
// -print-tools). It is the single source of truth for "which guards exist": a new guard added
// to this list is automatically enumerated, so the catalog cannot silently drift from the set
// the loop actually runs. The instances carry representative config (the message + stage are
// config-independent); the view reads Name/Describe/Stage off them.
func GuardCatalog() []Guard {
	return []Guard{
		WorkAudit{RequireMutation: true},
		RequiredReads{Paths: []string{"<declared-contract-source>"}},
		FakeGreenGuard{},
		ScaffoldFabricationGuard{},
	}
}

// StageName renders a GuardStage for display (the -print-guards view).
func (s GuardStage) String() string {
	switch s {
	case StageFabrication:
		return "fabrication (pre-verify; refuses -> Fabricated)"
	case StageFakeGreen:
		return "fake-green (post-green-gate; refuses -> Escalate)"
	default:
		return "unknown"
	}
}
