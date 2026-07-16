// Package profile defines the job-profile — Corp-OS' capability-scoping unit.
//
// A job-profile is the duty-shaped envelope the orchestrator stamps on a worker
// when it spawns one: {minimal tools, governing skills, lean context-shapes,
// cheapest capable model tier}. corpos owns the loop, so it projects exactly the
// profile's envelope into each (sub)agent instead of mounting every MCP surface
// flat — the inversion that makes a rich owned-surface ecosystem survivable
// (design doc §0/§4). Even the top-level agent runs under a profile (orchestrate).
//
// # Permission posture (settled here; the SCOPE axis only)
//
// A profile's projected tool scope IS the capability allow-list: corpos projects
// only the surfaces/actions the profile declares (see mcp.Catalog.Project), so a
// worker simply cannot invoke a surface or action its profile did not expose
// (§4.4.4). That is the SCOPE axis — *which tools exist for this worker*.
//
// It is deliberately NOT the RISK axis — *whether a specific destructive or
// outward invocation should proceed* (rm, git push, fs.edit on protected paths,
// external sends). That axis is orthogonal to scope and is settled separately by
// the risk-gating layer (chain task risk-gating-for-autonomous-action; §7 gap #1).
// A profile being tool-capable never implies an invocation is risk-approved.
//
// This package is corpos-native loop policy, not an MCP surface: pushing profile
// resolution back out into a tool would re-create the very loop-policy-as-tool
// inversion the cannibalization map warns against (§2.3, §4.5).
package profile

import (
	"fmt"
	"sort"
	"strings"
)

// Tier is the model tier a profile runs on. Three-valued per the 2026-06-02
// routing decision (§4.6): local Qwen for mechanical work, mid (the default
// orchestrator seat) for routine reasoning, strong for escalation-gated hard
// reasoning. The router's full three-rung wiring lands on corpos-sub-orchestration
// (task upgrade-router-to-3-tier-bounded-opus); this type carries the field today.
type Tier string

// The three model tiers.
const (
	TierLocal  Tier = "local"
	TierMid    Tier = "mid"
	TierStrong Tier = "strong"
)

// validTiers is the closed tier taxonomy, used by Validate.
var validTiers = map[Tier]bool{TierLocal: true, TierMid: true, TierStrong: true}

// SurfaceScope is action-level tool scope: one MCP surface plus the actions
// allowed on it. An empty Actions slice means the whole surface is allowed (all
// its actions); a non-empty slice restricts the worker to exactly those actions.
// Action-level granularity is cheap because the enrich enum already enumerates
// per-surface actions (§4.2, open-q #3 resolved: action-level).
type SurfaceScope struct {
	Surface string   `toml:"surface"`
	Actions []string `toml:"actions"`
}

