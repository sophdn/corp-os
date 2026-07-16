package coding

import (
	"context"

	"corpos/internal/cost"
	"corpos/internal/model"
	"corpos/internal/router"
)

// InterventionRecord is the per-attempt telemetry the operator seat emits: which
// tier acted, the op it chose, whether it carried the point, and what it cost.
// These are the records the event-ledger projection (task 6) persists.
type InterventionRecord struct {
	Point        string
	Attempt      int
	Tier         string // "mid" | "strong"
	Model        string
	Op           OperatorOp
	Reason       string
	Carried      bool
	Escalated    bool
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	Classify     string
	LandedAtMid  bool
}

// SeatResult is the outcome of an operator-seat run over a chain.
type SeatResult struct {
	FinalStatus      ChainStatus
	Interventions    []InterventionRecord
	TotalUSD         float64
	EscalationCauses map[string]string // point → classification | authoring | undetermined
}

// CarriedFraction is the fraction of intervention points the MID tier carried
// end-to-end (landed AND never escalated) — the headline operator-carried metric.
func (r SeatResult) CarriedFraction() float64 {
	points := map[string]bool{}
	escalated := map[string]bool{}
	carried := map[string]bool{}
	for _, rec := range r.Interventions {
		points[rec.Point] = true
		if rec.Escalated {
			escalated[rec.Point] = true
		}
	}
	for _, rec := range r.Interventions {
		if rec.LandedAtMid && !escalated[rec.Point] {
			carried[rec.Point] = true
		}
	}
	if len(points) == 0 {
		return 1.0
	}
	return float64(len(carried)) / float64(len(points))
}

// OperatorSeat drives the intervention loop: on a gate failure it assembles
// context (incl. current-package files — Finding 0), asks the operator tier the
// corpos router selected for one decision, applies it, resumes, and escalates
// mid→strong after K failed mid attempts on a point. It is the corpos router's
// escalate_on contract realized for the coding chain.
type OperatorSeat struct {
	orch             *Orchestrator
	operator         Operator
	mid              model.Adapter
	coder            model.Adapter // optional intermediate authoring rung (DeepSeek)
	hasCoder         bool
	strong           model.Adapter
	k                int
	maxInterventions int
}

// SeatOption configures an OperatorSeat.
type SeatOption func(*OperatorSeat)

// WithK sets the mid-tier attempts per point before escalating to strong (≥1).
func WithK(k int) SeatOption {
	return func(s *OperatorSeat) {
		if k >= 1 {
			s.k = k
		}
	}
}

// WithMaxInterventions caps total interventions across the run (a safety bound).
func WithMaxInterventions(n int) SeatOption {
	return func(s *OperatorSeat) {
		if n > 0 {
			s.maxInterventions = n
		}
	}
}

// WithCoderRung inserts an intermediate coder rung (DeepSeek) BETWEEN mid and
// strong, so the hard-AUTHORING escalation is absorbed below the strong (Opus)
// price before falling through to it. The ladder becomes mid → coder → strong. A
// nil adapter is ignored.
func WithCoderRung(coder model.Adapter) SeatOption {
	return func(s *OperatorSeat) {
		if coder != nil {
			s.coder = coder
			s.hasCoder = true
		}
	}
}

// ladder returns the escalation rungs cheapest→strongest for this seat.
func (s *OperatorSeat) ladder() []model.Adapter {
	rungs := []model.Adapter{s.mid}
	if s.hasCoder {
		rungs = append(rungs, s.coder)
	}
	return append(rungs, s.strong)
}

