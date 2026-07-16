// Package router selects a model adapter per turn via a two-call contract
// (NextAdapter at the top of a turn, Observe after it), with symmetric
// availability fallback. The escalation engine is the toolkit's orchestrator-tier
// escalation contract (docs/ORCHESTRATOR_ESCALATION.md), ported from
// clients/escalation/{triggers.py,router.py}: a per-trigger detector over the
// closed 5-trigger taxonomy with de-escalation hysteresis. The contract's
// two-state cheap/escalated machine is generalised onto corpos' tier ladder —
// "cheap" is the floor rung, "escalated" is any rung above it.
//
// The router is an ordered ladder of model tiers (cheapest→strongest). A worker
// rests on its floor rung and climbs ONE rung per escalation edge, descending
// back toward the floor on clean turns. Two-tier {cheap, strong} is just a
// two-rung ladder (the New sugar); the three-rung {local, mid, strong} ladder of
// the 2026-06-02 routing decision (§4.6) is NewLadder, and the strong (Opus) rung
// is usage-bounded via WithBoundedTop so the frontier is escalation-only, not
// reachable per-turn by default.
//
// Observe returns an Edge describing any transition so the loop can emit an
// EscalationProposed event (on escalate edges) and record the local telemetry
// row. The router itself stays pure (sans-IO): emission is the caller's job.
package router

import (
	"fmt"
	"strconv"

	"corpos/internal/model"
)

// State is the router's coarse tier state: at the floor (resting) or above it
// (escalated). It stays two-valued across any ladder depth — finer position is
// read off NextAdapter().Model().
type State string

const (
	// StateCheap means the router rests on its floor rung.
	StateCheap State = "cheap"
	// StateEscalated means the router has climbed above its floor rung.
	StateEscalated State = "escalated"
)

// Trigger is one escalation trigger kind — the closed taxonomy mirrored from the
// toolkit's escalation contract (docs/ORCHESTRATOR_ESCALATION.md §2; the CHECK
// constraint in migration 080; the EscalationProposed event enum).
type Trigger string

const (
	// TriggerRetryExhaustion fires when a tool-call retry loop consumes its
	// budget without converging (corpos: the loop's max tool rounds exhausted).
	TriggerRetryExhaustion Trigger = "retry_exhaustion"
	// TriggerLowConfidence fires when the model's self-reported confidence in its
	// action falls below a floor. nil confidence (unmeasured) never fires.
	TriggerLowConfidence Trigger = "low_confidence"
	// TriggerRepeatedToolError fires on repeated structured tool errors in a turn.
	TriggerRepeatedToolError Trigger = "repeated_tool_error"
	// TriggerParseFailure fires when the model's output repeatedly fails to parse.
	TriggerParseFailure Trigger = "parse_failure"
	// TriggerExplicitHandoff fires on an explicit "I need the strong tier" signal.
	TriggerExplicitHandoff Trigger = "explicit_handoff"
)

// priority is the evaluation order when several triggers fire on one turn — the
// first match wins (ported from escalation/triggers.py PRIORITY). Explicit
// self-escalation first, then hard failures (won't-converge), then the soft
// confidence floor.
var priority = []Trigger{
	TriggerExplicitHandoff,
	TriggerRetryExhaustion,
	TriggerParseFailure,
	TriggerRepeatedToolError,
	TriggerLowConfidence,
}

// TriggerConfig is one trigger's tunable: the threshold it crosses and whether it
// is evaluated at all. Mirrors one escalation_thresholds row.
type TriggerConfig struct {
	ThresholdValue float64
	Enabled        bool
}

// Config is the effective per-trigger escalation config plus the de-escalation
// hysteresis K (consecutive clean turns before descending a rung). It mirrors the
// toolkit's escalation_thresholds rows, fetched at session start.
type Config struct {
	Triggers          map[Trigger]TriggerConfig
	DeEscalationTurns int
}

// DefaultConfig is the built-in fallback used when the toolkit threshold config
// can't be fetched — the global-default rows seeded by migration 080.
func DefaultConfig() Config {
	return Config{
		DeEscalationTurns: 2,
		Triggers: map[Trigger]TriggerConfig{
			TriggerRetryExhaustion:   {ThresholdValue: 2, Enabled: true},
			TriggerLowConfidence:     {ThresholdValue: 0.35, Enabled: true},
			TriggerRepeatedToolError: {ThresholdValue: 3, Enabled: true},
			TriggerParseFailure:      {ThresholdValue: 2, Enabled: true},
			TriggerExplicitHandoff:   {ThresholdValue: 1, Enabled: true},
		},
	}
}

