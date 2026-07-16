// Command corpos is the Corp-OS agent runtime — a hand-written Go agent loop
// that drives the existing toolkit-server over MCP (client-first). This is the
// walking skeleton: one prompt → model → tool calls → answer, all locally.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"corpos/internal/agent"
	"corpos/internal/arcreview"
	"corpos/internal/coding"
	"corpos/internal/cost"
	"corpos/internal/discipline"
	"corpos/internal/escalation"
	"corpos/internal/fsorgan"
	"corpos/internal/grounding"
	"corpos/internal/hooks"
	"corpos/internal/laddercfg"
	"corpos/internal/mcp"
	"corpos/internal/memory"
	"corpos/internal/model"
	"corpos/internal/orchestrator"
	"corpos/internal/profile"
	"corpos/internal/profilehooks"
	"corpos/internal/repl"
	"corpos/internal/risk"
	"corpos/internal/router"
	"corpos/internal/routing"
	"corpos/internal/session"
	"corpos/internal/skills"
	"corpos/internal/sysorgan"
	"corpos/internal/tool"
	"corpos/internal/version"
	"corpos/internal/web"
)

const systemPrompt = "You are Corp-OS, an agent operating over toolkit-server. " +
	"You have tools named for substrate surfaces (work, knowledge, fs, measure, sys, web); each takes an " +
	"{action, params, rationale} envelope. Each tool's description lists its real action " +
	"names and the params each one takes — use those exact names and keys, do not invent them. " +
	"Use a tool when you need live data; otherwise answer directly. " +
	"To work in a large file, do NOT read it whole — fs.grep for the symbol you need (with " +
	"show_line_numbers), then fs.read with offset/limit for a bounded line range around the hit. " +
	"A truncated tool result means narrow the call (grep a tighter pattern, add a limit/offset, " +
	"read a line range) — never re-issue the same broad call."

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable entry point; main only translates its exit code. Keeping
// os.Exit out of run lets `defer cancel()` actually run.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("corpos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prompt := fs.String("prompt", "", "the user prompt (reads stdin when empty)")
	mcpURL := fs.String("mcp-url", mcp.DefaultToolkitURL, "toolkit-server HTTP daemon (canonical :3001 post-flip; native :3000 is retired)")
	mcpConfig := fs.String("mcp-config", "", "path to a .mcp.json-equivalent listing the MCP servers to mount (default: toolkit + web)")
	modelURL := fs.String("model-url", "http://localhost:8081/v1", "OpenAI-compatible model endpoint")
	providerName := fs.String("provider", "openai", "model provider: openai (OpenAI-compatible, e.g. local Qwen) | anthropic")
	modelID := fs.String("model", "", "model identifier (defaults per provider)")
	project := fs.String("project", "corpos-toolkit", "project scope for tool calls (the ledger project this run's tool calls are attributed to)")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-turn timeout")
	workerTimeout := fs.Duration("worker-timeout", 15*time.Minute, "per-spawned-worker turn budget, detached from the orchestrator's per-turn -timeout. A spawned worker is a full multi-round sub-loop that the orchestrator BLOCKS on, so sharing the orchestrator's per-turn deadline starved a coding worker's first slow local-tier call on a large context (bug 1123). 0 = no outer worker deadline (bounded by the per-call HTTP timeout + -max-rounds)")
	maxRounds := fs.Int("max-rounds", 0, "per-turn tool-round budget (0 = built-in default of 12). Each tool call AND each -verify revise cycle consumes a round, so a long write→verify→revise convergence may need more than the default")
	maxCostUSD := fs.Float64("max-cost-usd", 0, "circuit-breaker: hard per-run cost ceiling in USD (0 = no ceiling). When the session ledger reaches it the run stops with an honest 'stuck, $X spent' verdict instead of a runaway")
	maxSpendTokens := fs.Int("max-spend-tokens", 0, "circuit-breaker: hard per-run token budget across input+output (0 = no budget). Distinct from -max-context-tokens (which sizes compaction); this caps total spend and stops the run with a verdict when reached")
	noProgressRounds := fs.Int("no-progress-rounds", 0, "circuit-breaker: stop the run after this many consecutive tool-rounds with no file written and no verify-state change (0 = disabled). Catches a worker that explores/greps without converging")
	goalReminderRounds := fs.Int("goal-reminder-rounds", 0, "re-surface the active goal as a terse reminder near the transcript tail every N tool rounds (0 = disabled). Keeps a long investigation from drifting off-target after the pinned first prompt is buried")
	spawnForceRounds := fs.Int("spawn-force-rounds", 3, "must-spawn guard: for a SPAWN-CAPABLE (orchestrate) agent, inject a forcing 'spawn now' reminder after this many tool rounds with no agent.spawn, re-armed every N further read-only rounds (0 = disabled). No-op for non-spawn-capable workers. Backstops the orchestrate prompt's spawn-early guidance so the read-only orchestrator delegates instead of investigating forever (bug 1072)")
	maxSpawns := fs.Int("max-spawns", 16, "spawn-count budget: cap the total agent.spawn workers an orchestrate run may fan out across its whole tree (0 = unbounded). Once reached, a further spawn is refused with a directive to stop decomposing and synthesize — bounds same-sub-goal respawn thrash that the cost ceiling only catches late (Run-42: 34 workers for one test). Permissive enough for a legitimate multi-duty chain")
	maxDutyRespawns := fs.Int("max-duty-respawns", 3, "per-duty respawn cap: the max times the coding worker→gate loop may be (re-)entered for ONE atom across a duty (initial run + operator-seat resumes). At the cap the duty stops with an honest 'escalation exhausted' verdict instead of re-spawning the same non-converging atom toward the cost ceiling (chain 392). Counts loop ENTRIES, never the per-loop revise rounds, so a legitimate revise budget is never truncated. Distinct from -max-spawns (tree-wide). 0 = unbounded")
	maxCodingRespawns := fs.Int("max-coding-respawns", 3, "cross-invocation coding-respawn cap: the max CONSECUTIVE non-converging coding-path invocations the orchestrate agent may fan out for one run before the coding path REFUSES further spawns with a stuck verdict. A success resets the count, so a legitimate multi-atom feature (each atom greens) is unaffected; a stuck bug can no longer explode into many escalating workers (Run-53: 5–6 workers climbed to Opus for $3.65 without a fix). Distinct from -max-duty-respawns (within one organ) and -max-spawns (tree-wide worker count). 0 = unbounded")
	sessionDir := fs.String("session-dir", "", "session DB directory (default: $XDG_STATE_HOME/corpos/sessions)")
	resume := fs.String("resume", "", "resume a prior session by run id (replays its conversation thread)")
	inspect := fs.String("inspect", "", "print the sub-orchestration telemetry tree (per-worker cost, escalations) for a session run id and exit")
	inspectSession := fs.String("inspect-session", "", "dump one session's transcript + cost + tool_calls by run id and exit (uses -session-dir)")
	runRate := fs.Bool("run-rate", false, "project monthly API run-rate from session telemetry over -session-dir within [-since,-until] and exit")
	since := fs.String("since", "", "run-rate period start (YYYY-MM-DD or RFC3339; default: earliest session)")
	until := fs.String("until", "", "run-rate period end (YYYY-MM-DD or RFC3339; default: now)")
	midProvider := fs.String("mid-provider", "", "mid-tier provider (openrouter|openai|anthropic); empty = no mid rung (the ladder collapses to local↔strong)")
	midModel := fs.String("mid-model", "", "mid-tier model id (defaults per provider; openrouter → google/gemini-3.1-flash-lite)")
	midURL := fs.String("mid-model-url", "", "mid-tier OpenAI-compatible endpoint (openrouter defaults to its API base)")
	strongProvider := fs.String("strong-provider", "", "escalation-tier provider (openai|anthropic|openrouter); empty = single-tier (no escalation)")
	strongModel := fs.String("strong-model", "", "escalation-tier model id (defaults per strong-provider)")
	strongURL := fs.String("strong-model-url", "", "escalation-tier OpenAI-compatible endpoint (defaults to -model-url)")
	strongBound := fs.Int("strong-bound", 2, "max turns the strong (Opus) rung may serve per loop; 0 = unbounded (the frontier stays escalation-gated)")
	maxStrongTurns := fs.Int("max-strong-turns", 0, "cross-invocation strong-turn budget: the max total turns the strong (Opus) rung may serve across a run's WHOLE spawn tree. -strong-bound caps ONE worker's Opus turns, but a stuck atom that respawns N coding workers hands each a fresh bound, so the tree re-climbs Opus N times (bug 1165: 5 workers rode Opus to ~86% of spend). This budget pools those turns tree-wide: once spent, a respawn can no longer re-climb Opus. Bounds TOTAL strong turns (atom-agnostic), like -max-cost-usd bounds total spend. 0 = unbounded (tracking-only; default-off, matching -max-cost-usd)")
	escalateAfter := fs.Int("escalate-after", 1, "escalate one rung after this many tool errors in a turn (needs a mid or strong rung)")
	profileName := fs.String("profile", "", "job-profile to scope this agent's tools/context (empty = auto-select from the prompt via the deterministic matcher, else -default-profile; e.g. task-lifecycle, code-review, orchestrate)")
	defaultProfile := fs.String("default-profile", "orchestrate", "safe-default job-profile used when -profile is empty and the prompt matches no profile's signals (or there is no prompt, as in the REPL). Must be a real tool-bearing profile; set empty to keep the legacy unprojected (full-surface) behavior on a no-match")
	profilesDir := fs.String("profiles-dir", "", "directory of *.toml job-profile defs (overrides the embedded starter library)")
	skillsDir := fs.String("skills-dir", defaultSkillsDir(), "on-disk skills tree overlaid on the embedded baseline (disk wins per skill); empty/absent uses the embedded skills only")
	printTools := fs.Bool("print-tools", false, "print the (projected) tool-spec footprint and exit — the schema-tax measurement")
	printGuards := fs.Bool("print-guards", false, "print the active post-turn audit guard set (the Guard pipeline) and exit — sibling to -print-tools")
	riskGate := fs.String("risk-gate", "enforce", "risk gate for high-blast actions (sys.exec, fs.remove, forge_delete): enforce (fail-closed; gated calls blocked) | build-test (auto-approve build/test/inspection sys.exec so a coding worker can self-verify; deny everything else gated) | off (allow gated calls)")
	contextProbe := fs.Bool("context-probe", false, "fire parse_context each turn and inject the profile-pruned references (needs -profile; one extra substrate round/turn)")
	maxContextTokens := fs.Int("max-context-tokens", 0, "override the compaction budget: compact when TOTAL context (offered tool specs + transcript) exceeds this many tokens. 0 (default) auto-sizes the budget to the detected model context window (see -context-window); set it only to override the auto-sized default. If below the fixed tool-spec overhead, narrow the surface with -profile")
	contextWindow := fs.Int("context-window", 0, "model context window in tokens (0 = auto-detect from the model endpoint). Compaction is sized to it by default; corpos also refuses to start when the tool surface leaves no room in the window (narrow with -profile or raise this)")
	recencyTurns := fs.Int("recency-turns", 6, "turn-groups kept verbatim at the tail when compaction fires")
	contextFidelity := fs.String("context-fidelity", "auto", "context-budget fidelity preset (auto|low|mid|high|extreme): how generously fixed context (injected skills/context, single tool-result bodies) is provisioned within the model window. auto derives it from the detected window (small→low, wide→extreme); pin a level to override")
	lazyToolSpecs := fs.Bool("lazy-tool-specs", false, "offer only the surface envelope (action enum + names) instead of the full per-action catalog; the model fetches param docs on demand via admin.action_describe. The deepest cut to fixed tool-spec overhead (corpos #3100). Opt-in pending live validation that capability holds")
	verifyCmd := fs.String("verify", "", "orchestrator-owned verification command run after the agent claims done; on a non-zero exit its output is fed back and the agent revises (e.g. \"go test ./...\"). Safe under -risk-gate=enforce: the loop runs this FIXED command itself, not the agent via sys.exec")
	verifyDir := fs.String("verify-dir", "", "working directory for -verify (default: current directory)")
	verifyMax := fs.Int("verify-max", 3, "max verify-fail revise cycles before returning with the gate still failing")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "corpos %s\n", version.Version())
		return 0
	}

	// -inspect reads the local session DBs and prints the sub-orchestration tree
	// (the root session plus every worker it spawned), with per-worker cost,
	// escalation edges, and the full-tree run-rate — the swap's go/no-go signal
	// computed over the whole tree, not just the root (T6). No model or substrate
	// calls; it exits before any are made.
	if *inspect != "" {
		return runInspect(stdout, stderr, dirOrDefault(*sessionDir), *inspect)
	}

	// -inspect-session / -run-rate are read-only telemetry views over the local
	// session DBs; they short-circuit before any model adapter / tool provider is
	// built. -inspect-session dumps one session's detail; -run-rate projects the
	// monthly API spend (docs/SWAP_VALIDATION_CRITERIA.md §5).
	if *inspectSession != "" {
		return runInspectSession(dirOrDefault(*sessionDir), *inspectSession, stdout, stderr)
	}
	if *runRate {
		return runRunRate(dirOrDefault(*sessionDir), *since, *until, time.Now(), stdout, stderr)
	}

	// Fail-fast (bug corpos-fs-cwd-vs-verify-dir-silent-nonconvergence): the coding
	// worker's fs surface resolves relative paths against the process working
	// directory, but the build-test gate runs in -verify-dir. When those two point at
	// DISJOINT trees (neither contains the other), the worker edits one tree while the
	// gate verifies another, so the run can NEVER reach green — silently, after burning
	// real model spend climbing the escalation ladder against an unfixable red. Refuse
	// to start here, before any adapter/spec build or model call. The default
	// single-repo ergonomics are untouched: CWD==verify-dir, a verify-dir subdir of CWD
	// (the shipped demos), or a CWD subdir of verify-dir all keep the worker's edits and
	// the gate on the same tree, so only genuinely disjoint trees are refused.
	if *verifyDir != "" {
		if wd, werr := os.Getwd(); werr == nil {
			if div, rc, rv := verifyDirDivergent(wd, *verifyDir); div {
				fmt.Fprintf(stderr, "corpos: FATAL: -verify-dir points at a tree disjoint from the working directory, so a coding run can never converge.\n"+
					"  working directory: %s\n"+
					"  -verify-dir:       %s\n"+
					"The coding worker edits files under the working directory while the build-test gate runs in -verify-dir; the worker's fix never lands in the tree the gate checks, so the run stays red across escalation (real spend, no convergence).\n"+
					"Fix: run corpos from inside the repo you point -verify-dir at (cd %s), or pass a -verify-dir at or under the working directory.\n",
					rc, rv, rv)
				return 2
			}
		}
	}

	m, err := buildAdapter(*providerName, *modelID, *modelURL)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: %v\n", err)
		return 2
	}
	baseCtx := context.Background()

	// Mount the configured MCP servers behind one aggregating provider (the
	// multi-server seam, §1.2/§5#4). Default: the toolkit-server plus the owned web
	// surface; -mcp-config overrides with a .mcp.json-equivalent. Each server sources
	// its own specs — toolkit enriched from the live substrate (real action names +
	// param keys, fail-soft to thin static specs when unreachable), web from its
	// static catalog — and the aggregator routes each call by surface and offers the
	// UNION of their specs. Spec-building is bounded by the turn timeout.
	cfg := mcp.DefaultConfig(*mcpURL, *project)
	if *mcpConfig != "" {
		cfg, err = mcp.LoadConfig(*mcpConfig)
		if err != nil {
			fmt.Fprintf(stderr, "corpos: %v\n", err)
			return 2
		}
	}
	specCtx, specCancel := context.WithTimeout(baseCtx, *timeout)
	servers, err := buildServers(specCtx, cfg, *lazyToolSpecs)
	specCancel()
	if err != nil {
		fmt.Fprintf(stderr, "corpos: %v\n", err)
		return 2
	}
	provider, err := mcp.NewAggregator(servers...)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: %v\n", err)
		return 2
	}
	rawSpecs := provider.Specs()

	// Daily-driver auto-selection (corpos #3096): with no explicit -profile, pick the
	// matching job-profile from the prompt itself — deterministically, via the prompt's
	// signal keywords plus the parse_context envelope's reference shapes — so corpos
	// scopes its own tools/tier without a hand-passed flag. A oneshot prompt is the
	// classifier input; the REPL (no up-front prompt) starts on the safe default. A
	// no-match falls back to -default-profile. selectionJSON carries the decision's
	// features into the session header as the labeled dataset for the later ML matcher.
	effectiveProfile := *profileName
	var selectionJSON string
	if effectiveProfile == "" {
		effectiveProfile, selectionJSON = autoSelectProfile(baseCtx, provider, strings.TrimSpace(*prompt), *defaultProfile, *profilesDir, *timeout, stderr)
	}

	// Capability scoping: when a profile is named (or was auto-selected), project the
	// full surface set down to that profile's action-level envelope (mcp.Project) so
	// the model sees ONLY its scoped tool subset — the projected tools ARE the
	// allow-list. With no profile (an empty -default-profile and a no-match) the agent
	// runs unprojected (the full set), preserving prior behavior.
	activeProfile, specs, err := scopeSpecs(rawSpecs, effectiveProfile, *profilesDir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: %v\n", err)
		return 2
	}

	// Fail-fast (bug 1030): a TOOL-DEPENDENT profile (it scopes fs write/edit or sys
	// exec — it must write code or run commands) that projects 0 tool surfaces is
	// unrunnable. mcp.Project fail-closes an action-level scope on a thin (enum-less)
	// spec, so an unreachable MCP endpoint degrades the scoped surfaces to nothing and
	// the projection comes back empty ("projected 0 of N surfaces"). Starting anyway
	// yields a toolless run that can only spin to its timeout and emit a fake-empty
	// result. Abort loudly here, BEFORE the loop. Non-tool-dependent / read-only
	// profiles (and the unprojected full-surface default) still run. The degraded
	// signal distinguishes "MCP unreachable" from "catalog legitimately empty".
	if abort, reason := profile.ToollessAbort(activeProfile, len(specs), scopedSurfacesDegraded(activeProfile, rawSpecs)); abort {
		fmt.Fprintf(stderr, "corpos: FATAL: %s\n", reason)
		return 2
	}

	// Apply the canonical model ladder when the operator passed NO ladder override,
	// so a bare `corpos` runs the benchmarked-and-locked qwen → gemini-3.1-flash-lite
	// → opus ladder rather than collapsing to single-tier (bug
	// corpos-locked-model-ladder-not-encoded-as-runtime-default). An explicit
	// -mid-*/-strong-* flag wins and is announced as a deviation, so the active
	// ladder is always visible and a drift (e.g. a stale -strong-model haiku example)
	// can never pass silently.
	if ladderOverridden(fs) {
		fmt.Fprintf(stderr, "corpos: model ladder OVERRIDDEN by flags — the canonical (benchmarked) ladder is %s\n", canonicalLadderString())
	} else {
		*midProvider, *midModel = canonicalMidProvider, canonicalMidModel
		*strongProvider, *strongModel = canonicalStrongProvider, canonicalStrongModel
	}

	// Resolve the model ladder (§4.6): the base (local Qwen) rung always present,
	// plus the optional mid (Gemini-Flash-Lite via OpenRouter) and strong (bounded
	// Opus via Anthropic) rungs. Built once and shared by the top-level router and
	// the worker Spawner so a profile's Tier routes consistently. A misconfigured
	// tier is fatal before any turn runs (don't silently drop a rung).
	tiers, err := buildTiers(m, *midProvider, *midModel, *midURL,
		*strongProvider, *strongModel, strongURLOr(*strongURL, *modelURL), *strongBound, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: %v\n", err)
		return 2
	}

	// Escalation contract (docs/ORCHESTRATOR_ESCALATION.md): corpos owns the policy
	// (the router decides WHEN to escalate) and reaches the toolkit's admin surface
	// over MCP for the two halves the toolkit keeps — reading the effective
	// per-trigger thresholds and emitting each escalate edge as an EscalationProposed
	// event. The threshold fetch falls back to the built-in defaults (migration 080)
	// when the toolkit is unreachable, so a cold substrate never dead-starts.
	escClient := escalation.New(mcp.New(*mcpURL, mcp.WithProject(*project)))
	escCfgCtx, escCancel := context.WithTimeout(baseCtx, *timeout)
	escCfg, escErr := escClient.Thresholds(escCfgCtx, *project)
	escCancel()
	if escErr != nil {
		fmt.Fprintf(stderr, "corpos: escalation thresholds unavailable (%v) — using built-in defaults\n", escErr)
	}

	// skillBudget caps the per-worker injected skill text to the model window. It is
	// assigned once the window is resolved (below), then captured by the spawn-worker
	// hook factory and reused by the top-level loop's skill injector — so a
	// narrow-window worker gets terse skill tiers instead of overflowing on the full
	// discipline corpus (the floor-fit keystone). 0 until set / when no window known.
	var skillBudget int

	// Shared tree-cost meter (bug 1124): when -max-cost-usd is set, one meter is threaded
	// through the top-level loop, the spawner (so every worker accrues to it and stops at
	// the ceiling), and the spawn tool (so the orchestrator refuses to delegate past it).
	// This makes the cost ceiling whole-tree — a delegating orchestrate run otherwise saw
	// only its own cheap ledger while the spawned workers spent the money unbounded. Nil
	// when no ceiling is set, leaving a non-tree run bounded only by its per-loop breaker.
	var costMeter *cost.Meter
	if *maxCostUSD > 0 {
		costMeter = cost.NewMeter(*maxCostUSD)
	}

	// Shared tree-wide strong-turn budget (bug 1165(b)): when -max-strong-turns is set, one budget
	// is threaded through the top-level router AND the spawner (every worker's router), so all the
	// run's Opus turns draw from ONE pool. -strong-bound caps a single worker; this caps the tree,
	// so a stuck atom's respawns can't each re-climb Opus. Nil (default) leaves the per-worker bound
	// as the only strong-turn limit — the prior behavior.
	var strongBudget *cost.StrongTurnBudget
	if *maxStrongTurns > 0 {
		strongBudget = cost.NewStrongTurnBudget(*maxStrongTurns)
	}

	// Sub-orchestration: when the active profile may spawn (it scopes the agent
	// surface — only orchestrate does), mount the agent.spawn tool. The orchestrator
	// then decomposes a goal into duties, delegates each to a scoped worker (a child
	// loop per duty under the named profile, on the cheap tier), and reconciles their
	// answers in its own synthesis turn. Workers run on the BASE provider (without the
	// spawn surface) projected to their own profile; the orchestrator gets the agent
	// surface added so it can call it.
	//
	// terminalGreenGate, when the coding path arms below, is the read-only "is the repo
	// green?" backstop handed to the top-level loop so a spawn-only orchestrator whose
	// worker landed a passing fix reports success at a strong-bound/exhaustion halt rather
	// than a false-negative "no final answer" (bug 1148). Set inside the orchestration
	// block (where the coding target dir is resolved); consumed when the top-level opts are
	// assembled below.
	var terminalGreenGate *agent.VerifyGate
	if activeProfile != nil && profileScopesAgent(activeProfile) {
		reg, rerr := loadProfiles(*profilesDir)
		if rerr != nil {
			fmt.Fprintf(stderr, "corpos: %v\n", rerr)
			return 2
		}
		base := provider
		loader, lerr := skills.BuiltinWithOverride(*skillsDir)
		if lerr != nil {
			fmt.Fprintf(stderr, "corpos: %v\n", lerr)
			return 2
		}
		gate := riskGateFor(*riskGate)
		// Workers route over the same local→mid→strong ladder as the orchestrator:
		// their profile Tier picks the floor rung and tool-error escalation climbs
		// it (the strong/Opus rung usage-bounded). A tier=mid worker runs on Gemini.
		sopts := []agent.SpawnerOption{agent.WithEscalationContract(escClient, escCfg)}
		// Each spawned worker runs under its OWN turn budget instead of the orchestrator's
		// per-turn deadline residue: the orchestrator BLOCKS on the worker dispatch while
		// its own -timeout keeps ticking, so a coding worker spawned mid-turn used to inherit
		// only the leftover and its first slow local-tier call on a large context timed out
		// with zero tool calls before escalation could lift it (bug 1123). A slow first call
		// now hits the per-call HTTP timeout — which the loop's timeout recovery escalates
		// off — rather than a spent turn deadline that ends the turn with no progress.
		sopts = append(sopts, agent.WithWorkerTimeout(*workerTimeout))
		// Thread the shared tree-cost meter into every spawned worker so a single
		// non-converging worker thrashing to the frontier can't overshoot the run ceiling
		// between the orchestrator's spawn-decision checks (bug 1124).
		if costMeter != nil {
			sopts = append(sopts, agent.WithSpawnerCostMeter(costMeter))
		}
		// Spawn-count budget: bound the tree-wide worker fan-out so the orchestrator (and the
		// coding organ's operator-seat interventions, which spawn through this same spawner)
		// can't respawn workers for one sub-goal without limit (Run-42: 34 workers for one test).
		// One budget on the spawner — the chokepoint every spawn flows through — caps the whole
		// tree; a refused spawn surfaces a "stop decomposing, synthesize" directive.
		if *maxSpawns > 0 {
			sopts = append(sopts, agent.WithSpawnerSpawnBudget(cost.NewSpawnBudget(*maxSpawns)))
		}
		if tiers.mid != nil {
			sopts = append(sopts, agent.WithMidTier(tiers.mid))
		}
		if tiers.strong != nil {
			sopts = append(sopts, agent.WithStrongTier(tiers.strong, *escalateAfter))
		}
		if tiers.bound > 0 {
			sopts = append(sopts, agent.WithStrongBound(tiers.bound))
		}
		// Shared tree-wide strong-turn pool into every worker's router (bug 1165(b)): a re-spawned
		// coding worker draws Opus turns from the same budget, so a stuck atom can't re-climb Opus.
		if strongBudget != nil {
			sopts = append(sopts, agent.WithSpawnerStrongTurnBudget(strongBudget))
		}
		// Intermediate authoring rung (DeepSeek-V3.2) for opted-in coding profiles
		// (atomic-coding-chain): inserted mid→strong so an authoring escalation carries
		// on the cheaper coder rung before the bounded Opus rung (canonical ladder).
		// Captured here (the SAME canonical ladder rung) for reuse as the operator
		// seat's coder rung below — not a second ladder.
		var codingRung model.Adapter
		if cr, cerr := buildAdapter(canonicalMidProvider, canonicalCodingRung, ""); cerr == nil {
			if !cr.Available() {
				fmt.Fprintf(stderr, "corpos: coding rung %s unavailable (no key?) — coding workers escalate mid→strong\n", cr.Model())
			}
			codingRung = cr
			sopts = append(sopts, agent.WithCodingRung(cr))
		}
		// Auto-verify dir (bug 1073): a spawned coding worker is held to its profile's
		// build/test gate (profile.VerifyCommand), run by the loop in this dir. The
		// worker writes into the corpos process tree, so the gate runs where the agent
		// runs — the explicit -verify-dir when set, else the process CWD (execVerify's
		// default for an empty dir). Reuses the same dir knob as the top-level -verify.
		if vdir := *verifyDir; vdir != "" {
			sopts = append(sopts, agent.WithSpawnVerifyDir(vdir))
		}
		// Per-worker telemetry: each spawned worker gets its own session DB linked
		// to the parent run id (read off the context), so the decomposition tree,
		// per-worker cost, and escalation events are all recoverable via -inspect (T6).
		sessDir := dirOrDefault(*sessionDir)
		strongID := m.Model()
		if tiers.strong != nil {
			strongID = tiers.strong.Model()
		}
		sopts = append(sopts, agent.WithChildStore(func(parentRunID, profileName, duty string) *session.Store {
			st, cerr := session.Create(sessDir, session.Header{
				Project: *project, ParentRunID: parentRunID, Profile: profileName, Duty: duty,
				ModelCheap: m.Model(), ModelStrong: strongID,
			})
			if cerr != nil {
				return nil // best-effort: a worker without a store just runs untelemetered
			}
			return st
		}))
		// Enforce each worker's scope at the dispatch boundary too: a worker
		// dispatches through a provider that DENIES any surface/action its profile
		// did not grant (bug 1044) — the projected specs become a hard capability
		// boundary, not just what the worker is shown.
		sopts = append(sopts, agent.WithScopedProvider(func(p *profile.JobProfile) tool.Provider {
			// Reconcile bug_read results at the worker layer too (bug 1145, defense in depth):
			// a bug-fix worker handed a status='fixed' bug must still reproduce and let the
			// gate decide, not trust the ledger status.
			return mcp.NewGroundTruthReconciler(mcp.NewScoped(base, scopeOf(p)))
		}))
		spawner := agent.NewSpawner(base,
			func(p *profile.JobProfile) []tool.Spec { return mcp.Project(base.Specs(), scopeOf(p)) },
			func(p *profile.JobProfile) *hooks.Surface { return profileWorkerHooks(p, gate, loader, skillBudget) },
			m, sopts...)
		// Duty→profile routing (task decomposition-and-profile-routing-design): the
		// orchestrator may omit a spawn's profile and let the classifier-driven router
		// pick the cheapest-capable one. The routing input is the existing
		// measure.classify_session_routing_trigger rubric, reached over the same MCP
		// provider; a classifier error falls back to a cheap general worker.
		dutyRouter := routing.NewRouter(routing.NewMCPClassifier(base), routing.DefaultTable, "task-lifecycle")
		spOpts := []orchestrator.Option{orchestrator.WithRouter(dutyRouter)}
		// Refuse to spawn a new worker once the cumulative tree cost has hit the ceiling
		// (bug 1124): the orchestrator's per-loop breaker only guards its own next model
		// call, but a spawn is a mid-round tool call that would otherwise fire unbounded.
		if costMeter != nil {
			spOpts = append(spOpts, orchestrator.WithCostMeter(costMeter))
		}
		// Live coding path (activate the operator-seat organ): route a coding duty
		// (the atomic-coding-chain profile) into the organ instead of a bare worker.
		// The organ is a thin orchestration over the SAME spawn primitive — its model
		// worker reuses THIS spawner; its escalation ladder reuses the canonical
		// tiers.mid/strong + codingRung. A real GitRepo over the worker's target dir
		// is required (branch_fix rewinds the integration ref + forks worktrees; a
		// NoopRepo cannot rewind).
		//
		// DEFAULT-ON (bug corpos-orchestrate-spawn-coding-done-claim-not-gated-…): when
		// -verify-dir is not set, default the target to the git repo corpos is running
		// in, so coding duties are GATE-VERIFIED (red-before-green + verify + protected
		// path) by default instead of silently falling through to a bare, UNGATED worker
		// that can self-report a false "done". The organ still needs a strong rung (its
		// escalation top) and a real git repo (to fork worktrees); when either is absent
		// the coding path stays off and corpos WARNS, because code edits then run through
		// the bare worker and are NOT gate-verified.
		codingTargetDir := *verifyDir
		if codingTargetDir == "" {
			wd, _ := os.Getwd()
			codingTargetDir = gitRepoRoot(wd)
		}
		switch {
		case codingTargetDir != "" && tiers.strong != nil:
			worktreeRoot := filepath.Join(os.TempDir(), "corpos-coding-worktrees")
			codingPath := buildCodingPath(spawner, tiers, codingRung, codingTargetDir, worktreeRoot, *maxDutyRespawns, *maxCodingRespawns)
			spOpts = append(spOpts, orchestrator.WithCodingPath(codingPath))
			fmt.Fprintf(stderr, "corpos: live coding path on — coding duties run through the operator-seat organ (target %s)\n", codingTargetDir)
			// Terminal green backstop (bug 1148): the orchestrator owns no verify gate of its
			// own (it is read-only, it delegates), so when it halts on the strong bound with a
			// worker's fix ALREADY green in the target repo, opportunisticGreen has nothing to
			// check and the run reports "no final answer" — a false negative on a verified fix.
			// Give the top loop a read-only green check on the SAME gate the coding worker uses,
			// in the resolved module root, ONLY when no explicit -verify command already covers
			// it (that path installs a full gate). It converts a green-at-halt into an honest
			// success without arming any revise loop on the read-only orchestrator.
			if strings.TrimSpace(*verifyCmd) == "" {
				gateDir := codingTargetDir
				if md, ok := agent.ResolveGoModuleDir(codingTargetDir); ok {
					gateDir = md
				}
				terminalGreenGate = &agent.VerifyGate{Command: defaultCodingGate, Dir: gateDir}
			}
		default:
			fmt.Fprintf(stderr, "corpos: WARNING — gated coding path OFF (%s); coding duties will run through a BARE, UNGATED worker whose edits are NOT gate-verified and can self-report a false \"done\". Run inside a git repo with a strong rung, or pass -verify-dir <repo>, to enable the operator-seat organ.\n",
				codingPathOffReason(codingTargetDir, tiers.strong != nil))
		}
		spawnProvider := orchestrator.NewSpawnProvider(spawner, reg, spOpts...)
		aug, aerr := mcp.NewAggregator(append(servers, mcp.Server{
			Name: orchestrator.Surface, Provider: spawnProvider, Specs: spawnProvider.Specs(),
		})...)
		if aerr != nil {
			fmt.Fprintf(stderr, "corpos: %v\n", aerr)
			return 2
		}
		provider = aug
		specs = mcp.Project(aug.Specs(), scopeOf(activeProfile))
		fmt.Fprintf(stderr, "corpos: orchestration on — %q may spawn workers via agent.spawn\n", activeProfile.Name)
	}

	// -print-tools: report the per-turn schema tax of the (projected) tool set and
	// exit before any model call — the measurement the validate step records.
	if *printTools {
		printFootprint(stdout, effectiveProfile, specs)
		return 0
	}

	// -print-guards: enumerate the post-turn audit guard pipeline (the Guard registry) and
	// exit before any model call — sibling to -print-tools. It renders the declarative guard
	// catalog (the single source of truth for which guards exist) with each guard's stage and
	// its actionable refusal message, so the active guard set is inspectable, not implicit.
	if *printGuards {
		printGuardSet(stdout, activeProfile)
		return 0
	}

	// Local session ledger (flag F4): conversation state persists across turns
	// (and across runs via -resume). Best-effort — an unwritable dir degrades to
	// in-memory rather than failing the run.
	sess, err := openSession(dirOrDefault(*sessionDir), *resume, *project, m.Model(), effectiveProfile, selectionJSON, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: %v\n", err)
		return 2
	}
	if sess.store != nil {
		defer func() { _ = sess.store.Close() }()
	}

	// Append the active profile's system-prompt posture (e.g. atomic-coding-chain's
	// faithful-reporting clauses, T6) to the base prompt for the top-level loop.
	basePrompt := systemPrompt
	if activeProfile != nil && activeProfile.SystemPrompt != "" {
		basePrompt += "\n\n" + activeProfile.SystemPrompt
	}
	opts := []agent.Option{agent.WithSystemPrompt(basePrompt), agent.WithEscalationEmitter(escClient)}
	// Terminal green backstop for the spawn-only orchestrator (bug 1148): armed above when
	// the coding path turned on without an explicit -verify command. A no-op when nil.
	if terminalGreenGate != nil {
		opts = append(opts, agent.WithTerminalGreen(terminalGreenGate))
	}
	// Per-turn tool-round budget: operator-tunable so a long write→verify→revise
	// convergence (each tool call + each verify revise cycle consumes a round) is
	// not starved by the built-in default. 0 leaves the default (defaultMaxRounds).
	if *maxRounds > 0 {
		opts = append(opts, agent.WithMaxRounds(*maxRounds))
	}
	// Circuit-breaker: bound a runaway turn with a cost/token ceiling + a no-progress
	// detector, ending it with an honest verdict rather than spending unbounded (bug
	// 1045). A run-7-style coding run should always set -max-cost-usd. Off when every
	// field is 0 (prior behavior).
	if breaker := (agent.CircuitBreaker{MaxCostUSD: *maxCostUSD, MaxTokens: *maxSpendTokens, NoProgressRounds: *noProgressRounds}); *maxCostUSD > 0 || *maxSpendTokens > 0 || *noProgressRounds > 0 {
		opts = append(opts, agent.WithCircuitBreaker(breaker))
	}
	// Tree-cost meter on the top-level loop (bug 1124): for a delegating orchestrate run
	// this is the load-bearing cost bound — its own ledger sees only cheap planning calls,
	// so breakerTrip must check the shared whole-tree total. The per-loop breaker above
	// still carries the token + no-progress ceilings (which the meter does not). For a
	// non-spawning run the meter tracks only this loop's calls, so the ceiling fires at the
	// same point the per-loop cost breaker would (semantics preserved).
	if costMeter != nil {
		opts = append(opts, agent.WithCostMeter(costMeter))
	}
	// Goal-anchor reinforcement: keep the active goal salient through a long loop.
	if *goalReminderRounds > 0 {
		opts = append(opts, agent.WithGoalReminder(*goalReminderRounds))
	}
	// Must-spawn guard: for a spawn-capable (orchestrate) agent, structurally force
	// delegation after a few read-only rounds so the read-only orchestrator can't stall
	// by investigating forever and never spawning (bug 1072). Armed by default; a no-op
	// for any non-spawn-capable worker, so it is safe to set unconditionally.
	if *spawnForceRounds > 0 {
		opts = append(opts, agent.WithSpawnForcing(*spawnForceRounds))
	}
	// Self-verify gate: the loop runs a fixed verification command after the agent
	// claims done and feeds failures back for revision (orchestrator-owned; safe
	// under risk-gate=enforce). Off unless -verify is set.
	if strings.TrimSpace(*verifyCmd) != "" {
		vdir := *verifyDir
		if vdir == "" {
			vdir, _ = os.Getwd()
		}
		cmd := strings.Fields(*verifyCmd)
		// Fail fast on a non-runnable gate (bug 1075): a go build/test gate pointed at a dir
		// with no reachable go.mod can only error — distinct from a RED result. Surface the
		// misconfig before the run rather than handing the worker a gate it can only game.
		if err := agent.VerifyGateRunnable(cmd, vdir); err != nil {
			fmt.Fprintf(stderr, "corpos: %v\n", err)
			return 2
		}
		opts = append(opts, agent.WithVerify(&agent.VerifyGate{Command: cmd, Dir: vdir, MaxRounds: *verifyMax}))
		// No-work audit (false-green prevention): a top-level -verify gate's green only proves
		// a FIX when the run could AND did change files. For a file-mutating profile, arm the
		// no-work audit so a done-claim with zero substantive fs mutations is refused
		// (Result.Fabricated → non-zero exit) instead of riding a gate that passed VACUOUSLY on
		// unchanged code — the chain-365 dogfood failure: a top-level coding run (bug-fix /
		// atomic-coding-chain) that edited nothing exited 0 because `go test` passed on the
		// untouched tree. Read-only profiles (review/summary) and the spawn-only orchestrator
		// scope no fs writes (MutatesFiles=false) and stay unarmed (they legitimately mutate
		// nothing); spawned workers keep their own scaffold/fake-green guards on the spawn path.
		if armNoWorkAudit(activeProfile) {
			opts = append(opts, agent.WithWorkAudit(agent.WorkAudit{RequireMutation: true}))
		}
	}
	// Context compaction sized to the model window, ON BY DEFAULT (suggestion
	// default-enable-context-compaction-sized-to-window): detect the model's context
	// window (-context-window override, else a fail-soft endpoint probe) and size the
	// compaction budget to it, so a local-tier run no longer overflows mid-loop
	// without per-run hand-tuning. -max-context-tokens overrides the budget. When the
	// fixed tool-spec overhead leaves no room for even a minimal conversation, refuse
	// to start with guidance instead of dying on a mid-loop 400.
	// Two windows matter. localWindow is the BASE/local model's window — what a
	// spawned LOCAL worker runs in — so it sizes the per-worker skill-injection
	// budget. runWindow is the window of the model THIS loop actually runs on: a
	// tier=mid/strong profile (e.g. orchestrate) runs ABOVE the base rung, so its
	// compaction budget + floor-fit guard must reflect that rung's window, not the
	// base's. For a tier=local run the two are identical. Sizing compaction/floor-fit
	// to the base (Qwen 8192) while the orchestrator ran on the mid tier made it
	// over-compact and route ITSELF up to Opus on spawn-result overflow (swap-rehearsal
	// task-2 run — the gemini orchestrator escalated via the mis-applied 8192 guard).
	probeWindow := func(a model.Adapter) int {
		if *contextWindow > 0 {
			return *contextWindow
		}
		wp, ok := a.(model.WindowProber)
		if !ok {
			return 0
		}
		probeCtx, probeCancel := context.WithTimeout(baseCtx, *timeout)
		defer probeCancel()
		if w, wok := wp.ContextWindow(probeCtx); wok {
			return w
		}
		return 0
	}
	localWindow := probeWindow(m)
	// Footprint tier floor (corpos #3096 criterion 4): Qwen-first UNLESS the fixed tool
	// footprint won't fit the local window — then start on mid instead of refusing to
	// start below. Applied to the auto/declared LOCAL tier BEFORE the run model is
	// resolved, so a heavy-surface profile auto-climbs to the wider mid window rather
	// than tripping the "leaves no room" refusal. Only when a mid rung actually exists.
	if activeProfile != nil && tiers.mid != nil {
		_, fp, _ := mcp.Footprint(specs)
		if floor := profile.FootprintFloor(activeProfile.Tier, fp, minRecencyTokens, localWindow); floor != activeProfile.Tier {
			fmt.Fprintf(stderr, "corpos: tool footprint ~%d tok exceeds the local window %d → starting tier %s (was %s)\n", fp, localWindow, floor, activeProfile.Tier)
			activeProfile.Tier = floor
		}
	}
	runModel := tiers.runAdapter(activeProfile)
	runWindow := localWindow
	if runModel != m {
		runWindow = probeWindow(runModel)
	}
	// Context-fidelity budget allocator (corpos #3099): one place derives the
	// compaction budget, the skill/inject caps, and the per-result cap from the model
	// window + a fidelity preset. -context-fidelity=auto sizes fidelity to the window
	// (a small local window auto-tunes the fixed cost down without a manual profile);
	// a pinned level overrides. The skill cap keys off the LOCAL window (spawned
	// workers run local; the orchestrator's own skills are minimal), the compaction
	// budget + per-result cap off the RUN window (the model this loop runs on).
	runAlloc := allocateFor(runWindow, *contextFidelity, stderr)
	localAlloc := allocateFor(localWindow, *contextFidelity, stderr)
	skillBudget = localAlloc.SkillBudget

	_, specTokens, _ := mcp.Footprint(specs)
	if runWindow > 0 && specTokens+minRecencyTokens > runWindow {
		fmt.Fprintf(stderr, "corpos: tool surface (~%d tok) leaves no room in the model context window (%d tok) for the conversation (need ~%d tok of headroom). Narrow it with -profile, or raise the window with -context-window / a larger llama-server --ctx-size. Refusing to start to avoid a mid-loop overflow.\n", specTokens, runWindow, minRecencyTokens)
		return 2
	}
	budget := *maxContextTokens
	if budget <= 0 && runWindow > 0 {
		budget = runAlloc.CompactionBudget
	}
	if budget > 0 {
		opts = append(opts, agent.WithCompaction(budget, *recencyTurns, m))
		// Per-result cap derives from the same allocator (criterion 3): one tool result
		// may use at most the fidelity's share of the transcript budget (chars ≈ tokens×4),
		// so a small window truncates big results harder while a wide one keeps them
		// verbatim. 0 (no detected window) leaves the loop's own budget-derived default.
		if runAlloc.PerResultCap > 0 {
			opts = append(opts, agent.WithToolResultCap(runAlloc.PerResultCap*4))
		}
		if *maxContextTokens <= 0 && runWindow > 0 {
			fmt.Fprintf(stderr, "corpos: context compaction on by default — budget %d tok, fidelity %s (model window %d, tool-spec overhead ~%d)\n", budget, runAlloc.Fidelity, runWindow, specTokens)
		}
		// Per-rung compaction budgets, aligned to the router's tier order (base, then
		// mid, then strong when configured — mirrors buildRouterFromTiers). The loop
		// re-points the compactor at the ACTIVE rung's budget each round, so a worker
		// that escalates off the small-window floor onto a wider tier gets that tier's
		// context instead of the floor's — bug 1088 GAP-2a (escalation that bought a
		// better model but not more context starved the coding worker, every read
		// evicted on arrival). With -max-context-tokens set, every rung shares that
		// explicit budget; otherwise each rung is sized from its OWN window. Every
		// rung now reports a window: the local floor probes its VRAM-fixed n_ctx; a
		// cloud rung reports its known static window (the WindowProber fix —
		// OpenRouter/Anthropic carry it as a configured value, not a /models probe).
		// The base and run rungs reuse their already-resolved windows; a third rung
		// is probed once here.
		windowFor := func(a model.Adapter) int {
			if *contextWindow > 0 {
				return *contextWindow // an explicit override applies to every rung
			}
			switch a {
			case m:
				return localWindow
			case runModel:
				return runWindow
			default:
				return probeWindow(a)
			}
		}
		var tierBudgets []int
		for _, a := range []model.Adapter{tiers.base, tiers.mid, tiers.strong} {
			if a == nil {
				continue
			}
			rb := *maxContextTokens
			if rb <= 0 {
				w := windowFor(a)
				if w <= 0 {
					// A genuinely unknown rung (no probe, no known static window): size
					// from a logged last-resort default so compaction stays ON rather than
					// silently collapsing to the run budget. Demoted from the former
					// wideTierContextWindow, which applied this to EVERY cloud rung.
					w = laddercfg.FallbackUnknownWindow
					fmt.Fprintf(stderr, "corpos: tier %s window unknown (no probe, no known static window) — sizing compaction from a last-resort %d-tok default\n", a.Model(), w)
				}
				rb = laddercfg.CompactionBudgetForWindow(w)
			}
			tierBudgets = append(tierBudgets, rb)
		}
		opts = append(opts, agent.WithTierBudgets(tierBudgets))
	} else if runWindow <= 0 {
		fmt.Fprintf(stderr, "corpos: context compaction off — the run-tier model window was not detected (set -context-window or -max-context-tokens to bound long sessions)\n")
	}
	// Proactive floor-fit guard: pass the detected floor (local) window so the loop
	// routes a would-overflow prompt UP to a larger-window rung BEFORE sending it to
	// the floor — the floor window is VRAM-fixed (can't grow it), so up is the only
	// fit. Without this the floor 400s on overflow and, with a spent strong bound,
	// bounces a stuck floor to death (bug escalation-bound-exhaustion-...).
	if runWindow > 0 {
		opts = append(opts, agent.WithFloorWindow(runWindow))
	}
	if sess.store != nil {
		opts = append(opts, agent.WithSession(sess.store.RunID(), *project), agent.WithStore(sess.store))
	}
	if len(sess.history) > 0 {
		opts = append(opts, agent.WithResumed(sess.history, sess.nextTurn))
	}
	surface := hooks.NewSurface()
	hooked := false
	// Risk gate (orthogonal to scope): gate destructive/mutating actions before
	// dispatch, regardless of profile. enforce = fail-closed (gated calls blocked
	// with an actionable reason); off = explicit opt-out for trusted runs. This is
	// the safety axis that must exist before autonomous spawning (§7 gap #1).
	if gate := riskGateFor(*riskGate); gate != nil {
		_ = surface.Register(hooks.PreToolUse, "risk-gate", risk.Guard(gate))
		hooked = true
	}
	if activeProfile != nil {
		opts = append(opts, agent.WithProfile(activeProfile))
		// Wire the profile's context seams (§5#2): inject its governing skills into
		// the system prompt and prune any parse_context payload to its shapes.
		// BuiltinWithOverride: the embedded baseline ships every discipline the builtin
		// profiles name, so a fresh install injects them even with no ~/.claude/skills;
		// a populated *skillsDir still overrides per-skill.
		loader, lerr := skills.BuiltinWithOverride(*skillsDir)
		if lerr != nil {
			fmt.Fprintf(stderr, "corpos: %v\n", lerr)
			return 2
		}
		_ = surface.Register(hooks.PreUserPrompt, "profile-skills", profilehooks.SkillInjector(loader, skillBudget))
		_ = surface.Register(hooks.PreUserPrompt, "profile-context-prune", profilehooks.ContextPruner())
		hooked = true
		// -context-probe fires parse_context on each prompt; the pruner above trims
		// it to the profile's shapes and this injector renders the survivors into the
		// system prompt (registered AFTER the pruner). Opt-in: it costs one extra
		// substrate round per turn — smarter firing policy is the refresolve chain.
		if *contextProbe {
			_ = surface.Register(hooks.PreUserPrompt, "profile-context-inject", profilehooks.ContextInjector())
			// Client-side discipline firing (toolkit-decomposition T5): the prober
			// calls parse_context with discipline_firing=client, so the envelope
			// carries raw candidate_disciplines; this hook applies the cap + dedup +
			// recent-fire suppression corpos-side and injects the survivors. Registered
			// after the pruner (which preserves candidate_disciplines).
			discTracker := discipline.NewFireTracker()
			_ = surface.Register(hooks.PreUserPrompt, "discipline-fire", discTracker.PreUserPromptHook())
			opts = append(opts, agent.WithContextProber(parseContextProber(provider)))
		}
	}
	// Arc-close filing review (ported from the toolkit's Stop hooks): when a
	// session is active, count turns + watch the last user message for an
	// arc-close shape; on a trigger, fire review_arc_for_filing over MCP and
	// dispatch the result (auto-forge short filings, surface author/confirm
	// reminders). corpos owns the firing decision; the toolkit keeps the action +
	// ArcCloseFilingReviewed event as surfaces. Fail-open throughout.
	if sess.store != nil {
		reviewer := arcreview.New(mcp.New(*mcpURL, mcp.WithProject(*project)), sess.store.RunID())
		_ = surface.Register(hooks.PostTurn, "arc-close-review", reviewer.PostTurnHook())
		_ = surface.Register(hooks.PreUserPrompt, "arc-close-surface", reviewer.PreUserPromptHook())
		// RAG implicit-feedback (toolkit-decomposition T5 piece 3): at session end,
		// detect which search hits were followed/cited/mentioned from the persisted
		// transcript + tool_calls and record the click signals via
		// knowledge.record_query_interaction (linked by the search's span_id). The
		// toolkit already recorded grounding_events server-side for each MCP search.
		rec := grounding.New(mcp.New(*mcpURL, mcp.WithProject(*project)), sess.store, sess.store.RunID())
		_ = surface.Register(hooks.SessionEnd, "grounding-feedback", rec.SessionEndHook())
		hooked = true
	}
	// Memory injection (ported from the toolkit's materialize-memory SessionStart
	// hook): fetch the project's persistent memory digest via knowledge.memory_read
	// and inject it at session start. corpos owns the firing (a SessionStart hook);
	// the vault + the digest assembly stay a toolkit surface, reached over MCP
	// (holding the F4 boundary — no direct vault FS read). Fail-open.
	if *project != "" {
		inj := memory.New(mcp.New(*mcpURL, mcp.WithProject(*project)), *project)
		_ = surface.Register(hooks.SessionStart, "memory-inject", inj.SessionStartHook())
		hooked = true
	}
	if hooked {
		opts = append(opts, agent.WithHooks(surface))
	}

	// Cost-routed tiering over the model ladder (§4.6): the active profile's Tier
	// picks the loop's floor rung (the orchestrate profile rests on the mid/Gemini
	// seat; a leaf or unprofiled run rests on local Qwen) and tool-error escalation
	// climbs it toward the bounded Opus rung. internal/cost prices each tier so the
	// run-rate is measurable. With no mid or strong rung it stays single-tier (the
	// prior default behavior).
	rt := buildRouterFromTiers(tiers, activeProfile, *escalateAfter, escCfg, strongBudget, stderr)
	if strongBudget != nil {
		fmt.Fprintf(stderr, "corpos: strong-turn budget on — the whole run tree may serve at most %d Opus turn(s); respawns draw from one pool (bug 1165)\n", strongBudget.Cap())
	}
	// Capability scoping is enforced at the DISPATCH boundary, not just by hiding
	// the un-granted specs (bug 1044): when a profile is active, the loop dispatches
	// through a provider that denies any surface/action the profile did not grant,
	// returning a tool_error the model can adapt to. Without a profile the loop runs
	// unprojected (full surface set), preserving prior behavior. The raw provider is
	// kept for corpos-internal calls (e.g. the parse_context prober), which are not
	// model tool calls and must not be scoped.
	dispatch := tool.Provider(provider)
	if activeProfile != nil {
		sopts := []mcp.ScopedOption{}
		// Profile-rescope ladder (corpos #3097): when this run's profile is a
		// best-guess that turns out too narrow (a "review" task that needs fs.write),
		// the boundary may widen toward the profile's declared rescope_to rungs rather
		// than hard-block — the profile analog of model-tier escalation, bounded by the
		// rung count and logged.
		if ladder := rescopeLadder(activeProfile, *profilesDir, stderr); len(ladder) > 0 {
			sopts = append(sopts,
				mcp.WithRescopeLadder(activeProfile.Name, ladder, len(ladder)),
				mcp.WithRescopeLog(func(from, to, surface, action string) {
					fmt.Fprintf(stderr, "corpos: profile re-scope %s → %s (granted %s.%s; tool-blocked recovery)\n", from, to, surface, action)
				}))
		}
		dispatch = mcp.NewScoped(provider, scopeOf(activeProfile), sopts...)
	}
	// Reconcile work.bug_read results so this agent treats the ledger's resolution STATUS
	// as bookkeeping, not ground truth (bug 1145): a status='fixed' bug whose repo gate is
	// RED must be FIXED, not "verified". Wraps the final dispatch seam so both the scoped
	// and unprojected top-level paths are covered.
	dispatch = mcp.NewGroundTruthReconciler(dispatch)
	loop := agent.New(rt, dispatch, specs, opts...)
	defer loop.Close()
	defer printCostSummary(stderr, loop, rt)

	// A non-empty -prompt is a single oneshot turn; an empty -prompt opens the
	// multi-turn REPL on stdin. The loop's transcript carries context across REPL
	// turns, so the REPL itself stays a thin wrapper.
	if oneshot := strings.TrimSpace(*prompt); oneshot != "" {
		turnCtx, cancel := context.WithTimeout(baseCtx, *timeout)
		defer cancel()
		res, err := loop.Run(turnCtx, oneshot)
		if err != nil {
			fmt.Fprintf(stderr, "corpos: %v\n", err)
			return 1
		}
		printResult(stdout, stderr, res)
		// A oneshot run that did NOT deliver a verified, usable answer must exit non-zero so a
		// script or a principal cannot mistake an incomplete/unverified/halted run for success
		// (bug 1102: an orchestrate synthesis turn timed out before its verify gate and exited 0
		// with an empty, unverified artifact — a principal trusting it would ship a RED tree).
		return runOutcomeCode(res)
	}

	if err := repl.Run(baseCtx, stdin, stdout, stderr, loop, repl.Config{Prompt: "corpos> ", TurnTimeout: *timeout}); err != nil {
		fmt.Fprintf(stderr, "corpos: %v\n", err)
		return 1
	}
	return 0
}