// NewOperatorSeat builds the seat over an orchestrator, an operator, and the mid +
// strong adapters (the router ladder rungs).
func NewOperatorSeat(orch *Orchestrator, operator Operator, mid, strong model.Adapter, opts ...SeatOption) *OperatorSeat {
	s := &OperatorSeat{orch: orch, operator: operator, mid: mid, strong: strong, k: 2, maxInterventions: 30}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run drives the operator seat over a failed/paused chain until it reaches a
// terminal state, hits an impasse (strong also failed a point), or the
// intervention cap. It returns the per-point telemetry + escalation-cause scoring.
func (s *OperatorSeat) Run(ctx context.Context, state *RunState) SeatResult {
	res := SeatResult{}
	total := 0
	for needsOperator(state.Status) && total < s.maxInterventions {
		point := failedPoint(state)
		if point == "" {
			break
		}
		// A fresh per-point router over the rung ladder (mid floor → [coder] →
		// strong top): escalate after K tool errors, top rung bounded to one turn.
		rt := router.NewLadder(s.ladder(), 0,
			router.WithEscalation(s.k, 1), router.WithBoundedTop(1))
		attempt := 0
		failedAttempts := 0
		pointCarried := false
		for total < s.maxInterventions {
			adapter := rt.NextAdapter()
			tier := s.tierName(adapter)
			onTopRung := adapter.Model() == s.strong.Model()
			attempt++

			octx := s.gatherContext(ctx, state, point)
			dec, usage, err := s.operator.Decide(ctx, adapter, octx)
			total++
			if err != nil {
				return s.finish(res, state)
			}
			applyErr := s.apply(ctx, state, point, dec)
			if applyErr != nil {
				return s.finish(res, state)
			}
			s.orch.Resume(ctx, state)
			carried := pointCleared(state, point)

			c := cost.Price(adapter.Model(), usage.InputTokens, usage.OutputTokens)
			res.TotalUSD += c
			res.Interventions = append(res.Interventions, InterventionRecord{
				Point: point, Attempt: attempt, Tier: tier, Model: adapter.Model(),
				Op: dec.Op, Reason: dec.Reason, Carried: carried, Escalated: tier != "mid",
				InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CostUSD: c,
				Classify: octx.ClassifyHint, LandedAtMid: carried && tier == "mid",
			})

			if carried {
				pointCarried = true
				break
			}
			// A spent per-duty respawn budget is a structural bound no operator edit can
			// lift — the orchestrator refused to re-enter the worker→gate loop. Halt
			// honestly here instead of spending more interventions toward the cost ceiling
			// (chain 392 task 3315). Distinct from the onTopRung impasse: the cap can bite
			// while still on a cheap rung.
			if cap := state.findAT(point); cap != nil && cap.WorkerStatus == WorkerRespawnCapReached {
				return s.finish(res, state)
			}
			if onTopRung {
				// The strong top rung also failed this point → impasse; stop.
				return s.finish(res, state)
			}
			// Cumulative failed-attempt count drives the router's repeated-tool-
			// error trigger: at K it escalates the ladder one rung (mid→strong).
			failedAttempts++
			rt.Observe(router.Signals{ToolErrors: failedAttempts})
		}
		if !pointCarried {
			break
		}
	}
	return s.finish(res, state)
}

// finish stamps the final status + escalation-cause scoring.
func (s *OperatorSeat) finish(res SeatResult, state *RunState) SeatResult {
	res.FinalStatus = state.Status
	res.EscalationCauses = scoreEscalationCauses(res.Interventions)
	return res
}

// tierName maps an adapter to its rung label (mid | coder | strong).
func (s *OperatorSeat) tierName(a model.Adapter) string {
	switch a.Model() {
	case s.strong.Model():
		return "strong"
	case s.midModel():
		return "mid"
	default:
		if s.hasCoder && a.Model() == s.coder.Model() {
			return "coder"
		}
		return "mid"
	}
}

// midModel returns the mid adapter's model id ("" if unset, for tests).
func (s *OperatorSeat) midModel() string {
	if s.mid == nil {
		return ""
	}
	return s.mid.Model()
}

// gatherContext assembles the operator's input for a failed point, including the
// mandatory current-package files (Finding 0) and the worker's diff.
func (s *OperatorSeat) gatherContext(ctx context.Context, state *RunState, point string) OperatorContext {
	ar := state.findAT(point)
	if ar == nil {
		return OperatorContext{FailedATSlug: point}
	}
	_, hint := classifyFailure(ar)
	return OperatorContext{
		FailedATSlug: point,
		Goal:         ar.Spec.Goal,
		WorkerStatus: ar.WorkerStatus,
		Diagnostic:   ar.Diagnostic,
		GateTails:    ar.Diagnostic,
		PackageFiles: currentPackageFiles(ctx, s.orch.repo, ar.Spec),
		Diff:         captureDiff(ctx, s.orch.repo, ar),
		ClassifyHint: hint,
	}
}

// apply maps an operator decision onto an orchestrator intervention.
func (s *OperatorSeat) apply(ctx context.Context, state *RunState, point string, dec OperatorDecision) error {
	switch dec.Op {
	case OpEdit:
		return s.orch.InterveneEdit(state, Edit{Goal: dec.Goal})
	case OpBranchFix:
		target := dec.TargetAT
		if target == "" {
			target = point
		}
		return s.orch.InterveneBranchFix(ctx, state, target, "", 0)
	case OpForceAdvance:
		return s.orch.InterveneForceAdvance(ctx, state, dec.CommitSHA, dec.Justification)
	case OpSkip:
		return s.orch.Skip(state)
	case OpAbort:
		s.orch.Abort(state)
		return nil
	default:
		return nil
	}
}

// needsOperator reports whether the chain is awaiting an operator decision.
func needsOperator(st ChainStatus) bool {
	return st == ChainFailed || st == ChainPaused
}

// failedPoint returns the slug the operator should act on: the failed AT slug, or
// the current AT slug when paused.
func failedPoint(state *RunState) string {
	if state.FailedATSlug != "" {
		return state.FailedATSlug
	}
	if ar := state.current(); ar != nil {
		return ar.Slug
	}
	return ""
}

// pointCleared reports whether a point is no longer the blocker: the chain reached
// a non-operator state, the point's AT succeeded/was superseded, or the chain is
// now failing on a different slug.
func pointCleared(state *RunState, point string) bool {
	if !needsOperator(state.Status) {
		return true
	}
	if ar := state.findAT(point); ar != nil && (ar.Status == ATSuccess || ar.Status == ATSkipped) {
		return true
	}
	// Still awaiting an operator: cleared only if the blocker moved to a different
	// slug (e.g. a branch_fix superseded this point and a downstream AT now fails).
	return state.FailedATSlug != "" && state.FailedATSlug != point
}

// scoreEscalationCauses labels, for each escalated point, WHY the mid tier failed
// it: "authoring" if mid tried the op that ultimately landed but its payload missed
// (diagnosis right, code wrong), "classification" if mid never tried the landing
// op (mis-diagnosed), else "undetermined".
func scoreEscalationCauses(records []InterventionRecord) map[string]string {
	causes := map[string]string{}
	byPoint := map[string][]InterventionRecord{}
	for _, r := range records {
		byPoint[r.Point] = append(byPoint[r.Point], r)
	}
	for point, recs := range byPoint {
		anyEscalated := false
		for _, r := range recs {
			if r.Escalated {
				anyEscalated = true
			}
		}
		if !anyEscalated {
			continue
		}
		landing := OperatorOp("")
		for _, r := range recs {
			if r.Carried {
				landing = r.Op
				break
			}
		}
		midOps := map[OperatorOp]bool{}
		for _, r := range recs {
			if r.Tier == "mid" {
				midOps[r.Op] = true
			}
		}
		switch {
		case landing == "":
			causes[point] = "undetermined"
		case midOps[landing]:
			causes[point] = "authoring"
		default:
			causes[point] = "classification"
		}
	}
	return causes
}
