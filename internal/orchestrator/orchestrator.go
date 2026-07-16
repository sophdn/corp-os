// Package orchestrator gives corpos its sub-agent primitive: the agent.spawn tool.
//
// The orchestrate-profile agent decomposes a goal into duties and DELEGATES each by
// calling agent.spawn(profile, duty). The SpawnProvider runs a scoped worker — a
// child agent.Loop under the named job-profile, via agent.Spawner — and returns its
// answer. So decomposition is emergent (the orchestrator spawns the duties it
// decides on) and reconciliation is the orchestrator's own synthesis turn over the
// workers' answers. This is the cost-via-decomposition payoff (design doc §4.1/§5#5):
// a leaf duty gets one surface and a cheap model, not the full surface set and the
// frontier. The spawn tool is a tool.Provider mounted into the orchestrator's
// aggregator like any other surface; only profiles whose scope includes the agent
// surface (the orchestrate profile) can call it.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"corpos/internal/agent"
	"corpos/internal/cost"
	"corpos/internal/mcp"
	"corpos/internal/pathglob"
	"corpos/internal/profile"
	"corpos/internal/routing"
	"corpos/internal/tool"
)

// Surface is the tool name the orchestrator calls to delegate a duty.
const Surface = "agent"

// spawnAction delegates one duty to a scoped worker.
const spawnAction = "spawn"

// defaultMaxDepth bounds nested spawning (an orchestrate worker spawning more
// orchestrators). Leaf profiles cannot spawn at all — their projected scope omits
// the agent surface — so this only guards recursive orchestration.
const defaultMaxDepth = 3

// depthKey carries the current spawn depth through the context so nested spawns
// can be bounded without per-call state.
type depthKey struct{}

// codingProfileName is the resolved profile a coding duty routes to (the duty
// router short-circuits coding work onto this single name). When a CodingPath is
// wired, a duty resolving to it runs through the operator-seat organ (the live
// coding path) instead of a bare worker.
const codingProfileName = "atomic-coding-chain"

// CodingPath routes a coding duty into the operator-seat organ (internal/coding):
// it wraps the duty as a single-AT coding chain, drives the orchestrator's
// worker→gate→revise loop, and on failure hands the run to the OperatorSeat
// (branch_fix carryover + K→strong escalation). It returns the synthesized answer
// and the operator-decision cost. Like a bare worker reporting honest failure, it
// returns a Go error ONLY for an infra fault (a seed/validation error) — an organ
// run that ends red is a normal answer, not an error. nil (the default) leaves
// Dispatch on the bare worker, so existing behavior is unchanged.
type CodingPath func(ctx context.Context, duty string, p *profile.JobProfile) (answer string, costUSD float64, err error)

// SpawnProvider is the agent.spawn tool: a tool.Provider that delegates a duty to a
// scoped worker under a named job-profile (resolved from the registry) via
// agent.Spawner, returning the worker's answer and accrued cost.
type SpawnProvider struct {
	spawner    *agent.Spawner
	registry   *profile.Registry
	maxDepth   int
	router     *routing.Router
	codingPath CodingPath
	meter      *cost.Meter
}

// Option configures a SpawnProvider.
type Option func(*SpawnProvider)

// WithMaxDepth overrides the nested-spawn depth bound (values < 1 are ignored).
func WithMaxDepth(n int) Option {
	return func(sp *SpawnProvider) {
		if n > 0 {
			sp.maxDepth = n
		}
	}
}

// WithRouter wires the duty→profile router (the classifier-driven routing input,
// task decomposition-and-profile-routing-design). With it, a spawn call may omit
// the profile (or pass "auto") and the router picks the cheapest-capable profile
// for the duty. Without it, the profile is required.
func WithRouter(r *routing.Router) Option {
	return func(sp *SpawnProvider) {
		if r != nil {
			sp.router = r
		}
	}
}