// printResult renders one turn's per-tool status (to errOut) and final answer
// (to out), matching the REPL's rendering so oneshot and REPL look identical.
func printResult(out, errOut io.Writer, res agent.Result) {
	for _, d := range res.Dispatches {
		status := "ok"
		if !d.OK {
			status = string(d.ErrorClass)
		}
		fmt.Fprintf(errOut, "  [tool %s.%s -> %s]\n", d.Call.Surface, d.Call.Action, status)
	}
	if res.PersistErr != nil {
		fmt.Fprintf(errOut, "corpos: session persist warning: %v\n", res.PersistErr)
	}
	if res.ModelFault != "" {
		fmt.Fprintf(errOut, "corpos: turn ended on a recovered model-call fault (%s) — the run was not aborted; resume to continue\n", res.ModelFault)
	}
	if res.Stopped != "" {
		fmt.Fprintf(errOut, "corpos: %s\n", res.Stopped)
	}
	// The honest terminal verdicts (T7) — surface them so a non-zero exit (runOutcomeCode) is
	// always explained, and a run that could not verify never looks like a clean success.
	if res.Escalate != "" {
		fmt.Fprintf(errOut, "corpos: %s\n", res.Escalate)
	}
	if res.Fabricated != "" {
		fmt.Fprintf(errOut, "corpos: %s\n", res.Fabricated)
	}
	if res.VerifyFailed && res.Escalate == "" {
		fmt.Fprintln(errOut, "corpos: the verify gate did not pass after the revise budget was exhausted")
	}
	printCompactionNotice(errOut, res.Compaction)
	fmt.Fprintln(out, res.Text)
}

