# Architecture

Corp-OS is built on one commitment: **own the loop, rent only the model.** Everything an agent
runtime needs — tool dispatch, model selection, cost accounting, verification, sub-orchestration —
is code in this repo, structured so the model is the *only* replaceable, rented part. This
document walks the design from the kernel outward.

For the capability catalog and package map, see the [README](./README.md). This doc is the *why*
and the *how it fits together*.

---

## 1. The kernel: a provider-agnostic loop

The agent loop (`internal/agent`) is the kernel. It drives a model through **bounded tool rounds**:
offer the model the current transcript plus the available tools, receive a response, dispatch any
tool calls, append results, repeat until the turn is final. It fires the lifecycle hooks and
accumulates cost along the way. It is bespoke Go — no agent SDK.

Two seams make the loop indifferent to what it's driving:

- **`tool.Provider`** (`internal/tool`) — the loop dispatches *every* tool call through this
  interface. The backing implementation is swappable without touching the loop: today a
  `toolkit-server` MCP client (`internal/mcp`), native in-process organs (`fs`/`sys`/`web`), or an
  external MCP server. The loop never knows the difference.
- **`model.Adapter`** (`internal/model`) — an adapter turns *a transcript plus the offered tools*
  into *one response*. Concrete adapters: `Echo` (tests), `OpenAICompat` (local Qwen / DeepSeek),
  `Anthropic`. Tool requests cross the seam as the shared `tool.Call` type, so the loop dispatches
  them with **no translation**.

Because both seams are pure interfaces, the same loop runs a local GPU model against native organs
*and* Anthropic against a remote MCP substrate — the difference is which adapter and which provider
get wired at the composition root (`cmd/corpos`).

Session state (`internal/session`) is one SQLite DB per run (`<dir>/<run_id>.db`), single-writer,
schema-versioned. It holds the *local* conversation, telemetry, and cost — deliberately **not** the
substrate's event ledger, which stays remote over MCP. The two are never conflated. `-resume`
reopens a session DB; `-inspect` reads it back.

```
             ┌────────────────────────── agent loop (kernel) ──────────────────────────┐
 transcript  │  router.NextAdapter → model.Adapter.Respond → dispatch tool.Calls → …    │
 ──────────► │        ▲                                             │                    │
             │        │ Observe(edge)                               ▼                    │
             │   escalation/telemetry                        tool.Provider               │
             └──────────────────────────────────────────────────┬──────────────────────┘
                                                                 │
                        ┌────────────────────────────────────────┼───────────────┐
                        ▼                     ▼                    ▼               ▼
                  fs/sys/web organs     mcp client (MCP)     agent.spawn      web.search…
```

---

## 2. The model layer and the router

The **router** (`internal/router`) selects a model adapter per turn via a **two-call contract**:
`NextAdapter` at the top of a turn, `Observe` after it. It is an ordered **ladder** of tiers,
cheapest → strongest. A worker rests on its *floor* rung and climbs **one rung per escalation
edge**, descending back toward the floor on clean turns (de-escalation hysteresis). The
frontier rung (e.g. Opus) is **usage-bounded** (`WithBoundedTop`) so it is escalation-only, never
reachable per-turn by default — the expensive model is a fallback, not a habit.

Escalation triggers are a **closed 5-trigger taxonomy** with a two-state cheap/escalated machine,
generalised onto the tier ladder: "cheap" is the floor, "escalated" is any rung above it. The
router stays **pure (sans-IO)** — `Observe` returns an `Edge` describing any transition, and
*emitting* the `EscalationProposed` event / telemetry row is the caller's job. This keeps model
selection unit-testable in isolation.

Three of the five triggers are driven in the shipped loop (`retry_exhaustion`,
`repeated_tool_error`, `parse_failure`). The other two — `low_confidence` and `explicit_handoff` —
are **wired but not yet driven**: the router evaluates them and the signal fields exist
(`internal/hooks` `Context.EscalationConfidence` / `RequestEscalation`), but no in-tree hook
populates them yet, so they never fire until a scorer / sentinel hook lights them up. The seams are
deliberate, not oversights.

---

## 3. Cost-via-decomposition

The economic thesis: **route each duty to the cheapest model capable of it.** Three packages
implement it, and getting any of them wrong inverts the economics — over-escalate and the savings
evaporate; under-scope and the worker fails, forcing rework that costs *more* than routing right.

- **`profile`** — the **job-profile**, Corp-OS's capability-scoping unit: a duty-shaped envelope of
  `{minimal tools, governing skills, lean context-shapes, cheapest capable model tier}`. Because
  Corp-OS owns the loop, it *projects exactly this envelope* into each (sub)agent instead of
  mounting every MCP surface flat. That inversion is what makes a rich owned-surface ecosystem
  survivable: a worker simply cannot invoke a surface or action its profile didn't expose. Even the
  top-level agent runs under a profile (`orchestrate`).

- **`routing`** — turns a duty into the cheapest-capable profile. The input is a Qwen-local
  classifier on the substrate's `measure` surface, reached through a `Classifier` seam (so the
  package stays sans-IO); its label maps to a profile via a table, falling back to a cheap general
  profile when unmapped.