// WithRepeatedToolError returns a copy of c with the repeated_tool_error trigger
// pinned to threshold (enabled), leaving the other triggers and K intact. It lets
// a caller keep an operator-set tool-error knob (corpos' -escalate-after flag)
// while taking the rest of the escalation policy from the toolkit config.
func (c Config) WithRepeatedToolError(threshold int) Config {
	triggers := make(map[Trigger]TriggerConfig, len(c.Triggers)+1)
	for k, v := range c.Triggers {
		triggers[k] = v
	}
	triggers[TriggerRepeatedToolError] = TriggerConfig{ThresholdValue: float64(threshold), Enabled: true}
	return Config{Triggers: triggers, DeEscalationTurns: c.DeEscalationTurns}
}

// Signals are the per-turn signals fed to Observe — the escalation trigger
// source. They mirror escalation/triggers.py TurnSignals: counts are per-turn
// window values; Confidence is nil when unmeasured (low_confidence then never
// fires). Detail is free-form evidence appended to the fired trigger's detail.
type Signals struct {
	ToolErrors      int
	ParseFailures   int
	RetriesUsed     int
	ExplicitHandoff int
	Confidence      *float64
	Detail          string
}

// EdgeDirection labels a transition Observe produced.
type EdgeDirection string

const (
	// EdgeNone means the turn caused no tier transition.
	EdgeNone EdgeDirection = ""
	// EdgeEscalate means the router climbed a rung; the caller SHOULD emit an
	// EscalationProposed event.
	EdgeEscalate EdgeDirection = "escalate"
	// EdgeDeescalate means the router descended a rung (local telemetry only — the
	// toolkit's escalation_propose requires a trigger, which de-escalation lacks).
	EdgeDeescalate EdgeDirection = "deescalate"
)

// Edge describes a transition produced by Observe. On an escalate edge Trigger /
// FiredThreshold / Detail are set; on a de-escalate edge they are zero. FromModel
// and ToModel are the rung models on either side of the transition. StateBefore /
// StateAfter use the toolkit's three-value enum (cheap / escalated / de_escalated)
// so the caller can fill the EscalationProposed payload directly.
type Edge struct {
	Direction      EdgeDirection
	Trigger        Trigger
	FiredThreshold float64
	FromModel      string
	ToModel        string
	StateBefore    string
	StateAfter     string
	Detail         string
}

// Router picks a tier adapter per turn over an ordered ladder.
type Router struct {
	tiers   []model.Adapter // ordered cheapest(index 0)→strongest(index len-1)
	floor   int             // resting rung index
	cur     int             // current rung index
	highest int             // highest rung index actually SERVED across the run

	config      Config // per-trigger escalation config + de-escalation K
	cleanStreak int

	boundRung    int // bounded rung index (-1 = none); the usage-capped top rung
	boundMax     int // max turns the bounded rung may serve (0 = unbounded)
	boundServed  int // turns the bounded rung has served
	boundBlocked int // climbs refused because the bound was spent

	// stickyTop is set when a no-progress stall (EscalateForNoProgress, the
	// circuit-breaker's last resort) escalates onto the top rung. While set, the
	// per-turn usage bound is bypassed so the strong rung is NOT bounced back into a
	// floor that already proved it can't progress (the run-10 trap: opus served 2
	// bounded turns, then 8 escalations were blocked and the run bounced the stuck
	// floor to death). The bound still gates ROUTINE escalation (Observe / faults);
	// only the stuck last-resort sticks, and total spend stays capped by the
	// loop's -max-cost-usd ceiling. Cleared on any de-escalation (a clean recovery
	// returns the session to bounded frontier use).
	stickyTop bool

	coldStartFallbacks        int
	strongUnavailableFallback int

	// strongBudget, when set, is a SHARED cross-worker ceiling on strong-rung (Opus) turns. Unlike
	// boundMax (a per-router turn cap that resets for every fresh worker), it survives respawns: a
	// stuck atom's re-spawned workers all share one budget, so once the run's strong turns are spent
	// the frontier is refused tree-wide (bug 1165(b)). It guards the same top rung as boundMax and is
	// enforced alongside it in NextAdapter. Nil = no shared budget (the prior per-worker-only behavior).
	strongBudget StrongBudget
}

// StrongBudget is a shared, cross-worker ceiling on strong-rung (Opus) turns. A router with one
// installed peeks Exhausted() before serving the guarded top rung and calls Take() when it does, so
// every worker in a spawn tree draws Opus turns from ONE pool (cost.StrongTurnBudget satisfies it).
// The interface keeps the router sans-IO (no dependency on internal/cost).
type StrongBudget interface {
	// Exhausted reports whether the run has served its strong-turn cap. It is a non-mutating peek —
	// the router calls it to decide whether to serve the strong rung, then Take()s when it does.
	Exhausted() bool
	// Take tallies one served strong-rung turn.
	Take()
}

// Option configures a Router.
type Option func(*Router)