// exitIncomplete is the process exit code for a oneshot run that executed but did NOT deliver a
// verified, usable answer — distinct from a usage/arg error (2) and a hard run error (1). It is
// the bright line bug 1102 requires: a run that timed out before synthesis/verify, halted on a
// breaker with no answer, or ended on an honest unverified/fabricated verdict must NOT exit 0.
const exitIncomplete = 3

// runOutcomeCode maps a completed run's Result to a process exit code. 0 only when the run
// produced a usable, non-failing answer. exitIncomplete when it ended without one: a graceful
// model-call fault (e.g. timeout) with no answer, an honest unverified/escalate or fabricated
// verdict (a worker's self-report in Text never overrides these — anti-fake-pass, T7), a failed
// verify gate, or a breaker halt that produced no answer. A Go error from Run is handled by the
// caller (exit 1) before this is reached.
func runOutcomeCode(res agent.Result) int {
	switch {
	case res.Escalate != "", res.Fabricated != "", res.VerifyFailed, res.ModelFault != "":
		return exitIncomplete
	case res.Stopped != "" && strings.TrimSpace(res.Text) == "":
		return exitIncomplete
	default:
		return 0
	}
}

// minRecencyTokens is the headroom (tokens) corpos insists the model window must
// retain after the fixed tool-spec overhead — enough for the pinned goal plus a
// minimal exchange. When the surface leaves less, corpos refuses to start rather
// than overflowing mid-loop (suggestion default-enable-context-compaction-sized-to-window).
const minRecencyTokens = 1024