// JobProfile is the capability envelope stamped on a worker. It is the single
// object the projection step (build #1) and sub-orchestration (build #5) both
// read: the projector builds the tool subset from Tools, the router reads Tier
// and EscalateOn, and a PreTurn hook prunes parse_context to ContextShapes and
// injects Skills.
type JobProfile struct {
	// Name is the unique profile id (kebab-case), referenced when spawning.
	Name string `toml:"name"`
	// Duty is the human-readable charge this profile fulfils.
	Duty string `toml:"duty"`
	// Signals are the keywords whose presence in a top-level prompt selects this
	// profile when no -profile is named (the deterministic prompt→profile matcher,
	// corpos #3096). They are matched whole-word, case-insensitive by the Select
	// path; the highest distinct-signal count wins, with a shape-affinity tiebreak
	// from ContextShapes. Empty = the profile is never auto-selected (it can still
	// be named explicitly or reached as a rescope target). Hand-authored per
	// profile so selection stays explainable.
	Signals []string `toml:"signals"`
	// RescopeTo is the ordered profile-escalation ladder (corpos #3097): when a
	// worker under this profile is hard-blocked for lack of a TOOL (a scope-denied
	// dispatch), the dispatch boundary may widen to the first listed profile that
	// grants the denied surface/action — the profile analog of the model escalation
	// ladder. The named profile's scope is UNIONED into the current one (never
	// replaced), so a re-scope only ever widens capability and never drops a grant.
	// Declare it only on profiles whose narrowness is a best-guess that a
	// mis-classified task may need to recover from (e.g. a "review" task that turns
	// out to need fs.write → code-review rescopes to bug-fix); a deliberately
	// read-only posture (e.g. orchestrate) simply leaves it empty. Empty = no
	// rescoping (the worker stays hard-blocked, today's behavior).
	RescopeTo []string `toml:"rescope_to"`
	// Tools is the action-level tool scope (the capability allow-list).
	Tools []SurfaceScope `toml:"tools"`
	// Skills are the discipline names injected into this worker's system prompt.
	Skills []string `toml:"skills"`
	// ContextShapes are the parse_context candidate-shapes to surface; the rest
	// are pruned from the worker's context payload.
	ContextShapes []string `toml:"context_shapes"`
	// RequiredShape, when set, gates AUTO-SELECTION: the profile is only auto-selected
	// when the parse_context envelope actually references that shape. It is how a profile
	// whose whole purpose keys off a specific referent declares "do not pick me on my
	// signal keywords ALONE." bug-fix ("fix ONE FILED bug end-to-end") sets it to
	// "bug_slug", so a free-form code-fix prompt (a path but no filed bug) falls through to
	// the coding-capable default instead of the flat single-worker bug-fix profile (bug
	// 1144). Empty = the prior behavior (signals alone can auto-select). It never affects an
	// EXPLICIT -profile choice, only the deterministic auto-matcher.
	RequiredShape string `toml:"required_shape"`
	// Tier is the model tier this profile runs on.
	Tier Tier `toml:"tier"`
	// EscalateOn optionally lists signals that bump the tier via the router
	// (e.g. "tool_error"). Empty means no profile-driven escalation.
	EscalateOn []string `toml:"escalate_on"`
	// SystemPrompt is profile-specific system-prompt text projected into a worker
	// running this profile (appended to the base prompt at the top level, seeded as a
	// system message on a spawned worker). It carries posture the duty alone doesn't —
	// e.g. the atomic-coding-chain faithful-reporting clauses (T6). Empty = none.
	SystemPrompt string `toml:"system_prompt"`
	// CodingRung opts this profile into the intermediate authoring rung
	// (DeepSeek-V3.2) inserted between the mid and strong rungs on the escalation
	// ladder — the atomic-coding-chain operator-escalation path
	// (ATOMIC_CODING_CHAIN.md §5.8). Ignored when the spawner has no coding rung
	// configured.
	CodingRung bool `toml:"coding_rung"`
	// VerifyCommand is the orchestrator-owned build/test gate a spawned worker
	// running this profile is held to: AFTER the worker claims done, the LOOP (not
	// the model) runs this fixed command, and on a non-zero exit feeds the output
	// back and lets the worker revise — a bounded write→verify→revise loop the
	// worker cannot skip (bug 1073: a coding worker landed non-compiling edits and
	// stopped because the build-check was prompt-only). Empty = no auto-verify (the
	// prior behavior; the worker's self-report is taken at face value). It is run as
	// an argv vector via the same orchestrator-owned exec seam the top-level -verify
	// gate uses, so it is safe under risk-gate=enforce.
	VerifyCommand []string `toml:"verify_command"`
	// VerifyMaxRounds bounds the verify-fail → revise cycles for a spawned worker's
	// auto-verify gate (<=0 → the agent loop's default). It is the self-repair cap:
	// the worker gets at most this many revise attempts to turn the gate green
	// before the spawn returns an honest unverified/escalate verdict.
	VerifyMaxRounds int `toml:"verify_max_rounds"`
	// ProtectPaths are glob patterns (forward-slash, ** spans segments) a worker
	// running this profile may NOT write or edit. It keeps the repair loop honest:
	// a coding worker must fix PRODUCTION code, never edit a *_test.go / acceptance
	// path to force the verify gate green (the verification-integrity constraint).
	// Enforced as a dispatch-boundary pre_tool_use denial on fs.write/edit. Empty =
	// no path is protected for this profile.
	ProtectPaths []string `toml:"protect_paths"`
	// MaxToolRounds overrides the spawned worker loop's per-cycle tool-round budget
	// (<=0 → the agent loop's default of 12). A coding worker that must read → grep →
	// locate the integration point → edit → verify in ONE fresh conversation (attempts
	// don't share memory, by design — the prompt stays bounded) needs more headroom
	// than a generic read-only worker; 12 cut a real fix off mid-investigation (the
	// t3b dogfood "exceeded max tool rounds"). Scoped per profile so only work types
	// that need the rounds pay the runaway budget; floor-model rounds are free.
	MaxToolRounds int `toml:"max_tool_rounds"`
}

