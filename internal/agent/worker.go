package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"corpos/internal/cost"
	"corpos/internal/escalation"
	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/router"
	"corpos/internal/session"
	"corpos/internal/tool"
)

// parentRunKey carries the spawning loop's session run id through the dispatch
// context so a spawned worker can link its own session to the parent (the
// sub-orchestration tree edge, T6).
type parentRunKey struct{}

// WithParentRunID stamps the parent session's run id onto ctx.
func WithParentRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, parentRunKey{}, runID)
}

// ParentRunID reads the parent session's run id from ctx (empty when unset).
func ParentRunID(ctx context.Context) string {
	id, _ := ctx.Value(parentRunKey{}).(string)
	return id
}

// ChildStore opens a fresh session store for a spawned worker, linked to the
// parent that spawned it (parentRunID) and stamped with the worker's profile and
// duty. It is injected by the composition root so package agent stays decoupled
// from session-store construction. It may return nil (the worker runs without a
// store — telemetry off for that worker).
type ChildStore func(parentRunID, profileName, duty string) *session.Store

// Project projects the full (unprojected) tool-spec set down to a job-profile's
// action-level scope, returning the worker's scoped spec subset. It is injected
// into the Spawner by the composition root so package agent stays decoupled from
// the mcp projection layer — and the projected tools ARE the worker's allow-list
// (the SCOPE axis; see package profile).
type Project func(p *profile.JobProfile) []tool.Spec

// BuildHooks builds a worker's per-profile hook surface — skill injection,
// parse_context pruning, the risk gate. Injected so agent stays decoupled from
// the skills/risk packages. It may return nil (the worker runs without hooks).
type BuildHooks func(p *profile.JobProfile) *hooks.Surface

// ScopeProvider wraps the shared tool provider so a worker can ONLY dispatch the
// surfaces/actions its profile grants — the dispatch-boundary half of capability
// scoping (the SPEC half is Project). Injected by the composition root so agent
// stays decoupled from the mcp scoping layer. When nil a worker dispatches
// through the unscoped shared provider (prior behavior). It must return non-nil.
type ScopeProvider func(p *profile.JobProfile) tool.Provider

// Spawner stamps a job-profile onto a child Loop — the worker-spawn primitive
// sub-orchestration consumes (design doc §4.2/§5#5). Given a profile it projects
// the profile's tool scope, routes the worker onto its tier's adapter (escalating
// to the strong tier on tool error when the profile's EscalateOn asks for it, via
// the existing two-call router), and wires the profile's hooks. The shared
// dependencies (provider, projection, hook-building, adapters) are injected once;
// each Spawn produces a fresh, scoped worker so a leaf duty gets one surface and a
// cheap model, not the full surface set and the frontier.
type Spawner struct {
	provider      tool.Provider
	scopeProvider ScopeProvider
	project       Project
	buildHooks    BuildHooks
	base          model.Adapter
	mid           model.Adapter
	hasMid        bool
	strong        model.Adapter
	hasStrong     bool
	coding        model.Adapter // intermediate authoring rung (DeepSeek-V3.2), inserted mid→strong for CodingRung profiles
	hasCoding     bool
	escalateAfter int
	strongBound   int    // max turns a worker may serve on the strong rung (0 = unbounded)
	verifyDir     string // working dir the auto-verify gate runs a coding worker's build/test in ("" = process CWD)
	// verifyRun overrides how a spawned worker's auto-verify gate executes its command;
	// nil → the gate's own execVerify (pure-Go os/exec). Injected by tests to drive the
	// gate deterministically (the same seam VerifyGate.run exposes).
	verifyRun  func(ctx context.Context, command []string, dir string, timeout time.Duration) (int, string)
	childStore ChildStore

	// detachDeadline, when set, gives each spawned worker its OWN turn budget instead
	// of inheriting the orchestrator's per-turn deadline residue (bug 1123). A spawned
	// worker is a full multi-round sub-loop; left sharing the parent turn's deadline, a
	// coding worker spawned late in the orchestrator's turn inherited a near-spent
	// budget, so its first slow local-tier model call on a large context timed out with
	// ZERO tool calls (recoverGraceful before any escalation could fire). workerTimeout
	// is that fresh budget; <=0 means no outer deadline (bounded by the per-call HTTP
	// timeout + the round cap). The parent's real CANCELLATION still propagates; only
	// the parent's DEADLINE is dropped. Off by default (WithWorkerTimeout opts in), so
	// a spawner built without it keeps the prior inherit-the-parent-ctx behavior.
	detachDeadline bool
	workerTimeout  time.Duration

	emitter      escalation.Emitter // emits worker escalate edges (nil = local only)
	escConfig    router.Config      // full per-trigger escalation config (toolkit)
	hasEscConfig bool

	// meter, when set, is the SHARED tree-cost meter (cost.Meter) wired into every
	// worker this spawner builds, so a worker's own model calls accrue to the shared
	// tree total and the worker stops before its next call once the run-level ceiling is
	// hit — bounding even a single in-flight worker's contribution, not just the
	// orchestrator's spawn decision (bug 1124). Nil = workers run unmetered (the prior
	// behavior; the run is then bounded only by per-worker timeout + strong-bound).
	meter *cost.Meter
	// budget caps the COUNT of workers spawned across the whole tree (the count analogue of
	// meter's cost ceiling). EVERY spawn — the orchestrator's direct agent.spawn AND the coding
	// organ's internal worker/operator-intervention spawns — flows through Spawner.Run, so one
	// budget here is the single chokepoint that bounds tree-wide fan-out (Run-42: 34 workers for
	// one test, only 5 of them direct orchestrator spawns — the rest operator-seat interventions).
	// Nil = unbounded (the prior behavior; bounded only by depth + the cost ceiling).
	budget *cost.SpawnBudget
	// strongBudget, when set, is the SHARED tree-wide strong-turn (Opus) budget wired into every
	// worker's router, so all workers a run spawns — the first coding worker AND every respawn — draw
	// Opus turns from ONE pool. A per-worker WithStrongBound resets for each fresh worker, so a stuck
	// atom re-climbs Opus once per respawn; this pool bounds the tree's TOTAL strong turns (bug 1165(b):
	// 5 workers each climbed to Opus). Nil = unbounded (the prior per-worker-only behavior).
	strongBudget *cost.StrongTurnBudget
}

