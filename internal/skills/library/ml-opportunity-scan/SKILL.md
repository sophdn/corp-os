---
name: ml-opportunity-scan
description: "Triage discipline for identifying trained-model candidates. Codifies the agent-first triage rubric (token savings, determinism, round-trips eliminated, per-call frequency, data readiness), the substrate-signals checklist (which projections / logs / call-sites to grep), and the output structure (numbered build-sequence files in ~/Documents/files/ideas-to-process/ml-temp/ + incubation in General Ideas/). Run after a new substrate ships, after a chain's design names a future trained model, or when the user explicitly asks for an ML scan. Cross-project: produces candidate files that any project's ml-training pipeline can consume."
triggers:
  # User-directive phrases — the user explicitly asks for a scan.
  - ml opportunity scan
  - ml opportunity
  - scan for ml
  - run an ml scan
  - what could we train
  - what should we train
  - trained model candidates
  - ml candidates
  - new ml chain
  - new trained model
  - ml roadmap
  - find ml work
  # Agent-initiated phrases — the agent is about to surface a candidate
  # and wants the discipline applied before filing.
  - could be a trained model
  - this is a trained model
  - this could be ml
  - ml candidate
  - want to train a model
  - might want to train
  - burns context every time
  - burn tokens reasoning
  - every call is a reasoning step
  - deterministic alternative
  - agent reasons about
  # Substrate-stamp phrases — a new substrate just landed and this is
  # the canonical "what does this unblock" follow-on.
  - substrate just landed
  - substrate closed
  - new training-data shape
  - new projection that could feed ml
---

# ml-opportunity-scan

## When to run

Three trigger conditions — any one fires the discipline:

1. **A new substrate ships.** Each closing substrate (`agent-first`, `query-telemetry`, `reference-resolution`, `ml-capability`, future) unlocks new training-data or new tool-surface candidates. The agent's job at close-time is to scan for what just became plannable.
2. **A chain's design names a future trained model.** Look for "future ML capability chain (not yet forged)" or "trained classifier swaps in via T7" — those are forward-dependencies the scan should harvest into proper candidate files.
3. **The user explicitly asks.** Phrases listed in `triggers:` above; "what could we train" / "find ml work" / "ml roadmap scan."

The scan is NOT a per-session reflex. It's a discrete artifact-producing pass — start it, finish it, ship the candidate files, then stop.

## The agent-first triage rubric

Five axes, each scored 1-5. A candidate scoring ≥18 / 25 is build-worthy; 12-17 is incubate; <12 is reject (or revisit when the data shape changes).

| Axis | What it measures | 5-shape | 1-shape |
|---|---|---|---|
| **Token savings** | How many agent tokens does the trained model replace per call? | Per call: ≥200 tokens of in-LLM reasoning / scoring / classification become a sub-50ms ONNX call. | Per call: <30 tokens saved; barely worth the plumbing. |
| **Determinism** | How much does run-to-run drift hurt today? | Today's path is stochastic (Qwen rubric, in-LLM scoring) and the drift causes real workflow churn (cache invalidation, agent re-runs, inconsistent results). | Today's path is already deterministic; trained model is a quality bump only. |
| **Round-trips eliminated** | How many context-eating LLM round-trips does this collapse into one ONNX call? | ≥3 round-trips today (e.g. "should I search vault?" → search → "filter results" → "re-rank" becomes one route_query call + reranker). | ≤1 round-trip; the trained model is just faster, not architecturally different. |
| **Per-call frequency** | How often does the path fire per agent session? | Every retrieval / every forge / every prompt — frequency dominates the substrate amortization math. | Once a week / per-incident only — substrate cost won't amortize. |
| **Data readiness** | How close is the training corpus? | Projection already shipped and populated (e.g. `proj_training_data_for_reranker`); cold-start volume already past usable. | No labeled corpus; a capture pass needs to land before training is plannable. |

The scoring is honest, not aspirational. A "5" on Token savings requires *naming the specific in-LLM reasoning step* the model replaces, with a token count. A "5" on Data readiness requires *naming the projection and its current row count*.

## The substrate-signals checklist

When the scan fires, walk these surfaces in order. The candidates surface from concrete signals, not speculation:

### 1. Recent substrate closures

Read the closing audit event of every substrate closed in the last 90 days. Look for:
- "Forward dependency on a future ML capability chain" → the chain WAS the dependency; harvest the named model.
- "T7 cancelled-as-deferred into ml-capability-substrate" → same.
- "When the ML capability chain lands" → same.

### 2. Open chains with `bug` or `chain` text mentioning "trained model"

```
mcp__toolkit-server__work(action='chain_find', pattern='trained')
mcp__toolkit-server__work(action='bug_list', surface='ml,trained-model,classifier')
```

A chain that names a future trained model in its `design_decisions` is a candidate already-named; the scan's job is to write the file under `ml-temp/` and link the chain.

