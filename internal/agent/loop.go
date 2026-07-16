// Package agent runs the Corp-OS agent loop — the kernel. It drives a model
// (selected per turn by a router) through bounded tool rounds, dispatching the
// model's tool calls via a tool.Provider until a turn is final, firing the
// lifecycle hook surface and accumulating cost along the way. Bespoke Go, no
// Agent SDK. Ported/composed from bridge-harness session.py.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"corpos/internal/cost"
	"corpos/internal/escalation"
	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/router"
	"corpos/internal/session"
	"corpos/internal/tool"
)

// defaultMaxRounds caps tool-use rounds within a single Run (runaway guard).
const defaultMaxRounds = 12

// defaultVerifyStuckEscalateAfter is how many consecutive verify-RED revise
// cycles on the current rung constitute a "stuck floor" worth escalating: every
// Nth consecutive failure lifts the ladder one rung so the strong rung is tried
// before the revise budget is exhausted (bug
// escalation-does-not-fire-on-persistently-stuck-floor). The floor still gets the
// first N-1 attempts (local-tier-first). 0 disables stuck-floor escalation.
const defaultVerifyStuckEscalateAfter = 2

// defaultFabricationReprompts bounds how many times a no-work/fabricated done-claim
// is re-prompted WITHIN the same turn before the loop gives up and reports
// Fabricated (bug 1078). A fabricating worker is handed the audit verdict and told
// to actually dispatch the change, mirroring the verify-gate revise loop — so a
// model that merely narrated "done" gets a real chance to act in the SAME
// conversation instead of the claim terminating and a fresh worker respawning to
// re-narrate the same no-work. 0 disables the re-prompt (the pre-1078 terminal).
const defaultFabricationReprompts = 2

// fabricationRepromptPreamble leads the corrective user turn injected when a
// done-claim fails the fabrication audit. It is a stable marker (tests assert on it)
// followed by the audit's actionable verdict.
const fabricationRepromptPreamble = "Your previous turn claimed done but was REJECTED by the work audit: "

// Loop is one agent session: a router (model selection), a tool provider, the
// offered specs, a hook surface, a cost ledger, and a persistent transcript so
// a REPL can reuse the Loop across prompts.
type Loop struct {
	router    *router.Router
	provider  tool.Provider
	specs     []tool.Spec
	hooks     *hooks.Surface
	ledger    *cost.Ledger
	maxRounds int
	sessionID string
	project   string
	started   bool
	turns     int

	// breaker bounds a runaway turn with a cost/token ceiling and a no-progress
	// detector, ending the turn with an honest verdict (Result.Stopped) rather than
	// letting a stuck worker spend unbounded (bug no-convergence-or-cost-circuit-
	// breaker-…). The zero value disables every check. totalTokens is the running
	// input+output token count across the loop's lifetime, for the token ceiling.
	breaker     CircuitBreaker
	totalTokens int

	// meter, when set, is the SHARED tree-cost accumulator threaded across the spawn
	// tree (this loop + every worker the orchestrator spawns + the spawn tool). Each
	// priced model call is added to it, and breakerTrip stops the loop once the meter's
	// CUMULATIVE total reaches its ceiling — so the run-level cost ceiling bounds the
	// whole tree, not just this loop's own ledger (bug 1124). A delegating orchestrator's
	// own ledger sees only its cheap planning calls while the workers spend on separate
	// child ledgers; the meter is what makes -max-cost-usd whole-tree. Nil = a standalone
	// loop bounded only by its own per-loop CircuitBreaker (prior behavior).
	meter *cost.Meter

	// goalReminderEvery, when > 0, re-surfaces the goal anchor as a terse reminder
	// near the transcript tail every N tool rounds, so a long investigation doesn't
	// drift from its target after the pinned first prompt is buried under many rounds
	// (bug goal-anchor-reinforcement-…). 0 disables it (the goal is still always
	// present as the first user message; this only refreshes its salience).
	goalReminderEvery int

	// spawnForceEvery, when > 0, arms the must-spawn guard: a SPAWN-CAPABLE loop (an
	// orchestrate-profile agent — one whose scope includes the agent/spawn surface)
	// that completes this many tool rounds with ZERO agent.spawn dispatches gets a
	// terse forcing reminder injected near the transcript tail (refreshSpawnForce),
	// re-armed every spawnForceEvery further read-only rounds until it spawns. The
	// orchestrate profile is deliberately read-only, so its job is to DELEGATE; left
	// to prompt guidance alone the Gemini orchestrator stalls by investigating forever
	// and never spawns (bug 1072). This is the deterministic structural lever the
	// prompt's "spawn within your first one or two tool calls" guidance could not
	// enforce on its own. 0 disables it; it is a no-op for non-spawn-capable workers.
	spawnForceEvery int
	// spawnCapableMemo memoises the (profile-walking) spawnCapable check: -1 = not yet
	// computed, 0 = no, 1 = yes. The profile is immutable for the loop's life, so the
	// answer is stable.
	spawnCapableMemo int

	// profile is the job-profile this loop runs under, when one is active. The
	// loop's specs are already projected to it (see mcp.Project); profile is
	// carried so profile-aware hooks can read the worker's envelope. Nil = the
	// loop runs unprojected (the full surface set).
	profile *profile.JobProfile

	// contextProber, when set, runs once per turn on the user prompt before the
	// pre_user_prompt hooks fire; its result becomes the hook Metadata (e.g. a
	// parse_context envelope a pruner/injector then shapes). Nil = no probe.
	contextProber ContextProber

	// store, when set, durably records each turn's conversation messages AND its
	// telemetry (turn/cost/tool-call/escalation rows) to the local session ledger
	// (SQLite). It is LOCAL state (flag F4) — never the toolkit-server substrate
	// ledger, which stays remote over MCP.
	store *session.Store

	// emitter, when set, emits an EscalationProposed event (via the toolkit's
	// admin surface) on each router escalate edge; the returned event id is stamped
	// onto the local escalation row. Nil = local-only escalation telemetry.
	emitter escalation.Emitter

	// compactor, when set, bounds the transcript against a token budget at each
	// turn boundary (see compaction.go). Nil = the transcript grows unbounded.
	compactor *Compactor
	// goalAnchor is the session's first user prompt, captured once and preserved
	// verbatim across every compaction (the active-goal pin). summaryBody is the
	// latest rolling-summary text, carried so the next compaction subsumes rather
	// than stacks it. toolSpecTokens is the calibrated fixed overhead of the offered
	// tool specs — the provider's measured input-token count minus the estimated
	// transcript that produced it — which the compactor adds to the transcript
	// estimate so the size signal reflects the TOTAL context (specs are not part of
	// the transcript and cannot be evicted). It persists across compactions.
	goalAnchor     string
	summaryBody    string
	toolSpecTokens int
	// toolSpecCalibrated guards toolSpecTokens against the conflation bug: the
	// fixed overhead is set ONCE from the first measured call (transcript minimal)
	// and never grown, so a later code-heavy turn's tokenizer under-estimate flows
	// into the compactor's tokenRatio instead of inflating "overhead".
	toolSpecCalibrated bool
	// narratedSeq is a monotonic counter that assigns a unique id to each recovered
	// narrated tool call. A structured tool call carries a provider-assigned id; a
	// recovered one does not, and an empty id breaks the Anthropic adapter on
	// escalation (tool_use.id is required, and the tool_result must reference it).
	narratedSeq int

	// floorWindow is the local floor model's context window in tokens (detected at
	// setup; 0 = unknown/disabled). The proactive floor-fit guard uses it to keep a
	// would-overflow prompt OFF the small-window floor — escalating to a larger-window
	// rung BEFORE the call rather than after a 400. The window is fixed by VRAM (24GB),
	// so routing up is the only fit; we cannot grow the floor.
	floorWindow int

	// tierBudgets is the compaction budget for each router rung, indexed to match the
	// router's tiers (0 = floor … len-1 = strong). When set, the loop re-points the
	// compactor's budget at the ACTIVE rung's entry each round (syncBudgetToRung), so a
	// worker that escalates off the small-window floor onto a wide-window tier actually
	// gets that tier's context — not the floor's 6144 (bug 1088 GAP-2a: escalation that
	// bought a better model but not more context left the coding worker starved, every
	// read evicted on arrival). Empty = the static WithCompaction budget stands (the
	// single-tier / no-ladder case).
	tierBudgets []int

	// verify, when set, is the orchestrator-owned verification gate run after the
	// agent claims it is done: on a non-zero exit its output is fed back and the
	// agent revises (a write→verify→revise loop the agent cannot bypass). Nil = no
	// self-verification. verifyRounds counts the verify-fail cycles spent.
	verify       *VerifyGate
	verifyRounds int
	// terminalGreen, when set, is a read-only "is the repo already green?" backstop the
	// terminal halt paths (strong-bound / round-exhaustion, via opportunisticGreen)
	// consult ONLY when no full verify gate (l.verify) is configured. The spawn-only
	// orchestrator owns no gate of its own, yet it drives coding workers that DO land a
	// passing fix in the shared verify-dir; without this, a run whose repo is genuinely
	// green at halt time is reported "stuck / no final answer" (bug 1148: a false-negative
	// verdict on a landed, verified fix). Unlike l.verify it arms NO mid-flight stop-when-
	// green, revise loop, or no-work audit — it is purely the terminal green check, still
	// gated by the fake-green guard so a suspect green is not counted.
	terminalGreen *VerifyGate
	// opportunisticEvery enables the mid-flight stop-when-green check: after a round that
	// landed a substantive mutation, run the verify gate at most once per this many rounds
	// and finish successfully the moment it passes — so a worker that produced a passing
	// fix but keeps editing (it never cleanly claims done) is captured instead of burning
	// the whole round budget and discarding the fix. 0 disables the mid-flight check; the
	// exhaustion-time check (before the round-budget error) is always on when a verify gate
	// is set. lastOpportunisticRound throttles the mid-flight cadence.
	opportunisticEvery     int
	lastOpportunisticRound int
	// lastReconcileOutput dedupes the mid-flight reconcile feedback (bug 1103): when the
	// throttled opportunistic gate runs RED, the loop feeds the failing output back so the
	// worker reconciles a guessed/output-dependent assertion against the REAL test output
	// BEFORE claiming done — but only when the output CHANGED, so a worker that hasn't
	// touched the failing code isn't re-nudged with byte-identical text every cadence tick.
	lastReconcileOutput string
	// verifyDirOverride, when set, replaces the verify gate's configured Dir after all
	// options are applied. The orchestrator-owned coding path forks a fresh worktree per
	// attempt and the worker edits THERE, but the spawner's gate Dir is the fixed target
	// repo — so the worker's self-verify (and the opportunistic gate) would otherwise run
	// against an UNEDITED tree and never see the fix. The orchestrator passes the worktree
	// here so the gate runs where the worker actually edits.
	verifyDirOverride string
	// startRungModel, when set, lifts the router's resting floor to the rung whose
	// model id matches, after all options are applied (mirrors verifyDirOverride). The
	// coding orchestrator passes the tier a prior respawn of the same atom escalated to,
	// so this attempt begins there instead of re-climbing from the local floor (chain
	// 392 task 3314). The lift is capped below the bounded top rung by the router, so a
	// carried floor never rests on bounded Opus. Empty = the profile's own floor.
	startRungModel string
	// verifyStuckEscalateAfter: every Nth consecutive verify-RED cycle lifts the
	// ladder one rung (a stuck floor is a form of retry exhaustion). 0 disables it.
	verifyStuckEscalateAfter int
	// goDocResolve resolves a Go symbol's `go doc` text for the structural grounding
	// reflex: when the verify gate fails with compile errors naming undefined symbols /
	// unknown struct fields, the loop resolves them and injects the REAL signatures into
	// the revise feedback, so the worker stops guessing internal APIs (follow-on to bug
	// 1090 — the go_doc tool alone wasn't reached for). nil at use-site falls back to
	// realGoDoc; tests inject a deterministic resolver.
	goDocResolve goDocResolver
	// goListResolve resolves a WRONG import path to the correct module import path for the
	// structural grounding reflex's import-path arm: when the gate fails because the worker
	// wrote a repo-relative import (e.g. "go/internal/x") instead of the module path
	// ("toolkit/internal/x"), the loop resolves the real path via `go list` and injects it
	// into the revise feedback (bug corpos-grounding-does-not-handle-import-path-errors). nil
	// at use-site falls back to realGoListImportPath; tests inject a deterministic resolver.
	goListResolve goListResolver
	// dirExists checks whether a derived package dir exists under the verify root, gating
	// the verify-gate test scoping (only narrow `go test ./...` when the edited package
	// actually exists, else run whole-repo — a wrong scope would be a false failure). nil
	// at use-site falls back to realDirExists; tests inject a deterministic checker.
	dirExists func(string) bool
	// fabricationReprompts bounds the in-turn re-prompts of a no-work/fabricated
	// done-claim before the loop reports Fabricated (bug 1078). fabricationRounds
	// counts the re-prompts spent this Run. 0 reprompts = the pre-1078 terminal.
	fabricationReprompts int
	fabricationRounds    int
	// toolResultCharCap caps a single tool result's serialized size before it enters
	// the transcript (0 = derive from the compaction budget). A single oversized
	// result must never blow the context window.
	toolResultCharCap int

	// guards is the post-turn audit pipeline: the registered Guards the loop runs uniformly
	// at the done-claim (work-audit, required-read, fake-green, + future). It replaces the
	// per-guard bespoke fields/branches — adding a guard is register, not a new done-claim
	// edit. The fabrication-stage guards run BEFORE the verify gate (refuse → Result.Fabricated);
	// the fake-green-stage guards run AFTER a PASSED gate (refuse → Result.Escalate). Empty =
	// no audits (the read-only-duty default). See guards.go.
	guards guardRegistry

	// truncatedView is the set of fs paths whose most-recent read RESULT was truncated
	// by capToolResult (the model saw only part of the file). A subsequent whole-file
	// fs.write to such a path would overwrite the unseen remainder with whatever the
	// model reconstructed from its partial view — the run-9 data-loss footgun, where a
	// worker rewrote read.go from a truncated read and replaced the unseen body with a
	// placeholder stub. fs.write to a tainted path is refused (steer to fs.edit); the
	// taint clears when the path is read whole-file without truncation. fs.edit/move/
	// remove are never blocked (surgical edits don't depend on a complete view).
	truncatedView map[string]bool

	transcript []model.ChatMessage
}