// SpawnerOption configures a Spawner.
type SpawnerOption func(*Spawner)

// WithMidTier gives the spawner the default mid (Gemini-Flash-Lite) rung — the
// middle of the local→mid→strong ladder (§4.6). A tier=mid worker rests on it; a
// tier=local worker escalating on tool error climbs through it before strong.
// Without it the ladder collapses to local↔strong (the prior two-tier shape). A
// nil adapter is ignored.
func WithMidTier(mid model.Adapter) SpawnerOption {
	return func(s *Spawner) {
		if mid == nil {
			return
		}
		s.mid = mid
		s.hasMid = true
	}
}

// WithStrongTier gives the spawner a distinct escalation adapter and the
// tool-error threshold at which a worker escalates to it. Without it every worker
// runs single-tier on the base adapter (no escalation). A nil strong adapter is
// ignored; a threshold below 1 is clamped to 1.
func WithStrongTier(strong model.Adapter, escalateAfter int) SpawnerOption {
	return func(s *Spawner) {
		if strong == nil {
			return
		}
		if escalateAfter < 1 {
			escalateAfter = 1
		}
		s.strong = strong
		s.hasStrong = true
		s.escalateAfter = escalateAfter
	}
}

// WithStrongBound caps how many turns a worker may serve on the strong (Opus)
// rung, so escalation can't make the frontier reachable per-turn. maxTurns <= 0
// leaves the strong rung unbounded.
func WithStrongBound(maxTurns int) SpawnerOption {
	return func(s *Spawner) {
		if maxTurns > 0 {
			s.strongBound = maxTurns
		}
	}
}

// WithCodingRung gives the spawner the intermediate authoring rung (DeepSeek-V3.2)
// inserted between the mid and strong rungs, used only by profiles that opt in via
// CodingRung (atomic-coding-chain; ATOMIC_CODING_CHAIN.md §5.8). A coding worker's
// tool-error escalation then climbs base→mid→coding→strong, carrying the
// authoring-escalation class on the cheaper coder rung before the bounded Opus
// rung. A nil adapter is ignored.
func WithCodingRung(coding model.Adapter) SpawnerOption {
	return func(s *Spawner) {
		if coding == nil {
			return
		}
		s.coding = coding
		s.hasCoding = true
	}
}

