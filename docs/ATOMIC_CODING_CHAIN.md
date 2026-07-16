# Atomic coding chain — behavioral spec, characterization net, and concept map

This is the design contract for the **gate-verified atomic coding chain** as a
native corpos organ. corpos subsumes the pattern proven in the Python
`atomic-tasks` research bench (`~/dev/atomic-tasks`): decompose a goal into
ordered atomic tasks (ATs), run each through a worker behind an
orchestrator-owned gate, escalate operator interventions on gate failure, and
integrate green ATs onto a single integration branch.

This document is **language-neutral on purpose** (chain `atomic-tasks-cannibalize`,
task `inventory-and-parity-net`). It captures the *observable behavior* of the
bench so the Go organ implements a contract, not a transliteration. The Python
bench is the source oracle and stays the cheap chain-design bench (it is not
hidden — it is openly the experimental predecessor); the Go organ is the
production owner.

The guiding rule for the port: **compose existing corpos seams, do not fork an
orchestrator.** §4 (the concept map) is the load-bearing section — every
atomic-tasks concept resolves to an existing corpos home (`agent.Spawner`,
`router`, `risk.Gate`, `tool.Provider`, `model`, the toolkit event ledger). New
code is glue and types, not a parallel runtime.

---

## 1. Domain model (the nouns)

### Chain
An ordered list of ATs over a single target git repo. Fields: `slug`,
`target_repo`, `base_branch` (default `main`), `tasks` (≥1 AT). Validation
invariants:
- at least one task;
- task slugs are unique within the chain;
- every AT input reference points at an **earlier** task in the chain (no
  forward or self references).

### AtomicTask (AT)
One atomic, gate-verified unit of work. Fields:
- `slug` — unique within the chain.
- `goal` — natural-language instruction to the worker.
- `inputs` — named references of the form `{from: <upstream-AT-slug>, field:
  <extraction-name>}`. Resolved from upstream ATs' extracted outputs before the
  worker runs.
- `workspace` — glob allowlist of paths the worker may write. Empty allowlist =
  no writes permitted. `**` matches zero or more path segments.
- `gate` — ordered list of commands (each an argv vector). All must exit 0, in
  order, for the AT to pass. The gate is the success oracle.
- `output_contract` — `assertions` (commands that must exit 0 as
  post-conditions) and `extractions` (commands whose stdout becomes a named
  typed output, `format` ∈ {`string`, `json`}). Run against the post-success
  workspace.
- `worker` — discriminated union over worker kinds (see §1/Worker).
- `conventions_ref` — paths whose contents are injected into the worker prompt
  (meaningful only for the LLM worker kind).
- `max_iterations` — write→gate→revise loop ceiling (≥1, default 5).

### Worker (kinds)
A discriminated union; the orchestrator routes an AT to the worker matching its
kind:
- **deterministic** ("code"): runs one fixed shell command, then the gate.
  One-shot, no revision loop. (atomic-tasks: `CodeWorkerConfig`.)
- **model** ("llm"): a model drives a write→gate→revise loop, writing files and
  re-trying until the gate passes or `max_iterations` is exhausted.
  (atomic-tasks: `LlmWorkerConfig`.)
- **pipeline** ("ml"): placeholder; validates but is not runnable. Out of scope
  for the first organ.