- **`orchestrator`** — the `agent.spawn` primitive. The `orchestrate` agent decomposes a goal into
  duties and delegates each with `agent.spawn(profile, duty)`, which runs a **scoped child loop**
  under the named profile and returns its answer. Decomposition is *emergent* (the orchestrator
  spawns the duties it decides on); reconciliation is its own synthesis turn over the workers'
  answers. A leaf duty gets one surface and a cheap model — not the full surface set and the
  frontier. `agent.spawn` is itself just a `tool.Provider`, mounted only for profiles whose scope
  includes the `agent` surface.

`cost`, `runrate`, and `escalation` close the loop: per-call pricing (preferring the
provider-reported charge when present), a projected monthly run-rate from persisted telemetry, and
the client half of the escalation contract.

---

## 4. Two orthogonal axes: capability vs. risk

A recurring design principle, settled explicitly in `profile` and `risk`:

- **Capability (scope)** — *which* tools exist for a worker. Set by the job-profile's projected
  tool scope, which **is** the allow-list.
- **Risk** — *whether* a specific destructive or outward-facing invocation should proceed (a
  removal, a `git push`, an edit on a protected path, an external send). Handled by the **risk gate**
  (`internal/risk`), with shell-shape deny rules in `shellsafe` and path matching in `pathglob`.

The axes are independent by design: a profile being *tool-capable* never implies an invocation is
*risk-approved*. You can hand a worker a broad toolset and still block the dangerous call. The gate
runs in `enforce` mode by default; `-risk-gate off` is for trusted, supervised runs.

---

## 5. Gate-enforced autonomous coding

This is the headline capability, and it exists because a model **cannot be trusted to self-report**
that it fixed something. Corp-OS's answer is to make "done" an *observable, orchestrator-computed*
fact, not a claim.

**The hard gate (Tier 1).** A coding worker is done only when an acceptance test goes **red →
green** — `go build && go test` exits 0. For a *bug fix* the oracle pre-exists. For a *feature*, the
test must be **authored first** (see the gate-authoring bridge below).

**Anti-fake-green (all orchestrator-owned, all computed from the diff):**

| Guard | Catches |
|---|---|
| Protected paths (`**/*_test.go`) | The worker editing the oracle to make it pass |
| Red-before-green | A tautological test that passes on the *pre-fix* tree |
| Test-only diff | "Green" achieved by touching only test files |
| Independent verifier | Non-PASS / no-evidence verdicts |
| Fake-green / scaffold-fab | The worker authoring the certifying test or a build scaffold |

**The quality report (Tier 2)** — the [two-tier green design](./docs/TWO_TIER_GREEN_DESIGN.md).
Once Tier 1 passes, Corp-OS runs a *scoped* coverage pass over the worker's diff and grades the
green: **confirmed** (every changed production line is exercised) or **proposed** (some changed line
is uncovered). This is **advisory — it never hard-fails**; it colors the green and feeds back to the
worker as a non-blocking note. Tier 1 remains the only law.

The invariant across all of it: **verdicts are computed by the loop from the diff, never narrated by
the worker.**

**The feature pipeline** — the [gate-authoring bridge](./docs/GATE_AUTHORING_BRIDGE.md) turns a
chain of prose tasks (each with `acceptance_criteria`) into something the executor can carry to
green: atomize each task to a crisp, localized change, author a red-before-green acceptance test per
atom, then let the executor satisfy each oracle, threading outputs across atoms and escalating tier
per-atom. The structural half (`BuildFeatureChain`) is automated; the planning half (atomization +
gate authoring) is the intelligence, staged from principal-authored toward automated. See also
[`docs/ATOMIC_CODING_CHAIN.md`](./docs/ATOMIC_CODING_CHAIN.md).

---

## 6. The substrate boundary

Corp-OS is the harness; **`toolkit-server`** is its runtime substrate — a separate MCP service
providing the work-ledger, memory, and knowledge surfaces. The `internal/mcp` client speaks to it
over plain `net/http`: `POST /mcp/<surface>` with a `{action, params}` JSON body. The loop neither
imports nor assumes the substrate — it's reached only through the `tool.Provider` seam.

Over time, surfaces migrate from *remote MCP* to *owned in-process organs*: `fsorgan`, `sysorgan`,
and `web` are native today, running in Corp-OS's own process on the host. This is the "cannibalize
the rented harness" trajectory — each owned organ removes a dependency. Lifecycle reflexes that
began as external hooks (`parse_context`, memory load, arc-close filing) are likewise **loop-fired**
now, via `internal/hooks`, `internal/memory`, and `internal/arcreview`.

The contract Corp-OS depends on — which surfaces/actions it calls and what a substitute substrate
must provide — is documented separately (see [`docs/`](./docs)), so the decoupling reads as a
designed boundary rather than a missing dependency.

---

## 7. Testing and the gate

Every commit passes a single gate (`scripts/gate.sh`, wired as both the pre-commit hook and the
CI-equivalent): `gofmt -s` · `go vet` · `golangci-lint` · `govulncheck` · `go build` ·
`go test -race`, with a **95% coverage floor** on `internal/…`. The design pays for this: the pure
packages (`router`, `routing`, `laddercfg`, the diff/coverage parsers) are sans-IO and table-tested,
and the seams (`tool.Provider`, `model.Adapter`, the runner) make the IO-touching code injectable.
Dev tools run pinned via `go run <tool>@<ver>`, keeping `go.mod` minimal and CGo-free.