// The model-ladder sizing policy (compaction/skill budgets, tier→rung floor) lives in
// internal/laddercfg so this composition root keeps wiring, not arithmetic (chain 379).

// printCompactionNotice reports a context compaction to the operator (no-op when
// none fired this turn) — the live signal that the transcript was bounded.
func printCompactionNotice(errOut io.Writer, c *agent.CompactionEvent) {
	if c == nil {
		return
	}
	fmt.Fprintf(errOut, "corpos: context compacted at turn %d: %d→%d tok (budget %d), %d turn-group(s) summarized\n",
		c.TurnIndex, c.TokensBefore, c.TokensAfter, c.Budget, c.GroupsEvicted)
	if c.OverBudget() {
		fmt.Fprintf(errOut, "corpos: note: still over budget %d — fixed tool-spec overhead ~%d tok dominates; raise -max-context-tokens or narrow the tool surface (-profile)\n",
			c.Budget, c.Overhead)
	}
}

// tierSet is the resolved model ladder: the base (local) adapter always present,
// plus the optional mid (Gemini-Flash-Lite) and strong (Opus) rungs and the
// strong-rung usage bound. A nil mid or strong means that rung is unconfigured.
type tierSet struct {
	base   model.Adapter
	mid    model.Adapter
	strong model.Adapter
	bound  int
}