// WithSpawnVerifyDir sets the working directory a spawned worker's auto-verify gate
// (profile.VerifyCommand) runs its build/test in. A worker writes into the corpos
// process tree, so the default (empty → the process CWD via execVerify) is correct
// for a single-repo target; an operator driving a worktree/checkout out of the
// process CWD points the gate at it here. An empty dir is a no-op (keeps the default).
func WithSpawnVerifyDir(dir string) SpawnerOption {
	return func(s *Spawner) {
		if dir != "" {
			s.verifyDir = dir
		}
	}
}

// WithEscalationContract gives spawned workers the full escalation contract: the
// per-trigger threshold config (the toolkit's effective escalation_thresholds, so
// worker routers run the same 5-trigger policy as the orchestrator) and an emitter
// that lands an EscalationProposed event on each worker escalate edge. A nil
// emitter leaves worker escalations as local telemetry only. Without this option
// workers keep the single repeated_tool_error trigger and emit no events.
func WithEscalationContract(emitter escalation.Emitter, cfg router.Config) SpawnerOption {
	return func(s *Spawner) {
		s.emitter = emitter
		if cfg.Triggers != nil {
			s.escConfig = cfg
			s.hasEscConfig = true
		}
	}
}

// WithScopedProvider wires a per-profile provider wrapper so each spawned worker
// dispatches through a provider that ENFORCES its profile's scope (denies an
// un-granted surface/action with a tool_error) — not just one that hides the
// un-granted specs. Without it workers dispatch through the unscoped shared
// provider (a model that emits an out-of-scope call would have it dispatched). A
// nil factory is ignored.
func WithScopedProvider(fn ScopeProvider) SpawnerOption {
	return func(s *Spawner) {
		if fn != nil {
			s.scopeProvider = fn
		}
	}
}

// WithChildStore wires a per-worker session-store factory so each spawned worker
// gets its own telemetry DB linked to the parent (T6). Without it workers run
// without a store (no per-worker telemetry). A nil factory is ignored.
func WithChildStore(fn ChildStore) SpawnerOption {
	return func(s *Spawner) {
		if fn != nil {
			s.childStore = fn
		}
	}
}

// WithWorkerTimeout gives each spawned worker its OWN turn budget, detaching it from
// the orchestrator's per-turn deadline (bug 1123). A spawned worker is a full
// multi-round sub-loop, and the orchestrator BLOCKS on the worker dispatch while its
// own per-turn deadline keeps ticking — so a worker spawned mid-turn inherited only
// the residue, and its first slow local-tier call on a large context timed out with
// zero tool calls before any escalation could lift it off the floor. With this option
// the worker runs under a fresh budget instead, so a slow first call hits the per-call
// HTTP timeout (which the loop's timeout recovery escalates off) rather than a spent
// turn deadline (which ends the turn gracefully with no progress). budget<=0 detaches
// the parent deadline but imposes no outer worker deadline (bounded by the per-call
// HTTP timeout and the round cap). Without this option a worker inherits the parent
// ctx unchanged (the prior behavior).
func WithWorkerTimeout(budget time.Duration) SpawnerOption {
	return func(s *Spawner) {
		s.detachDeadline = true
		s.workerTimeout = budget
	}
}

// WithSpawnerCostMeter threads the SHARED tree-cost meter into every worker the spawner
// builds (bug 1124). Each worker's priced model calls then accrue to the shared total,
// and the worker's breaker stops it before its next call once the run-level ceiling is
// reached — so a single non-converging worker thrashing to the frontier can't itself
// overshoot the tree ceiling between the orchestrator's spawn-decision checks. A nil
// meter is ignored, leaving workers unmetered (the prior behavior).
func WithSpawnerCostMeter(m *cost.Meter) SpawnerOption {
	return func(s *Spawner) {
		if m != nil {
			s.meter = m
		}
	}
}

// WithSpawnerSpawnBudget threads a SHARED spawn-COUNT budget into the spawner, so every worker
// it spawns — orchestrator-direct AND coding-organ-internal (operator-seat interventions) —
// draws from one pool. Once N workers have been spawned across the tree, a further Spawn.Run is
// refused with ErrSpawnBudgetExhausted, bounding the same-sub-goal respawn thrash the cost
// ceiling only catches late (Run-42). A nil/zero-cap budget is ignored (unbounded).
func WithSpawnerSpawnBudget(b *cost.SpawnBudget) SpawnerOption {
	return func(s *Spawner) {
		if b != nil {
			s.budget = b
		}
	}
}