// WithEscalation enables a simple single-trigger escalation: climb one rung once
// a turn reports >= toolErrorThreshold tool errors, and descend one rung after
// cleanTurns consecutive clean turns. It is sugar over WithConfig that enables
// only the repeated_tool_error trigger — the back-compat path for callers that
// don't consume the toolkit threshold config. Without it (or WithConfig) the
// router stays single-tier.
func WithEscalation(toolErrorThreshold, cleanTurns int) Option {
	return func(r *Router) {
		if r.config.Triggers == nil {
			r.config.Triggers = map[Trigger]TriggerConfig{}
		}
		r.config.Triggers[TriggerRepeatedToolError] = TriggerConfig{
			ThresholdValue: float64(toolErrorThreshold), Enabled: true,
		}
		if cleanTurns > 0 {
			r.config.DeEscalationTurns = cleanTurns
		}
	}
}

// WithConfig installs the full per-trigger escalation config (the toolkit's
// effective escalation_thresholds for the session's project). A non-nil Triggers
// map replaces the router's; a positive DeEscalationTurns overrides K.
func WithConfig(c Config) Option {
	return func(r *Router) {
		if c.Triggers != nil {
			r.config.Triggers = c.Triggers
		}
		if c.DeEscalationTurns > 0 {
			r.config.DeEscalationTurns = c.DeEscalationTurns
		}
	}
}

// WithBoundedTop caps how many turns the top (strongest) rung may serve in a
// session: a climb onto a spent top rung is refused (the router stays one rung
// below and records the block), so the frontier tier is escalation-only and
// bounded, not reachable per-turn. maxTurns <= 0 leaves the top rung unbounded.
func WithBoundedTop(maxTurns int) Option {
	return func(r *Router) {
		if maxTurns > 0 {
			r.boundRung = len(r.tiers) - 1
			r.boundMax = maxTurns
		}
	}
}

// WithSharedStrongBudget installs a cross-worker strong-turn budget (bug 1165(b)). When it reports
// Exhausted, the router refuses a climb onto the strong (top) rung exactly as a spent per-worker
// WithBoundedTop does — bouncing one rung down — and every served strong turn draws from the shared
// pool. It marks the top rung as guarded even without WithBoundedTop, so the shared budget alone can
// gate the frontier. A nil budget, or a ladder with no strong rung (fewer than two tiers), is a no-op.
func WithSharedStrongBudget(b StrongBudget) Option {
	return func(r *Router) {
		if b == nil || len(r.tiers) < 2 {
			return
		}
		r.strongBudget = b
		if r.boundRung < 0 {
			r.boundRung = len(r.tiers) - 1
		}
	}
}

// New builds a two-rung router over the cheap (floor) and strong adapters — the
// classic two-tier ladder.
func New(cheap, strong model.Adapter, opts ...Option) *Router {
	return NewLadder([]model.Adapter{cheap, strong}, 0, opts...)
}