// Tool-result size-guard constants. A single tool result is truncated to a
// character cap before it enters the transcript so one oversized result (a full
// work.bug_list dump, an unscoped fs.grep, a whole-file read) cannot exceed the
// model's context window — the rehearsal run-6 overflow, where a 61k-token result
// hit an 8k window and no compaction could shrink a single message. The cap is a
// fraction of the compaction budget when known, else a safe default; results over
// it are truncated with an actionable marker that nudges the agent to narrow.
const (
	approxCharsPerToken    = 4    // matches estimateTokens' heuristic
	minToolResultChars     = 1024 // floor so even a tiny budget leaves a usable result
	defaultToolResultChars = 4096 // ~1k tokens when no budget is known
)

// CircuitBreaker bounds a runaway turn. MaxCostUSD ends the turn once the session
// ledger reaches the ceiling; MaxTokens ends it once cumulative input+output
// tokens reach the budget; NoProgressRounds ends it after that many consecutive
// tool-rounds with no file written AND no verify-state change (the grep-thrash
// signature). Each field is independent and any zero value disables that check.
// The breaker is the honest stopping condition the 8k-window overflow used to
// provide by accident (bug no-convergence-or-cost-circuit-breaker-…): when it
// trips, the turn ends cleanly with a verdict on Result.Stopped — never a silent
// kill and never a runaway.
//
// MaxCostUSD is a SOFT (between-call) ceiling, not a hard cap (bug 1142). breakerTrip
// runs at the top of every round, BEFORE that round's single priced model call, and the
// loop makes exactly one priced call per round — so once cumulative spend reaches the
// ceiling the loop stops before the NEXT call. A call already in flight when the ceiling
// is crossed still completes, so realized spend can overshoot MaxCostUSD by at most ONE
// model call's cost, and never more (the check fires between every call, including across
// an escalation to a costlier rung — so no two costlier-rung calls can both land past the
// ceiling). Overshoot below one call is not achievable reactively: a call's true cost is
// unknown until it returns, and refusing a call on a pre-estimate would starve a
// legitimate cheap final call. The one-call bound is asserted in cost_ceiling_overshoot_test.go.
type CircuitBreaker struct {
	MaxCostUSD       float64
	MaxTokens        int
	NoProgressRounds int
}

// enabled reports whether any check is configured.
func (c CircuitBreaker) enabled() bool {
	return c.MaxCostUSD > 0 || c.MaxTokens > 0 || c.NoProgressRounds > 0
}

// Option configures a Loop.
type Option func(*Loop)

// WithMaxRounds overrides the per-Run tool-round cap.
func WithMaxRounds(n int) Option {
	return func(l *Loop) {
		if n > 0 {
			l.maxRounds = n
		}
	}
}

// WithCircuitBreaker installs a cost/token/no-progress breaker that ends a runaway
// turn with an honest verdict instead of letting a stuck worker spend unbounded.
// A zero-value breaker (every field 0) is a no-op, preserving prior behavior.
func WithCircuitBreaker(b CircuitBreaker) Option {
	return func(l *Loop) { l.breaker = b }
}

// WithCostMeter threads a SHARED tree-cost meter through the loop: each priced model
// call is added to it, and the breaker stops the loop once the meter's CUMULATIVE tree
// total reaches its ceiling (bug 1124). The same meter is wired into every worker the
// orchestrator spawns (via the Spawner) and into the agent.spawn provider, so the
// run-level cost ceiling is enforced across the whole spawn tree — not per-loop, where
// a delegating orchestrator's own ledger never sees the workers' spend. A nil meter is
// a no-op, leaving the loop bounded only by its own CircuitBreaker.
func WithCostMeter(m *cost.Meter) Option {
	return func(l *Loop) { l.meter = m }
}

// WithGoalReminder re-surfaces the active goal near the transcript tail every
// everyRounds tool rounds, keeping it salient through a long loop so the worker
// doesn't substitute its target (run-6d turned "fix task_stamp_sha" into "analyze
// bug_list"). everyRounds <= 0 disables it. Terse + deduplicated (one reminder at a
// time), so it doesn't bloat the window.
func WithGoalReminder(everyRounds int) Option {
	return func(l *Loop) {
		if everyRounds > 0 {
			l.goalReminderEvery = everyRounds
		}
	}
}

// WithSpawnForcing arms the must-spawn guard for a spawn-capable (orchestrate)
// loop: after afterRounds tool rounds with zero agent.spawn dispatches, a terse
// forcing reminder is injected near the transcript tail (and re-armed every
// afterRounds further read-only rounds) until the orchestrator delegates. It is the
// deterministic structural backstop to the orchestrate prompt's spawn-early
// guidance (bug 1072): prompt text alone let the orchestrator investigate forever
// and never spawn. afterRounds <= 0 disables it; it is a no-op for any loop that is
// not spawn-capable (a normal worker is never nagged to spawn). Scoped this way so
// only the delegation-shaped profile is affected.
func WithSpawnForcing(afterRounds int) Option {
	return func(l *Loop) {
		if afterRounds > 0 {
			l.spawnForceEvery = afterRounds
		}
	}
}

// WithSystemPrompt seeds the transcript with a system message.
func WithSystemPrompt(prompt string) Option {
	return func(l *Loop) {
		if prompt != "" {
			l.transcript = append(l.transcript, model.ChatMessage{Role: model.RoleSystem, Content: prompt})
		}
	}
}

// WithHooks installs a hook surface (else an empty one is used).
func WithHooks(h *hooks.Surface) Option {
	return func(l *Loop) {
		if h != nil {
			l.hooks = h
		}
	}
}

// WithSession sets the session id and project stamped on hook contexts.
func WithSession(sessionID, project string) Option {
	return func(l *Loop) {
		l.sessionID = sessionID
		l.project = project
	}
}

// WithProfile sets the job-profile this loop runs under. The caller is expected
// to have already projected the loop's specs to the profile (mcp.Project); the
// profile is threaded so profile-aware hooks (skill injection, parse_context
// pruning) can read the worker's envelope. Nil leaves the loop unprojected.
func WithProfile(p *profile.JobProfile) Option {
	return func(l *Loop) { l.profile = p }
}

// ContextProber runs once per turn on the user prompt and returns hook Metadata
// to attach before the pre_user_prompt hooks fire (or nil to attach nothing). The
// composition root supplies it — e.g. a closure that fires knowledge.parse_context
// and returns {parse_context: envelope} — keeping the loop agnostic to what it
// probes.
type ContextProber func(ctx context.Context, prompt string) map[string]any

// WithContextProber installs a per-turn context probe (see ContextProber).
func WithContextProber(p ContextProber) Option {
	return func(l *Loop) { l.contextProber = p }
}

// WithStore wires a local session store: each turn's conversation messages are
// persisted to it so a run's state survives the process (and a later --resume).
// Persistence is best-effort — a write failure surfaces on Result.PersistErr,
// never aborts the turn.
func WithStore(s *session.Store) Option {
	return func(l *Loop) { l.store = s }
}

// WithEscalationEmitter wires an escalation emitter: on each router escalate edge
// the loop emits an EscalationProposed event through it and stamps the returned
// event id onto the local escalation row. Nil leaves escalation telemetry local.
func WithEscalationEmitter(e escalation.Emitter) Option {
	return func(l *Loop) { l.emitter = e }
}

// WithFloorWindow records the local floor model's context window (tokens), enabling
// the proactive floor-fit guard: a prompt projected to exceed a safe fraction of the
// floor window is compacted and, if it still won't fit, escalated to a larger-window
// rung BEFORE the call — so a would-overflow prompt never reaches the floor (a 400
// that, with a spent strong bound, used to bounce a stuck floor to death). 0 disables
// the guard (the reactive overflow recovery still applies).
func WithFloorWindow(tokens int) Option {
	return func(l *Loop) {
		if tokens > 0 {
			l.floorWindow = tokens
		}
	}
}

// WithTierBudgets records the per-rung compaction budget (indexed to the router's
// tiers: 0 = floor … len-1 = strong), enabling the loop to track the ACTIVE rung's
// window each round instead of holding the floor's budget for the whole run. Without
// it the compactor's budget is whatever WithCompaction set at startup — correct for
// the floor, but starvation once the worker escalates to a wider tier (bug 1088). A
// nil/empty slice is a no-op; non-positive entries are ignored at sync time.
func WithTierBudgets(budgets []int) Option {
	return func(l *Loop) {
		l.tierBudgets = budgets
	}
}

// syncBudgetToRung re-points the compactor's budget at the rung the router is now
// resting on, so the within-turn bound, turn-boundary compaction, force-compact and
// the per-result cap all measure against the ACTIVE tier's window. Called at the top
// of every round (the rung was set by the previous round's escalation edge). A no-op
// without a compactor or per-tier budgets, or for an out-of-range / non-positive
// entry — the existing budget stands rather than collapsing to zero.
func (l *Loop) syncBudgetToRung() {
	if l.compactor == nil || len(l.tierBudgets) == 0 {
		return
	}
	rung := l.router.CurrentRung()
	if rung < 0 || rung >= len(l.tierBudgets) {
		return
	}
	if b := l.tierBudgets[rung]; b > 0 {
		l.compactor.budget = b
	}
}

// floorFitFraction is how much of the floor window a prompt may use before the
// floor-fit guard routes it up. The headroom (1 - fraction) absorbs the token
// ESTIMATE's known underestimate vs the real tokenizer plus the model's response
// reserve — deliberately conservative so it is "very difficult" for a would-overflow
// prompt to reach the floor (the operator constraint: the floor window is VRAM-fixed).
const floorFitNum, floorFitDen = 4, 5 // 80%

// floorFits reports whether a projected prompt of n tokens safely fits a window of
// w tokens for the floor rung (n below floorFitFraction·w). w <= 0 means "unknown" →
// fits (guard disabled).
func floorFits(n, w int) bool {
	if w <= 0 {
		return true
	}
	return n < w*floorFitNum/floorFitDen
}

// WithCompaction bounds the transcript against a token budget: when the live
// context size (measured-where-known, estimated otherwise) exceeds budget at a
// turn boundary, the loop folds the turns between the pinned head/goal anchor and
// the last recencyTurns turn-groups into one rolling summary produced by the
// summarizer adapter (see compaction.go / docs/CONTEXT_COMPACTION.md). A no-op
// unless budget > 0, recencyTurns > 0, and summarizer is non-nil.
func WithCompaction(budget, recencyTurns int, summarizer model.Adapter) Option {
	return func(l *Loop) {
		if budget > 0 && recencyTurns > 0 && summarizer != nil {
			l.compactor = &Compactor{budget: budget, recencyTurns: recencyTurns, summarizer: summarizer, tokenRatio: 1.0}
		}
	}
}

// WithExtraHook appends an additional hook to the loop's surface, creating one if the
// loop has none, so it COMPOSES with any profile-built hooks (the risk gate, skill
// injection) rather than replacing them like WithHooks. The coding worker uses it to
// inject a per-attempt dispatch-boundary guard (the principal-owned acceptance path is
// outside the worker's writable scope) on top of the profile's risk gate. A nil fn is ignored.
func WithExtraHook(kind hooks.Kind, name string, fn hooks.Func) Option {
	return func(l *Loop) {
		if fn == nil {
			return
		}
		// l.hooks is always non-nil (New seeds an empty surface; WithHooks only
		// replaces it with a non-nil one), so registering composes with whatever the
		// profile already wired.
		_ = l.hooks.Register(kind, name, fn)
	}
}

// WithVerify wires an orchestrator-owned verification gate: after the agent claims
// it is done, the loop runs the fixed gate command itself; on a non-zero exit it
// feeds the output back and lets the agent revise (bounded by the gate's MaxRounds),
// closing a self-verify loop the agent cannot skip. Safe under risk-gate=enforce —
// the loop runs the fixed command, not the agent via sys.exec. A nil gate is ignored.
func WithVerify(g *VerifyGate) Option {
	return func(l *Loop) {
		if g != nil && len(g.Command) > 0 {
			l.verify = g
		}
	}
}

// WithTerminalGreen installs a read-only terminal green backstop: a gate consulted ONLY
// at the terminal halt paths (via opportunisticGreen) when the loop has no full verify
// gate of its own, so a spawn-only orchestrator whose workers landed a passing fix reports
// the achieved green instead of a false-negative "stuck / no final answer" halt (bug 1148).
// It never drives a mid-flight done, a revise loop, or the no-work audit — those belong to
// WithVerify. A nil/empty-command gate, or a loop that already has WithVerify, ignores it
// (a full gate supersedes the backstop). Independent of WithVerify so the two never conflict.
func WithTerminalGreen(g *VerifyGate) Option {
	return func(l *Loop) {
		if g != nil && len(g.Command) > 0 {
			l.terminalGreen = g
		}
	}
}

// WithOpportunisticVerify enables the mid-flight stop-when-green check at a cadence of
// at most once per n rounds that landed a mutation (n<=0 disables it). It only has
// effect alongside a verify gate (WithVerify). The exhaustion-time check is independent
// and always on when a gate is set, so a worker's landed fix is never discarded just
// because it failed to cleanly claim done within the round budget.
func WithOpportunisticVerify(n int) Option {
	return func(l *Loop) {
		if n < 0 {
			n = 0
		}
		l.opportunisticEvery = n
	}
}