// Validate checks a profile is well-formed: a name, a known tier, and no empty
// or duplicated surface in its tool scope. It returns the first problem found.
func (p JobProfile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("profile has no name")
	}
	if p.Tier == "" {
		return fmt.Errorf("profile %q has no tier", p.Name)
	}
	if !validTiers[p.Tier] {
		return fmt.Errorf("profile %q has unknown tier %q (want local|mid|strong)", p.Name, p.Tier)
	}
	seen := map[string]bool{}
	for _, t := range p.Tools {
		s := strings.TrimSpace(t.Surface)
		if s == "" {
			return fmt.Errorf("profile %q has a tool scope with no surface", p.Name)
		}
		if seen[s] {
			return fmt.Errorf("profile %q scopes surface %q more than once", p.Name, s)
		}
		seen[s] = true
	}
	return nil
}

// Surfaces returns the surface names this profile scopes, sorted — the set of
// MCP surfaces the worker may reach at all.
func (p JobProfile) Surfaces() []string {
	out := make([]string, 0, len(p.Tools))
	for _, t := range p.Tools {
		out = append(out, t.Surface)
	}
	sort.Strings(out)
	return out
}

// The tool-execution surfaces and the actions on them that make a profile unable
// to do its job from the substrate ledger alone: a profile that scopes fs
// write/edit or sys exec MUST be able to mutate the owned filesystem or run a
// command. With 0 of those projected (the MCP catalog was unreachable or empty at
// startup) such a profile can only spin to its timeout and emit a fake-empty
// result, so it must fail fast (bug 1030). A read-only fs scope (read/grep/glob/ls)
// or read-only sys introspection (ps/ports/units/containers) is NOT tool-dependent
// — that work can still be answered from the substrate, so it is allowed to run.
const (
	surfaceFS  = "fs"
	surfaceSys = "sys"
)

var mutatingActions = map[string]map[string]bool{
	surfaceFS:  {"write": true, "edit": true},
	surfaceSys: {"exec": true},
}

// MutatesFiles reports whether this profile's tool scope lets it CHANGE files: it scopes
// the fs surface with a write-class action (write/edit/move/remove) or the whole fs surface
// (empty Actions = every action). A read-only fs scope (read/grep/glob/ls), a sys-exec-only
// scope (git-process runs commands but writes no files via fs), or no fs at all does not
// count. It mirrors the agent loop's isMutatingWrite (the fs actions the no-work audit
// counts), and is the predicate for arming that audit: a verify-gate green is only evidence
// of a FIX when the run could mutate files — so only a file-mutating profile is held to
// having mutated. Distinct from ToolDependent, which also counts sys.exec.
func (p JobProfile) MutatesFiles() bool {
	for _, t := range p.Tools {
		if t.Surface != "fs" {
			continue
		}
		if len(t.Actions) == 0 {
			return true // whole-surface fs grant includes write/edit/move/remove
		}
		for _, a := range t.Actions {
			switch a {
			case "write", "edit", "move", "remove":
				return true
			}
		}
	}
	return false
}

