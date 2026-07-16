# Context compaction — design (chain `corpos-context-compaction`, task `design-compaction`)

> Status: **design**, feeds `implement-and-validate-compaction` (task 3001).
> Scope: corpos prunes/summarizes the agent loop's transcript against a token budget when a
> session grows large, **preserving task continuity** (no silent loss of the active goal).

## 1. The problem

The kernel keeps one growing conversation in `Loop.transcript []model.ChatMessage`
(`internal/agent/loop.go`). Every turn appends the user message, one assistant message per
tool-round (carrying `ToolCalls`), and one `RoleTool` result message per dispatched call. Nothing
ever leaves. A long decomposed session (the sub-orchestration workloads corpos is built for)
therefore grows the transcript without bound until the model call's input exceeds the context
window — at which point the provider rejects the turn and the session dies mid-goal.

Claude Code gives compaction "for free" (design-orientation doc, layer 5: *context management —
compaction, window assembly, memory injection*). Owning the loop means owning this. This chain
builds it.

## 2. What we can lean on (and what we can't)

- **A real budget signal already exists, no tokenizer required.** corpos is CGo-free and ships no
  tokenizer. We do **not** need one: each model call returns `resp.Usage.InputTokens`, the
  provider's own count of the *entire current transcript* fed to that call. After any turn, the
  last observed `InputTokens` is an authoritative measurement of the live context size. For the
  un-measured tail (messages appended since the last model call) we estimate with a cheap
  `ceil(len(content)/4)` heuristic. Measured-where-known, estimated-only-on-the-tail keeps us
  honest (mirrors the cost ledger's "never invent cost — stored is authoritative" posture).

- **Hard pairing constraint.** `ResumeState` (loop.go:441) already documents it: a `RoleTool`
  result replayed *without* its originating `RoleAssistant` tool_use block makes the Anthropic
  Messages API return 400. Compaction therefore **must cut only on turn boundaries** and must
  never separate a tool_use from its tool_result. The unit of eviction is a *turn group*, never a
  bare message.

- **Sans-IO / DI house style.** The compactor must be testable with no network: summary
  generation goes through an injected `model.Adapter` (the `EchoAdapter` scripts it in tests), and
  the trigger logic is pure arithmetic over the transcript + a measured size. No package-level
  state.

## 3. Strategy decision: **hybrid** (rolling summary + recency window + pinned anchors)

The three candidates and why hybrid wins:

| Strategy | What it does | Why not alone |
|---|---|---|
| **Token-budget pruning** | Drop oldest turn-groups until under budget. | Cheap and deterministic, but *silently loses the active goal* and any mid-session decisions — violates the continuity constraint outright. |
| **Rolling summary** | Replace older turns with an LLM summary. | Preserves gist, but a summary of *everything* loses the verbatim recent context the model needs to act on the current step, and re-summarizing the whole transcript each event is expensive. |
| **Hybrid (chosen)** | Pin anchors, keep a verbatim recency window, summarize only the evicted middle into a rolling note. | Bounds tokens *and* preserves continuity. The recency window keeps the model's working set verbatim; the rolling summary preserves the arc of the middle; the pins guarantee the goal never drops. |

**Chosen: hybrid.** This is a reversible, documented design call (the user delegated "start in on
the chain"); it can be revisited if validation shows pruning-only suffices. Hybrid is the only
option that satisfies *both* halves of the completion condition — "stays within budget" **and**
"without losing continuity."

### 3.1 Transcript regions after a compaction event

```
[ PINNED  ] system prompt(s)            ← never evicted (region head, stays first)
[ PINNED  ] anchor: first user prompt   ← the session's task statement = the active goal
[ SUMMARY ] one RoleSystem msg:         ← rolling, regenerated each event, absorbs the
            "## Compacted context …"       evicted middle turn-groups
[ RECENT  ] last K turn-groups verbatim ← the working set, kept intact w/ tool pairs
[ <live>  ] the in-flight turn          ← never touched mid-turn
```

## 4. The preservation contract (what survives a compaction event)

A compaction event MUST preserve, in order:

1. **All `RoleSystem` messages** present at the head of the transcript (the seeded system prompt
   and any earlier compaction summary). They stay first and are never evicted.
2. **The active-goal anchor** — the *first* `RoleUser` message of the session. This is the task
   statement; dropping it is the "silent loss of the active goal" the constraint forbids. Pinned
   verbatim for the session's life.
3. **A rolling summary** of every turn-group evicted so far, rendered as a single `RoleSystem`
   message tagged so it is recognizable and *replaceable* on the next event (the new summary
   subsumes the prior one — summaries never stack unbounded).
4. **The recency window** — the last `K` complete turn-groups, verbatim, with every
   tool_use/tool_result pair intact. `K` is configured so the window alone stays comfortably under
   budget.
5. **Turn-group integrity** — no event ever splits an assistant tool_use from its tool_result, and
   no event fires *during* an in-flight turn. Eviction operates on whole turn-groups at turn
   boundaries only.

Continuity invariant, stated once: **after any compaction event the model can still answer "what
is the goal, what was decided, and what just happened"** — (2) answers the goal, (3) answers what
was decided, (4) answers what just happened.

## 5. Trigger & mechanism

- **Budget.** A configured `maxContextTokens` (default derived from the active model's window, e.g.
  a fraction like 75% as the high-water mark) plus a `recencyTurns` (K) knob. Off by default unless
  configured, like the other loop options (`WithCompaction(budget, K)` functional option).
- **When it fires.** At a **turn boundary** — checked at the top of `Run` before the new user
  message is appended (and optionally re-checked before a model call inside the round loop if a
  single turn's tool spray blows the budget). Firing at the boundary guarantees the pairing
  constraint for free.
- **Decision.** `currentSize = lastMeasuredInputTokens + estimate(tail since last measure)`. If
  `currentSize <= budget`, no-op. Else compact: collect the turn-groups *between* the pinned head
  and the recency window, summarize them via the injected adapter into the rolling-summary message,
  and rebuild the transcript as `[pinned][anchor][summary][recency]`.
- **Summary generation.** A dedicated, cheap-tier model call (the local Qwen tier by default — free
  in the cost ledger) over the evicted span with a fixed instruction: *"Summarize the following
  agent transcript span, preserving decisions made, facts established, files/identifiers touched,
  and any open threads. Be terse and factual."* The prior rolling summary is fed in as prefix so
  the new summary is cumulative.
- **Telemetry.** Each event records a row (turn index, tokens before/after, groups evicted,
  summary tokens) via the session store, best-effort like all loop telemetry — so the validation
  test and `-inspect` can show compaction actually happened and bounded the size.

## 6. Validation plan (task 3001 acceptance)

Sans-IO test with `EchoAdapter`: script a long multi-turn session (well past the budget), assert
that (a) measured/estimated context size stays `<= budget` across the run, (b) the first user
prompt (goal anchor) is present in the transcript at every turn, (c) the rolling summary message
exists and does not stack (exactly one summary region), (d) no tool_result appears without its
tool_use after any event, and (e) a fact established before the first compaction is still
retrievable (via summary) after it. A live smoke (skip-unless-live) runs a real long Qwen session
and confirms the turn no longer 400s on context overflow.

## 7. Surfaces to touch (for task 3001)

- `internal/agent/` — new `compaction.go`: the `Compactor` (pure trigger logic + region rebuild) +
  `WithCompaction` option; wire the boundary check into `Run`.
- `internal/agent/loop.go` — track `lastInputTokens` off `resp.Usage`; call the compactor at the
  turn boundary.
- `internal/session/store.go` — optional `compaction` telemetry row (best-effort).
- Config/CLI — a `-max-context-tokens` / `-recency-turns` knob if exposed to operators.