// WithSpawnerStrongTurnBudget threads a SHARED tree-wide strong-turn (Opus) budget into the spawner,
// so every worker's router draws Opus turns from one pool (bug 1165(b)). Once the run has served the
// budget's cap of strong turns, a further climb onto the frontier is refused tree-wide — a re-spawned
// coding worker can no longer re-climb Opus on a stuck atom, which the per-worker WithStrongBound
// (fresh for each worker) could not bound. A nil/zero-cap budget is ignored (unbounded, tracking-only).
func WithSpawnerStrongTurnBudget(b *cost.StrongTurnBudget) SpawnerOption {
	return func(s *Spawner) {
		if b != nil {
			s.strongBudget = b
		}
	}
}

// ErrSpawnBudgetExhausted marks a refused spawn: the run's tree-wide spawn-count budget is spent,
// so no further worker may be spawned. It is a structural bound no stronger MODEL can lift, so the
// spawn boundary classifies it ClassUsage (non-escalatable, bug 1095) — the orchestrator receives
// the "stop decomposing, synthesize" directive and adapts rather than climbing the ladder. Callers
// test it with errors.Is.
var ErrSpawnBudgetExhausted = errors.New("spawn budget exhausted")

// ErrWorkerToolsUnrunnable marks a worker MISCONFIGURATION: a file-mutating (coding) profile
// projected to a spec set that DROPPED its fs surface (bug 1080). The raw toolkit fs spec is
// enum-less, so mcp.Project fails closed under fs[write,edit,…] action-scoping and drops the
// whole surface while sys (which carries an enum) survives — so the projected count is > 0 and
// the bug-1030 ToollessAbort never fires, yet the worker has ZERO file tools and can only
// fabricate a zero-fs-dispatch 'done'. Like ErrVerifyGateUnrunnable it is a config error no
// stronger MODEL can fix, so the spawn boundary classifies it ClassUsage (non-escalatable):
// the orchestrator gets an actionable directive instead of climbing the ladder. Test with
// errors.Is.
var ErrWorkerToolsUnrunnable = errors.New("worker tools not runnable")