// runAdapter returns the adapter the top-level loop runs on for the active profile:
// a tier=mid/strong profile is promoted above the base rung (mirrors laddercfg.FloorForTier),
// clamping down to the nearest configured rung. A nil profile or an unconfigured
// tier rests on the base. Used to size compaction/floor-fit to the window of the
// model that actually serves the loop's turns, not the base rung's.
func (t tierSet) runAdapter(p *profile.JobProfile) model.Adapter {
	if p != nil {
		switch p.Tier {
		case profile.TierMid:
			if t.mid != nil {
				return t.mid
			}
		case profile.TierStrong:
			if t.strong != nil {
				return t.strong
			}
			if t.mid != nil {
				return t.mid
			}
		}
	}
	return t.base
}

// Canonical model ladder — the benchmarked-and-locked corpos tiers, kept as the
// ONE runtime source of truth (bug
// corpos-locked-model-ladder-not-encoded-as-runtime-default). Worker/floor =
// local Qwen (the base rung); mid/orchestrator = Gemini-3.1-Flash-Lite (the
// orch-spike winner, §4.6); fallback = bounded Opus (the reason-spike bar).
// The atomic-coding-chain profile additionally inserts DeepSeek-V3.2 as an
// intermediate authoring rung (ATOMIC_CODING_CHAIN.md). A bare invocation runs
// this ladder; explicit -mid-*/-strong-* flags override it and are announced as
// a deviation. NB: Gemma was never benchmarked — the mid seat is Gemini-Flash-Lite.
const (
	canonicalLowModel       = "Qwen2.5-32B-Instruct-Q4_K_M.gguf"
	canonicalMidProvider    = "openrouter"
	canonicalMidModel       = "google/gemini-3.1-flash-lite"
	canonicalStrongProvider = "anthropic"
	canonicalStrongModel    = "claude-opus-4-8"
	canonicalCodingRung     = "deepseek/deepseek-v3.2" // openrouter; atomic-coding-chain's intermediate mid→strong rung
)

// canonicalLadderString renders the locked ladder cheapest→strongest for banners.
func canonicalLadderString() string {
	return fmt.Sprintf("%s → %s → %s", canonicalLowModel, canonicalMidModel, canonicalStrongModel)
}

// ladderOverridden reports whether the operator explicitly passed any ladder flag.
// When false, main applies the canonical ladder; when true, the operator's exact
// choice is honored (and announced as a deviation) — so an override can never pass
// silently the way a stale -strong-model claude-haiku example once did.
func ladderOverridden(fs *flag.FlagSet) bool {
	ladderFlags := map[string]bool{
		"mid-provider": true, "mid-model": true, "mid-model-url": true,
		"strong-provider": true, "strong-model": true, "strong-model-url": true,
	}
	overridden := false
	fs.Visit(func(fl *flag.Flag) {
		if ladderFlags[fl.Name] {
			overridden = true
		}
	})
	return overridden
}

// buildTiers resolves the optional mid and strong rungs around the base adapter.
// An empty provider leaves that rung unconfigured (the ladder collapses around
// it); a build error is fatal. An unavailable rung (e.g. no API key) is not
// fatal — the router degrades around it — but it is announced.
func buildTiers(base model.Adapter, midProvider, midModel, midURL, strongProvider, strongModel, strongURL string, bound int, stderr io.Writer) (tierSet, error) {
	t := tierSet{base: base, bound: bound}
	if midProvider != "" {
		mid, err := buildAdapter(midProvider, midModel, midURL)
		if err != nil {
			return tierSet{}, fmt.Errorf("mid tier: %w", err)
		}
		if !mid.Available() {
			fmt.Fprintf(stderr, "corpos: mid tier %s unavailable (no key?) — the ladder degrades around it\n", mid.Model())
		}
		t.mid = mid
	}
	if strongProvider != "" {
		strong, err := buildAdapter(strongProvider, strongModel, strongURL)
		if err != nil {
			return tierSet{}, fmt.Errorf("strong tier: %w", err)
		}
		if !strong.Available() {
			fmt.Fprintf(stderr, "corpos: strong tier %s unavailable (no key?) — escalations degrade down the ladder\n", strong.Model())
		}
		t.strong = strong
	}
	return t, nil
}

// codingOperatorK is the operator seat's mid-tier attempts per intervention point
// before escalating one rung (mid → coder → strong). Kept small (2) so the strong
// (Opus) rung is reachable within a coding duty's round budget — the bug-1076 fix is
// that the operator escalates instead of stalling on the cheap rung.
const codingOperatorK = 2

// codingGateTimeout bounds each gate command (build/test) the organ's orchestrator
// runs, so a hung verify command can't wedge a coding duty indefinitely.
const codingGateTimeout = 10 * time.Minute

// defaultCodingGate is the build+test gate the coding workers run (mirrors the
// atomic-coding-chain profile's verify_command). It is the command the top-level
// terminal green backstop (bug 1148) runs to answer "is the target repo green?" when
// the spawn-only orchestrator halts — the SAME gate the worker converged against, so the
// orchestrator's green check and the worker's are one definition, not two that can drift.
var defaultCodingGate = []string{"sh", "-c", "go build ./... && go test ./..."}