// NewLadder builds a router over an ordered tier ladder (cheapest→strongest),
// with the worker resting on the floor rung. floor is clamped into range. A
// single-element ladder is a single-tier router that never escalates.
func NewLadder(tiers []model.Adapter, floor int, opts ...Option) *Router {
	if floor < 0 {
		floor = 0
	}
	if floor > len(tiers)-1 {
		floor = len(tiers) - 1
	}
	r := &Router{
		tiers:     tiers,
		floor:     floor,
		cur:       floor,
		highest:   floor,
		config:    Config{Triggers: map[Trigger]TriggerConfig{}, DeEscalationTurns: 2},
		boundRung: -1,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// CurrentModel reports the model id of the rung the router is currently resting on
// (r.cur), without the per-call bound/availability adjustment NextAdapter applies.
// The loop peeks it between rounds to detect a mid-turn rung change (an escalation
// from EscalateForFault/EscalateForStuckVerify lifts r.cur immediately) so the
// no-progress breaker can treat the lift as a fresh attempt rather than counting
// the pre-escalation stall against the freshly-escalated rung.
func (r *Router) CurrentModel() string { return r.tiers[r.cur].Model() }

// CurrentRung is the index of the rung the router is currently resting on (0 =
// cheapest/floor, len(tiers)-1 = strongest). The loop reads it to size the
// compaction budget to the ACTIVE rung's window: a worker that escalated off the
// floor onto a wide-window tier must get that tier's budget, not stay pinned to the
// floor model's (bug 1088 GAP-2a — escalation that buys a better model but not more
// context leaves the task starved).
func (r *Router) CurrentRung() int { return r.cur }

// State reports whether the router is resting on its floor or escalated above it.
func (r *Router) State() State {
	if r.cur > r.floor {
		return StateEscalated
	}
	return StateCheap
}

// WillServeFloor reports whether the NEXT NextAdapter call would route to the floor
// rung — either because the router is resting on it, OR because a spent usage bound
// would bounce the current (top) rung back down to the floor (the run-12 path: cur
// at the bounded top, bound spent, NextAdapter drops to the floor). It mirrors
// NextAdapter's bound logic WITHOUT mutating state, so the loop's proactive
// floor-fit guard can decide — before the call — whether a would-overflow prompt is
// about to be sent to the small-window floor. (Availability fallback is not peeked:
// an overflow forces escalation regardless.)
func (r *Router) WillServeFloor() bool {
	target := r.cur
	if r.topBounce(target) && target > 0 {
		target--
	}
	return target == r.floor
}

// ColdStartFallbacks counts low-rung-unavailable climbs (a cold floor tier).
func (r *Router) ColdStartFallbacks() int { return r.coldStartFallbacks }

// StrongUnavailableFallbacks counts climbs degraded back down because a higher
// rung was unavailable.
func (r *Router) StrongUnavailableFallbacks() int { return r.strongUnavailableFallback }

// BoundedTurns is how many turns the bounded (top) rung has served.
func (r *Router) BoundedTurns() int { return r.boundServed }

// BoundBlocked is how many climbs onto the bounded rung were refused because its
// usage cap was spent.
func (r *Router) BoundBlocked() int { return r.boundBlocked }

// BoundMax is the bounded rung's usage cap (0 = no bound configured).
func (r *Router) BoundMax() int { return r.boundMax }

// StrongBoundExhausted reports whether the run is resting on the bounded top rung as
// the no-progress LAST RESORT (stickyTop) with its usage bound already spent. The
// stickyTop bypass deliberately keeps NextAdapter on the strong rung rather than
// bouncing a proven-stuck floor to death (the run-10 trap) — but with the bound spent
// that bypass let the frontier serve UNBOUNDED turns (Opus served 12 against a bound
// of 2, $3.45 unbudgeted). This signal lets the loop HALT in that state instead: the
// frontier had its bounded turns and the task is still stuck, so neither more frontier
// nor a bounce to the dead floor is right — stop with an honest verdict. The bound
// thus caps frontier TURN CONSUMPTION while resting on the rung, not only the
// escalation EDGES onto it (bug 1087).
func (r *Router) StrongBoundExhausted() bool {
	return r.stickyTop && r.boundRung >= 0 && r.cur == r.boundRung && r.strongSpent()
}

// StrongBudgetExhausted reports whether the SHARED tree-wide strong-turn pool is spent — the reason a
// StrongBoundExhausted halt fired for the whole run rather than this one worker's own bounded turns.
// The loop reads it to word the halt verdict accurately (bug 1165(b)).
func (r *Router) StrongBudgetExhausted() bool { return r.strongBudgetExhausted() }

// bounded reports whether rung i is the per-worker usage-capped rung (WithBoundedTop).
func (r *Router) bounded(i int) bool { return r.boundMax > 0 && i == r.boundRung }

// guardsTop reports whether rung i is the strong (top) rung under a turn guard — a per-worker usage
// bound (WithBoundedTop) and/or a shared cross-worker strong budget (WithSharedStrongBudget). Both
// gate the same rung; either alone makes it guarded.
func (r *Router) guardsTop(i int) bool {
	return r.boundRung >= 0 && i == r.boundRung && (r.boundMax > 0 || r.strongBudget != nil)
}

// strongSpent reports whether the guarded top rung's turn allowance is spent — either the per-worker
// bound (boundServed >= boundMax) or the shared strong budget (Exhausted across the whole tree).
func (r *Router) strongSpent() bool {
	return r.perWorkerBoundSpent() || r.strongBudgetExhausted()
}

// perWorkerBoundSpent reports whether THIS router's own WithBoundedTop turn cap is used up.
func (r *Router) perWorkerBoundSpent() bool { return r.boundMax > 0 && r.boundServed >= r.boundMax }

// strongBudgetExhausted reports whether the SHARED cross-worker strong-turn pool is spent.
func (r *Router) strongBudgetExhausted() bool {
	return r.strongBudget != nil && r.strongBudget.Exhausted()
}

// topBounce reports whether a climb onto the guarded top rung must be refused (bounced one rung down).
// The two guards differ on the stickyTop bypass: the PER-WORKER bound respects it — a proven-stuck
// worker keeps resting on the frontier for its bounded turns rather than bouncing its dead floor to
// death (the run-10 trap; the loop then halts via StrongBoundExhausted). The SHARED budget does NOT
// bypass on sticky: it is the TREE-WIDE "we've spent enough Opus" verdict, so a fresh respawn's
// stickyTop must not inherit an unbounded frontier pass once the pool is spent (bug 1165(b)) — the
// original miss was exactly this, retry_exhaustion/parse_failure climbs set stickyTop and skipped it.
func (r *Router) topBounce(i int) bool {
	if !r.guardsTop(i) {
		return false
	}
	return (r.perWorkerBoundSpent() && !r.stickyTop) || r.strongBudgetExhausted()
}

// NextAdapter returns the adapter that should drive the next turn. It applies the
// usage bound first (a climb onto a spent top rung is refused, dropping a rung),
// then symmetric availability fallback so a turn is never routed into an adapter
// that would fail at Complete time: a cold low rung climbs to an available higher
// one (cold start); an unavailable higher rung degrades toward the floor.
func (r *Router) NextAdapter() model.Adapter {
	target := r.cur
	if r.topBounce(target) {
		r.boundBlocked++
		if target > 0 {
			target--
		}
	}
	chosen := r.tiers[target]
	if !chosen.Available() {
		if alt, ok := r.availableNeighbour(target); ok {
			target = alt
			chosen = r.tiers[target]
		}
	}
	if r.guardsTop(target) {
		// A served strong turn draws from BOTH the per-worker bound (if set) and the shared
		// cross-worker budget (if set), so respawns can't re-climb a spent pool.
		if r.boundMax > 0 {
			r.boundServed++
		}
		if r.strongBudget != nil {
			r.strongBudget.Take()
		}
	}
	if target > r.highest {
		r.highest = target
	}
	return chosen
}

// HighestModel reports the model id of the HIGHEST rung this router actually SERVED
// across the run (the rung NextAdapter handed out — not merely a rung escalation state
// climbed to without a turn). The coding orchestrator reads it after a spawned worker
// returns, to carry the tier that attempt reached into the next respawn's starting
// floor instead of re-paying the climb from the local floor (chain 392 task 3314).
func (r *Router) HighestModel() string { return r.tiers[r.highest].Model() }

// LiftFloorToModel raises the router's resting floor to the rung whose adapter model
// id is modelID, when that rung is ABOVE the current floor — so a coding respawn can
// begin at the tier a prior attempt escalated to rather than restarting at the local
// floor (chain 392 task 3314). The lift is CAPPED one rung below the usage-bounded top
// rung: a carried floor may rest on mid/coder but NEVER on the bounded strong (Opus)
// rung, so the frontier stays escalation-only (WithBoundedTop intact). An unknown
// model id, or one at/below the current floor (after the cap), is a no-op.
func (r *Router) LiftFloorToModel(modelID string) {
	if modelID == "" {
		return
	}
	target := -1
	for i, a := range r.tiers {
		if a.Model() == modelID {
			target = i
			break
		}
	}
	if target <= r.floor {
		return
	}
	// Never rest on the usage-bounded top rung: cap one rung below it so the bounded
	// frontier is reachable only by escalation, not adopted as a resting floor.
	if r.boundMax > 0 && r.boundRung >= 0 && target >= r.boundRung {
		target = r.boundRung - 1
	}
	if target <= r.floor {
		return
	}
	r.floor = target
	r.cur = target
	if target > r.highest {
		r.highest = target
	}
}

// availableNeighbour finds an available rung to fall back to when tiers[target]
// is unavailable: a rung at/below the floor climbs UP (cold start); a rung above
// the floor degrades DOWN toward the floor. It returns false when no other rung
// can serve (the caller then routes into the unavailable adapter unchanged).
func (r *Router) availableNeighbour(target int) (int, bool) {
	if target > r.floor {
		for i := target - 1; i >= 0; i-- {
			if r.tiers[i].Available() {
				r.strongUnavailableFallback++
				return i, true
			}
		}
		return 0, false
	}
	for i := target + 1; i < len(r.tiers); i++ {
		if r.tiers[i].Available() {
			r.coldStartFallbacks++
			return i, true
		}
	}
	return 0, false
}

// Observe folds a finished turn's signals into the ladder position and returns
// the transition, if any. The escalation decision is the ported contract: the
// highest-priority enabled trigger that fired (detect) climbs one rung; K
// consecutive clean (no-trigger) turns descend one rung. A fired trigger at the
// top rung is saturated — it resets the clean streak but climbs no further and
// emits no edge (two-state reference parity). De-escalation carries no trigger.
func (r *Router) Observe(s Signals) Edge {
	trig, threshold, det, fired := r.detect(s)
	above := r.cur > r.floor

	if fired {
		r.cleanStreak = 0
		if r.cur < len(r.tiers)-1 {
			from := r.tiers[r.cur].Model()
			r.cur++
			return Edge{
				Direction:      EdgeEscalate,
				Trigger:        trig,
				FiredThreshold: threshold,
				FromModel:      from,
				ToModel:        r.tiers[r.cur].Model(),
				StateBefore:    stateLabel(above),
				StateAfter:     "escalated",
				Detail:         det,
			}
		}
		return Edge{Direction: EdgeNone} // top rung: saturated, no further climb
	}

	if above {
		r.cleanStreak++
		if r.cleanStreak >= r.config.DeEscalationTurns {
			from := r.tiers[r.cur].Model()
			r.cur--
			r.cleanStreak = 0
			r.stickyTop = false // a clean recovery returns to bounded frontier use
			return Edge{
				Direction:   EdgeDeescalate,
				FromModel:   from,
				ToModel:     r.tiers[r.cur].Model(),
				StateBefore: "escalated",
				StateAfter:  "de_escalated",
			}
		}
	}
	return Edge{Direction: EdgeNone}
}

// EscalateForFault climbs one rung in response to a mid-turn model-call fault
// (a context overflow, a repeated timeout, or a malformed tool call) that the
// loop's local recovery could not absorb. Unlike Observe it is event-driven, not
// turn-boundary folded: it fires immediately so the retry uses the stronger rung,
// and it does not touch the de-escalation streak's hysteresis beyond resetting it
// (a fault is not a clean turn). The fault is mapped onto the closed escalation
// taxonomy (malformed→parse_failure, overflow/timeout→retry_exhaustion) so the
// emitted EscalationProposed event stays within the contract. It returns an
// EdgeEscalate edge, or EdgeNone when already at the top rung (no higher rung to
// lift to — the caller then falls back to local recovery or a clear failure).
//
// This is the fix for escalation-ladder-ignores-model-call-faults: the ladder
// previously climbed only on repeated_tool_error, so a floor model dying on a 400
// left the strong rung unused. Fault escalation is always available (it is not
// threshold-gated) because an exhausted-local-recovery fault is a definite, not a
// tunable, escalation trigger.
func (r *Router) EscalateForFault(f model.FaultKind) Edge {
	trig := faultTrigger(f)
	if trig == "" || r.cur >= len(r.tiers)-1 {
		return Edge{Direction: EdgeNone}
	}
	above := r.cur > r.floor
	from := r.tiers[r.cur].Model()
	r.cur++
	r.cleanStreak = 0
	return Edge{
		Direction:      EdgeEscalate,
		Trigger:        trig,
		FiredThreshold: 1,
		FromModel:      from,
		ToModel:        r.tiers[r.cur].Model(),
		StateBefore:    stateLabel(above),
		StateAfter:     "escalated",
		Detail:         "model_call_fault=" + string(f),
	}
}

// EscalateForStuckVerify climbs one rung when the orchestrator's verify gate has
// stayed RED across repeated revise cycles on the current rung — a stuck floor.
// Like EscalateForFault it is event-driven (mid-turn, not turn-boundary folded):
// the rung lifts immediately so the next revise attempt uses the stronger model,
// instead of the floor exhausting the whole revise budget with no rescue. A
// persistently-failing gate is a retry exhaustion (the rung cannot converge the
// gate despite retries), so it maps onto that closed-taxonomy trigger; the detail
// names the verify-stuck specifics and the consecutive-failure count. Returns an
// EdgeEscalate edge, or EdgeNone when already at the top rung.
//
// This is the fix for escalation-does-not-fire-on-persistently-stuck-floor: the
// ladder previously climbed only on per-turn tally triggers and model-call faults,
// so a floor that kept producing verify-RED code left the strong rung unused.
func (r *Router) EscalateForStuckVerify(consecutiveFails int) Edge {
	if r.cur >= len(r.tiers)-1 {
		return Edge{Direction: EdgeNone}
	}
	above := r.cur > r.floor
	from := r.tiers[r.cur].Model()
	r.cur++
	r.cleanStreak = 0
	return Edge{
		Direction:      EdgeEscalate,
		Trigger:        TriggerRetryExhaustion,
		FiredThreshold: float64(consecutiveFails),
		FromModel:      from,
		ToModel:        r.tiers[r.cur].Model(),
		StateBefore:    stateLabel(above),
		StateAfter:     "escalated",
		Detail:         fmt.Sprintf("verify_stuck consecutive_verify_fails=%d", consecutiveFails),
	}
}

// EscalateForNoProgress climbs one rung when the agent loop's no-progress circuit-
// breaker is about to hard-stop a stuck floor (stalledRounds tool-rounds with no
// file written and no verify-state change — a worker thrashing tool errors that
// never claims done, or a read-only loop that never converges). Like
// EscalateForFault/EscalateForStuckVerify it is event-driven (mid-turn, not
// turn-boundary folded): the rung lifts immediately so a stronger model gets a turn
// BEFORE the run is killed, instead of the floor's thrash dying at the cheap rung.
// A persistently-stalled rung cannot converge despite its rounds, so it maps onto
// the retry_exhaustion trigger; the detail names the stall length. Returns an
// EdgeEscalate edge, or EdgeNone when already at the top rung — the caller then
// lets the breaker hard-stop with its honest terminal verdict (T7).
//
// This is the fix for no-progress-breaker-pre-empts-repeated-tool-error-escalation-
// mid-turn: the repeated_tool_error trigger previously reached the ladder only via
// Observe at the turn boundary, which a within-turn thrash never reaches because it
// never claims done — so the breaker is made the escalation point of last resort.
func (r *Router) EscalateForNoProgress(stalledRounds int) Edge {
	if r.cur >= len(r.tiers)-1 {
		return Edge{Direction: EdgeNone}
	}
	above := r.cur > r.floor
	from := r.tiers[r.cur].Model()
	r.cur++
	r.cleanStreak = 0
	if r.cur == len(r.tiers)-1 {
		// Reached the top rung as the breaker's last resort: stick here so the bound
		// doesn't bounce the strong rung back into the stuck floor (run-10). Spend
		// stays capped by -max-cost-usd; cleared on any de-escalation.
		r.stickyTop = true
	}
	return Edge{
		Direction:      EdgeEscalate,
		Trigger:        TriggerRetryExhaustion,
		FiredThreshold: float64(stalledRounds),
		FromModel:      from,
		ToModel:        r.tiers[r.cur].Model(),
		StateBefore:    stateLabel(above),
		StateAfter:     "escalated",
		Detail:         fmt.Sprintf("no_progress stalled_rounds=%d", stalledRounds),
	}
}

// EscalateForRetryExhaustion climbs one rung when the agent loop has spent its entire
// per-cycle tool-round budget on the current rung WITHOUT converging — distinct from
// EscalateForNoProgress: here the worker DID make progress every round (it kept mutating, so
// the no-progress breaker never tripped) yet never claimed done, and with no verify gate
// stuck-verify never tripped either. Round-exhaustion is then the ONLY signal the rung cannot
// finish, so it is the escalation point of last resort: lift the rung and let the caller grant
// a fresh round budget, instead of dying on the floor when a stronger model would converge in a
// few rounds. Maps onto the retry_exhaustion trigger. Returns an EdgeEscalate edge, or EdgeNone
// at the top rung (the caller then lets the run end with its honest terminal verdict). Fix for
// coding-worker-thrashes-to-tool-round-exhaustion-without-escalating.
func (r *Router) EscalateForRetryExhaustion(rounds int) Edge {
	if r.cur >= len(r.tiers)-1 {
		return Edge{Direction: EdgeNone}
	}
	above := r.cur > r.floor
	from := r.tiers[r.cur].Model()
	r.cur++
	r.cleanStreak = 0
	if r.cur == len(r.tiers)-1 {
		// Reached the top rung as the last resort: stick here so a spent bound does not
		// bounce the strong rung back into the exhausting floor (the EscalateForNoProgress
		// run-10 lesson). Spend stays capped by -max-cost-usd.
		r.stickyTop = true
	}
	return Edge{
		Direction:      EdgeEscalate,
		Trigger:        TriggerRetryExhaustion,
		FiredThreshold: float64(rounds),
		FromModel:      from,
		ToModel:        r.tiers[r.cur].Model(),
		StateBefore:    stateLabel(above),
		StateAfter:     "escalated",
		Detail:         fmt.Sprintf("round_budget_exhausted rounds=%d", rounds),
	}
}

// EscalateForOverflow lifts the router in response to a context overflow on the
// floor — a MANDATORY escalation, not a routine cost choice: the floor rung
// physically cannot hold the prompt (and with a 24GB-VRAM-capped local window we
// cannot grow it), so routing to a larger-window rung is the ONLY fit. Unlike
// EscalateForFault, this BYPASSES the usage bound (it sets stickyTop on reaching /
// while at the top rung), because a floor that can't even accept the prompt must
// NOT be bounced back into by a spent bound — the run-12 death: the bounded top was
// spent, NextAdapter bounced to the floor, the floor overflowed, and EscalateForFault
// (cur already at top) returned EdgeNone, so the run died. Two cases:
//   - below the top: climb one rung; stick if that reaches the top.
//   - already at the top but a spent bound is bouncing it to the floor: set stickyTop
//     so the next NextAdapter serves the top rung instead of the overflowing floor.
//
// Spend stays capped by the loop's -max-cost-usd ceiling. EdgeNone only when there is
// genuinely no higher rung AND no bound to unstick (a true two-tier dead end).
func (r *Router) EscalateForOverflow() Edge {
	if r.cur >= len(r.tiers)-1 {
		// Already on the top rung. If a spent bound is bouncing it down to the floor,
		// unstick so NextAdapter serves the top; otherwise there is no fit to be had.
		if r.bounded(r.cur) && r.boundServed >= r.boundMax && !r.stickyTop {
			r.stickyTop = true
			return Edge{
				Direction:   EdgeEscalate,
				Trigger:     faultTrigger(model.FaultContextOverflow),
				FromModel:   r.tiers[r.floor].Model(), // the floor it was being bounced into
				ToModel:     r.tiers[r.cur].Model(),
				StateBefore: "escalated",
				StateAfter:  "escalated",
				Detail:      "context_overflow: unstick spent bound (floor cannot hold prompt)",
			}
		}
		return Edge{Direction: EdgeNone}
	}
	above := r.cur > r.floor
	from := r.tiers[r.cur].Model()
	r.cur++
	r.cleanStreak = 0
	if r.cur == len(r.tiers)-1 {
		r.stickyTop = true // mandatory floor-overflow escalation is not bound-gated
	}
	return Edge{
		Direction:   EdgeEscalate,
		Trigger:     faultTrigger(model.FaultContextOverflow),
		FromModel:   from,
		ToModel:     r.tiers[r.cur].Model(),
		StateBefore: stateLabel(above),
		StateAfter:  "escalated",
		Detail:      "context_overflow: floor window too small for prompt (mandatory escalation)",
	}
}

// DeEscalateToFloor drops the router straight back to the free local floor in
// response to a rate-limit on a paid rung: retrying a throttled rung is pointless,
// and the floor is free and not subject to the org tokens-per-minute cap. It is
// event-driven (mid-turn), like EscalateForFault, not turn-boundary folded. It
// returns an EdgeDeescalate edge, or EdgeNone when already at the floor (no lower
// rung to drop to — the caller then ends the turn gracefully rather than abort).
// This is half the fix for model-call-rate-limit-429-not-recoverable-aborts-run:
// a rate-limited strong rung de-escalates instead of killing the run.
func (r *Router) DeEscalateToFloor() Edge {
	if r.cur <= r.floor {
		return Edge{Direction: EdgeNone}
	}
	from := r.tiers[r.cur].Model()
	r.cur = r.floor
	r.cleanStreak = 0
	r.stickyTop = false // dropped to the floor: bounded frontier use resumes
	return Edge{
		Direction:   EdgeDeescalate,
		FromModel:   from,
		ToModel:     r.tiers[r.cur].Model(),
		StateBefore: "escalated",
		StateAfter:  "de_escalated",
		Detail:      "model_call_fault=" + string(model.FaultRateLimit),
	}
}

// faultTrigger maps a recoverable model-call fault onto the closed escalation
// taxonomy. A malformed tool call is a parse failure of the model's output; an
// overflow or timeout the loop could not locally absorb is a retry exhaustion
// (the local rung cannot converge). An unrecognised fault maps to no trigger.
func faultTrigger(f model.FaultKind) Trigger {
	switch f {
	case model.FaultMalformedToolCall:
		return TriggerParseFailure
	case model.FaultContextOverflow, model.FaultTimeout:
		return TriggerRetryExhaustion
	default:
		return ""
	}
}

// detect returns the highest-priority enabled trigger whose condition holds for
// the turn's signals (ported from escalation/triggers.py detect). Only enabled,
// configured triggers are evaluated; ties break by priority.
func (r *Router) detect(s Signals) (trig Trigger, threshold float64, detail string, fired bool) {
	for _, k := range priority {
		tc, ok := r.config.Triggers[k]
		if !ok || !tc.Enabled {
			continue
		}
		if fires(k, s, tc.ThresholdValue) {
			return k, tc.ThresholdValue, triggerDetail(k, s), true
		}
	}
	return "", 0, "", false
}

// fires evaluates one trigger's condition: a count comparison for the four count
// triggers, a confidence-floor comparison for low_confidence (nil never fires).
func fires(k Trigger, s Signals, threshold float64) bool {
	switch k {
	case TriggerRetryExhaustion:
		return float64(s.RetriesUsed) >= threshold
	case TriggerRepeatedToolError:
		return float64(s.ToolErrors) >= threshold
	case TriggerParseFailure:
		return float64(s.ParseFailures) >= threshold
	case TriggerExplicitHandoff:
		return float64(s.ExplicitHandoff) >= threshold
	case TriggerLowConfidence:
		return s.Confidence != nil && *s.Confidence < threshold
	default:
		return false
	}
}

// triggerDetail synthesises per-trigger evidence, appending any caller detail.
func triggerDetail(k Trigger, s Signals) string {
	var base string
	switch k {
	case TriggerRetryExhaustion:
		base = fmt.Sprintf("retries_used=%d", s.RetriesUsed)
	case TriggerRepeatedToolError:
		base = fmt.Sprintf("tool_errors=%d", s.ToolErrors)
	case TriggerParseFailure:
		base = fmt.Sprintf("parse_failures=%d", s.ParseFailures)
	case TriggerExplicitHandoff:
		base = fmt.Sprintf("explicit_handoff=%d", s.ExplicitHandoff)
	case TriggerLowConfidence:
		c := "nil"
		if s.Confidence != nil {
			c = strconv.FormatFloat(*s.Confidence, 'g', -1, 64)
		}
		base = "confidence=" + c
	default:
		base = string(k)
	}
	if s.Detail != "" {
		return base + " " + s.Detail
	}
	return base
}

// stateLabel maps the router's floor/above-floor position onto the contract's
// cheap/escalated enum for the EscalationProposed state_before field.
func stateLabel(above bool) string {
	if above {
		return "escalated"
	}
	return "cheap"
}