### 3. Per-call high-frequency Qwen rubric paths

The `go/internal/measure/` rubric registry is the canonical "we use an LLM to score things" surface. Every rubric there is a trained-model candidate when its corpus is rich enough.

```
ls ~/dev/mcp-servers/blueprints/rubrics/         # one toml per rubric
ls ~/dev/mcp-servers/go/internal/measure/        # rubric handlers
grep -rn "rubric_classify\|Qwen.*rubric" ~/dev/mcp-servers/ | head -20
```

Each rubric → candidate file with rubric name in slug.

### 4. New projections in toolkit.db

After a telemetry-substrate-like chain closes, new `proj_*` views often unlock training corpora. Walk `crates/shared-db/migrations/` for `CREATE VIEW proj_*` or check the projections package.

A new projection that names a feature column + label column → candidate file naming the model that consumes it.

### 5. Friction reports in bugs

```
mcp__toolkit-server__work(action='bug_list', surface='agent-behavior,token-cost,reasoning-overhead')
```

Bugs filed with token-cost / agent-flow-interruption / reasoning-overhead surface tags often name patterns a trained model would resolve. Cross-reference against the rubric.

### 6. The user's auto-memory and feedback

```
ls ~/.claude/projects/-home-sophi/memory/feedback_*.md
```

Memory entries describing manual reflex-prompts, per-session rituals, or recurring agent friction — each names a workflow shape that may be a trained-model candidate (the `feedback_manual_reflex_prompts.md` motivates the skill-auto-loader candidate, per the existing ml-temp/05 file).

## Output structure

Candidate files live in **`~/Documents/files/ideas-to-process/ml-temp/`**. One file per candidate. Filename convention: `NN-<task-kebab>.md` where NN is the build-sequence position the scan recommends (01, 02, …). Existing candidates as of 2026-05-19:

```
01-source-router.md
02-curation-classifier.md
03-cross-encoder-reranker.md
04-bug-surface-tagger.md
05-skill-auto-loader.md
```

A scan that adds a sixth candidate creates `06-<slug>.md`. The numbering is descriptive (sequence the scan recommends), not prescriptive (chains can be forged in any order once `ml-capability-substrate` closed).

Per-file body — copy this skeleton:

```markdown
# <Task name in sentence case>

**Build-sequence position:** #<N>  (<scan-date>)
**Status:** Idea — not in chain DB yet

## Core idea
<one paragraph naming the WHAT: input → output, what existing path it replaces>

## Why this position in build order
- <bullet on data readiness>
- <bullet on token savings>
- <bullet on dependencies on prior candidates>

## Agent-first value-prop
- **Replaces** <named in-LLM step>
- **Eliminates** <named round-trip / context burn>
- **Improves determinism** — <how>
- **Composes with** <other candidates / chains>

## Triage rubric scores

| Axis | Score (1-5) | Reason |
|---|---|---|
| Token savings | <N> | <named step, token count> |
| Determinism | <N> | <how today's path drifts> |
| Round-trips eliminated | <N> | <named round-trips> |
| Per-call frequency | <N> | <calls/session estimate> |
| Data readiness | <N> | <projection name + row count> |

**Total:** <sum>/25 — <build / incubate / reject>

## Training data shape
<projection / table name + columns + label semantics>

## Model shape
- **v1:** <minimal-viable architecture>
- **v2 (only if v1 plateaus):** <next-tier architecture>

## Stack fit
- **Tool surface:** <new MCP action OR transparent swap OR forge integration>
- **Failure mode:** <fail-open behavior>

## Open questions
- <bullet> ...

## Related
- <pointers to existing chains, vault notes, ml-temp candidates>
```

**Incubation overflow:** if a candidate scores 12-17 (incubate), file it under `~/Documents/files/ideas-to-process/General Ideas/ml-incubation-<slug>.md` instead of ml-temp/. These are revisited in the next scan after relevant substrate work lands.

## Cross-references

- `~/dev/mcp-servers/docs/ML_CAPABILITY_SUBSTRATE.md` — the substrate that makes these candidates forgeable
- `~/.claude/vault/learnings/general/2026-05-15_ml-capability-vs-models-framing.md` — the framing trap this skill prevents (per-model chains instead of capability-once)
- `~/dev/ml-training/` — where the per-task training scripts live; each candidate's chain ships a `training/<task>/train.py`
- `~/dev/mcp-servers/go/internal/ml/` — the serving subsystem each candidate's trained model loads through

## When the scan finishes

Report a one-paragraph summary naming:
1. How many candidates were filed (new files under ml-temp/).
2. How many incubated (filed under General Ideas/).
3. How many existing candidates the scan re-scored (with any score changes).
4. The single top-recommended next chain to forge (by build-sequence position + score).

Then stop. The agent doesn't forge chains from the scan — those are the user's decision to make, informed by the candidate files. The scan's role is exclusively "produce well-shaped candidate files."