// buildCodingPath constructs the orchestrator.CodingPath closure that activates the
// gitRepoRoot returns the nearest ancestor of dir that contains a .git entry (the
// git repo root), or "" when dir is empty or no ancestor is a repo. It is how the
// coding path defaults its target so gated coding is on whenever corpos runs inside
// a repo, instead of silently falling back to a bare, ungated worker.
func gitRepoRoot(dir string) string {
	if dir == "" {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached the filesystem root
			return ""
		}
		dir = parent
	}
}

// verifyDirDivergent reports whether an explicit -verify-dir points at a tree that
// is disjoint from the process working directory — the silent-nonconvergence footgun
// (bug corpos-fs-cwd-vs-verify-dir-silent-nonconvergence). The coding worker's fs
// surface resolves relative paths against the process CWD, while the build-test gate
// runs in -verify-dir; when neither tree contains the other, the worker's edits can
// never reach the tree the gate verifies. Nested trees — equal, verify-dir under CWD
// (as the shipped demos do), or CWD under verify-dir — still let the worker's edits
// land where the gate looks, so only genuinely disjoint trees are divergent. Both
// paths are resolved to absolute, symlink-evaluated form first (so /tmp vs its
// symlink target does not read as disjoint); the resolved pair is returned for the
// diagnostic. A path that cannot be resolved is treated as non-divergent (fail open —
// this guard never blocks a run it cannot reason about).
func verifyDirDivergent(cwd, verifyDir string) (divergent bool, resolvedCWD, resolvedVerify string) {
	rc := resolvePath(cwd)
	rv := resolvePath(verifyDir)
	if rc == "" || rv == "" {
		return false, rc, rv
	}
	if pathContains(rc, rv) || pathContains(rv, rc) {
		return false, rc, rv
	}
	return true, rc, rv
}

// resolvePath returns p as an absolute, cleaned, symlink-evaluated path, falling back
// to the absolute-cleaned form when the path does not yet exist (EvalSymlinks fails on
// a missing path), and to "" when even Abs fails.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	if ev, err := filepath.EvalSymlinks(abs); err == nil {
		return ev
	}
	return abs
}

// pathContains reports whether parent equals child or is an ancestor directory of it,
// using filepath.Rel so the check is separator-correct and not fooled by a shared name
// prefix (e.g. /a/b does not contain /a/bc).
func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// codingPathOffReason explains, for the bare-worker fallback warning, WHY the gated
// coding path could not be wired: no git repo at/above CWD, no strong rung, or both.
func codingPathOffReason(targetDir string, haveStrong bool) string {
	switch {
	case targetDir == "" && !haveStrong:
		return "no git repo at CWD and no strong rung"
	case targetDir == "":
		return "no git repo at CWD"
	default:
		return "no strong rung (the organ's escalation top)"
	}
}

// operator-seat organ as the live coding path. Per coding duty it builds a real
// GitRepo over the worker's target dir (branch_fix needs ResetTo + worktree forks —
// a NoopRepo cannot rewind), a coding.Orchestrator whose model worker REUSES the
// passed spawner (the organ's worker IS the same atomic-coding-chain sub-agent the
// bare path spawns), and an OperatorSeat over the SAME canonical ladder (tiers.mid →
// codingRung → tiers.strong). It wraps the duty as a single-AT chain and drives it
// through the bridge, returning the synthesized answer + the operator-decision cost.
func buildCodingPath(spawner *agent.Spawner, tiers tierSet, codingRung model.Adapter, targetDir, worktreeRoot string, perDutyRespawnCap, maxCodingRespawns int) orchestrator.CodingPath {
	// Orchestrate-layer tier carry (bug 1146): the agent orchestrator may re-invoke this
	// coding path for a stuck coding goal, and each invocation builds a FRESH organ — so the
	// within-organ carry cannot bridge the tier a prior invocation's worker escalated to.
	// This run-scoped variable (the closure is built once per run) carries the highest tier
	// any prior coding-path invocation reached into the next one's seed, so a re-spawned
	// coding worker begins there instead of re-climbing from the Qwen floor. Carrying a tier
	// is monotonic and never refuses work. A cross-invocation respawn CAP is deliberately NOT
	// added here — RESOLVED as a documented decision (bug 1151): a precise per-atom cap is not
	// cleanly achievable at this layer because the atom's identity lives only in the orchestrator
	// LLM, which RE-WORDS the duty each respawn, so there is no stable key to count against — duty
	// text is too narrow (it changes every respawn) and targetDir is too coarse (it would refuse a
	// legitimate multi-atom feature that spawns several distinct duties against one repo). The
	// cross-invocation thrash is already bounded WITHOUT a precise cap: every coding-worker spawn —
	// across every re-invocation, since each fresh organ reuses THIS one budget-wired spawner —
	// draws from the shared tree-wide SpawnBudget (-max-spawns, default 16), the shared cost Meter
	// (-max-cost-usd), and the honest per-invocation stuck verdict; and the self-verify trap that
	// DROVE the re-invocations is fixed (3f1714f), so in practice the orchestrator stops after one
	// invocation on a stuck duty (an unsolvable rehearsal stopped at 1 invocation / $0.026). The
	// shared-budget guarantee is pinned by coding.TestSharedSpawnBudgetBoundsRespawnsAcrossInvocations
	// and agent.TestSpawnerRunSpawnBudget.
	//
	// REVISITED (Run-53, 2026-07-13): the SpawnBudget-suffices claim held on COUNT but failed on COST — a
	// stuck bug fanned out into 5–6 coding workers, all climbing to Opus, for $3.65 with no fix, well under
	// -max-spawns 16. A precise per-atom key is still not achievable (the LLM re-words the duty), but a COARSE
	// cross-invocation cap IS: count CONSECUTIVE non-success coding-path invocations and refuse further spawns
	// past maxCodingRespawns with a stuck verdict. A success RESETS the count, so a legitimate multi-atom
	// feature (each atom greens) is never refused — only the consecutive-failure thrash shape is stopped.
	var carriedTier string
	consecutiveCodingFail := 0
	return func(ctx context.Context, duty string, p *profile.JobProfile) (string, float64, error) {
		// Cross-invocation coding-respawn cap (Run-53): once this run has burned maxCodingRespawns
		// CONSECUTIVE non-converging coding invocations, refuse another rather than let the
		// orchestrator fan the stuck goal out into more escalating workers. The returned verdict is
		// terminal — the orchestrate prompt reads it as "stop, report blocked".
		if maxCodingRespawns > 0 && consecutiveCodingFail >= maxCodingRespawns {
			return fmt.Sprintf("coding respawn cap reached: %d consecutive coding attempts on this run failed to converge — do NOT spawn another coding worker for this goal; report it as STUCK/blocked to the principal with the last diagnostic. Re-decomposing the same goal only re-thrashes.", consecutiveCodingFail), 0, nil
		}
		runID := coding.NewRunID()
		repo := coding.NewGitRepo(coding.ExecRunner{}, targetDir, filepath.Join(worktreeRoot, runID))
		orch := coding.New(
			coding.WithRepo(repo),
			coding.WithModelWorker(coding.NewModelWorker(spawner, p)),
			coding.WithGateTimeout(codingGateTimeout),
			// Carry the tier a prior coding-path invocation reached into this fresh organ.
			coding.WithSeededTier(carriedTier),
			// Tier-2 advisory: grade a gate-green as substantiated (changed lines exercised)
			// vs proposed; a proposed green attaches a non-blocking coverage advisory.
			coding.WithCoverageGrade(),
			// Normalize the worker's Go edits (gofmt -w) before the gate so a model's
			// whitespace/indent drift becomes a gofmt-clean deliverable that survives the
			// full gate's gofmt -s stage (bug gofmt-dirty-go-edit-on-exact-match).
			coding.WithGofmtNormalize(),
			// Per-duty respawn cap: stop re-attempting one non-converging atom after a small
			// number of worker→gate loop entries with an honest stuck verdict, rather than
			// thrashing to the cost ceiling (chain 392 task 3315). Counts entries, not revise
			// rounds, so the orchestrator-owned revise budget is never truncated.
			coding.WithPerDutyRespawnCap(perDutyRespawnCap),
		)
		seatOpts := []coding.SeatOption{coding.WithK(codingOperatorK)}
		if codingRung != nil {
			seatOpts = append(seatOpts, coding.WithCoderRung(codingRung))
		}
		seat := coding.NewOperatorSeat(orch, coding.ModelOperator{}, tiers.mid, tiers.strong, seatOpts...)
		chain := coding.BridgeChain(runID, duty, targetDir, p)
		res, err := coding.RunDuty(ctx, orch, seat, chain, runID)
		if err != nil {
			return "", 0, err
		}
		// Carry this invocation's reached tier forward (bug 1146). The organ was seeded at
		// carriedTier, so the reported tier is monotonic (never below the seed) — storing the
		// latest never lowers the carried floor.
		if res.HighestTierModel != "" {
			carriedTier = res.HighestTierModel
		}
		// Track the consecutive-failure streak the respawn cap above bounds: a converged (green)
		// invocation resets it; a non-success one extends it toward the cap.
		if res.Success {
			consecutiveCodingFail = 0
		} else {
			consecutiveCodingFail++
		}
		return res.Answer, res.CostUSD, nil
	}
}

// buildRouterFromTiers builds the top-level router over the resolved ladder. With
// only the base rung it stays single-tier (the prior default). Otherwise it
// enables one-rung-per-turn threshold escalation (climb after escalateAfter tool
// errors, descend after 2 clean turns); the active profile's Tier picks the floor
// rung the loop rests on, and the strong (Opus) rung is usage-bounded.
func buildRouterFromTiers(t tierSet, active *profile.JobProfile, escalateAfter int, escCfg router.Config, strongBudget *cost.StrongTurnBudget, stderr io.Writer) *router.Router {
	tiers := []model.Adapter{t.base}
	midIdx, strongIdx := -1, -1
	if t.mid != nil {
		tiers = append(tiers, t.mid)
		midIdx = len(tiers) - 1
	}
	if t.strong != nil {
		tiers = append(tiers, t.strong)
		strongIdx = len(tiers) - 1
	}
	if len(tiers) == 1 {
		fmt.Fprintf(stderr, "corpos: model ladder — %s (single-tier, no escalation)\n", ladderModels(tiers))
		return router.New(t.base, t.base)
	}
	if escalateAfter < 1 {
		escalateAfter = 1
	}
	floor := laddercfg.FloorForTier(active, midIdx, strongIdx)
	// The full 5-trigger escalation contract (toolkit config), with the legacy
	// -escalate-after flag retained as the operator's knob for the
	// repeated_tool_error threshold; the other four triggers + K come from config.
	opts := []router.Option{router.WithConfig(escCfg.WithRepeatedToolError(escalateAfter))}
	if t.strong != nil && t.bound > 0 {
		opts = append(opts, router.WithBoundedTop(t.bound))
	}
	// Shared tree-wide strong-turn pool (bug 1165(b)): the top-level loop draws Opus turns from the
	// same budget as its spawned workers, so the ceiling is truly whole-tree. Nil is a no-op.
	if t.strong != nil && strongBudget != nil {
		opts = append(opts, router.WithSharedStrongBudget(strongBudget))
	}
	fmt.Fprintf(stderr, "corpos: model ladder — %s; floor=%s; escalation: full contract (repeated_tool_error after %d)%s\n",
		ladderModels(tiers), tiers[floor].Model(), escalateAfter, boundNote(t))
	return router.NewLadder(tiers, floor, opts...)
}

// ladderModels renders the ladder's model ids cheapest→strongest for the banner.
func ladderModels(tiers []model.Adapter) string {
	ids := make([]string, len(tiers))
	for i, a := range tiers {
		ids[i] = a.Model()
	}
	return strings.Join(ids, " → ")
}

// boundNote renders the Opus bound suffix for the ladder banner (empty when the
// strong rung is absent or unbounded).
func boundNote(t tierSet) string {
	if t.strong == nil || t.bound <= 0 {
		return ""
	}
	return fmt.Sprintf("; opus bounded to %d turn(s)", t.bound)
}

// strongURLOr returns the strong-tier endpoint, defaulting to the cheap tier's
// when unset (an OpenAI-compatible strong tier on the same host).
func strongURLOr(strongURL, modelURL string) string {
	if strongURL != "" {
		return strongURL
	}
	return modelURL
}