// WithVerifyDir overrides the verify gate's working directory for this spawn (no-op on
// an empty dir or a gate-less loop). The orchestrator passes the per-attempt WORKTREE so
// the worker's self-verify + opportunistic gate run where the worker edits, not against
// the spawner's fixed (unedited) target repo.
func WithVerifyDir(dir string) Option {
	return func(l *Loop) {
		l.verifyDirOverride = dir
	}
}

// WithStartRung lifts this loop's router to begin resting on the rung whose model id
// is modelID (no-op on an empty id, an unknown id, or one at/below the profile floor).
// The coding orchestrator passes the tier a prior respawn of the same atom escalated
// to, so the worker starts there instead of re-paying the climb from the local floor
// each respawn (chain 392 task 3314). The router caps the lift below its bounded top
// rung, so a carried floor never rests on bounded Opus.
func WithStartRung(modelID string) Option {
	return func(l *Loop) {
		l.startRungModel = modelID
	}
}

// WithWorkAudit wires the post-turn fabrication/no-work audit (workaudit.go). At a
// done-claim the loop cross-checks the claim against the real dispatch record and refuses
// a "done" not backed by work — surfacing the verdict on Result.Fabricated. The coding
// worker enables RequireMutation (its job is to change files); read-only duties leave it off.
func WithWorkAudit(a WorkAudit) Option {
	return func(l *Loop) {
		l.guards.register(a)
	}
}

// WithRequiredReads wires the post-turn required-read guard (requiredread.go). At a
// done-claim the loop checks that every path the duty declared as a REQUIRED contract source
// was read successfully; a required read that FAILED (or was never attempted) refuses the
// done-claim — surfacing the verdict on Result.Fabricated — so a worker cannot invent a
// contract on a failed required read and report a false green (bug 1033). An empty path set
// is a no-op (no required-read guard for this duty).
func WithRequiredReads(r RequiredReads) Option {
	return func(l *Loop) {
		if len(r.Paths) > 0 {
			l.guards.register(r)
		}
	}
}

// WithScaffoldGuard wires the post-turn scaffold-fabrication guard (scaffoldfab.go). At a
// done-claim with a PASSED gate it refuses a green the worker manufactured by writing a
// build-scaffold (go.mod) into the verify-dir — surfacing the verdict on Result.Escalate (bug
// 1075). It carries the gate's working dir as config (the boundary the fabricated manifest is
// measured against), so it is registered explicitly by the spawner that knows that dir, not
// unconditionally in New.
func WithScaffoldGuard(g ScaffoldFabricationGuard) Option {
	return func(l *Loop) {
		l.guards.register(g)
	}
}

// WithVerifyStuckEscalation sets how many consecutive verify-RED revise cycles
// trip a stuck-floor escalation (every Nth failure lifts one rung). n<=0 disables
// it. Defaults to defaultVerifyStuckEscalateAfter.
func WithVerifyStuckEscalation(n int) Option {
	return func(l *Loop) {
		if n < 0 {
			n = 0
		}
		l.verifyStuckEscalateAfter = n
	}
}

// WithFabricationReprompts sets how many times a no-work/fabricated done-claim is
// re-prompted within the same turn before the loop reports Fabricated (bug 1078).
// n<=0 disables the re-prompt (restoring the pre-1078 terminal behavior). Defaults
// to defaultFabricationReprompts.
func WithFabricationReprompts(n int) Option {
	return func(l *Loop) {
		if n < 0 {
			n = 0
		}
		l.fabricationReprompts = n
	}
}

// WithToolResultCap overrides the per-tool-result character cap (0 = derive from
// the compaction budget). A single tool result is truncated to this before it
// enters the transcript, so one oversized result can't blow the context window.
func WithToolResultCap(chars int) Option {
	return func(l *Loop) {
		if chars < 0 {
			chars = 0
		}
		l.toolResultCharCap = chars
	}
}

// WithResumed seeds the transcript with a prior conversation thread (see
// ResumeState) and continues turn numbering from startTurn. Pass it AFTER
// WithSystemPrompt so the system message stays first.
func WithResumed(history []model.ChatMessage, startTurn int) Option {
	return func(l *Loop) {
		l.transcript = append(l.transcript, history...)
		if startTurn > 0 {
			l.turns = startTurn
		}
	}
}

// New builds a Loop over the router, tool provider, and offered specs.
func New(r *router.Router, p tool.Provider, specs []tool.Spec, opts ...Option) *Loop {
	l := &Loop{
		router:                   r,
		provider:                 p,
		specs:                    specs,
		hooks:                    hooks.NewSurface(),
		ledger:                   cost.NewLedger(),
		maxRounds:                defaultMaxRounds,
		verifyStuckEscalateAfter: defaultVerifyStuckEscalateAfter,
		fabricationReprompts:     defaultFabricationReprompts,
		spawnCapableMemo:         -1, // lazily computed on first use
	}
	for _, o := range opts {
		o(l)
	}
	// A carried start-rung (the tier a prior respawn of this atom escalated to) lifts the
	// router floor after all options are applied, so the worker begins there instead of
	// re-climbing from the local floor (chain 392 task 3314). The router caps the lift
	// below its bounded top rung. A nil router can't occur via New (router is required),
	// but guard anyway so a hand-built loop in a test is safe.
	if l.startRungModel != "" && l.router != nil {
		l.router.LiftFloorToModel(l.startRungModel)
	}
	// A per-spawn verify-dir override (the orchestrator's worktree) wins over the gate's
	// configured Dir, so the worker self-verifies WHERE it edits — not against the fixed,
	// unedited target repo. Applied after the options loop so it overrides regardless of
	// option order.
	if l.verifyDirOverride != "" && l.verify != nil {
		// Resolve to the Go module root so an override pointing at a subdir-module repo's
		// root (the worktree) gates in its go/ module, not the module-less root (bug 1094).
		l.verify.Dir = l.verifyDirOverride
		if md, ok := ResolveGoModuleDir(l.verifyDirOverride); ok {
			l.verify.Dir = md
		}
	}
	// The fake-green audit is part of the pipeline whenever a verify gate may run: it is
	// consulted only at the StageFakeGreen point (after a PASSED gate), so registering it
	// unconditionally is a no-op for a gate-less loop. Registered last so it runs after the
	// fabrication-stage guards, matching the pre-refactor done-claim order.
	l.guards.register(FakeGreenGuard{})
	return l
}

// Result summarizes one driven turn.
type Result struct {
	// Text is the final assistant answer.
	Text string
	// Dispatches are the tool calls made during the turn, in order.
	Dispatches []tool.Result
	// CostUSD is the running session cost after this turn.
	CostUSD float64
	// PersistErr is a non-fatal session-store write failure for this turn, if
	// any. The turn's answer is still valid; the caller may surface this as a
	// warning. Nil when there is no store or persistence succeeded.
	PersistErr error
	// Compaction is set when a context compaction fired at this turn's boundary
	// (nil otherwise), carrying the before/after size and turn-groups evicted.
	Compaction *CompactionEvent
	// VerifyFailed is true when a verify gate was configured and the loop returned
	// with it still failing after exhausting its revise budget (the answer did not
	// pass verification). False when there was no gate or it passed.
	VerifyFailed bool
	// ModelFault, when non-empty, is the recoverable model-call fault class that
	// ended this turn gracefully (e.g. "timeout" when the per-turn deadline was
	// spent mid-loop). The turn returned without a final answer but did NOT abort
	// the run — a REPL/-resume can continue. Empty on a normal turn.
	ModelFault string
	// Stopped, when non-empty, is the circuit-breaker verdict that ended the turn
	// early (e.g. "cost ceiling $5.00 reached ($5.02 spent)…", "no file written … in
	// 8 rounds…"). The turn ended cleanly with this honest verdict — not a runaway
	// and not a silent kill — without a final answer; a principal/rehearsal log
	// consumes it. Empty on a normal turn.
	Stopped string
	// Fabricated, when non-empty, is the post-turn work-audit verdict that refused a
	// done-claim not backed by real work (no-work: zero substantive mutations on a
	// mutation-expecting task; or a tool-call envelope narrated in prose). The turn
	// ended with this honest verdict instead of a fabricated success. Empty when there
	// was no audit or the claim was sound. See WithWorkAudit / workaudit.go.
	Fabricated string
	// Escalate, when non-empty, is the honest terminal verdict for a run that could NOT
	// satisfy the orchestrator-owned verify gate within bounds: the revise budget was
	// exhausted, or the no-progress/cost breaker tripped while verification was still
	// unsatisfied. It reads "unverified/escalate: …" and is the anti-fake-pass terminal
	// state (T7) — a worker's self-reported success (in Text) NEVER overrides it: the
	// caller keys off this field, not the narration. Empty when verification passed or
	// no gate was configured.
	Escalate string
	// HighestTierModel is the model id of the highest rung this run's router actually
	// served. The coding orchestrator carries it into the next respawn of the same atom
	// so the respawn begins at >= the tier this attempt reached, instead of re-paying the
	// local→mid→coder→strong climb every respawn (chain 392 task 3314). Stamped on every
	// Run return path; empty only for a router with no served rung (never, in practice).
	HighestTierModel string
}

// escalateVerdict renders the honest "unverified/escalate" terminal verdict for a run that
// could not satisfy the verify gate within bounds (T7) — never reported as done.
func escalateVerdict(reason string) string {
	return "unverified/escalate: " + reason
}

func (l *Loop) hookCtx(kind hooks.Kind, turn int) *hooks.Context {
	return &hooks.Context{
		Kind:       kind,
		SessionID:  l.sessionID,
		Project:    l.project,
		TurnIndex:  turn,
		Transcript: &l.transcript,
		Profile:    l.profile,
	}
}

// Profile returns the job-profile this loop runs under (nil when unprojected).
func (l *Loop) Profile() *profile.JobProfile { return l.profile }

// Guards returns the post-turn audit guards registered on this loop, in run order — the
// enumerable active guard set the -print-guards view renders. Read-only.
func (l *Loop) Guards() []Guard { return l.guards.all() }

// offeredSurfaces is the set of surface names this loop advertises to the model,
// used to gate narrated-call recovery so a recovered call only ever targets a real
// offered surface (never dispatches into the void).
func (l *Loop) offeredSurfaces() map[string]bool {
	if len(l.specs) == 0 {
		return nil
	}
	set := make(map[string]bool, len(l.specs))
	for _, s := range l.specs {
		set[s.Name] = true
	}
	return set
}

// Transcript returns a copy of the loop's current conversation transcript (the
// working, possibly-compacted view — not the full session-store ledger). Read-only
// for inspection and tests.
func (l *Loop) Transcript() []model.ChatMessage {
	return append([]model.ChatMessage(nil), l.transcript...)
}

// ContextTokens reports the live TOTAL context size (calibrated tool-spec overhead
// plus the transcript estimate) of the current transcript, or 0 when no compactor
// is configured.
func (l *Loop) ContextTokens() int {
	if l.compactor == nil {
		return 0
	}
	return l.compactor.currentSize(l.transcript, l.toolSpecTokens)
}

// breakerTrip evaluates the circuit-breaker before the next model call and returns
// a human-readable verdict when the turn should stop, or "" to continue. It checks
// the cost ceiling and token budget against the cumulative ledger/token counts, and
// the no-progress detector against rounds since the last file write / verify-state
// change. Disabled checks (zero-value fields) never trip.
// breakerTrip evaluates the circuit-breaker before the next model call. It returns
// the stop verdict (empty when no check fired) and whether that stop is
// escalatable: a no-progress stall is a stuck floor a stronger rung might converge,
// so the caller lifts the rung instead of hard-stopping when a higher rung is
// unused (bug no-progress-breaker-pre-empts-repeated-tool-error-escalation-mid-
// turn). A cost or token ceiling is a hard spend limit and is NOT escalatable — a
// costlier rung would defeat the very ceiling that fired.
func (l *Loop) breakerTrip(round, lastProgressRound int) (verdict string, escalatable bool) {
	// Shared tree-cost ceiling (bug 1124): when a cost meter is wired, the honest
	// ceiling is the CUMULATIVE spend across the spawn tree (this loop + every worker it
	// spawned), not this loop's own ledger — which for a delegating orchestrator sees
	// only its cheap planning calls while the workers spend on separate child ledgers.
	// Checked before the per-loop breaker (and before its enabled() short-circuit, since
	// a spawned worker carries the meter but no per-loop breaker) so ANY loop in the tree
	// stops before its next model call once the shared ceiling is hit. Like the per-loop
	// cost ceiling, it is a hard spend limit and is NOT escalatable — a costlier rung
	// would defeat the very ceiling that fired.
	if l.meter != nil && l.meter.Exceeded() {
		return fmt.Sprintf("circuit-breaker: tree cost ceiling $%.2f reached ($%.4f spent across the spawn tree) — stopping before the next model call, no final answer", l.meter.Ceiling(), l.meter.Total()), false
	}
	b := l.breaker
	if !b.enabled() {
		return "", false
	}
	if b.MaxCostUSD > 0 {
		if spent := l.ledger.TotalUSD(); spent >= b.MaxCostUSD {
			return fmt.Sprintf("circuit-breaker: cost ceiling $%.2f reached ($%.4f spent over %d rounds) — stopping before the next model call, no final answer", b.MaxCostUSD, spent, round), false
		}
	}
	if b.MaxTokens > 0 && l.totalTokens >= b.MaxTokens {
		return fmt.Sprintf("circuit-breaker: token budget %d reached (%d tokens spent over %d rounds) — stopping before the next model call, no final answer", b.MaxTokens, l.totalTokens, round), false
	}
	if b.NoProgressRounds > 0 {
		if stalled := round - lastProgressRound; stalled >= b.NoProgressRounds {
			return fmt.Sprintf("circuit-breaker: no file written or verify-state change in %d rounds — stopping (stuck exploring without converging), no final answer", stalled), true
		}
	}
	return "", false
}