The **file-write protocol** is the one deliberate behavioral change in the port
(§3). The bench's model worker offered two brittle protocols — OpenAI-style tool
calls (malformed JSON on code-heavy args at the local tier) and regex-parsed
fenced blocks (`` ```lang file:<path> `` headers; header-drop on small models, a
permissive single-file fallback, raw-response debug dumps). The organ keeps
**only structured tool-call file writes** through `tool.Provider` and drops the
fenced-block parser and its fallbacks entirely.

---

## 2. Run lifecycle (the verbs) — clean-room state machine

### Chain status
`PENDING → RUNNING → {SUCCESS | FAILED | PAUSED | ABORTED}`. Interventions move a
terminal-or-paused chain back to `PENDING`; `resume`/`run` drives it forward
again.

### AT status
`PENDING → RUNNING → {SUCCESS | FAILED | SKIPPED}`.

### `start`
Initialize run state and create the integration branch off `base_branch`'s HEAD.
Materialize one AT record per task in `PENDING`. Persist the run (state +
resolved chain). Emit `chain_start`. Does **not** execute ATs.

### `run` / `run_to_completion`
Drive remaining ATs in order from `current_position`. For each position:
1. If a pause was requested, consume the flag, set `PAUSED`, emit `chain_paused`,
   return. (Pause is honored at AT boundaries; an AT in flight finishes first.)
2. If the AT is already `SUCCESS`/`SKIPPED` (e.g. a superseded branch), advance
   without re-running.
3. Otherwise run the AT (§ "run one AT"). On AT failure: set chain `FAILED`,
   record `failed_at_slug`, emit `chain_complete{status:failed}`, return
   (fail-fast — later ATs do not run). On success: advance `current_position`.
4. When all ATs are resolved: set `SUCCESS`, emit `chain_complete{status:success}`.

An unexpected exception sets `FAILED`, emits `chain_complete{status:exception}`,
and re-raises. State is persisted at every AT boundary so resume is crash-safe.

### run one AT (the inner contract)
1. Mark AT `RUNNING`; emit `at_start{worker_type}`.
2. Resolve `inputs` from upstream ATs' extracted outputs (§ "input
   resolution"). A missing upstream AT or missing field is a hard error.
3. Capture the **fork point**: for an original AT, the integration branch's
   current HEAD; for a branch AT (from a branch-fix intervention), a pre-set
   parent sha so the branch forks from the same upstream state its target saw.
4. Create a fresh worktree off the fork point on a per-AT branch.
5. Run the worker. Record `iterations`, `worker_status`, `diagnostic`.
6. If the worker did **not** succeed: mark AT `FAILED`, emit `at_failed`,
   **preserve the worktree** for operator inspection, return failure.
7. Verify the `output_contract` (assertions then extractions). A violation marks
   the AT `FAILED` (`worker_status = output_contract_violation`), emits
   `at_failed`, returns failure.
8. Store extracted outputs. Commit worktree changes (if any) and fast-forward the
   integration branch to the AT's commit (or to current HEAD if the worker wrote
   nothing). Clean up the worktree. Mark AT `SUCCESS`; emit `at_complete`.

### Worker outcomes
`SUCCESS | GATE_FAILURE | COMMAND_ERROR | MAX_ITERATIONS_EXHAUSTED |
WORKSPACE_VIOLATION`. The gate runs commands in order, stops at the first
non-zero exit, and is **passed** only if every command ran and all exited 0
(an empty gate never passes). On a failed revision iteration the model worker
resets the conversation (prompt stays bounded) but leaves prior files on disk,
and feeds the gate diagnostic back into the next attempt.

### Input resolution (branch-aware)
For each input `{from, field}`, pick the **most recent successful** AT whose slug
== `from` **or** whose `parent_at_slug` == `from`. This lets a branch-fix
supersede the original target's outputs: downstream ATs that referenced the
original by slug transparently read the winning branch's outputs. Resolved values
are passed to the worker (the model worker inlines them into its prompt).

---

## 3. Intervention vocabulary (the operator seat)

Interventions are the operator's moves on a `FAILED`/`PAUSED` chain. **Each
intervention only STAGES** — it mutates run/chain state and sets the chain back
to `PENDING`; nothing re-executes until `resume`/`run`. Every intervention
requires the chain to be `PAUSED`/`FAILED`/`PENDING` and emits an `intervention`
event tagged with `triggered_by` (`claude` | `qwen` | `auto` | `human`).

| Op | Effect | Required args |
|---|---|---|
| `retry` | Reset current AT to `PENDING`, clean its worktree, zero its run fields. | — |
| `skip` | Mark current AT `SKIPPED`, advance past it. | — |
| `edit` | Replace current AT's spec in place (slug must match — in-place revision, not rename), then `retry`. Omitted fields **inherit** from the existing spec (top-level fields, `output_contract.assertions`/`extractions`, and per-key `inputs` merge). | `spec` (or `template`+`target_at`) |
| `inject` | Insert a new AT at a position (default current), re-validate the chain, renumber records. | `spec`, optional `position` |
| `branch_fix` | Insert a branch AT that re-implements a target AT, augmenting its goal with the downstream diagnostic + prior-attempt diffs; supersede the target (and prior failed branches) as `SKIPPED`; rewind the integration branch to the target's fork point; resume from the branch. Caps at `max_branches` (default 3) per target; target must be the failed AT, the immediately-prior AT, or an AT whose own branch chain is failing. | `target_at` |
| `re_extract` | Re-run a target AT's extractions against its commit (ephemeral worktree) or preserved failed worktree; update its outputs. Does **not** re-run the worker. | `target_at` |
| `force_advance` | Operator-authorized gate override: mark current AT `SUCCESS` with a supplied (verified-to-exist) commit sha, advance the integration branch to it, reset the next AT to `PENDING`. Auditable via recorded justification. | `commit_sha`, `justification` |
| `abort` | Mark chain `ABORTED` (worktrees preserved on disk). | — |

**Supersession + fork semantics (branch_fix):** a branch's `parent_sha` is
pre-set to the target's fork point, so successive branches all fork from the same
upstream state regardless of intervening commits; the integration ref is rewound
to that point before the branch runs; the original target and prior branches are
marked `SKIPPED` (audit trail + the run loop steps past them); input resolution's
`parent_at_slug` match makes downstream ATs read the winning branch.

**Operator decision contract (from the validated spike).** On a gate failure the
operator emits exactly one structured decision: `op` ∈ {`edit`, `branch_fix`,
`skip`, `force_advance`, `abort`} with op-specific fields (`target_at`, `goal`
for edit, `commit_sha`+`justification` for force_advance, a one-line `reason`).
Hard rule: **never weaken or rewrite a TEST to match a wrong implementation —
fix the implementation.** Heuristic mapping observed to work: impl doesn't
compile → `branch_fix`; test compile/assertion error with a correct impl →
`edit` with a precise, API-pinned goal.

**Finding 0 — current-package context is mandatory (not optional).** A cheap-tier
operator only works when fed the **current non-test source of the target AT's
package(s)** from the integration branch — the ground truth for "what symbols
exist NOW" — in addition to the diff + gate output. Without it even the strong
tier impasses: the worker regenerates the whole file each attempt and plays
whack-a-mole (references not-yet-built sibling symbols). The operator's `edit`
goal must pin exact signatures/sentinels and forbid referencing symbols that
later ATs will add. This is a corpos context-assembly requirement (task
`wire-operator-seat-to-router-with-package-context`).

---

## 4. Concept map — atomic-tasks → corpos home (the spine of the port)

The port is **composition over the existing corpos runtime**. Each row resolves a
bench concept to where it already lives in corpos; "new" rows are the genuinely
new glue.

| atomic-tasks concept | corpos home | Notes |
|---|---|---|
| model worker (write→gate→revise) | `agent.Spawner.Run(ctx, profile, duty)` with a **coding** `JobProfile` | The worker IS a spawned scoped sub-agent on the local/mid tier. Files are written via `fs` `tool.Provider` calls, not fenced blocks. |
| deterministic ("code") worker | direct gate run via the owned exec surface | No model; run the fixed command then the gate. A thin path, not a spawned loop. |
| fenced-block / OpenAI-tool file-write protocols | `tool.Provider` `fs.write`/`fs.edit` tool calls | **Dropped** the regex parser + permissive fallback + raw-debug dump (§3). Structured tool calls only. |
| gate runner (`run_gate`, ordered, fail-fast, tail capture) | orchestrator-owned gate over the exec surface | Gate is **orchestrator-owned + immutable**; the worker cannot run or mutate it (task `owned-immutable-gate-and-falsepass-guard`). |
| operator seat (classify → compose intervention → escalate after K) | `internal/router` ladder (`NewLadder`, `NextAdapter`, `Observe`) + `escalation` contract | mid default, bounded strong rung; `escalate_on` keyed on the **external gate result**, never the worker's self-report. |
| intermediate authoring rung | `model.OpenAICompat` (DeepSeek-V3.2, OpenAI-compatible) inserted mid→strong | No new adapter shape (task `intermediate-coder-rung`). |
| gate-bypass safety (force_advance is the only sanctioned override) | `internal/risk` `Classify` + `Gate` + `Guard` hook | Worker edits to the gate path are rejected via the risk gate (scope-of-mutation). |
| run state (`state.json`) + telemetry (`events.jsonl`, cursor reader) | toolkit-server append-only event ledger; chain state = **projection** over typed events | No bespoke `state.json`; resume = re-fold, branch_fix = log fork (task `event-ledger-native-state-as-projection`). |
| output_contract assertions/extractions | organ-owned post-success verification over the exec surface | Extractions feed downstream input resolution; behavior preserved. |
| input resolution (branch-aware, parent_at_slug) | organ logic over projected AT records | Preserve the "most recent successful slug-or-parent" rule verbatim. |
| worktree-per-AT + integration branch | organ logic over git via the exec surface | Fork-point capture, fast-forward, rewind-on-branch-fix preserved. |
| forward-parity false-pass guard + SPEC-ambiguity allowlist | organ end-of-run check; allowlist explicit + auditable | Guard keys on reference-tests-vs-final-impl; allowlist suppresses the known defensible `add`-no-args case only (task `owned-immutable-gate-and-falsepass-guard`). |
| pause flag (filesystem) | event-ledger pause signal / control event | Consumed at AT boundaries. |
| heuristic classifier (`at_chain_classify`) | operator-prompt input (advisory) + organ helper | Deterministic triage hint; the router/operator owns the decision. |

corpos **gaps the port must close** (tracked, not blockers): (1) no
owned exec-shell surface yet — `sys.exec` on toolkit-server is the only exec path
and is risk-gated (coding chains run **locally**, not in the deployed container,
sidestepping the `no /bin/sh` container bug); (2) no event-store surface exposed
to corpos yet — task 6 lands it. Until §6, the core port (task 2) may run the
gate through the exec seam directly and keep run state in a minimal local
projection, with the ledger swap isolated to task 6.

---

## 5. Characterization net — behavioral catalog (the gate for the port)

These are the source-derived, observable behaviors the Go organ must reproduce.
They are written as input→expected-outcome cases so they become Go table tests in
the port tasks (they are the parity contract; "do not port yet" — this task only
captures them). Grouped by area; **[core]** = task 2, **[op]** = task 3, **[gate]**
= task 4, **[ledger]** = task 6.

### 5.1 Spec / chain validation [core]
1. Chain with zero tasks → rejected.
2. Duplicate task slugs → rejected.
3. Input referencing a non-earlier task (forward ref) → rejected.
4. Input referencing itself → rejected.
5. `max_iterations < 1` → rejected.
6. `conventions_ref` on a non-model worker → rejected.
7. Valid chain round-trips through serialize/deserialize unchanged.

### 5.2 Gate runner [gate]
8. Empty gate → never passes (`passed=false`).
9. All commands exit 0 → `passed=true`, runs recorded in order.
10. First command non-zero → stop immediately; later commands not run; diagnostic
    names the failing command + exit code + stdout/stderr.
11. Gate stdout/stderr tails are truncated to a byte cap with a truncation marker
    on failure; pass-side events stay payload-free.
12. Command-not-found → exit 127; timeout → exit 124, both surfaced as failures.

### 5.3 Worker loop [core]
13. Deterministic worker: command exits 0 then gate passes → `SUCCESS`,
    `iterations=1`.
14. Deterministic worker: command non-zero → `COMMAND_ERROR` (gate not reached).
15. Model worker: gate passes on iteration N → `SUCCESS`, `iterations=N`.
16. Model worker: gate never passes within `max_iterations` →
    `MAX_ITERATIONS_EXHAUSTED`, diagnostic carries the last gate failure.
17. Write outside the workspace allowlist → refused (surfaced as a tool error to
    the model, not a crash); write escaping the worktree → refused.
18. Revision iteration resets the conversation but leaves prior files on disk and
    feeds the prior gate diagnostic forward.

### 5.4 Orchestrator run [core]
19. All ATs pass → chain `SUCCESS`; integration branch carries one commit per
    file-producing AT in order.
20. An AT fails → chain `FAILED` fail-fast; `failed_at_slug` set; later ATs never
    run; failed AT's worktree preserved.
21. `output_contract` assertion non-zero → AT `FAILED`
    (`worker_status=output_contract_violation`).
22. Extraction with `format=json` and non-JSON stdout → failure; `format=string`
    strips trailing whitespace.
23. Already-`SUCCESS`/`SKIPPED` AT on resume → advanced without re-running.
24. State persisted at each AT boundary; resume continues from
    `current_position` after a simulated crash.

### 5.5 Input resolution [core]
25. Downstream input reads the upstream AT's extracted field by name.
26. Missing upstream success → hard error naming the input + source.
27. Missing field on a found upstream → hard error listing available outputs.
28. Branch-aware: after branch_fix, a downstream input that referenced the
    original slug reads the **branch's** outputs (parent_at_slug match, most
    recent success wins).

### 5.6 Interventions [op]
29. `retry` resets the current AT and re-runs it on resume.
30. `skip` marks `SKIPPED` and advances.
31. `edit` with a partial spec inherits omitted fields (incl.
    `output_contract.extractions` — the dropped-extractions regression must not
    recur); slug mismatch → rejected.
32. `inject` inserts and re-validates; bad position → rejected.
33. `branch_fix`: inserts a `<target>-fixN` branch, supersedes target+prior
    branches as `SKIPPED`, rewinds integration to the fork point, augments the
    goal with diagnostic + prior diffs, resumes from the branch; `max_branches`
    exhausted → rejected; out-of-scope target → rejected.
34. `re_extract` updates outputs from commit or preserved worktree without
    re-running the worker; no extractions declared → no-op.
35. `force_advance` requires non-empty commit_sha (must exist in repo) +
    justification; marks `SUCCESS`, advances integration, resets next AT.
36. `abort` → chain `ABORTED`, worktrees preserved.
37. Every intervention only STAGES (chain → `PENDING`); nothing runs until
    resume; each emits an `intervention` event with `triggered_by`.

### 5.7 Operator seat / escalation [op]
38. On gate failure the operator receives: failed-AT slug, current goal, worker
    status + diagnostic, failing gate tails, **current-package files**, and the
    worker's diff. (Finding 0: package files are mandatory.)
39. After K failed mid-tier attempts on a point, the next attempt escalates to the
    strong rung; escalation keys on the **external gate**, not self-report.
40. The operator never proposes weakening a test to match a wrong impl.
41. Per-point telemetry records tier, op, carried, escalation_cause
    (classification vs authoring), and cost.
42. Reproduces the spike shape on a real target: mid owns diagnosis (classify
    correct) + light authoring; escalation cause is **authoring**, not
    classification; chain reaches real `SUCCESS`.

### 5.8 Coder rung [op, task 5]
43. Router routes operator escalation mid → coder-mid (DeepSeek) → bounded strong.
44. Measured: coder-rung carry rate on the authoring-escalation class and the
    resulting strong-share reduction vs the mid→strong baseline.

### 5.9 Gate integrity + false-pass guard [gate]
45. A worker attempt that edits the gate path is rejected/flagged via the risk
    gate (scope-of-mutation).
46. Forward-parity guard: chain `SUCCESS` but reference tests fail on the final
    impl → flagged false-pass — **except** entries on the explicit
    SPEC-ambiguity allowlist (the defensible `add`-no-args case), which are not
    flagged. Allowlist is auditable, not a silent suppression.

### 5.10 Event ledger / projection [ledger]
47. Coding-chain events are typed events on the toolkit ledger; chain state is a
    projection (no bespoke state file).
48. Resume re-folds state from events; a branched/superseded AT is a log fork.
49. A long chain compacts by folding green segments to a summary, re-expandable
    from the log.

### 5.11 Parity audit [task 7]
50. The Go organ reproduces the Python bench's behavior on the `todo` and
    `calculator` targets (no behavioral regression; operator-seat shape matches
    the spike).

---

## 6. Telemetry event set (the ledger vocabulary)

The bench emits these typed events (schema-validated JSONL); the organ maps them
onto toolkit ledger events (task 6). Event types: `chain_start`,
`chain_complete{status}`, `chain_aborted`, `chain_paused`, `chain_resumed`,
`at_start{worker_type}`, `at_complete{iterations, outputs, commit_sha}`,
`at_failed{worker_status, diagnostic}`, `worker_tool_call{tool_name, tool_args,
result_summary}`, `worker_revision{iteration, reason}`, `gate_check{passed,
exit_code, command, stdout_tail?, stderr_tail?}`, `intervention{operation,
target_position?, target_at_slug?, branch_index?, triggered_by, ...}`. The organ
adds operator-turn telemetry (tier, op, carried, escalation_cause, cost) per §5.7.

---

## 7. Source provenance (non-shipped pointer)

Behavior in this spec is derived by reading the `atomic-tasks` Python research
bench (`~/dev/atomic-tasks`: `src/atomic_tasks/{spec,orchestrator,worker,
code_worker,llm_worker,output_contract,telemetry,path_match,api}.py` and
`scripts/operator_spike.py`) plus its run findings and the vault decisions
`mid-tier-carries-the-atomic-tasks-operator-seat`,
`corpos-local-model-mechanical-capability-floor`, and
`corpos-3-tier-model-routing-decision`. The bench remains the chain-design
research bench (task `parity-audit-and-retire-python-bench`); this organ is the
production owner.