// printCostSummary writes the session's run-rate to errOut: total USD, the
// per-model breakdown (tokens + USD, flagged free/UNPRICED), and the router's
// escalation-fallback counts — the signal the swap validation period measures.
// No-op when no model call was made (e.g. an immediate-EOF REPL).
func printCostSummary(errOut io.Writer, loop *agent.Loop, rt *router.Router) {
	total, breakdown := loop.Cost()
	if len(breakdown) == 0 {
		return
	}
	fmt.Fprintf(errOut, "corpos: session cost $%.4f\n", total)
	for _, b := range breakdown {
		var tag string
		switch {
		case !b.Priced:
			tag = " [UNPRICED]"
		case b.USD == 0:
			tag = " [free]"
		default:
			// Distinguish a real provider charge from a table estimate so a run-rate
			// built on guesses is never mistaken for one built on real bills (bug 1046).
			tag = fmt.Sprintf(" [priced: %s]", b.PricedFrom)
		}
		cached := ""
		if b.CachedInputTokens > 0 {
			cached = fmt.Sprintf(" (%d cached)", b.CachedInputTokens)
		}
		fmt.Fprintf(errOut, "  %s: %d in%s + %d out tok → $%.4f%s\n", b.Model, b.InputTokens, cached, b.OutputTokens, b.USD, tag)
	}
	if cs, su := rt.ColdStartFallbacks(), rt.StrongUnavailableFallbacks(); cs > 0 || su > 0 {
		fmt.Fprintf(errOut, "  router fallbacks: cheap-unavailable=%d, strong-unavailable=%d\n", cs, su)
	}
	// The Opus bound's effect, alongside the priced Opus line above: how much of
	// the frontier budget the session spent and how many climbs it refused.
	if rt.BoundMax() > 0 {
		fmt.Fprintf(errOut, "  opus bound: %d/%d turn(s) used, %d escalation(s) blocked\n",
			rt.BoundedTurns(), rt.BoundMax(), rt.BoundBlocked())
	}
}

// runInspect loads and prints the sub-orchestration telemetry tree for a root run
// id: each session's profile, per-session cost, turn/tool-call counts, and
// escalation edges, plus the full-tree run-rate. A bad decomposition (a worker
// over-escalated to Opus, or a duty that ballooned cost) is visible at a glance.
func runInspect(out, errOut io.Writer, dir, runID string) int {
	root, err := session.LoadTree(dir, runID)
	if err != nil {
		fmt.Fprintf(errOut, "corpos: inspect: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "session tree %s — %d session(s), tree cost $%.4f\n", runID, root.Size(), root.TreeCostUSD())
	printTreeNode(out, root, 0)
	return 0
}

// printTreeNode renders one session and recurses into its spawned workers.
func printTreeNode(out io.Writer, n *session.TreeNode, depth int) {
	indent := strings.Repeat("  ", depth)
	label := n.Header.RunID
	if n.Header.Profile != "" {
		label = fmt.Sprintf("%s [%s]", n.Header.RunID, n.Header.Profile)
	}
	fmt.Fprintf(out, "%s- %s: $%.4f, %d turn(s), %d tool call(s)\n", indent, label, n.CostUSD, n.Turns, n.ToolCalls)
	if n.Header.Duty != "" {
		fmt.Fprintf(out, "%s    duty: %s\n", indent, truncate(n.Header.Duty, 90))
	}
	for _, e := range n.Escalations {
		fmt.Fprintf(out, "%s    ↑ %s %s→%s (%s)\n", indent, e.Edge, e.FromModel, e.ToModel, e.Trigger)
	}
	for _, c := range n.Children {
		printTreeNode(out, c, depth+1)
	}
}

// truncate shortens s to at most n runes, appending an ellipsis when it cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sessionBundle is the resolved session: an optional store plus the prior
// conversation thread + next turn index when resuming.
type sessionBundle struct {
	store    *session.Store
	history  []model.ChatMessage
	nextTurn int
}

// openSession opens (resume) or creates a session store under dir. A resume
// failure is fatal (the user named a session that must exist); a create failure
// is NOT — persistence is a convenience, so we warn and run in-memory.
func openSession(dir, resume, project, modelID, profileName, selectionJSON string, stderr io.Writer) (sessionBundle, error) {
	if resume != "" {
		st, err := session.Open(dir, resume)
		if err != nil {
			return sessionBundle{}, fmt.Errorf("resume %s: %w", resume, err)
		}
		msgs, err := st.Messages()
		if err != nil {
			_ = st.Close()
			return sessionBundle{}, fmt.Errorf("resume %s: load messages: %w", resume, err)
		}
		history, nextTurn := agent.ResumeState(msgs)
		fmt.Fprintf(stderr, "corpos: resumed session %s (%d prior messages)\n", resume, len(msgs))
		return sessionBundle{store: st, history: history, nextTurn: nextTurn}, nil
	}
	st, err := session.Create(dir, session.Header{Project: project, ModelCheap: modelID, ModelStrong: modelID,
		Profile: profileName, Selection: selectionJSON})
	if err != nil {
		fmt.Fprintf(stderr, "corpos: session persistence off (%v)\n", err)
		return sessionBundle{}, nil
	}
	fmt.Fprintf(stderr, "corpos: session %s (resume with -resume %s)\n", st.RunID(), st.RunID())
	return sessionBundle{store: st}, nil
}

// dirOrDefault returns the configured session dir, or the default under
// $XDG_STATE_HOME (then $HOME/.local/state, then a temp dir).
func dirOrDefault(configured string) string {
	if configured != "" {
		return configured
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "corpos", "sessions")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".local", "state", "corpos", "sessions")
	}
	return filepath.Join(os.TempDir(), "corpos-sessions")
}

// defaultSkillsDir is the on-disk skills tree overlaid on corpos's embedded skills
// baseline: the shared ~/.claude/skills corpus (§3.3 — skills are a data corpus
// corpos loads). The embedded library already ships every discipline the builtin
// profiles name, so corpos injects them with no on-disk tree; this overlay lets a
// populated ~/.claude/skills override per skill. Empty when no home dir resolves.
func defaultSkillsDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".claude", "skills")
	}
	return ""
}

// buildServers constructs one mcp.Server per configured entry, in the config's
// deterministic (sorted) name order. A toolkit-http server enriches its specs from
// the live substrate (the client itself is the introspection transport); a web
// server contributes its static catalog. The type switch is exhaustive over the
// validated config — an unrecognized type is defensive (Config.Validate already
// rejects it). Building (enrichment) is bounded by ctx.
func buildServers(ctx context.Context, cfg mcp.Config, lazySpecs bool) ([]mcp.Server, error) {
	servers := make([]mcp.Server, 0, len(cfg.Servers))
	var toolkitProvider tool.Provider // delegate for the native sys organ's introspection actions
	for _, name := range cfg.Names() {
		sc := cfg.Servers[name]
		switch sc.Type {
		case mcp.ServerTypeToolkitHTTP:
			client := mcp.New(sc.URL, mcp.WithProject(sc.Project))
			toolkitProvider = client
			specs := mcp.EnrichedSpecs(ctx, client)
			if lazySpecs {
				// Lazy tool-spec loading (corpos #3100): offer only the surface envelope
				// (action enum + names), not the full per-action catalog; the model fetches
				// param docs on demand via admin.action_describe. Opt-in — the deep overhead
				// cut, pending live validation that capability holds before defaulting on.
				specs = mcp.LazyEnrichedSpecs(ctx, client)
			}
			servers = append(servers, mcp.Server{Name: name, Provider: client, Specs: specs})
		case mcp.ServerTypeWeb:
			var opts []web.Option
			if sc.SearchURL != "" {
				opts = append(opts, web.WithSearchURL(sc.SearchURL))
			}
			if sc.UserAgent != "" {
				opts = append(opts, web.WithUserAgent(sc.UserAgent))
			}
			wp := web.New(opts...)
			servers = append(servers, mcp.Server{Name: name, Provider: wp, Specs: wp.Specs()})
		default:
			return nil, fmt.Errorf("unsupported server type %q for server %q", sc.Type, name)
		}
	}
	return mountNativeSys(mountNativeFS(servers), toolkitProvider), nil
}

// mountNativeSys makes the host-native sys organ the sole owner of the "sys"
// surface (chain corpos-substrate-topology T5). It strips the toolkit's "sys"
// spec, then mounts the organ with the toolkit provider as its delegate. The
// effect: sys.exec runs IN-PROCESS on the host with a real shell + the host
// toolchain (the deployed toolkit is distroless — no /bin/sh — so its sys.exec
// dead-ends agentic coding; SWAP_VALIDATION §0 exec gate). The read-only
// introspection actions (ps/ports/units/containers) forward to the toolkit
// unchanged, so the surface stays whole. Only sandbox=none is implemented;
// bwrap/podman are rejected (a tracked upgrade).
func mountNativeSys(servers []mcp.Server, delegate tool.Provider) []mcp.Server {
	for i := range servers {
		servers[i].Specs = dropSurfaceSpec(servers[i].Specs, sysorgan.Surface)
	}
	organ := sysorgan.New(delegate)
	return append(servers, mcp.Server{Name: "sys-native", Provider: organ, Specs: organ.Specs()})
}

// mountNativeFS makes the host-native fs organ the sole owner of the "fs"
// surface (chain corpos-substrate-topology T4). It strips any "fs" spec the
// configured (toolkit) servers advertised — so there is no surface collision in
// the aggregator — then appends the organ as its own server. The effect is the
// T4 cutover: corpos's fs read/write/edit/move/remove/ls/grep/glob run IN-PROCESS
// on host paths, sharing one filesystem namespace with corpos's own verify gate,
// instead of hopping through the containerized toolkit (whose mount set silently
// diverged from the host — the run-5 fs-namespace breakage, bug 1031). The
// toolkit's substrate-native fs upgrade modes (provenance/outline reads, write
// event emission) are not carried by the parity organ yet; re-adding them is a
// later opt-in upgrade pass, tracked on the chain.
func mountNativeFS(servers []mcp.Server) []mcp.Server {
	for i := range servers {
		servers[i].Specs = dropSurfaceSpec(servers[i].Specs, fsorgan.Surface)
	}
	organ := fsorgan.New()
	return append(servers, mcp.Server{Name: "fs-native", Provider: organ, Specs: organ.Specs()})
}

// dropSurfaceSpec returns specs without the entry for the named surface.
func dropSurfaceSpec(specs []tool.Spec, surface string) []tool.Spec {
	out := make([]tool.Spec, 0, len(specs))
	for _, s := range specs {
		if s.Name != surface {
			out = append(out, s)
		}
	}
	return out
}

// buildAdapter selects a model adapter by provider name. anthropic uses the
// Messages API + ANTHROPIC_API_KEY; openai uses an OpenAI-compatible endpoint
// (local Qwen by default) + OPENAI_API_KEY; openrouter is the OpenAI-compatible
// OpenRouter gateway (default model Gemini-3.1-Flash-Lite, §4.6) + OPENROUTER_API_KEY,
// which it requires (so a keyless run degrades gracefully). Keys come from the
// environment and are never logged or committed.
// cloudCallTimeout is the per-call HTTP cap for the cloud OAC rungs (Gemini mid, DeepSeek
// coder over OpenRouter). It is deliberately well below the per-turn budget (default -timeout
// 2m) so a STALLED cloud call is abandoned with budget to spare — the loop's timeout recovery
// then shrinks/retries or escalates to another rung instead of one stall ending the turn (the
// orchestrate synthesis-turn timeout). 90s is generous for a legitimate Gemini-Flash-Lite /
// DeepSeek single-turn completion (seconds to tens of seconds) while cutting a genuine stall
// early. The local floor (openai/"") and Opus (anthropic) keep their longer defaults.
const cloudCallTimeout = 90 * time.Second

func buildAdapter(providerName, modelID, modelURL string) (model.Adapter, error) {
	switch providerName {
	case "anthropic":
		id := modelID
		if id == "" {
			id = canonicalStrongModel // locked fallback = Opus, never the bring-up-era Haiku
		}
		return model.NewAnthropic(id, model.WithAnthropicKey(os.Getenv("ANTHROPIC_API_KEY"))), nil
	case "openrouter":
		id := modelID
		if id == "" {
			id = canonicalMidModel
		}
		url := modelURL
		if url == "" {
			url = "https://openrouter.ai/api/v1"
		}
		return model.NewOpenAICompat(id, url,
			model.WithOACKey(os.Getenv("OPENROUTER_API_KEY")), model.WithOACRequireKey(),
			// OpenRouter returns the call's actual cost + cache-token breakdown when
			// asked; the ledger consumes it as the source of truth (bug 1046).
			model.WithOACUsageAccounting(),
			// Cloud rungs (Gemini/DeepSeek) get a per-call cap WELL below the per-turn
			// budget so a STALLED call is abandoned early and the loop's timeout recovery
			// retries/escalates, instead of one stall eating the whole turn (the
			// orchestrate synthesis-turn timeout root cause). The local floor keeps its
			// longer default for a slow large-context first call (bug 1123).
			model.WithOACTimeout(cloudCallTimeout),
			// OpenRouter's GET /models carries no n_ctx, so the live probe yields
			// nothing; give the adapter the model's known static window so a
			// cloud-tier run still sizes compaction (bug
			// corpos-no-context-window-knowledge-for-cloud-tiers). 0 (unrecognized
			// id) leaves the probe to fail soft, handled by the last-resort default.
			model.WithOACWindow(model.KnownContextWindow(id))), nil
	case "openai", "":
		id := modelID
		if id == "" {
			id = "Qwen2.5-32B-Instruct-Q4_K_M.gguf"
		}
		return model.NewOpenAICompat(id, modelURL, model.WithOACKey(os.Getenv("OPENAI_API_KEY"))), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (want openai|anthropic|openrouter)", providerName)
	}
}