// WithCodingPath wires the operator-seat organ as the live coding path: a duty
// that resolves to the atomic-coding-chain profile is run through the organ (the
// single-AT chain + branch_fix carryover + K→strong escalation) instead of a bare
// worker. A nil fn (the default) is ignored, so without it Dispatch behaves exactly
// as before for every profile.
func WithCodingPath(fn CodingPath) Option {
	return func(sp *SpawnProvider) {
		if fn != nil {
			sp.codingPath = fn
		}
	}
}

// WithCostMeter wires the SHARED tree-cost meter so the spawn tool refuses to dispatch
// a new worker once the cumulative tree cost has reached the run's ceiling (bug 1124).
// The orchestrator's own per-loop breaker stops it from making its next MODEL call, but
// a spawn is a TOOL call dispatched mid-round; without this check the orchestrator could
// fire off one more (expensive) worker after the ceiling is already blown. A nil meter
// is ignored, leaving the spawn tool without a pre-spawn ceiling check (prior behavior).
func WithCostMeter(m *cost.Meter) Option {
	return func(sp *SpawnProvider) {
		if m != nil {
			sp.meter = m
		}
	}
}

// NewSpawnProvider builds the spawn tool over a worker spawner and the profile
// registry that resolves a duty's named profile.
func NewSpawnProvider(spawner *agent.Spawner, registry *profile.Registry, opts ...Option) *SpawnProvider {
	sp := &SpawnProvider{spawner: spawner, registry: registry, maxDepth: defaultMaxDepth}
	for _, o := range opts {
		o(sp)
	}
	return sp
}

// Specs returns the single agent.spawn tool spec (the {action, params} envelope
// shape every corpos surface presents).
func (sp *SpawnProvider) Specs() []tool.Spec {
	entries := []string{
		"spawn(profile, duty) — delegate one duty to a scoped worker running under the " +
			"named job-profile (its lean tool scope + model tier); returns the worker's answer + cost. " +
			"profile is OPTIONAL: omit it (or pass \"auto\") to let the duty→profile router pick the " +
			"cheapest-capable profile via the session-routing classifier.",
	}
	return []tool.Spec{mcp.EnvelopeSpec(Surface,
		"Delegate a sub-task to a scoped worker. Decompose the goal into duties, spawn one worker per "+
			"duty under the cheapest capable profile, then synthesize their answers.",
		[]string{spawnAction}, entries)}
}