// specSurfaceNames returns the surface names of a projected spec set (tool.Spec.Name IS the
// surface). It is the input to profile.MutatorSurfaceDropped's bug-1080 fs-surface check.
func specSurfaceNames(specs []tool.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// workerContext gives a spawned worker its own turn budget instead of inheriting the
// parent's per-turn deadline residue (bug 1123). It carries the parent's VALUES
// (parent-run-id, etc.) and re-attaches the parent's real CANCELLATION — an operator
// Ctrl-C or overall-run cancel still stops the worker — but deliberately ignores a
// parent DEADLINE expiry, which is exactly the residue we are detaching from. When
// budget>0 the worker gets that fresh deadline; otherwise it runs with no outer
// deadline (still bounded by the per-call HTTP timeout and the loop's round cap).
func workerContext(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(parent) // values only — no parent deadline, no parent cancel
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if budget > 0 {
		ctx, cancel = context.WithTimeout(base, budget)
	} else {
		ctx, cancel = context.WithCancel(base)
	}
	// Re-attach the parent's real cancellation (NOT its deadline): cancel the worker
	// only when the parent was explicitly canceled; a parent deadline expiry is the
	// per-turn residue we are intentionally detaching from, so it must not kill the
	// worker's fresh budget.
	stop := context.AfterFunc(parent, func() {
		if errors.Is(parent.Err(), context.Canceled) {
			cancel()
		}
	})
	return ctx, func() {
		stop()
		cancel()
	}
}

// NewSpawner builds a Spawner over the shared tool provider, the projection
// function, the per-profile hook builder, and the base (cheap/local) worker
// adapter. Strong-tier escalation is opt-in via WithStrongTier. project and
// buildHooks may be nil (no projection / no hooks).
func NewSpawner(provider tool.Provider, project Project, buildHooks BuildHooks, base model.Adapter, opts ...SpawnerOption) *Spawner {
	s := &Spawner{provider: provider, project: project, buildHooks: buildHooks, base: base, escalateAfter: 1}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Spawn builds a worker Loop stamped with p: specs projected to p's scope, routed
// onto p's tier, with p's hooks wired and the profile threaded onto the loop
// (WithProfile) so profile-aware hooks read the worker's envelope. The worker's
// transcript is fresh; the caller supplies the duty via Run and any
// WithSystemPrompt/WithSession through opts.
func (s *Spawner) Spawn(p *profile.JobProfile, opts ...Option) *Loop {
	var specs []tool.Spec
	if s.project != nil {
		specs = s.project(p)
	}
	base := []Option{WithProfile(p)}
	// Thread the shared tree-cost meter so this worker's spend accrues to the run-level
	// total and its breaker stops it once the tree ceiling is hit (bug 1124). Inert when
	// no meter is wired. Placed before caller opts so it is always present on the worker.
	if s.meter != nil {
		base = append(base, WithCostMeter(s.meter))
	}
	// Seed the worker with the profile's system-prompt posture (e.g. the atomic-coding
	// faithful-reporting clauses, T6) before caller opts so it is always present.
	if p != nil && p.SystemPrompt != "" {
		base = append(base, WithSystemPrompt(p.SystemPrompt))
	}
	// A profile may raise the per-cycle tool-round budget above the loop default: a
	// coding worker needs room to read → locate → edit → verify in one conversation,
	// where a generic worker does not. WithMaxRounds no-ops on <=0, so this stays inert
	// for profiles that don't set it.
	if p != nil && p.MaxToolRounds > 0 {
		base = append(base, WithMaxRounds(p.MaxToolRounds))
	}
	if s.buildHooks != nil {
		if h := s.buildHooks(p); h != nil {
			base = append(base, WithHooks(h))
		}
	}
	if s.emitter != nil {
		base = append(base, WithEscalationEmitter(s.emitter))
	}
	base = append(base, opts...)
	// Auto-verify (bug 1073): when the profile declares a build/test gate, hold the
	// spawned worker to it — after it claims done the LOOP runs the fixed command,
	// feeds a non-zero exit back, and lets the worker revise (bounded by the profile's
	// VerifyMaxRounds), so a non-compiling edit is detected and self-repaired rather
	// than landed broken. Appended AFTER caller opts so the profile's gate is the
	// authority. The protect-paths hook keeps the repair honest: the worker must fix
	// production code, never edit a *_test.go / acceptance path to force green. Both
	// are orchestrator-owned (the loop runs the fixed command; the model cannot skip
	// it), so this is safe under risk-gate=enforce.
	base = append(base, s.autoVerifyOpts(p)...)
	// Dispatch the worker through a provider that ENFORCES its scope (a denied
	// surface/action comes back as a tool_error), so the projected specs are a hard
	// boundary, not just what the model is shown. Falls back to the shared provider
	// when no scope wrapper is wired.
	prov := s.provider
	if s.scopeProvider != nil {
		prov = s.scopeProvider(p)
	}
	return New(s.routerFor(p), prov, specs, base...)
}

// codingOpportunisticEvery is the mid-flight stop-when-green cadence for a spawned
// coding worker: run the verify gate at most once per this many rounds that landed a
// mutation. It bounds the extra build/test runs (≈ maxRounds/this worst case) while
// catching a green tree well before the round budget is spent; the exhaustion-time check
// is the always-on backstop that guarantees a landed fix is never discarded.
const codingOpportunisticEvery = 4

// autoVerifyOpts builds the loop options that hold a spawned coding worker to its
// profile's build/test gate (bug 1073). When the profile declares a VerifyCommand it
// returns a WithVerify gate (loop-run, bounded by VerifyMaxRounds) plus a protect-paths
// pre_tool_use guard for the profile's ProtectPaths (so the repair loop fixes production
// code, never a *_test.go to fake green). The loop's default stuck-floor escalation
// (set in New) lifts a persistently-RED worker toward the strong rung within the cap.
// A profile with no VerifyCommand yields no options — non-coding/read-only workers are
// unaffected.
func (s *Spawner) autoVerifyOpts(p *profile.JobProfile) []Option {
	if p == nil || len(p.VerifyCommand) == 0 {
		return nil
	}
	// Run the gate in the resolved Go module root, so a subdir-module repo (module in
	// go/) is gated where its go.mod lives instead of dead-ending (bug 1094). Falls back
	// to the configured dir when no module resolves (Run's fail-fast already rejected that).
	gateDir := s.verifyDir
	if md, ok := ResolveGoModuleDir(s.verifyDir); ok {
		gateDir = md
	}
	opts := []Option{WithVerify(&VerifyGate{
		Command:   p.VerifyCommand,
		Dir:       gateDir,
		MaxRounds: p.VerifyMaxRounds,
		run:       s.verifyRun,
	})}
	// Stop-when-green mid-flight: a coding worker that lands a passing fix but keeps
	// editing should be captured the moment the tree is green, not run to the round
	// budget and discarded. Throttled so the gate (a real build/test) runs at most once
	// per few mutating rounds; the always-on exhaustion check is the backstop.
	opts = append(opts, WithOpportunisticVerify(codingOpportunisticEvery))
	if len(p.ProtectPaths) > 0 {
		opts = append(opts, WithExtraHook(hooks.PreToolUse, "protect-paths", protectPathGuard(p.ProtectPaths)))
	}
	// Scaffold-fabrication guard (bug 1075): a fake-green-stage audit that refuses a green the
	// worker manufactured by writing a build-scaffold (go.mod) into the verify-dir. It needs the
	// gate's working dir as config (the boundary a fabricated manifest is measured against), so it
	// is registered HERE — where s.verifyDir is known — rather than unconditionally in New. It arms
	// whenever a gate runs (every coding worker), the same activation as the fake-green audit.
	opts = append(opts, WithScaffoldGuard(ScaffoldFabricationGuard{VerifyDir: s.verifyDir}))
	return opts
}

// Run spawns a worker for p, drives the duty to a final answer, closes the worker,
// and returns its Result (the worker's accrued cost is on the Result). It is the
// orchestrator's one-call spawn-and-run entry for a single duty. When a child-store
// factory is wired the worker gets its own telemetry DB linked to the parent loop
// (read off the context, T6); the store is closed when the worker finishes.
func (s *Spawner) Run(ctx context.Context, p *profile.JobProfile, duty string, opts ...Option) (Result, error) {
	// Spawn-count budget: refuse a spawn once the tree-wide worker budget is spent, BEFORE any
	// setup so a refused spawn pays nothing (no worker runs, ≈0 cost). This is the ONE chokepoint
	// every spawn flows through — the orchestrator's direct agent.spawn and the coding organ's
	// operator-seat interventions alike — so it bounds the whole tree's fan-out, not just the
	// orchestrator's decision (Run-42: 34 workers, 29 of them operator-seat interventions). The
	// caller (SpawnProvider / coding organ) classifies ErrSpawnBudgetExhausted as non-escalatable
	// and surfaces the "stop decomposing, synthesize" directive.
	if s.budget != nil && !s.budget.Reserve() {
		return Result{}, fmt.Errorf("%w: %d workers already spawned across this run — STOP decomposing: do NOT spawn another worker. Synthesize a final answer from the workers' results so far, or report plainly what is blocking completion", ErrSpawnBudgetExhausted, s.budget.Count())
	}
	// Fail fast on a non-runnable verify gate (bug 1075): when the profile declares a go
	// build/test gate, validate AT SPAWN that its working dir has a reachable go.mod. A gate
	// that cannot run (no module) can only error — distinct from a RED test result — and the
	// 2026-06-11 dogfood showed a worker handed such a gate fabricates the module to pass it.
	// Surfacing the misconfig here, before the worker runs, is the actionable fix.
	if p != nil {
		if err := VerifyGateRunnable(p.VerifyCommand, s.verifyDir); err != nil {
			return Result{}, err
		}
		// Fail LOUD when a file-mutating (coding) profile projects to a spec set that DROPPED
		// its fs surface (bug 1080): the enum-less raw fs spec fails closed under fs[write,edit]
		// action-scoping and drops the whole surface, so the worker is handed ZERO file tools
		// while sys survives — projected count > 0, so ToollessAbort (projected == 0) misses it.
		// Such a worker can only fabricate a zero-fs-dispatch 'done', indistinguishable from a
		// real fabrication. Refuse at spawn with an actionable message instead (the fix is to
		// mount the native fs organ or advertise the fs action enum).
		if s.project != nil {
			if surfaces := specSurfaceNames(s.project(p)); profile.MutatorSurfaceDropped(p, surfaces) {
				return Result{}, fmt.Errorf("worker profile %q mutates files but projected no fs surface (projected: %v) — the action-scoped fs spec was dropped because the raw toolkit fs spec is enum-less and fails closed under projection; mount the native fs organ or advertise the fs action enum, else the worker has no file tools and can only fabricate a done: %w", p.Name, surfaces, ErrWorkerToolsUnrunnable)
			}
		}
	}
	// Give the worker its OWN turn budget rather than the orchestrator's per-turn
	// deadline residue (bug 1123). Done BEFORE reading ParentRunID so the detached ctx
	// still carries the parent's values (WithoutCancel preserves them).
	if s.detachDeadline {
		wctx, cancel := workerContext(ctx, s.workerTimeout)
		defer cancel()
		ctx = wctx
	}
	if s.childStore != nil {
		if st := s.childStore(ParentRunID(ctx), p.Name, duty); st != nil {
			defer func() { _ = st.Close() }()
			opts = append(opts, WithStore(st), WithSession(st.RunID(), ""))
		}
	}
	w := s.Spawn(p, opts...)
	defer w.Close()
	return w.Run(ctx, duty)
}

// routerFor builds the worker's per-turn router from its profile over the
// local→mid→strong ladder (§4.6). The profile's Tier picks the worker's FLOOR
// rung (where it rests); on tool error — when its EscalateOn asks for it — it
// climbs one rung at a time toward strong, with the strong (Opus) rung usage-
// bounded by WithStrongBound. A worker already resting on the top rung (a tier=
// strong profile, or a tier with no higher rung configured) runs single-tier
// (nowhere to escalate). With no escalation signal it also stays single-tier.
func (s *Spawner) routerFor(p *profile.JobProfile) *router.Router {
	tiers, floor := s.ladderFor(p)
	if floor >= len(tiers)-1 {
		top := tiers[floor]
		return router.New(top, top)
	}
	opts := []router.Option{}
	if escalatesOnToolError(p) {
		if s.hasEscConfig {
			// Full toolkit contract, with repeated_tool_error pinned to the worker's
			// configured tool-error threshold (the rest of the triggers + K from config).
			opts = append(opts, router.WithConfig(s.escConfig.WithRepeatedToolError(s.escalateAfter)))
		} else {
			opts = append(opts, router.WithEscalation(s.escalateAfter, 2))
		}
	}
	if s.hasStrong && s.strongBound > 0 {
		opts = append(opts, router.WithBoundedTop(s.strongBound))
	}
	// Shared tree-wide strong-turn pool: survives respawns, so a stuck atom can't re-climb Opus
	// once the run's strong turns are spent (bug 1165(b)). Nil-guarded inside the option.
	if s.strongBudget != nil {
		opts = append(opts, router.WithSharedStrongBudget(s.strongBudget))
	}
	return router.NewLadder(tiers, floor, opts...)
}

// ladderFor assembles the worker's tier ladder (cheapest→strongest: base, then
// mid and strong when configured) and the floor rung its profile Tier selects. A
// tier whose rung isn't configured clamps down to the nearest lower rung present.
func (s *Spawner) ladderFor(p *profile.JobProfile) ([]model.Adapter, int) {
	tiers := []model.Adapter{s.base}
	midIdx, strongIdx := -1, -1
	if s.hasMid {
		tiers = append(tiers, s.mid)
		midIdx = len(tiers) - 1
	}
	if s.hasCoding && p.CodingRung {
		// Intermediate authoring rung (DeepSeek-V3.2) between mid and strong: a
		// coding worker escalates base→mid→coding→strong, carrying the authoring-
		// escalation class on the cheaper coder rung before the bounded Opus rung.
		// It is an escalation step only — no profile Tier rests on it (no floor
		// maps here), so midIdx/strongIdx (the floor anchors) are unaffected.
		tiers = append(tiers, s.coding)
	}
	if s.hasStrong {
		tiers = append(tiers, s.strong)
		strongIdx = len(tiers) - 1
	}
	floor := 0
	switch p.Tier {
	case profile.TierMid:
		if midIdx >= 0 {
			floor = midIdx
		}
	case profile.TierStrong:
		switch {
		case strongIdx >= 0:
			floor = strongIdx
		case midIdx >= 0:
			floor = midIdx
		}
	}
	return tiers, floor
}

// escalatesOnToolError reports whether the profile asks to escalate on tool error.
func escalatesOnToolError(p *profile.JobProfile) bool {
	for _, sig := range p.EscalateOn {
		if sig == "tool_error" {
			return true
		}
	}
	return false
}