// riskGateFor maps the -risk-gate flag to a gate: "off" disables gating (nil — no
// guard registered, gated calls proceed); "build-test" uses the automated
// atomic-coding gate (auto-approves build/test/inspection sys.exec so a coding
// worker can self-verify, denies everything else gated); anything else is the
// fail-closed default (DenyGated), so an unrecognized value fails SAFE toward
// enforcement.
func riskGateFor(mode string) risk.Gate {
	switch mode {
	case "off":
		return nil
	case "build-test":
		return risk.BuildTestGate()
	default:
		return risk.DenyGated()
	}
}

// parseContextProber returns a per-turn context probe that fires
// knowledge.parse_context on the prompt and hands the envelope to the hook
// Metadata (under the agreed key) for the profile pruner/injector to shape. A
// failed call returns nil (the turn proceeds without a context payload).
func parseContextProber(p tool.Provider) agent.ContextProber {
	return func(ctx context.Context, prompt string) map[string]any {
		res := p.Dispatch(ctx, tool.Call{
			Surface: "knowledge",
			Action:  "parse_context",
			// discipline_firing=client: parse_context returns the raw applicable
			// disciplines in candidate_disciplines (and surfaces none inline) so the
			// discipline-fire hook owns the cadence corpos-side (toolkit-decomposition T5).
			Params: map[string]any{"message_text": prompt, "discipline_firing": "client"},
		})
		if !res.OK {
			return nil
		}
		env, ok := res.Value.(map[string]any)
		if !ok {
			return nil
		}
		return map[string]any{profilehooks.MetadataKeyParseContext: env}
	}
}

// printFootprint writes the per-surface and total approximate prompt cost of the
// (projected) tool set to out — the schema-tax measurement (§4.1.1). label names
// the scope ("full" when unprojected).
func printFootprint(out io.Writer, profileName string, specs []tool.Spec) {
	label := profileName
	if label == "" {
		label = "full (unprojected)"
	}
	per, total, enriched := mcp.Footprint(specs)
	fmt.Fprintf(out, "tool footprint — profile: %s — %d surface(s)\n", label, len(per))
	for _, f := range per {
		shape := "enriched"
		if !f.Enriched {
			shape = "THIN" // build-time fallback: under-reports the runtime spec
		}
		fmt.Fprintf(out, "  %-12s desc=%-5d schema=%-5d ~%d tok [%s]\n", f.Name, f.DescriptionBytes, f.SchemaBytes, f.ApproxTokens, shape)
	}
	fmt.Fprintf(out, "  %-12s ~%d tok/turn\n", "TOTAL", total)
	// Honesty guard (bug 1066 secondary): EnrichedSpecs reaches the live substrate at
	// build time; with the substrate unreachable a surface degrades to the thin static
	// spec, whose footprint under-reports the runtime enriched number. Flag that so a
	// window-fit measurement is never silently taken off a thin reading.
	if thin := len(per) - enriched; thin > 0 {
		fmt.Fprintf(out, "  NOTE: %d of %d surface(s) measured the THIN static spec (the substrate was unreachable at build time); the runtime enriched footprint is larger. Re-run with the toolkit-server reachable for an honest gauge.\n", thin, len(per))
	}
}

// printGuardSet writes the post-turn audit guard pipeline to out — the enumerable guard view
// (sibling to printFootprint's tool view). It renders the declarative guard catalog
// (agent.GuardCatalog, the single source of truth for which guards exist), each annotated with
// its stage and whether it is ACTIVE for the active profile, plus the actionable refusal
// message it emits. The activation rule mirrors the wiring: the fabrication-stage audits arm
// for a coding worker (a profile that declares a build/test verify gate — the job is to mutate
// files and not invent contracts), and the fake-green audit arms whenever a verify gate can run.
func printGuardSet(out io.Writer, p *profile.JobProfile) {
	label := "full (unprojected)"
	hasGate := false
	if p != nil {
		label = p.Name
		hasGate = len(p.VerifyCommand) > 0
	}
	fmt.Fprintf(out, "post-turn audit guards — profile: %s\n", label)
	for _, g := range agent.GuardCatalog() {
		active := guardActiveFor(g, hasGate)
		mark := "off"
		if active {
			mark = "ON "
		}
		fmt.Fprintf(out, "  [%s] %-14s %s\n", mark, g.Name(), g.Stage())
		fmt.Fprintf(out, "        %s\n", g.Describe())
	}
	if !hasGate {
		fmt.Fprintf(out, "  NOTE: no verify gate for this profile — the fabrication + fake-green audits arm for a coding worker (a profile with verify_command). A coding worker spawned by the orchestrator runs them per attempt.\n")
	}
}

// guardActiveFor reports whether a catalog guard is armed for the configured profile: the
// fake-green guard arms whenever a gate can run; the fabrication-stage guards arm for a
// coding-shaped profile (one declaring a verify gate).
func guardActiveFor(g agent.Guard, hasGate bool) bool {
	switch g.Stage() {
	case agent.StageFakeGreen:
		return hasGate
	case agent.StageFabrication:
		return hasGate
	default:
		return false
	}
}

// scopeSpecs resolves the active job-profile (if -profile is set) and projects
// the full spec set down to its action-level envelope. With no profile it returns
// the specs unchanged and a nil profile (the unprojected full-surface agent). A
// named profile requires -profiles-dir; an unknown name or an unreadable dir is a
// hard error (the operator asked for a scope that must exist). It announces the
// projection so the operator sees the leanness win the foundation buys.
func scopeSpecs(specs []tool.Spec, name, dir string, stderr io.Writer) (*profile.JobProfile, []tool.Spec, error) {
	if name == "" {
		return nil, specs, nil
	}
	reg, err := loadProfiles(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("load profiles: %w", err)
	}
	p, ok := reg.Get(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown profile %q (have: %s)", name, strings.Join(reg.Names(), ", "))
	}
	projected := mcp.Project(specs, scopeOf(&p))
	fmt.Fprintf(stderr, "corpos: profile %q (tier %s) — projected %d of %d surfaces\n", p.Name, p.Tier, len(projected), len(specs))
	return &p, projected, nil
}

// loadProfiles loads the job-profile registry: an explicit -profiles-dir, else the
// embedded starter library (§4.1).
func loadProfiles(dir string) (*profile.Registry, error) {
	if dir != "" {
		return profile.Load(dir)
	}
	return profile.Builtin()
}

// autoSelectProfile deterministically picks the job-profile for a no-`-profile` run
// (corpos #3096) and returns the chosen name plus the decision serialized as JSON for
// the session header (the labeled dataset, #3098). With no prompt to classify (the
// REPL) it starts on defaultName. Otherwise it scores the registry against the prompt
// + a best-effort parse_context envelope (a down substrate just drops the shape boost,
// never the run) and falls back to defaultName on a no-match or an ambiguous tie. A
// failure to load the registry degrades to defaultName rather than failing the run.
func autoSelectProfile(ctx context.Context, provider tool.Provider, prompt, defaultName, profilesDir string, timeout time.Duration, stderr io.Writer) (string, string) {
	if prompt == "" {
		if defaultName != "" {
			fmt.Fprintf(stderr, "corpos: no -profile and no prompt to classify → safe default %q\n", defaultName)
		}
		return defaultName, ""
	}
	reg, err := loadProfiles(profilesDir)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: profile auto-select unavailable (%v) → default %q\n", err, defaultName)
		return defaultName, ""
	}
	var env map[string]any
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	if md := parseContextProber(provider)(probeCtx, prompt); md != nil {
		env, _ = md[profilehooks.MetadataKeyParseContext].(map[string]any)
	}
	cancel()
	sel := profile.Select(prompt, env, reg, defaultName)
	fmt.Fprintf(stderr, "corpos: %s\n", sel.Reason)
	b, err := json.Marshal(sel)
	if err != nil {
		return sel.Profile, ""
	}
	return sel.Profile, string(b)
}

// scopedSurfacesDegraded reports whether any tool surface this profile scopes is
// present in the raw catalog but fell back to the thin (enum-less) spec — the
// "MCP endpoint was unreachable at spec-build time" signal that explains a
// projected-0 outcome (mcp.Project fail-closes an action-level scope on a thin
// spec, dropping the surface). It is the cheap "unreachable vs legitimately empty"
// discriminator threaded into profile.ToollessAbort for the FATAL message (bug
// 1030). A nil profile has nothing scoped, so it never reports degraded.
func scopedSurfacesDegraded(p *profile.JobProfile, rawSpecs []tool.Spec) bool {
	if p == nil {
		return false
	}
	scoped := make(map[string]bool, len(p.Tools))
	for _, t := range p.Tools {
		scoped[t.Surface] = true
	}
	for _, s := range rawSpecs {
		if scoped[s.Name] && !mcp.IsEnriched(s) {
			return true
		}
	}
	return false
}

// scopeOf builds the action-level projection scope (mcp.Scope) from a profile's
// tool list — the worker's capability allow-list.
// allocateFor resolves the -context-fidelity flag into a budget allocation for a
// window (corpos #3099). "auto" (or any unrecognized value, warned once) derives the
// fidelity from the window; a named preset pins it. Centralizing this keeps the
// compaction budget, skill/inject caps, and per-result cap coming from one place.
func allocateFor(window int, fidelity string, stderr io.Writer) laddercfg.Allocation {
	if fidelity == "" || fidelity == "auto" {
		return laddercfg.Allocate(window)
	}
	f, ok := laddercfg.ParseFidelity(fidelity)
	if !ok {
		fmt.Fprintf(stderr, "corpos: unknown -context-fidelity %q — using auto\n", fidelity)
		return laddercfg.Allocate(window)
	}
	return laddercfg.AllocateAt(window, f)
}

// armNoWorkAudit reports whether the top-level run should arm the no-work audit alongside
// a -verify gate: only a file-mutating profile (it scopes fs write/edit/move/remove) is held
// to having actually mutated, so a no-op done-claim can't ride a vacuously-green gate. An
// unprojected run (nil profile) is left unarmed — no declared coding intent — and read-only
// profiles mutate nothing by design. Called only when a -verify gate is configured.
func armNoWorkAudit(active *profile.JobProfile) bool {
	return active != nil && active.MutatesFiles()
}

func scopeOf(p *profile.JobProfile) mcp.Scope {
	scope := make(mcp.Scope, len(p.Tools))
	for _, t := range p.Tools {
		scope[t.Surface] = t.Actions
	}
	return scope
}

// rescopeLadder resolves a profile's declared RescopeTo names into ordered widening
// rungs (corpos #3097), each carrying the named profile's scope. An unknown name is
// skipped with a warning rather than failing the run — a stale rescope target should
// not ground the agent. A profile with no rescope_to (or one whose targets all fail
// to resolve) yields an empty ladder, leaving the boundary fixed.
func rescopeLadder(active *profile.JobProfile, profilesDir string, stderr io.Writer) []mcp.RescopeRung {
	if active == nil || len(active.RescopeTo) == 0 {
		return nil
	}
	reg, err := loadProfiles(profilesDir)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: rescope ladder unavailable (%v) — boundary fixed\n", err)
		return nil
	}
	ladder := make([]mcp.RescopeRung, 0, len(active.RescopeTo))
	for _, name := range active.RescopeTo {
		p, ok := reg.Get(name)
		if !ok {
			fmt.Fprintf(stderr, "corpos: rescope target %q (declared by %q) not found — skipping\n", name, active.Name)
			continue
		}
		ladder = append(ladder, mcp.RescopeRung{Name: p.Name, Scope: scopeOf(&p)})
	}
	return ladder
}

// profileScopesAgent reports whether a profile may spawn workers — i.e. its scope
// includes the orchestrator's agent surface. Only the orchestrate profile does, so
// only it gets the spawn tool mounted.
func profileScopesAgent(p *profile.JobProfile) bool {
	for _, t := range p.Tools {
		if t.Surface == orchestrator.Surface {
			return true
		}
	}
	return false
}

// profileWorkerHooks builds a spawned worker's hook surface: the risk gate (when
// enforcing) plus the worker profile's skill injection + parse_context pruning —
// the same safety + context seams the top-level loop wires, scoped to the worker.
// skillBudgetTokens caps the worker's injected skill text to its window (see
// profilehooks.SkillInjector). Returns nil when nothing is registered.
func profileWorkerHooks(p *profile.JobProfile, gate risk.Gate, loader *skills.Loader, skillBudgetTokens int) *hooks.Surface {
	surface := hooks.NewSurface()
	used := false
	if gate != nil {
		_ = surface.Register(hooks.PreToolUse, "risk-gate", risk.Guard(gate))
		used = true
	}
	if p != nil {
		_ = surface.Register(hooks.PreUserPrompt, "profile-skills", profilehooks.SkillInjector(loader, skillBudgetTokens))
		_ = surface.Register(hooks.PreUserPrompt, "profile-context-prune", profilehooks.ContextPruner())
		used = true
	}
	if !used {
		return nil
	}
	return surface
}
