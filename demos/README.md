# Demos

Three escalating scenarios, each run against the **real** corpos with the full
worker → mid → strong ladder, with the **actual captured transcript committed**
next to it. Nothing here is idealized or hand-edited — you're reading what corpos
really did.

| # | Demo | Shows |
|---|------|-------|
| 1 | [bug-fix](./01-bugfix/) | red→green on a one-line bug; the risk gate and the anti-fake-green refusal |
| 2 | [calculator](./02-calculator/) | building a small package + test from a prose goal |
| 3 | [todo](./03-todo/) | a larger multi-file feature (backend + a small frontend split) |

## The headline — the local floor works, for free

The newest capture in demo 1 is the whole thesis in one data point: the local
**Qwen2.5-32B** worker takes the failing bug to **clean green on its own** —
single-tier, no escalation to a paid rung — for **$0.00**.

| Same task, same clean-green result | Model doing the work | Cost/task |
| ---------------------------------- | -------------------------------- | ----------: |
| local floor                        | Qwen2.5-32B (local llama-server) | **$0.0000** |
| flat baseline                      | claude-opus-4-8                  |     $0.5125 |

Full capture: [`01-bugfix/transcript-qwen-floor.txt`](./01-bugfix/transcript-qwen-floor.txt).
The cheapest capable rung did the mechanical fix for free; the identical task on
the frontier model costs $0.51. That delta, summed over a mechanical workload, is
the point of the ladder.

## Honest framing — read this first

These transcripts are **real, and they show the seams.** corpos's own
[README](../README.md#status) names its current frontier plainly: *"convergence
reliability and cost discipline at the cheap tiers."* The demos run straight into
that frontier, and the transcripts are gap-aware on purpose:

- **The machinery works.** The provider-agnostic loop, the tiered ladder,
  per-call cost accounting, the risk gate, and the verification integrity all do
  their job. In demo 1 you can watch corpos **refuse to declare "done"** when it
  can't run the test to verify — it halts honestly rather than claiming a green it
  didn't observe. That refusal is the point.
- **Wider cheap-tier reliability is the active frontier.** The local Qwen floor
  carries a simple mechanical fix cleanly (the headline run above), but the cheap
  *hosted* floor (`gemini-3.1-flash-lite`) sometimes fumbles corpos's edit-tool
  format, which trips escalation and can leave a run halting on the frontier-usage
  bound — even on a trivial fix, and even when the edit actually landed and the
  test actually passes. Those are tracked, known gaps in the ledger (edit-prefix
  recovery, respawn escalation-state discard, and a false-negative terminal
  verdict when the repo is already green), not surprises.

This is a **home project**, and the demos are here to be *honest and legible*, not
to fake a flawless product. What a reviewer should take away: the architecture and
the verification discipline are real; the last mile — driving a wide range of
fixes to clean green on the cheap floor without escalation thrash — is active work.

## Reproducing these

Each demo directory has the exact scenario (a small git-tracked scaffold) and a
`run.sh` with the invocation. You need a running `toolkit-server`
([setup](../docs/SETUP.md)), a worker endpoint, and keys for the mid/strong rungs
([tiers](../docs/TIERS.md)). The runs here used:

```
corpos \
  -provider openai -model-url http://localhost:8081/v1 -model <local-worker> \
  -mid-provider openrouter -mid-model google/gemini-3.1-flash-lite \
  -strong-provider anthropic -strong-model claude-opus-4-8 -strong-bound <n> \
  -mcp-url http://localhost:3000 -project <scope> -verify-dir <repo> \
  -risk-gate build-test -prompt "<the task>"
```

`-risk-gate build-test` is what lets the worker run `go test` to verify itself
while still gating destructive/outward actions — without it, corpos correctly
refuses to run the test at all (demo 1 shows both). Each transcript notes its own
config and the real dollar cost printed by corpos at the end (these runs were
cents).