// Dispatch runs an agent.spawn call: resolve the profile, run a worker on the duty,
// and return its answer. Like every provider it never returns a Go error out of
// band — failures come back as a tool.Result with a non-empty ErrorClass so the
// orchestrator's loop folds them into its transcript and can adapt.
func (sp *SpawnProvider) Dispatch(ctx context.Context, c tool.Call) tool.Result {
	if c.Action != spawnAction {
		return failUsage(c, fmt.Sprintf("unknown action %q on %s (want %s)", c.Action, Surface, spawnAction))
	}
	name := strings.TrimSpace(asString(c.Params["profile"]))
	duty := strings.TrimSpace(asString(c.Params["duty"]))
	if duty == "" {
		return failUsage(c, "spawn requires a non-empty 'duty'")
	}

	// Tree-cost ceiling (bug 1124): refuse to spawn another worker once cumulative tree
	// spend has reached the run's ceiling. The orchestrator's own per-loop breaker only
	// stops it before its next MODEL call; a spawn is a TOOL call dispatched mid-round, so
	// without this an orchestrator that already blew the ceiling could still fire off one
	// more expensive worker. Checked before routing so a blown budget doesn't even pay for
	// the classifier call. Surfaced as a tool-class failure the loop folds into its
	// transcript (so the orchestrator sees the ceiling and synthesizes from what it has).
	if sp.meter != nil && sp.meter.Exceeded() {
		return fail(c, fmt.Sprintf("cost ceiling $%.2f reached ($%.4f spent across the spawn tree) — refusing to spawn another worker", sp.meter.Ceiling(), sp.meter.Total()))
	}

	depth, _ := ctx.Value(depthKey{}).(int)
	if depth >= sp.maxDepth {
		return failUsage(c, fmt.Sprintf("spawn depth limit %d reached (refusing to recurse further)", sp.maxDepth))
	}

	// The spawn-COUNT budget lives one layer down, in agent.Spawner.Run — the single chokepoint
	// EVERY spawn flows through (this direct agent.spawn AND the coding organ's operator-seat
	// interventions), so it bounds the whole tree's fan-out, not just this decision (Run-42: 34
	// workers, only 5 of them direct). A budget-exhausted spawn returns ErrSpawnBudgetExhausted,
	// which spawnRunFailure classifies ClassUsage so the orchestrator gets the synthesize directive
	// without climbing the ladder.

	// Single profile-resolution chokepoint: BOTH the auto-router and explicit-name paths flow
	// through resolveProfile, which guarantees a test-authoring duty never lands on a
	// test-protecting profile — so no downstream re-check is needed.
	r, errRes, ok := sp.resolveProfile(ctx, c, name, duty)
	if !ok {
		return errRes
	}
	p := r.profile

	// Live coding path: a duty resolving to the coding profile runs through the
	// operator-seat organ when one is wired (the single capable coding path —
	// branch_fix carryover + K→strong). Every other profile, and the default nil
	// CodingPath, stays on the bare worker below (unchanged).
	if r.name == codingProfileName && sp.codingPath != nil {
		answer, costUSD, err := sp.codingPath(context.WithValue(ctx, depthKey{}, depth+1), duty, &p)
		if err != nil {
			return spawnRunFailure(c, fmt.Sprintf("coding organ %q: %v", r.name, err), err)
		}
		return spawnOK(c, duty, answer, costUSD, r)
	}
	res, err := sp.spawner.Run(context.WithValue(ctx, depthKey{}, depth+1), &p, duty)
	if err != nil {
		return spawnRunFailure(c, fmt.Sprintf("worker %q: %v", r.name, err), err)
	}
	return spawnOK(c, duty, res.Text, res.CostUSD, r)
}

// profileResolution is the outcome of resolving a spawn's target profile: the chosen profile
// name + loaded JobProfile, plus the telemetry the result echoes (auto-route label, guardrail
// redirect).
type profileResolution struct {
	name              string
	profile           profile.JobProfile
	autoRouted        bool
	routedLabel       string
	guardrailRedirect string
}

// resolveProfile is the ONE chokepoint both the auto-router and explicit-name paths flow through.
// An omitted/"auto" profile is resolved by the classifier-driven router to the cheapest-capable
// profile (a classifier error still yields the fallback, so a spawn never blocks on routing input).
// Then it enforces the profile-compatibility guarantee in ONE place: a test-authoring duty must
// never run on a profile that protects *_test.go — the worker could never write its deliverable and
// would thrash protect-path denials up the ladder to Opus (bugs 1089/1098). The router already
// steers a test-authoring duty to the test-authoring profile on the auto path; here the SAME
// guarantee is applied to an explicitly-named test-protecting profile, so the orchestrator no longer
// re-checks it downstream. On failure it returns the (usage-class) failure result with ok=false.
func (sp *SpawnProvider) resolveProfile(ctx context.Context, c tool.Call, name, duty string) (profileResolution, tool.Result, bool) {
	var r profileResolution
	if name == "" || name == "auto" {
		if sp.router == nil {
			return r, failUsage(c, "spawn requires a 'profile' (no auto-router is configured)"), false
		}
		dec, _ := sp.router.Route(ctx, duty)
		name, r.routedLabel, r.autoRouted = dec.Profile, dec.Label, true
	}
	p, ok := sp.registry.Get(name)
	if !ok {
		return r, failUsage(c, fmt.Sprintf("unknown profile %q (have: %s)", name, strings.Join(sp.registry.Names(), ", "))), false
	}
	if routing.LooksLikeTestAuthoring(duty) && profileProtectsTests(p) {
		if ta := sp.testAuthoringProfileName(); ta != "" && ta != name {
			if tp, taOK := sp.registry.Get(ta); taOK {
				r.guardrailRedirect = name + "→" + ta
				name, p = ta, tp
			}
		}
	}
	r.name, r.profile = name, p
	return r, tool.Result{}, true
}