// spawnSurface / spawnAction name the delegation primitive (agent.spawn) the
// must-spawn guard watches for. Kept local so the guard does not depend on the
// orchestrator package (which would be an import cycle: orchestrator imports agent).
const (
	spawnSurface = "agent"
	spawnAction  = "spawn"
)

// spawnCapable reports whether this loop runs under a profile that scopes the
// agent/spawn surface — i.e. it is the orchestrate-shaped agent that CAN delegate.
// The must-spawn guard only ever fires for such a loop, so a normal worker (no agent
// surface) is never nagged to spawn. The answer is memoised (the profile is immutable
// for the loop's life). A loop with no profile is not spawn-capable.
func (l *Loop) spawnCapable() bool {
	if l.spawnCapableMemo < 0 {
		l.spawnCapableMemo = 0
		if l.profile != nil {
			for _, t := range l.profile.Tools {
				if t.Surface != spawnSurface {
					continue
				}
				// Whole-surface scope (no Actions) or an explicit spawn action grants it.
				if len(t.Actions) == 0 {
					l.spawnCapableMemo = 1
					break
				}
				for _, a := range t.Actions {
					if a == spawnAction {
						l.spawnCapableMemo = 1
						break
					}
				}
				if l.spawnCapableMemo == 1 {
					break
				}
			}
		}
	}
	return l.spawnCapableMemo == 1
}

// isSpawnCall reports whether a dispatch result is an agent.spawn call (regardless
// of OK) — the signal the must-spawn guard resets on. Even a FAILED spawn counts as
// "the orchestrator tried to delegate", so the guard stops nagging once it commits
// to delegation rather than re-firing on a spawn that erred (which the loop already
// feeds back as a tool_error for the model to adapt).
func isSpawnCall(r tool.Result) bool {
	return r.Call.Surface == spawnSurface && r.Call.Action == spawnAction
}

// verifyScoped runs the verify gate, scoping a whole-repo `go test ./...` to the
// package(s) the worker edited this turn so a single-package change isn't gated on the
// whole repository's test suite (which timed out on a large repo). Scoping is safe: it
// only narrows when every edited .go file maps to an existing package dir under the
// gate's module root, else the gate runs whole-repo unchanged.
func (l *Loop) verifyScoped(ctx context.Context, dispatches []tool.Result) (bool, string) {
	return l.verifyScopedGate(ctx, l.verify, dispatches)
}

// verifyScopedGate is verifyScoped against an explicit gate — shared by the full verify
// gate (l.verify) and the terminal green backstop (l.terminalGreen) so both scope the
// go-test run identically. On the spawn-only orchestrator the loop's own dispatches carry
// no .go edits (the WORKER edited, in its own sub-loop), so goPackagesFromEdits yields
// nothing and the gate runs whole-repo unchanged — exactly the "is the repo green?" check
// the backstop wants.
func (l *Loop) verifyScopedGate(ctx context.Context, g *VerifyGate, dispatches []tool.Result) (bool, string) {
	exists := l.dirExists
	if exists == nil {
		exists = realDirExists
	}
	return g.checkScoped(ctx, goPackagesFromEdits(dispatches, g.Dir, exists))
}

// isMutatingWrite reports whether a dispatch result is a successful filesystem
// mutation — the no-progress detector's "tangible progress" signal. A worker that
// only greps/reads for many rounds (the run-6d thrash) never trips this, so the
// detector fires; a worker that lands a write resets the stall counter.
func isMutatingWrite(r tool.Result) bool {
	if !r.OK || r.Call.Surface != "fs" {
		return false
	}
	switch r.Call.Action {
	case "write", "edit", "move", "remove":
		return true
	default:
		return false
	}
}

// maxRepeatedCallRounds is how many rounds of no progress (round-lastProgressRound)
// the loop tolerates while the model re-issues byte-identical tool calls before the
// repeated-identical-call breaker fires (bug 1088 GAP-2b). Set well below the per-cycle
// round cap so a read→evict→re-read loop is broken fast, but above a single legitimate
// retry so a one-off repeat is never misread as a stall. A configured no-progress
// breaker with a threshold this low or lower still fires first (it is checked at the
// round top); this breaker is the read-specific, faster-by-default backstop.
const maxRepeatedCallRounds = 3