// MutatorSurfaceDropped reports whether a file-mutating profile (MutatesFiles) had its fs
// surface DROPPED from the projected spec set — the bug-1080 fail-closed trap. The raw toolkit
// fs spec carries no action enum, so mcp.Project fails CLOSED when a profile action-scopes
// fs[write,edit,…] and drops the WHOLE fs surface, handing a coding worker ZERO file tools
// while OTHER scoped surfaces (e.g. sys, whose spec DOES carry an enum) survive. The projected
// surface count is then > 0, so the bug-1030 ToollessAbort (which keys off projected == 0)
// never fires, and the worker can only 'done' with zero fs dispatches — indistinguishable from
// a fabrication. This predicate is the fail-LOUD companion: a mutation-expecting profile whose
// projected surfaces no longer include fs is a misconfiguration the spawner refuses. A
// whole-surface fs grant (empty Actions) is not action-scoped, so its enum-less spec survives
// projection and this returns false; a read-only or fs-less profile never mutates, so it is
// exempt. projectedSurfaces is the set of surface names the projection produced.
func MutatorSurfaceDropped(p *JobProfile, projectedSurfaces []string) bool {
	if p == nil || !p.MutatesFiles() {
		return false
	}
	for _, s := range projectedSurfaces {
		if s == surfaceFS {
			return false // fs survived projection — the worker can edit
		}
	}
	return true
}

// ToolDependent reports whether this profile cannot do its job without real tool
// execution — i.e. it scopes a MUTATING tool surface action: fs write/edit
// (writing code/files) or sys exec (running commands). A whole-surface scope of fs
// or sys (empty Actions = every action allowed) includes those mutating actions and
// so is tool-dependent too. A read-only fs/sys scope, or no fs/sys at all, is not:
// such a profile (synthesis, web-research, code-review, orchestrate, …) produces a
// prose/decision deliverable it can answer from the substrate, so it still runs
// when the tool catalog is empty. This is the tool-dependence half of the bug-1030
// fail-fast predicate; see ToollessAbort for the projected-0 decision.
func (p JobProfile) ToolDependent() bool {
	for _, t := range p.Tools {
		muts, ok := mutatingActions[t.Surface]
		if !ok {
			continue
		}
		if len(t.Actions) == 0 {
			return true // whole-surface scope grants every action, incl. the mutating ones
		}
		for _, a := range t.Actions {
			if muts[a] {
				return true
			}
		}
	}
	return false
}

// ToollessAbort is the pure startup fail-fast decision (bug 1030): a tool-dependent
// profile that projected 0 tool surfaces is unrunnable and must abort loudly rather
// than start a toolless run that can only spin to its timeout. It returns (true, a
// human-readable FATAL reason) when the run must abort, else (false, "").
//
// It is conservative on the cases the bug's acceptance lists as runnable:
//   - p == nil (no -profile: the unprojected full-surface agent) never aborts;
//   - a non-tool-dependent / read-only profile never aborts (it may still run);
//   - a tool-dependent profile with projectedSurfaces > 0 never aborts.
//
// scopedSurfacesDegraded distinguishes the two 0-surface causes for the message
// (both fatal): true when the profile's scoped tool surfaces existed in the raw
// catalog but fell back to the thin (enum-less) spec because the MCP endpoint was
// unreachable at spec-build time — mcp.Project fail-closes an action-level scope on
// a thin spec, dropping the surface — and false when the catalog legitimately
// offered no such surface to project. Either way a tool-dependent profile with 0
// projected surfaces cannot run.
func ToollessAbort(p *JobProfile, projectedSurfaces int, scopedSurfacesDegraded bool) (bool, string) {
	if p == nil || !p.ToolDependent() || projectedSurfaces > 0 {
		return false, ""
	}
	cause := "the tool catalog is empty (no fs/sys surface to project)"
	if scopedSurfacesDegraded {
		cause = "the MCP endpoint was unreachable at startup (tool specs degraded to enum-less stubs, so the action-scoped surfaces were dropped)"
	}
	return true, fmt.Sprintf(
		"tool-dependent profile %q projects 0 tool surfaces — %s. A profile that writes code or runs commands cannot run toolless (it can only spin to its timeout and emit a fake-empty result). Bring up the MCP endpoint, or run a read-only/non-tool-dependent profile. Refusing to start.",
		p.Name, cause)
}