// spawnOK builds the success Result for a completed spawn, echoing the resolution telemetry
// (auto-route label + guardrail redirect) identically for the coding-organ and bare-worker paths.
func spawnOK(c tool.Call, duty, answer string, costUSD float64, r profileResolution) tool.Result {
	val := map[string]any{
		"profile":  r.name,
		"duty":     duty,
		"answer":   answer,
		"cost_usd": costUSD,
	}
	if r.autoRouted {
		val["auto_routed"] = true
		val["routed_label"] = r.routedLabel
	}
	if r.guardrailRedirect != "" {
		val["profile_guardrail"] = r.guardrailRedirect
	}
	return tool.Result{Call: c, OK: true, Value: val}
}

// asString coerces a param value to a string ("" for nil or a non-string).
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// profileProtectsTests reports whether a profile's protect_paths would deny a
// *_test.go write (e.g. atomic-coding-chain's ["**/*_test.go"]). test-authoring-chain,
// which protects production but EXCEPTS *_test.go (["**/*.go","!**/*_test.go"]), returns
// false — so the guardrail never redirects a duty that is already on the right profile.
func profileProtectsTests(p profile.JobProfile) bool {
	return pathglob.IsProtected("pkg/example_test.go", p.ProtectPaths)
}

// testAuthoringProfileName is the guardrail's redirect target — the router's configured
// test-authoring profile, or "" when no router is wired (then no redirect happens).
func (sp *SpawnProvider) testAuthoringProfileName() string {
	if sp.router != nil {
		return sp.router.TestAuthoringProfile()
	}
	return ""
}

// fail builds a tool-class failure result the loop folds back into the transcript.
// ClassTool counts toward the orchestrator's repeated_tool_error tally — reserved for a
// genuine worker/organ runtime fault, where a stronger model could plausibly help.
func fail(c tool.Call, msg string) tool.Result {
	return tool.Result{Call: c, OK: false, ErrorClass: tool.ClassTool, Value: map[string]any{"error": msg}}
}

// failUsage builds a usage-class failure for a spawn CONFIG/precondition error no stronger
// MODEL can fix — a missing/unknown profile, an empty duty, a bad action, the depth guard,
// a non-runnable verify gate. ClassUsage keeps it OUT of the repeated_tool_error tally, so
// the orchestrator does not climb the model ladder (and burn the frontier rung) on a
// condition escalation can never resolve (bug 1095) — mirroring the sysorgan/fsorgan
// usage-error precedent. The orchestrator still receives the actionable message and adapts.
func failUsage(c tool.Call, msg string) tool.Result {
	return tool.Result{Call: c, OK: false, ErrorClass: tool.ClassUsage, Value: map[string]any{"error": msg}}
}

// spawnRunFailure classifies an error surfaced while RUNNING a worker/coding-organ: a
// verify-gate precondition failure (agent.ErrVerifyGateUnrunnable) is a config error →
// ClassUsage (non-escalatable); any other runtime fault stays ClassTool (escalatable).
func spawnRunFailure(c tool.Call, msg string, err error) tool.Result {
	// A verify-gate precondition failure or a spent spawn budget is a structural bound no
	// stronger MODEL can lift → ClassUsage (non-escalatable, bug 1095): the orchestrator gets
	// the actionable message (for the budget, the "stop decomposing, synthesize" directive) and
	// adapts, instead of climbing the ladder on a condition escalation can never resolve.
	if errors.Is(err, agent.ErrVerifyGateUnrunnable) || errors.Is(err, agent.ErrSpawnBudgetExhausted) ||
		errors.Is(err, agent.ErrWorkerToolsUnrunnable) {
		return failUsage(c, msg)
	}
	return fail(c, msg)
}