// toolCallSignature returns a deterministic string identifying a round's tool calls
// by surface/action/params — NOT the provider-assigned call id, which varies per turn
// — so the loop can recognise the model re-issuing the exact same call(s). Params are
// JSON-marshalled (Go sorts map keys, so the encoding is stable). Empty when there are
// no tool calls (a plain answer is never a re-read loop).
func toolCallSignature(calls []tool.Call) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range calls {
		b.WriteString(c.Surface)
		b.WriteByte('.')
		b.WriteString(c.Action)
		if len(c.Params) > 0 {
			if p, err := json.Marshal(c.Params); err == nil {
				b.Write(p)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// boundWithinTurn keeps the live context under the compaction budget DURING a turn
// — not only at turn boundaries — by eliding the bodies of the oldest tool results
// once accumulation across many rounds nears the budget. The turn-group compactor
// can't help here (a single turn is one group), so a long investigation would
// otherwise overflow/bloat mid-turn (run-6b 14k overflow; run-6d 178 results) and
// hand the escalated strong rung a raw bloated transcript. Run at the TOP of every
// round (before the model call), it also gives the strong rung a bounded context on
// escalation — the call after any rung lift sees the elided transcript. The goal
// anchor and recent results are preserved. Best-effort + deterministic (no
// summarizer call); returns an event when it elided anything, else nil.
func (l *Loop) boundWithinTurn() *CompactionEvent {
	if l.compactor == nil || l.compactor.budget <= 0 {
		return nil
	}
	before := l.compactor.currentSize(l.transcript, l.toolSpecTokens)
	if before <= l.compactor.budget {
		return nil
	}
	// The within-turn keep is the SOFT tool-result floor (minKeepToolResults), NOT
	// the turn-group recency window (recencyTurns): the two are different units, and
	// using recencyTurns (e.g. 6) as a tool-result count let a 3-whole-file-read
	// coding carry evict NOTHING — every read stayed verbatim and the floor window
	// overflowed → escalated to Opus (bug 1066). Eviction targets the ratio-corrected
	// size (the same signal the floor-fit guard checks) and may fall below the soft
	// floor down to hardKeepToolResults under real window pressure, so a single capped
	// read fits the floor budget.
	keep := minKeepToolResults
	n := evictToolResults(l.transcript, l.compactor.budget, l.toolSpecTokens, keep, hardKeepToolResults, l.compactor.tokenRatio)
	if n == 0 {
		return nil
	}
	return &CompactionEvent{
		TurnIndex:     l.turns,
		TokensBefore:  before,
		TokensAfter:   l.compactor.currentSize(l.transcript, l.toolSpecTokens),
		GroupsEvicted: n,
		Budget:        l.compactor.budget,
		Overhead:      l.toolSpecTokens,
	}
}

// maybeCompact runs a turn-boundary compaction when a compactor is configured,
// returning the event (nil when nothing was compacted or none is configured). It
// is best-effort: a summarizer error leaves the transcript untouched and is
// swallowed so the turn proceeds. The calibrated tool-spec overhead persists across
// the rewrite (the specs are still offered next turn), unlike the transcript it
// rewrites.
func (l *Loop) maybeCompact(ctx context.Context) *CompactionEvent {
	if l.compactor == nil {
		return nil
	}
	if l.goalAnchor == "" {
		l.goalAnchor = firstUserContent(l.transcript)
	}
	newT, newSummary, ev, err := l.compactor.compact(ctx, l.transcript, l.goalAnchor, l.summaryBody, l.toolSpecTokens, l.turns)
	if err != nil || ev == nil {
		return nil
	}
	l.transcript = newT
	l.summaryBody = newSummary
	return ev
}

// Run drives one user prompt to a final answer, dispatching any tool calls the
// model requests along the way. The transcript persists across calls.
func (l *Loop) Run(ctx context.Context, prompt string) (res Result, err error) {
	// Stamp the highest rung this run actually served onto every return path, so the
	// coding orchestrator can carry the reached tier into the next respawn (chain 392
	// task 3314). The router always has at least one tier, so HighestModel is total.
	defer func() {
		if l.router != nil {
			res.HighestTierModel = l.router.HighestModel()
		}
	}()
	if !l.started {
		l.hooks.Fire(hooks.SessionStart, l.hookCtx(hooks.SessionStart, 0))
		l.started = true
	}

	// Context compaction (turn boundary): bound the transcript before this turn
	// grows it further. Fires only when a Compactor is configured and the live size
	// exceeds budget; it cuts on turn boundaries so tool_use/tool_result pairs stay
	// intact. Best-effort — a summarizer error skips compaction for this turn and
	// never aborts it. The rewrite invalidates the last measured token count.
	compaction := l.maybeCompact(ctx)

	turn := l.turns
	l.turns++
	startLen := len(l.transcript) // persist only this turn's new messages

	// Stamp this loop's run id onto the dispatch context so a worker spawned this
	// turn (via the agent.spawn tool) can link its session to this one — the
	// sub-orchestration tree edge the tree telemetry reads (T6).
	if l.sessionID != "" {
		ctx = WithParentRunID(ctx, l.sessionID)
	}

	pre := l.hookCtx(hooks.PreUserPrompt, turn)
	pre.UserPrompt = prompt
	// Optional context probe (e.g. firing parse_context on the prompt): its result
	// is stashed in the hook Metadata BEFORE pre_user_prompt hooks run, so a
	// profile-aware pruner/injector can shape it. Best-effort — a nil result (probe
	// failed/disabled) leaves Metadata untouched.
	if l.contextProber != nil {
		if md := l.contextProber(ctx, prompt); md != nil {
			pre.Metadata = md
		}
	}
	l.hooks.Fire(hooks.PreUserPrompt, pre)
	for _, add := range pre.SystemPromptAdditions {
		l.transcript = append(l.transcript, model.ChatMessage{Role: model.RoleSystem, Content: add})
	}

	// ts accumulates the turn's non-tally escalation signals (explicit handoff +
	// confidence) harvested from the hook contexts fired during the turn, plus the
	// round-budget-exhausted flag set if the loop falls out of the round cap.
	var ts turnSignals
	ts.harvest(pre)

	l.transcript = append(l.transcript, model.ChatMessage{Role: model.RoleUser, Content: prompt})
	if l.goalAnchor == "" {
		l.goalAnchor = prompt // the active-goal anchor, pinned across compactions
	}
	preTurn := l.hookCtx(hooks.PreTurn, turn)
	l.hooks.Fire(hooks.PreTurn, preTurn)
	ts.harvest(preTurn)

	var dispatches []tool.Result
	turnModel := ""
	var fr faultRecovery
	// cycleStart marks the round at which the current write→verify→revise cycle
	// began. The maxRounds runaway-guard is measured PER cycle (round-cycleStart),
	// not across the whole turn, so each verify-fail revise attempt gets a fresh
	// tool-round allowance instead of sharing one budget — a multi-fix gate loop is
	// no longer starved by the guard (bug max-tool-rounds-cap-...-starves-verify-
	// revise-loop). The overall turn stays bounded: verify-revise cycles are capped
	// by the gate's own MaxRounds, and a genuine runaway (tool calls that never claim
	// done) never resets cycleStart, so it still trips the guard at maxRounds.
	cycleStart := 0
	// lastProgressRound is the most recent round that made tangible progress — a
	// successful file mutation or a verify-state change. The no-progress breaker
	// measures rounds since it; it starts at 0 so the first NoProgressRounds rounds
	// without a write trip the detector (the run-6d grep-thrash signature).
	lastProgressRound := 0
	// prevRung tracks the router's resting rung between rounds. A mid-turn escalation
	// (EscalateForFault/EscalateForStuckVerify/EscalateForNoProgress lifts the rung
	// immediately) is a fresh attempt by a more-capable model, so it resets the
	// no-progress stall counter — otherwise the breaker counts the pre-escalation
	// stall against the just-escalated rung and stops the run before it gets a single
	// round (run-7: the strong rung was escalated-to but never produced a turn). The
	// no-progress breaker's own escalation (below) resets lastProgressRound directly;
	// this catch is for the fault/stuck-verify lifts that happen elsewhere in a round.
	prevRung := ""
	// lastSpawnRound anchors the must-spawn guard: the round at which the
	// orchestrator last delegated (its baseline). It starts at 0 so the first
	// spawnForceEvery read-only rounds without a spawn arm the forcing reminder. A
	// spawn dispatch advances it, clearing the nudge so a post-spawn synthesis turn is
	// not nagged. Used only when the loop is spawn-capable (bug 1072).
	lastSpawnRound := 0
	// prevCallSig / repeatedCallRounds drive the repeated-identical-call breaker (bug
	// 1088 GAP-2b): a within-turn eviction tells the model to "re-run the tool", and a
	// model that obliges gets the same too-big result evicted again — a read→evict→
	// re-read loop. Counting consecutive rounds whose tool calls are byte-identical (and
	// progress-free, since a landed write changes the calls) lets the loop break it fast
	// instead of spinning to the round cap.
	// prevCallSig holds the previous round's tool-call signature for the repeated-
	// identical-call breaker (bug 1088 GAP-2b): a within-turn eviction tells the model
	// to "re-run the tool", and a model that re-issues the SAME read just gets the same
	// too-big result evicted again — a read→evict→re-read loop. Pairing an identical
	// signature with the existing no-progress stall window (round-lastProgressRound)
	// lets the loop break it without a second, conflicting counter: a landed write
	// advances lastProgressRound (so repeated WRITES never trip it), and an escalation
	// resets the window (so it never counts across a rung change).
	prevCallSig := ""
	// exhaustionDetail, when set by a break, overrides the generic "exceeded max tool
	// rounds" terminal verdict with an honest reason (e.g. the repeated-read loop), so
	// the operator sees WHY the run stopped rather than a misleading round-cap message.
	exhaustionDetail := ""
	for round := 0; ; round++ {
		// Per-cycle round-budget guard. When the current rung spends its whole tool-round
		// budget without converging, escalate to a stronger rung with a FRESH budget before
		// giving up — a worker that keeps MUTATING (so the no-progress breaker never trips, it
		// "progresses" every round) yet never claims done, with no verify gate to trip
		// stuck-verify, leaves round-exhaustion as the only signal the rung cannot finish (bug
		// coding-worker-thrashes-to-tool-round-exhaustion-without-escalating). Only when no
		// higher rung remains do we break out to the green backstop + honest exhaustion verdict.
		if round-cycleStart >= l.maxRounds {
			// Backstop FIRST: a worker that produced a passing fix but never cleanly claimed
			// done has a green tree right now — capture it before spending another budget,
			// rather than discarding the landed fix or wastefully escalating an already-done
			// task (the always-on exhaustion backstop; same instinct as closeOnTerminalFault).
			if res, done := l.opportunisticGreen(ctx, dispatches, turn, startLen, turnModel, ts, compaction, ""); done {
				return res, nil
			}
			// Still not done: lift to a stronger rung with a FRESH budget before giving up.
			if edge := l.router.EscalateForRetryExhaustion(round - cycleStart); edge.Direction == router.EdgeEscalate {
				l.recordEscalationEdge(ctx, turn, edge)
				cycleStart = round        // the lifted rung gets a fresh per-cycle round budget
				lastProgressRound = round // and a fresh stall window
				continue
			}
			break // no higher rung — fall through to the honest exhaustion verdict
		}
		cur := l.router.CurrentModel()
		if prevRung != "" && cur != prevRung {
			lastProgressRound = round // a rung change is a fresh attempt
		}
		prevRung = cur
		// Re-point the compaction budget at the rung now serving, so escalation off the
		// small-window floor onto a wider tier actually buys context (bug 1088 GAP-2a):
		// the within-turn bound, compaction and per-result cap all key off compactor.budget.
		l.syncBudgetToRung()
		// Strong-bound halt (bug 1087): the frontier (bounded top) rung served its whole
		// turn budget as the no-progress last resort and the task is STILL stuck on it.
		// The old stickyTop bypass spent unbounded frontier here (Opus 12/2, $3.45); stop
		// instead — neither more frontier nor a bounce to a floor that already failed is
		// right. Capture any green fix the frontier landed first, then halt with an honest,
		// gate-evidenced verdict. Checked before the model call so the (bound+1)th frontier
		// turn is never served.
		if l.router.StrongBoundExhausted() {
			if res, done := l.opportunisticGreen(ctx, dispatches, turn, startLen, turnModel, ts, compaction, ""); done {
				return res, nil
			}
			perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
			reason := fmt.Sprintf("the frontier (%s) served its %d bounded turn(s)", l.router.CurrentModel(), l.router.BoundMax())
			if l.router.StrongBudgetExhausted() {
				// The halt is tree-wide, not this worker's own turns: the run's shared strong-turn
				// budget (-max-strong-turns) is spent, so the frontier is refused to every worker —
				// a stuck atom's respawns can't re-climb Opus (bug 1165(b)).
				reason = "the run's tree-wide strong-turn budget (-max-strong-turns) is spent, so the frontier is refused to further respawns"
			}
			verdict := fmt.Sprintf("circuit-breaker: strong-bound reached — %s with the task still stuck; halting rather than spending unbounded frontier on a run the floor can't finish, no final answer", reason)
			escalate := ""
			if l.verify != nil {
				escalate = escalateVerdict(verdict + " — with verification unsatisfied")
			}
			return Result{Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction, Stopped: verdict, Escalate: escalate}, nil
		}
		// Circuit-breaker: before spending another model call, stop a runaway turn
		// (cost/token ceiling reached, or no progress for too long) with an honest
		// verdict rather than a runaway or a silent kill (bug 1045). An escalation just
		// above reset the stall counter, so a freshly-lifted rung is never guillotined.
		if verdict, escalatable := l.breakerTrip(round, lastProgressRound); verdict != "" {
			// A no-progress stall is a stuck floor: rather than hard-stopping, lift the
			// rung first (a stronger model may converge where the floor thrashed tool
			// errors or read-only loops) when a higher rung is unused, and grant it a
			// fresh stall window. Only when no higher rung remains does the breaker
			// hard-stop — preserving the honest terminal verdict (T7). This is the fix
			// for no-progress-breaker-pre-empts-repeated-tool-error-escalation-mid-turn:
			// the floor's within-turn thrash never reaches the turn-boundary Observe, so
			// the breaker itself is the escalation point. Cost/token ceilings are not
			// escalatable (a costlier rung would defeat the ceiling) and always stop.
			if escalatable {
				if edge := l.router.EscalateForNoProgress(round - lastProgressRound); edge.Direction == router.EdgeEscalate {
					l.recordEscalationEdge(ctx, turn, edge)
					lastProgressRound = round // the lifted rung gets a fresh stall window
					continue
				}
			}
			perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
			// If a verify gate was configured we only reach the breaker with verification
			// still UNSATISFIED (a passed gate returns earlier), so the breaker's terminal
			// state is also unverified/escalate — never a silent done (T7).
			escalate := ""
			if l.verify != nil {
				escalate = escalateVerdict(verdict + " — with verification unsatisfied")
			}
			return Result{Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction, Stopped: verdict, Escalate: escalate}, nil
		}
		// Within-turn bound: before spending the next model call, keep the live
		// context under budget as tool results accumulate across rounds (the turn-
		// boundary compactor above can't cut inside one turn). Running here also means
		// the strong rung receives a bounded context on any escalation (bug 1047).
		if ev := l.boundWithinTurn(); ev != nil {
			compaction = ev
		}
		// Goal-anchor reinforcement: under a long loop the pinned first prompt gets
		// buried; re-surface it as a terse reminder near the tail every N rounds so
		// the worker keeps its target (bug 3093). Done after the within-turn bound so
		// the reminder isn't itself elided, and only between rounds (never mid-pair).
		if l.goalReminderEvery > 0 && round > 0 && round%l.goalReminderEvery == 0 {
			l.transcript = refreshGoalReminder(l.transcript, l.goalAnchor)
		}
		// Must-spawn guard (bug 1072): a spawn-capable (orchestrate) agent that has
		// run spawnForceEvery rounds without EVER delegating is over-investigating — its
		// own tools cannot edit anything, so reading further never makes progress. Inject a
		// terse forcing reminder near the tail (re-armed every spawnForceEvery further
		// read-only rounds) so the loop deterministically forces the delegation the prompt
		// only asked for. Scoped to spawn-capable loops, so a normal worker is never nagged.
		// PRE-FIRST-SPAWN ONLY (lastSpawnRound == 0): once the orchestrator has delegated,
		// the "never delegates" pathology is resolved — the post-spawn reads it does are the
		// declare-done confirming-read pattern, NOT over-investigation, and nagging them back
		// into a redundant spawn defeats declare-done and burns the frontier rung to a halt
		// (bug spawn-now-nudge-fires-on-post-success-confirming-reads). Run between rounds
		// (never mid tool_use/tool_result pair) and after the within-turn bound so the
		// reminder isn't itself elided.
		if l.spawnForceEvery > 0 && l.spawnCapable() && lastSpawnRound == 0 {
			if readRounds := round - lastSpawnRound; readRounds >= l.spawnForceEvery {
				l.transcript = refreshSpawnForce(l.transcript, readRounds)
			}
		}
		// Proactive floor-fit guard: if the next call would route to the small-window
		// floor (resting on it, OR a spent bound about to bounce there) and the
		// projected prompt won't safely fit the floor window, compact harder; if it
		// STILL won't fit, escalate to a larger-window rung BEFORE the call — bypassing
		// the usage bound, because a floor that physically cannot hold the prompt is
		// mandatory escalation (the VRAM-fixed floor window can't grow). This keeps a
		// would-overflow prompt OFF the floor: no wasted 400, and no bounce-the-stuck-
		// floor-to-death when the strong bound is spent (bug escalation-bound-exhaustion-
		// falls-back-to-overflowing-floor-and-dies-after-fix-landed).
		if l.floorWindow > 0 && l.router.WillServeFloor() && !floorFits(l.ContextTokens(), l.floorWindow) {
			if ev := l.forceCompact(ctx, 1); ev != nil {
				compaction = ev
			}
			if !floorFits(l.ContextTokens(), l.floorWindow) {
				if edge := l.router.EscalateForOverflow(); edge.Direction == router.EdgeEscalate {
					l.recordEscalationEdge(ctx, turn, edge)
					lastProgressRound = round // a fresh rung gets a fresh stall window
					continue
				}
				// No higher rung and can't compact under the floor window: fall through
				// and let the reactive overflow recovery give an honest, gate-evidenced
				// verdict (closeOnTerminalFault) rather than guess.
			}
		}
		adapter := l.router.NextAdapter()
		sentEstimate := estimateTokens(l.transcript) // the transcript portion of this call
		resp, err := adapter.Complete(ctx, l.transcript, l.specs)
		if err != nil {
			// A model-call fault no longer aborts the run: classify it and try to
			// recover in-loop (compact-and-retry on overflow, a bounded re-prompt on a
			// malformed tool call, shrink/escalate on timeout), lifting a stuck floor to
			// a stronger rung only when local recovery is exhausted (local-tier-first).
			action, rerr := l.recoverFromFault(ctx, err, &fr, turn)
			switch action {
			case recoverRetry:
				continue // recovered (compacted / re-prompted / escalated): retry the round
			case recoverGraceful:
				// The per-turn deadline itself is spent — no further call can succeed on
				// this context. The work may ALREADY have landed (a worker that edited then
				// timed out before it could cleanly claim done), so before ending the turn
				// run the orchestrator verify gate on the CURRENT tree: a green gate reports
				// SUCCESS and a red/unverified one yields an honest unverified/escalate
				// verdict — never exit 0 with an empty, unverified answer (bug 1102). The
				// gate runs under a fresh context (closeOnGracefulFault) because this ctx is
				// spent. With no gate configured this preserves the prior graceful end.
				if res, handled := l.closeOnGracefulFault(ctx, dispatches, turn, startLen, turnModel, ts, compaction, err); handled {
					return res, nil
				}
				// No verify gate to evidence a landed fix: end the turn without killing the
				// run (a REPL/-resume continues); surface the fault class instead of a fatal
				// error.
				perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
				return Result{Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction, ModelFault: string(model.ClassifyFault(err))}, nil
			default:
				// Terminal model-call fault (e.g. a floor overflow with no rung left to
				// reach). The work may ALREADY have landed — a strong-rung turn can make
				// the fix before a later round overflows the floor. So before reporting a
				// failure, run the orchestrator verify gate on the CURRENT tree: a green
				// gate means the fix is in (report success, not a bare error), and a red /
				// absent gate yields an honest, gate-evidenced verdict — never a silent
				// loss of a landed fix (bug escalation-bound-exhaustion-falls-back-to-
				// overflowing-floor-and-dies-after-fix-landed).
				if res, handled := l.closeOnTerminalFault(ctx, dispatches, turn, startLen, turnModel, ts, compaction, err); handled {
					return res, nil
				}
				// No verify gate to evidence a landed fix: the turn is aborting. Still CLOSE
				// the turn row (ended_at stamped, an `aborted` signal marker) so a started turn
				// never leaks as in-flight, and fold any best-effort persist failure into the
				// returned error (errors.Join) so a telemetry write lost during the abort stays
				// observable rather than vanishing behind a bare Result{} (suggestion 49).
				ts.aborted = true
				perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
				return Result{}, errors.Join(rerr, perr)
			}
		}
		l.accountForCall(resp, sentEstimate, turn, &turnModel)
		// Recover a narrated tool call: a model that WROTE its call as JSON text
		// instead of emitting a structured tool call gets that intent EXECUTED, not
		// discarded as a work-less answer (rehearsal runs 15–17: Qwen narrated
		// fs.edit/sys.exec). The recovered call rides the normal dispatch path below.
		if len(resp.ToolCalls) == 0 {
			if recovered := recoverNarrated(resp.Text, l.offeredSurfaces()); len(recovered) > 0 {
				// A recovered call has no provider-assigned id; synthesize a unique one
				// so the assistant tool_use block and its tool_result agree (and the
				// Anthropic adapter, which requires tool_use.id, accepts it on escalation).
				for i := range recovered {
					l.narratedSeq++
					recovered[i].ID = fmt.Sprintf("narrated-%d", l.narratedSeq)
				}
				resp.ToolCalls = recovered
			}
		}
		// Repeated-identical-call breaker (bug 1088 GAP-2b): when the within-turn bound
		// evicts a result and instructs "re-run the tool", a model that re-issues the
		// SAME read just gets the same too-big result evicted again — a read→evict→
		// re-read loop. Trip on byte-identical tool call(s) that have made no progress for
		// maxRepeatedCallRounds rounds (round-lastProgressRound: a landed write resets it,
		// so repeated writes are exempt; an escalation resets it, so it never counts across
		// a rung change — and the configured no-progress breaker, checked at the round TOP,
		// still wins when its threshold is no higher). Break it fast: escalate for the wider
		// context that dissolves the loop (post-GAP-2a a higher rung carries a bigger
		// budget), or — with no higher rung — capture any green fix and fall through to an
		// honest verdict instead of spinning to the round cap.
		sig := toolCallSignature(resp.ToolCalls)
		if sig != "" && sig == prevCallSig && round-lastProgressRound >= maxRepeatedCallRounds {
			if edge := l.router.EscalateForNoProgress(round - lastProgressRound); edge.Direction == router.EdgeEscalate {
				l.recordEscalationEdge(ctx, turn, edge)
				cycleStart = round
				lastProgressRound = round
				prevCallSig = ""
				continue
			}
			if res, done := l.opportunisticGreen(ctx, dispatches, turn, startLen, turnModel, ts, compaction, resp.Text); done {
				return res, nil
			}
			exhaustionDetail = fmt.Sprintf("stuck re-reading identical input for %d rounds with no rung left for more context — the active window cannot hold this result; narrow the read (a line range or symbol/outline view) instead of re-reading the whole file", round-lastProgressRound)
			break
		}
		prevCallSig = sig
		l.transcript = append(l.transcript, model.ChatMessage{
			Role:      model.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		})
		if len(resp.ToolCalls) == 0 {
			// The model emitted no tool calls — it claims done. The loop (not the model)
			// decides: fabrication audit → orchestrator-owned verify/revise → fake-green
			// audit → success. Each path either continues a revise cycle or returns a terminal
			// result; a continue re-baselines cycleStart/lastProgressRound for the fresh cycle.
			res, act := l.onDeclaredDone(ctx, resp, turn, startLen, round, turnModel, ts, compaction, dispatches, &cycleStart, &lastProgressRound)
			if act == actContinue {
				continue
			}
			return res, nil
		}
		roundMutated, spawnElapsed := l.dispatchRound(ctx, resp.ToolCalls, turn, round, turnModel, &ts, &dispatches, &lastProgressRound, &lastSpawnRound)
		// Exclude the wall-clock spent BLOCKED on agent.spawn from the orchestrator's per-turn
		// deadline: the spawned worker ran on its own budget, so that time is not the
		// orchestrator's thinking — counting it starved the synthesis turn and timed the run out
		// even when the orchestrator did nothing slow itself (Run-39/40). Push the deadline out
		// by the block so the orchestrator keeps its own budget for synthesis.
		if spawnElapsed > 0 {
			var extCancel context.CancelFunc
			ctx, extCancel = extendTurnDeadline(ctx, spawnElapsed)
			defer extCancel()
		}
		// Mid-flight reconcile: when this round landed a mutation and the throttle allows,
		// run the verify gate NOW. On GREEN, a worker that already produced a passing fix but
		// keeps editing (run-6: a correct fix, then a tail of failing edits until the round
		// budget was spent) is captured the moment the tree is green, instead of being
		// discarded as "exceeded max tool rounds". On RED, the failing gate output is fed back
		// so a test-authoring worker reconciles a guessed/output-dependent assertion against
		// the REAL test output before claiming done (bug 1103) — the in-loop test-execution
		// signal it can't get via the risk-gated sys.exec. The orchestrator re-certifies any
		// returned green with its own gate + red-before-green guards.
		if l.opportunisticEvery > 0 && roundMutated && round-l.lastOpportunisticRound >= l.opportunisticEvery {
			l.lastOpportunisticRound = round
			if res, done := l.opportunisticReconcile(ctx, dispatches, turn, startLen, round, turnModel, ts, compaction, resp.Text, &lastProgressRound); done {
				return res, nil
			}
		}
	}
	// The exhaustion guard at the top of the loop already ran the green backstop (and found
	// no green) and exhausted the escalation ladder before breaking here, so this is the
	// honest terminal verdict: the round budget is spent on every rung with the tree still red.
	ts.roundsExhausted = true
	perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
	verdict := fmt.Sprintf("exceeded max tool rounds (%d)", l.maxRounds)
	if exhaustionDetail != "" {
		verdict = exhaustionDetail
	}
	return Result{Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction}, fmt.Errorf("%s", verdict)
}

// accountForCall folds one model response into the run's accounting: it adds the call's cost to
// the ledger and the shared tree-cost meter (bug 1124), accrues token totals, calibrates the
// fixed tool-spec overhead / transcript token-ratio from the measured input count (split so a
// code-heavy turn cannot inflate "overhead" without bound), records the turn-start on the first
// call, and records the per-call cost row. turnModel is set on the first call and read thereafter.
func (l *Loop) accountForCall(resp model.Response, sentEstimate, turn int, turnModel *string) {
	usd := l.ledger.Add(resp.Model, cost.Usage{
		InputTokens:       resp.Usage.InputTokens,
		CachedInputTokens: resp.Usage.CachedInputTokens,
		CacheWriteTokens:  resp.Usage.CacheWriteTokens,
		OutputTokens:      resp.Usage.OutputTokens,
		ProviderCostUSD:   resp.Usage.CostUSD,
		ProviderReported:  resp.Usage.CostReported,
	})
	l.totalTokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
	// The shared tree-cost meter sums every model call across the spawn tree (this loop's and
	// every worker's) so the next breakerTrip on any of them sees the whole-tree total. Nil for
	// a standalone, per-loop-bounded run.
	if l.meter != nil {
		l.meter.Add(usd)
	}
	// Calibrate from the measured input count: the provider counts the offered specs + chat-
	// template scaffolding (fixed overhead) PLUS the transcript at the REAL tokenizer, while
	// sentEstimate is our len/4 transcript estimate. Split the two so a code-heavy turn's
	// tokenizer under-count is not folded into "overhead":
	//   - toolSpecTokens (fixed overhead): set ONCE from the first call, when the transcript is
	//     just the system+user seed so the residual ≈ real overhead.
	//   - tokenRatio (real/estimated transcript): folded into the compactor so the size signal
	//     corrects the len/4 under-count WITHOUT inflating overhead.
	if resp.Usage.InputTokens > 0 {
		if !l.toolSpecCalibrated {
			if ov := resp.Usage.InputTokens - sentEstimate; ov > 0 {
				l.toolSpecTokens = ov
			}
			l.toolSpecCalibrated = true
		} else if l.compactor != nil && sentEstimate > 0 {
			if realTr := resp.Usage.InputTokens - l.toolSpecTokens; realTr > 0 {
				l.compactor.observeTokenRatio(float64(realTr) / float64(sentEstimate))
			}
		}
	}
	if *turnModel == "" {
		*turnModel = resp.Model
		l.recordTurnStart(turn, resp.Model)
	}
	l.recordCost(turn, resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens, usd)
}

// dispatchRound executes the round's tool calls in order: for each it fires pre_tool_use (which
// may veto), runs the call (or builds the veto/truncated-write refusal result), records it, fires
// post_tool_use, and appends the tool result to the transcript. A landed mutating write advances
// *lastProgressRound (and reports roundMutated); a spawn re-baselines *lastSpawnRound and clears
// the must-spawn nudge. ts harvests each fired hook context. Returns whether any write mutated the
// tree this round (the mid-flight stop-when-green throttle).
func (l *Loop) dispatchRound(ctx context.Context, calls []tool.Call, turn, round int, turnModel string, ts *turnSignals, dispatches *[]tool.Result, lastProgressRound, lastSpawnRound *int) (bool, time.Duration) {
	roundMutated := false
	var spawnElapsed time.Duration // wall-clock spent BLOCKED on agent.spawn dispatches this round
	for _, call := range calls {
		call := call
		pc := l.hookCtx(hooks.PreToolUse, turn)
		pc.ToolCall = &call
		l.hooks.Fire(hooks.PreToolUse, pc)
		ts.harvest(pc)

		// A pre_tool_use hook (the risk gate) may veto the call. A blocked call
		// is not dispatched; the reason is fed back to the model as a tool_error
		// result so it can adapt (request a smaller action, or stop).
		var res tool.Result
		truncReason := l.truncatedWriteReason(call)
		dispatchStart := time.Now()
		switch {
		case pc.DenyToolCall:
			// A non-escalatable denial (protect-path, policy/config) is ClassUsage so it
			// does not trip repeated_tool_error — no stronger model can lift it (bug 1095).
			// A plain veto (the risk gate) stays ClassTool, its prior escalatable behavior.
			denyClass := tool.ClassTool
			if pc.DenyNonEscalatable {
				denyClass = tool.ClassUsage
			}
			res = tool.Result{Call: call, OK: false, ErrorClass: denyClass,
				Value: map[string]any{"error": pc.DenyReason, "risk_gate": "blocked"}}
		case truncReason != "":
			// Refuse a whole-file overwrite of a path the model only saw truncated:
			// the unseen remainder would be lost (run-9). A worker-recoverable
			// precondition (ClassUsage), fed back so the worker adapts to fs.edit or
			// re-reads the whole file first — it must not trip escalation on its own.
			res = tool.Result{Call: call, OK: false, ErrorClass: tool.ClassUsage,
				Value: map[string]any{"error": truncReason, "fs_guard": "truncated_view"}}
		default:
			res = l.provider.Dispatch(ctx, call)
		}
		*dispatches = append(*dispatches, res)
		if isMutatingWrite(res) {
			*lastProgressRound = round // a file landed: real progress this round
			roundMutated = true
		}
		if isSpawnCall(res) {
			// The orchestrator delegated: re-baseline the must-spawn guard and clear
			// any standing forcing reminder so a post-spawn synthesis turn isn't nagged
			// (bug 1072). Even a failed spawn counts as committing to delegation.
			*lastSpawnRound = round + 1
			l.transcript = clearSpawnForce(l.transcript)
			// A spawn dispatch BLOCKS while the spawned worker runs (on its own detached
			// budget). That wall-clock is the WORKER's time, not the orchestrator's
			// thinking — so the caller extends the orchestrator's per-turn deadline by it,
			// keeping a slow worker from starving the orchestrator's synthesis turn.
			spawnElapsed += time.Since(dispatchStart)
		}
		rawResult := encodeValue(res.Value)
		l.trackTruncatedView(call, rawResult)
		l.recordToolCall(turn, call, res, turnModel)

		oc := l.hookCtx(hooks.PostToolUse, turn)
		oc.ToolCall = &call
		oc.ToolResult = &res
		l.hooks.Fire(hooks.PostToolUse, oc)
		ts.harvest(oc)

		l.transcript = append(l.transcript, model.ChatMessage{
			Role:       model.RoleTool,
			ToolCallID: call.ID,
			Name:       call.Surface,
			Content:    l.capToolResult(rawResult),
		})
	}
	return roundMutated, spawnElapsed
}

// extendTurnDeadline returns a context like ctx but with its deadline pushed out by extra. It
// keeps ctx's CANCELLATION (e.g. SIGINT) but ignores ctx's own deadline EXPIRY — we are
// deliberately extending past it. It is how the orchestrator excludes blocking-spawn wall-clock
// from its per-turn budget: a spawned worker has its own budget, so the time the orchestrator
// spends BLOCKED on agent.spawn must not count against the orchestrator's deadline (otherwise a
// slow worker starves the synthesis turn → a false per-turn timeout). With no deadline on ctx, or
// a non-positive extension, it returns ctx with a no-op cancel.
func extendTurnDeadline(ctx context.Context, extra time.Duration) (context.Context, context.CancelFunc) {
	dl, ok := ctx.Deadline()
	if !ok || extra <= 0 {
		return ctx, func() {}
	}
	// WithoutCancel drops ctx's deadline + auto-cancel (keeps Values); re-arm our LATER deadline.
	dctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), dl.Add(extra))
	// Propagate a GENUINE cancellation of ctx (SIGINT), but not its (now extended-past) deadline.
	stop := context.AfterFunc(ctx, func() {
		if ctx.Err() == context.Canceled {
			cancel()
		}
	})
	return dctx, func() { stop(); cancel() }
}

// loopAction is what Run's round loop should do after onDeclaredDone: continue the current
// conversation (a fresh revise/re-prompt cycle) or return the carried terminal Result.
type loopAction int

const (
	actReturn   loopAction = iota // return the carried Result and end the run
	actContinue                   // continue the round loop (a revise/re-prompt cycle was queued)
)

// onDeclaredDone runs the loop's decision when the model emitted no tool calls (it claims done):
// the fabrication-stage audit (no-work / prose-narrated call / required-read), then — when a gate
// is configured — orchestrator-owned verification with a bounded revise loop (grounding-augmented
// feedback + stuck-floor escalation), then the fake-green-stage audit, then a clean success. A
// queued re-prompt/revise re-baselines *cycleStart and *lastProgressRound and returns actContinue;
// every terminal outcome (fabricated, unverified/escalate, fake-green refusal, or a verified
// success) returns the Result with actReturn. The model's self-report never overrides the gate (T7).
func (l *Loop) onDeclaredDone(ctx context.Context, resp model.Response, turn, startLen, round int, turnModel string, ts turnSignals, compaction *CompactionEvent, dispatches []tool.Result, cycleStart, lastProgressRound *int) (Result, loopAction) {
	// Fabrication-stage audits: the agent claims done, but the LOOP cross-checks that
	// claim against the REAL dispatch record (a worker cannot revise this — it is
	// computed after generation, like hermes's post-turn footer). The registry runs
	// every fabrication-stage guard in order (work-audit: no-work / prose-narrated call;
	// required-read: a declared contract source never read) and the FIRST refusal fails
	// the claim as fabrication. Adding a guard is register, not another branch here.
	guardIn := GuardInput{FinalText: resp.Text, Dispatches: dispatches, LoopGated: l.verify != nil}
	if verdict, refused := l.guards.assess(ctx, StageFabrication, guardIn); refused {
		// In-turn re-prompt (bug 1078): a no-work/fabricated done-claim is NOT a
		// terminal — within the re-prompt budget, hand the model the audit verdict
		// and require a REAL dispatch, then continue the SAME conversation (mirroring
		// the verify-gate revise loop below). This breaks the old failure where the
		// terminal Fabricated short-circuited the per-AT loop and a fresh worker
		// respawned only to re-narrate the same no-work until the budget burned. Only
		// once the re-prompts are spent does the claim terminate as Fabricated — the
		// hard backstop the operator seat escalates from.
		if l.fabricationRounds < l.fabricationReprompts {
			l.fabricationRounds++
			l.transcript = append(l.transcript, model.ChatMessage{
				Role: model.RoleUser,
				Content: fabricationRepromptPreamble + verdict.Reason +
					"\nNarration is not execution — actually CALL fs.edit/fs.write to make the change now, then stop. " +
					"If you genuinely cannot, say so plainly and do NOT claim done.",
			})
			// A re-prompt is a fresh revise cycle and is itself progress (the model
			// is acting on new feedback), so the runaway + no-progress breakers don't
			// kill a legitimate correction.
			*cycleStart = round + 1
			*lastProgressRound = round
			return Result{}, actContinue
		}
		perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
		return Result{Text: resp.Text, Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction, Fabricated: verdict.Reason}, actReturn
	}
	// Orchestrator-owned verification: the agent claims done, but the LOOP
	// decides — it runs the fixed gate itself. On a non-zero exit (within the
	// revise budget) the output is fed back as a user turn and the loop
	// continues so the agent fixes it; the agent cannot skip this gate.
	if l.verify != nil {
		ok, out := l.verifyScoped(ctx, dispatches)
		if !ok {
			if l.verifyRounds < l.verify.maxRoundsOrDefault() {
				l.verifyRounds++
				// Stuck-floor escalation: a verify gate that stays RED across
				// repeated revise cycles means the current rung cannot converge —
				// a retry exhaustion. Every Nth consecutive failure lifts the
				// ladder one rung so the strong rung gets the remaining revise
				// budget, instead of the floor burning it all (bug
				// escalation-does-not-fire-on-persistently-stuck-floor). The first
				// N-1 attempts stay on the floor (local-tier-first).
				if l.verifyStuckEscalateAfter > 0 && l.verifyRounds%l.verifyStuckEscalateAfter == 0 {
					edge := l.router.EscalateForStuckVerify(l.verifyRounds)
					if edge.Direction == router.EdgeEscalate {
						l.recordEscalationEdge(ctx, turn, edge)
					}
				}
				// Structural grounding (follow-on to bug 1090): if the gate failed with
				// Go compile errors naming undefined symbols / unknown struct fields, the
				// worker guessed an internal API — resolve the real signatures via `go doc`
				// and inject them, so the next revise SEES the API instead of guessing again.
				// A second arm grounds WRONG import paths (a repo-relative import instead of
				// the module path) via `go list`, the other common floor-model build error
				// (bug corpos-grounding-does-not-handle-import-path-errors). A third arm
				// grounds the SELF-import cycle a floor worker creates when it over-applies
				// the import-path correction to an in-package test (bug corpos-grounding-
				// overcorrects-in-package-test-into-self-import-cycle) — pure guidance, no lookup.
				grounds := l.groundGateOutput(ctx, out) // grounding parses the RAW output
				shown := distillGateOutput(out)         // but the worker sees the failure-salient lines
				content := fmt.Sprintf("Automated verification failed (`%s` exited non-zero):\n%s\n\nFix the code so it passes, then stop.",
					strings.Join(l.verify.Command, " "), shown)
				if len(grounds) > 0 {
					content = fmt.Sprintf("Automated verification failed (`%s` exited non-zero):\n%s\n\n%s\n\nFix the code so it passes, then stop.",
						strings.Join(l.verify.Command, " "), shown, strings.Join(grounds, "\n\n"))
				}
				l.transcript = append(l.transcript, model.ChatMessage{
					Role:    model.RoleUser,
					Content: content,
				})
				// A new revise cycle: give the fix attempt a fresh tool-round
				// allowance so the runaway guard doesn't starve it (see cycleStart).
				// A verify cycle is also progress — the gate gave new feedback the
				// agent is acting on — so the no-progress breaker doesn't kill a
				// legitimate write→verify→revise loop.
				*cycleStart = round + 1
				*lastProgressRound = round
				return Result{}, actContinue
			}
			// Revise budget exhausted: the gate could not be satisfied within bounds.
			// The honest terminal verdict is unverified/escalate — NOT done — even
			// though resp.Text may narrate success (the worker's self-report cannot
			// override the gate, T7).
			perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
			escalate := escalateVerdict(fmt.Sprintf("gate %q still failing after %d revise cycle(s) — escalating, not reporting done",
				strings.Join(l.verify.Command, " "), l.verifyRounds))
			return Result{Text: resp.Text, Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction, VerifyFailed: true, Escalate: escalate}, actReturn
		}
		// Fake-green-stage audits (bug 1050): the gate PASSED, but a green that the
		// worker's own authored test could be certifying is not a clean done. The
		// registry runs every fake-green-stage guard over the dispatch record AFTER the
		// gate ran (so the worker cannot forge it — the workaudit footer idiom); the
		// FIRST refusal fails the green as an escalation, not a done.
		if verdict, refused := l.guards.assess(ctx, StageFakeGreen, guardIn); refused {
			perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
			return Result{Text: resp.Text, Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction, VerifyFailed: true, Escalate: escalateVerdict(verdict.Reason)}, actReturn
		}
	}
	perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
	return Result{Text: resp.Text, Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction}, actReturn
}

// turnSignals are the per-turn escalation signals the loop sources outside the
// tool-call tally: the round-budget-exhausted flag (retry_exhaustion), a
// hook-set explicit-handoff request, and a hook-set confidence score.
type turnSignals struct {
	roundsExhausted bool
	explicitHandoff bool
	confidence      *float64
	// aborted marks a turn that ended via a fatal mid-turn model fault (not a
	// clean answer). It is stamped into the turn row's signals_json so an aborted
	// turn is distinguishable from a completed or still-in-flight one in persisted
	// telemetry (suggestion 49).
	aborted bool
}

// harvest folds a fired hook context's non-tally escalation signals (an explicit-handoff
// request, a confidence score) into the turn's accumulator. Called for every hook context
// fired during the turn.
func (ts *turnSignals) harvest(c *hooks.Context) {
	if c.RequestEscalation {
		ts.explicitHandoff = true
	}
	if c.EscalationConfidence != nil {
		ts.confidence = c.EscalationConfidence
	}
}

// opportunisticGreen runs the verify gate on the CURRENT tree mid-flight (or at round-
// budget exhaustion) and, on a CLEAN green, finishes the turn as a success. "Clean" means
// the gate passes AND the fake-green-stage guards do not refuse it — the same anti-fake-
// green bar a done-claim green is held to (a green a worker's own authored test could be
// certifying is not a done). On anything less it returns done=false so the caller keeps
// working (mid-flight) or falls through to the honest round-budget error (exhaustion). The
// orchestrator independently re-certifies the returned success with its own gate +
// red-before-green replay, so this never bypasses verification — it only avoids discarding
// a fix the worker already landed. finalText is the model's last text (empty at exhaustion).
func (l *Loop) opportunisticGreen(ctx context.Context, dispatches []tool.Result, turn, startLen int, turnModel string, ts turnSignals, compaction *CompactionEvent, finalText string) (Result, bool) {
	// A full verify gate wins; otherwise fall back to the terminal green backstop (bug
	// 1148) so a spawn-only orchestrator whose worker landed a green fix reports success
	// at halt time instead of "no final answer". One of the two, or neither (no check).
	gate := l.verify
	if gate == nil {
		gate = l.terminalGreen
	}
	if gate == nil {
		return Result{}, false
	}
	if ok, _ := l.verifyScopedGate(ctx, gate, dispatches); !ok {
		return Result{}, false
	}
	if _, refused := l.guards.assess(ctx, StageFakeGreen, GuardInput{FinalText: finalText, Dispatches: dispatches}); refused {
		return Result{}, false // a suspect green is not a clean done — keep working
	}
	perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
	return Result{Text: finalText, Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction}, true
}

// groundGateOutput resolves structural grounding from a failed verify gate's output — the
// REAL signatures behind undefined-symbol / unknown-field errors (`go doc`), the correct
// module import path behind a repo-relative import (`go list`), and self-import-cycle
// guidance — so a revise SEES the API instead of guessing it again. Shared by the post-done
// revise loop (onDeclaredDone) and the mid-flight reconcile check (opportunisticReconcile)
// so both feed back identical grounded signal. Returns nil when nothing grounds.
func (l *Loop) groundGateOutput(ctx context.Context, out string) []string {
	resolve := l.goDocResolve
	if resolve == nil {
		resolve = realGoDoc
	}
	resolveImport := l.goListResolve
	if resolveImport == nil {
		resolveImport = realGoListImportPath
	}
	var grounds []string
	if g := groundFromGateOutput(ctx, out, l.verify.Dir, resolve); g != "" {
		grounds = append(grounds, g)
	}
	if g := groundImportPaths(ctx, out, l.verify.Dir, resolveImport); g != "" {
		grounds = append(grounds, g)
	}
	if g := groundImportCycles(out); g != "" {
		grounds = append(grounds, g)
	}
	return grounds
}

// opportunisticReconcile is the mid-flight throttled gate check (the same cadence as the
// stop-when-green opportunisticGreen) extended to feed a RED result BACK to the worker, so a
// test-authoring worker reconciles a guessed/output-dependent assertion against the REAL test
// output BEFORE claiming done (bug 1103). On a CLEAN green it finishes as success exactly like
// opportunisticGreen. On RED — previously discarded — it appends the failing gate output (plus
// any structural grounding) as a user turn and returns done=false so the loop continues with
// the truth in context. It is SAFE under risk-gate=enforce: the LOOP runs the fixed gate, never
// the worker via sys.exec. No extra build/test cost is incurred — the gate run is the one the
// cadence already schedules; only the RED output that was being thrown away is now surfaced.
// Identical consecutive output is deduped (lastReconcileOutput) so an untouched failure isn't
// re-nudged. A RED feedback is progress (the worker has new signal to act on), so it advances
// *lastProgressRound to keep the no-progress breaker off a legitimate reconcile.
func (l *Loop) opportunisticReconcile(ctx context.Context, dispatches []tool.Result, turn, startLen, round int, turnModel string, ts turnSignals, compaction *CompactionEvent, finalText string, lastProgressRound *int) (Result, bool) {
	if l.verify == nil {
		return Result{}, false
	}
	ok, out := l.verifyScoped(ctx, dispatches)
	if ok {
		if _, refused := l.guards.assess(ctx, StageFakeGreen, GuardInput{FinalText: finalText, Dispatches: dispatches}); refused {
			return Result{}, false // a suspect green is not a clean done — keep working
		}
		perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
		return Result{Text: finalText, Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction}, true
	}
	// RED mid-flight: surface the real gate output so the worker reconciles its assertions
	// now, instead of editing blind until a post-done revise it may never reach. Dedupe an
	// unchanged failure so the worker isn't handed byte-identical text every cadence tick.
	if out == "" || out == l.lastReconcileOutput {
		return Result{}, false
	}
	l.lastReconcileOutput = out
	grounds := l.groundGateOutput(ctx, out) // grounding parses the RAW output
	shown := distillGateOutput(out)         // but the worker sees the failure-salient lines
	content := fmt.Sprintf("Checkpoint — the verification gate currently FAILS (`%s` exited non-zero):\n%s\n\nReconcile your changes against this REAL output now — in particular, correct any test assertion whose expected value differs from what the gate shows — then continue.",
		strings.Join(l.verify.Command, " "), shown)
	if len(grounds) > 0 {
		content = fmt.Sprintf("Checkpoint — the verification gate currently FAILS (`%s` exited non-zero):\n%s\n\n%s\n\nReconcile your changes against this REAL output now — in particular, correct any test assertion whose expected value differs from what the gate shows — then continue.",
			strings.Join(l.verify.Command, " "), shown, strings.Join(grounds, "\n\n"))
	}
	l.transcript = append(l.transcript, model.ChatMessage{Role: model.RoleUser, Content: content})
	*lastProgressRound = round
	return Result{}, false
}

// closeOnTerminalFault runs the orchestrator verify gate (when configured) on the
// CURRENT tree after a terminal model-call fault, so a fix that already landed (a
// strong-rung turn made it before a later round overflowed the floor) is reported as
// SUCCESS, and a real failure gets an honest, gate-evidenced verdict — never a bare
// error that hides a landed fix. Returns handled=false when no verify gate is
// configured, so the caller preserves the prior raw-error behavior. Fix for bug
// escalation-bound-exhaustion-falls-back-to-overflowing-floor-and-dies-after-fix-landed.
func (l *Loop) closeOnTerminalFault(ctx context.Context, dispatches []tool.Result, turn, startLen int, turnModel string, ts turnSignals, compaction *CompactionEvent, fault error) (Result, bool) {
	if l.verify == nil {
		return Result{}, false
	}
	perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
	ok, out := l.verifyScoped(ctx, dispatches)
	if ok {
		// Gate green on the current tree: the work landed despite the loop failing to
		// close cleanly. Report success — no Stopped/Escalate verdict.
		return Result{Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction}, true
	}
	stopped := fmt.Sprintf("terminal model fault (%s); the verify gate is still failing", model.ClassifyFault(fault))
	return Result{
		Dispatches: dispatches,
		CostUSD:    l.ledger.TotalUSD(),
		PersistErr: perr,
		Compaction: compaction,
		ModelFault: string(model.ClassifyFault(fault)),
		Stopped:    stopped,
		Escalate:   escalateVerdict(stopped + " — gate output: " + strings.TrimSpace(out)),
	}, true
}

// closeOnGracefulFault ends a turn whose per-turn deadline is spent (a recoverable
// model-call fault — typically a timeout — that can no longer make progress on this
// context) by EVIDENCING any landed work through the orchestrator verify gate, instead
// of returning a bare empty answer (bug 1102: an orchestrate run timed out before its
// -verify gate and exited 0 with an empty, unverified artifact; a principal trusting it
// would ship a RED tree). On a green gate the work landed despite the timeout → report
// SUCCESS (no verdict); on a red gate → an honest unverified/escalate verdict. The gate
// runs under a context derived with context.WithoutCancel because THIS ctx is spent —
// the gate's command would be cancelled before it could run otherwise; the gate applies
// its own internal timeout. Returns handled=false when no verify gate is configured, so
// the caller preserves the prior graceful end (ModelFault only, no verdict).
func (l *Loop) closeOnGracefulFault(ctx context.Context, dispatches []tool.Result, turn, startLen int, turnModel string, ts turnSignals, compaction *CompactionEvent, fault error) (Result, bool) {
	if l.verify == nil {
		return Result{}, false
	}
	perr := l.finishTurn(ctx, dispatches, turn, startLen, turnModel, ts)
	ok, out := l.verifyScoped(context.WithoutCancel(ctx), dispatches)
	if ok {
		// Gate green on the current tree: the fix landed before the turn deadline was
		// spent. Report success — no Stopped/Escalate verdict (matches the terminal-fault
		// green close), so a timed-out-but-green run is never reported as a failure.
		return Result{Dispatches: dispatches, CostUSD: l.ledger.TotalUSD(), PersistErr: perr, Compaction: compaction}, true
	}
	stopped := fmt.Sprintf("turn ended on a recovered model fault (%s); the verify gate is still failing", model.ClassifyFault(fault))
	return Result{
		Dispatches: dispatches,
		CostUSD:    l.ledger.TotalUSD(),
		PersistErr: perr,
		Compaction: compaction,
		ModelFault: string(model.ClassifyFault(fault)),
		Stopped:    stopped,
		Escalate:   escalateVerdict(stopped + " — gate output: " + strings.TrimSpace(out)),
	}, true
}

// finishTurn persists this turn's new messages (best-effort), assembles the
// turn's escalation signals, folds them through the router (which returns any
// tier-change edge), records the turn's telemetry — emitting an EscalationProposed
// event on an escalate edge — and fires the post_turn hook. The returned error is
// a non-fatal message-persistence failure surfaced on Result.PersistErr;
// telemetry writes are best-effort and never surface.
func (l *Loop) finishTurn(ctx context.Context, dispatches []tool.Result, turn, startLen int, turnModel string, ts turnSignals) error {
	perr := l.persistMessages(turn, startLen)
	tally := tool.Tally(dispatches)
	sig := router.Signals{
		ToolErrors:      tally.ToolErrors,
		ParseFailures:   tally.ParseFailures,
		ExplicitHandoff: boolToCount(ts.explicitHandoff),
		Confidence:      ts.confidence,
	}
	if ts.roundsExhausted {
		// The tool-call retry loop consumed its whole round budget without
		// converging — corpos' analog of retry_exhaustion.
		sig.RetriesUsed = l.maxRounds
		sig.Detail = "max tool rounds exhausted"
	}
	edge := l.router.Observe(sig)
	l.recordTurnEnd(ctx, turn, sig, turnModel, edge, ts.aborted)
	l.hooks.Fire(hooks.PostTurn, l.hookCtx(hooks.PostTurn, turn))
	return perr
}

// boolToCount maps a boolean signal onto the count the detector compares (1 = the
// signal is present, fired against an explicit_handoff threshold of normally 1).
func boolToCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// recordTurnStart opens the turn's telemetry row (best-effort).
func (l *Loop) recordTurnStart(turn int, model string) {
	if l.store != nil {
		_ = l.store.StartTurn(turn, model)
	}
}

// recordCost records one model call's token usage and priced cost (best-effort).
func (l *Loop) recordCost(turn int, model string, inputTokens, outputTokens int, usd float64) {
	if l.store != nil {
		_ = l.store.RecordCost(turn, model, inputTokens, outputTokens, usd)
	}
}

// recordToolCall records one dispatch as a telemetry row (best-effort). The
// dispatch's measured latency and error class come straight off the result.
func (l *Loop) recordToolCall(turn int, call tool.Call, res tool.Result, model string) {
	if l.store == nil {
		return
	}
	_ = l.store.RecordToolCall(turn, call.Surface, call.Action,
		encodeValue(call.Params), encodeValue(res.Value),
		res.OK, string(res.ErrorClass), int(res.LatencyMS), model, res.SpanID)
}

// recordTurnEnd stamps the turn's end + observed signals and records any router
// edge. On an escalate edge it emits an EscalationProposed event (best-effort,
// via the emitter) and stamps the returned event id onto the local escalation
// row; a de-escalate edge is local telemetry only (the toolkit's
// escalation_propose requires a trigger, which de-escalation carries none). All
// writes are best-effort — observability must not break a turn.
func (l *Loop) recordTurnEnd(ctx context.Context, turn int, sig router.Signals, turnModel string, edge router.Edge, aborted bool) {
	if l.store != nil {
		signals := turnSignalsJSON(sig, aborted)
		_ = l.store.EndTurn(turn, signals)
	}
	l.recordEscalationEdge(ctx, turn, edge)
}

// turnSignalsJSON renders the turn row's signals_json: the observed tool-error /
// parse-failure tallies, plus an `aborted: true` marker on a fatally-aborted turn
// so a started-but-not-completed turn is distinguishable in persisted telemetry
// from a clean turn (which omits the key) and an in-flight one (ended_at NULL).
func turnSignalsJSON(sig router.Signals, aborted bool) string {
	m := map[string]any{"tool_errors": sig.ToolErrors, "parse_failures": sig.ParseFailures}
	if aborted {
		m["aborted"] = true
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// recordEscalationEdge emits + records one router edge: on an escalate edge it
// emits an EscalationProposed event (best-effort, via the emitter) and stamps the
// returned event id onto the local escalation row; a de-escalate edge is local
// telemetry only (the toolkit's escalation_propose requires a trigger, which
// de-escalation carries none). A no-op for EdgeNone. Shared by the turn-boundary
// fold (recordTurnEnd) and the mid-turn fault escalation (escalateForFault), so
// both paths land identical telemetry. All writes are best-effort.
func (l *Loop) recordEscalationEdge(ctx context.Context, turn int, edge router.Edge) {
	if edge.Direction == router.EdgeNone {
		return
	}

	eventID := ""
	if edge.Direction == router.EdgeEscalate && l.emitter != nil {
		threshold := edge.FiredThreshold
		if id, err := l.emitter.Propose(ctx, escalation.Proposal{
			Trigger:        string(edge.Trigger),
			FromModel:      edge.FromModel,
			ToModel:        edge.ToModel,
			SessionID:      l.sessionID,
			TurnIndex:      turn,
			StateBefore:    edge.StateBefore,
			StateAfter:     edge.StateAfter,
			TriggerDetail:  edge.Detail,
			FiredThreshold: &threshold,
			ProjectID:      l.project,
			Reason:         fmt.Sprintf("%s fired: %s", edge.Trigger, edge.Detail),
		}); err == nil {
			eventID = id
		}
	}

	if l.store != nil {
		edgeStr := "escalate"
		trigger := string(edge.Trigger)
		if edge.Direction == router.EdgeDeescalate {
			edgeStr = "deescalate"
			trigger = edge.StateAfter // "de_escalated" — no firing trigger
		}
		_ = l.store.RecordEscalation(turn, edgeStr, trigger, edge.FromModel, edge.ToModel, eventID)
	}
}

// persistMessages durably appends this turn's conversation messages (the
// transcript tail from startLen) to the local session store. No-op when no
// store is wired. Returns the first write error, if any.
func (l *Loop) persistMessages(turn, startLen int) error {
	if l.store == nil {
		return nil
	}
	for _, m := range l.transcript[startLen:] {
		if _, err := l.store.AppendMessage(turn, m.Role, m.Content, ""); err != nil {
			return fmt.Errorf("persist session message: %w", err)
		}
	}
	return nil
}

// ResumeState projects stored messages into the replayable conversation thread
// for --resume and the turn index to continue from. Only user turns and
// assistant turns that carry text are replayed: RoleTool rounds and empty
// assistant turns are dropped, because the stored rows don't preserve
// tool_use/tool_result pairing and replaying a tool_result without its tool_use
// (or an empty assistant block) would 400 the Anthropic API. The system prompt
// is re-seeded fresh by the caller (WithSystemPrompt), so RoleSystem is dropped
// too. nextTurn is max(stored turn_index)+1, or 0 when there are no messages.
func ResumeState(msgs []session.Message) (history []model.ChatMessage, nextTurn int) {
	maxTurn := -1
	for _, m := range msgs {
		if m.TurnIndex > maxTurn {
			maxTurn = m.TurnIndex
		}
		switch m.Role {
		case model.RoleUser:
			history = append(history, model.ChatMessage{Role: model.RoleUser, Content: m.Content})
		case model.RoleAssistant:
			if m.Content != "" {
				history = append(history, model.ChatMessage{Role: model.RoleAssistant, Content: m.Content})
			}
		}
	}
	return history, maxTurn + 1
}

// Cost returns the session's running total USD and the per-model breakdown
// (priciest first) — the run-rate signal a cost-routed session accumulates
// (free local turns at $0, paid escalations priced). Surfaced so the caller can
// report what the swap validation period measures.
func (l *Loop) Cost() (float64, []cost.ModelTotal) {
	return l.ledger.TotalUSD(), l.ledger.Breakdown()
}

// Close fires the stop and session_end hooks (idempotent only-if-started).
func (l *Loop) Close() {
	if !l.started {
		return
	}
	l.hooks.Fire(hooks.Stop, l.hookCtx(hooks.Stop, l.turns))
	l.hooks.Fire(hooks.SessionEnd, l.hookCtx(hooks.SessionEnd, l.turns))
}

// encodeValue renders a tool result payload as the JSON string fed back to the
// model as a tool message.
func encodeValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// toolResultCap is the per-tool-result character budget: a single result is
// truncated to this so one oversized result can never blow the context window. It
// is ~1/4 of the compaction budget (so several results plus the transcript still
// fit) when a compactor is configured, an explicit override when set, else a safe
// default. Floored at minToolResultChars.
func (l *Loop) toolResultCap() int {
	if l.toolResultCharCap > 0 {
		return l.toolResultCharCap
	}
	cap := defaultToolResultChars
	if l.compactor != nil && l.compactor.budget > 0 {
		// budget is in tokens; a quarter of it, as chars (×4 chars/token, ÷4 share)
		// works out to ~budget chars ≈ budget/4 tokens per result.
		cap = (l.compactor.budget / 4) * approxCharsPerToken
	}
	if cap < minToolResultChars {
		cap = minToolResultChars
	}
	return cap
}

// capToolResult truncates a serialized tool result that exceeds the per-result
// cap, appending a marker that tells the agent the result was cut and how to get
// the part it needs (narrow the call) instead of letting an oversized result
// overflow the window. The cut is made on a UTF-8 boundary.
func (l *Loop) capToolResult(content string) string {
	cap := l.toolResultCap()
	if len(content) <= cap {
		return content
	}
	head := content[:cap]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1] // back off to a valid rune boundary (≤3 bytes)
	}
	return fmt.Sprintf("%s\n\n[tool result truncated: %d of %d chars shown. The full result is too large for the context window — narrow the call (fetch one record by id rather than listing all, add a limit/offset, read a line range, or grep a tighter pattern) to get the part you need.]",
		head, len(head), len(content))
}

// trackTruncatedView maintains the truncatedView taint set off each fs.read
// dispatch (rawResult is the un-capped serialized result, so its length decides
// truncation the same way capToolResult does). A read whose result exceeds the cap
// taints the path: the model is about to see only a prefix. A whole-file read that
// fits clears it: the model now has the complete file and a whole-file write is
// safe. A non-fs / non-read call, or a read with no resolvable path, is a no-op.
func (l *Loop) trackTruncatedView(c tool.Call, rawResult string) {
	if c.Surface != "fs" || c.Action != "read" {
		return
	}
	path := fsCallPath(c)
	if path == "" {
		return
	}
	if len(rawResult) > l.toolResultCap() {
		if l.truncatedView == nil {
			l.truncatedView = map[string]bool{}
		}
		l.truncatedView[path] = true
		return
	}
	if fsReadIsWholeFile(c) {
		delete(l.truncatedView, path) // a complete, untruncated view — write is safe again
	}
}

// truncatedWriteReason returns a non-empty refusal reason when c is a whole-file
// fs.write to a path whose most-recent read was truncated (the model never saw the
// full file, so the overwrite would drop the unseen remainder — run-9). It returns
// "" for any other call, including fs.edit/move/remove (surgical ops that don't
// depend on a complete view) and writes to paths read whole.
func (l *Loop) truncatedWriteReason(c tool.Call) string {
	if c.Surface != "fs" || c.Action != "write" {
		return ""
	}
	path := fsCallPath(c)
	if path == "" || !l.truncatedView[path] {
		return ""
	}
	return fmt.Sprintf("fs.write blocked: your most recent read of %q was truncated, so you have not seen the whole file — a whole-file write would overwrite the unseen part with whatever you reconstructed. Use fs.edit for a targeted change, or read the file whole (no offset/limit, small enough to not truncate) before writing it.", path)
}

// fsCallPath extracts the target path of an fs call: read/write/edit canonically
// use file_path; the directory/search actions use path. It returns "" when neither
// is a non-empty string (the alias resolution mirrors fs.ReadParams.UnmarshalJSON).
func fsCallPath(c tool.Call) string {
	if c.Params == nil {
		return ""
	}
	if p, ok := c.Params["file_path"].(string); ok && p != "" {
		return p
	}
	if p, ok := c.Params["path"].(string); ok && p != "" {
		return p
	}
	return ""
}

// fsReadIsWholeFile reports whether an fs.read requests the entire file — no line
// range (limit absent/≤0) starting at the top (offset absent/≤1). Only a whole-file
// read grants the complete view that clears a truncation taint; a ranged read is
// partial by construction and never clears it.
func fsReadIsWholeFile(c tool.Call) bool {
	return numParam(c, "limit") <= 0 && numParam(c, "offset") <= 1
}

// numParam reads a numeric call param as float64 (JSON numbers decode to float64;
// int/int64 are accepted for programmatically-built calls). Missing → 0.
func numParam(c tool.Call, key string) float64 {
	if c.Params == nil {
		return 0
	}
	switch v := c.Params[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}
