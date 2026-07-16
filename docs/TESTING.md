# Testing conventions

The gate (`scripts/gate.sh`) enforces `go test -race` + a **95% coverage floor on `./internal/...`**.
Coverage is **line-based and undifferentiated**: a line hit by any test counts, regardless of WHICH
behavioral path reached it. That is the gate's blind spot, and this doc names the test categories that
close it. New code brings its own tests (sans-IO: injectable transports, `httptest`, temp dirs/DBs — the
suites show the pattern); these categories say what SHAPE those tests must take for the cases the line
count cannot see.

## The scriptable seams (use these; don't reinvent)

- **`model.Echo`** (`internal/model/echo.go`) — scripts an ordered `[]model.Response`: tool calls,
  prose, per-response `Usage` (cost), `StopReason`. Cannot script a mid-sequence fault.
- **Faults** — return the wrapped sentinels from `internal/model/fault.go` (`ErrContextOverflow`,
  `ErrMalformedToolCall`, `ErrRateLimit`) from a small ad-hoc `model.Adapter`; `ClassifyFault` maps them
  onto the escalation taxonomy. ~30 ad-hoc adapters already exist (`errAdapter`, `scriptAdapter`,
  `varyingRunaway`, `growingAdapter`, `scriptedCoder`, …) — grep before adding one.
- **Tool side** — `fakeProvider` / `errProvider` (`internal/agent/*_test.go`) drive tool_error thrash.
- **Coding organ** — `fakeWorker`, `tierEchoWorker`, `recordingWorker` (scripted worker attempts),
  `countRunner{passAt:N}` (gate greens on round N), `fakeRedGreenRepo`, `fakeVerifier`
  (`internal/coding/*_test.go`). Drive the full write→gate→revise loop via `RunDuty` with these.

## Mandated categories

### 1. Policy-path matrix (the one that would have caught bug-1165(b))

A **cross-cutting router/loop policy** — a bound, budget, breaker, or gate that gates escalation or spend
(`WithBoundedTop`, `WithSharedStrongBudget`, the cost meter, the spawn budget, the no-progress /
retry-exhaustion / overflow breakers) — MUST be tested through **every escalation path that can reach the
gated rung**, not just the convenient one. Concretely, the router reaches the strong rung two ways with
DIFFERENT state:

- **Routine** — `Observe(Signals{ToolErrors:…})` / `repeated_tool_error`. Does NOT set `stickyTop`.
- **Sticky** — `EscalateForRetryExhaustion`, `EscalateForNoProgress`, `EscalateForOverflow`. These SET
  `stickyTop`, and several policies deliberately bypass on `stickyTop` (the run-10 trap). A policy that
  must bind *regardless* of `stickyTop` is exercised ONLY down the sticky path.

**The rule:** if a policy's correctness depends on the `stickyTop` state, it needs a test down BOTH paths.
100% line coverage via the routine path proves nothing about the sticky path — the lines are identical;
the STATE is not. This is exactly how bug-1165(b)'s first cut shipped: the shared strong-turn budget was
line-covered by routine-path tests, but the live-dominant sticky path (retry_exhaustion) skipped it. See
`internal/router/strong_budget_test.go` (`TestSharedStrongBudgetBoundsStickyEscalation`) and
`internal/agent/strong_turn_budget_test.go` (`…_StickyPath`) for the pattern.

**And test through the WIRING, not only the unit.** A policy proven in router isolation must ALSO be
proven through the layer that assembles it in production (`agent.Spawner.routerFor`, `buildRouterFromTiers`),
because the wiring is where a policy silently fails to be installed. The router-isolation test and the
wired-spawner test are both required.

### 2. Convergence / non-convergence of the coding revise loop

The coding organ's value is that a worker's WRONG edits get caught and either self-repaired or halted
honestly. Every such terminal state (`internal/coding/worker.go` `WorkerStatus`) must have a test that
reaches it through the real `RunDuty` / `runWorkerLoop` path, driven by a scripted worker + gate runner:

- **converges on round N** — worker wrong for N-1 rounds, right on N (`countRunner{passAt:N}` +
  scripted worker); assert green + the round count.
- **never converges → honest stuck** — `WorkerMaxIterationsExhausted` / `WorkerRespawnCapReached`
  (`respawn_carryover_test.go` shows the shape); assert the verdict carries the last real gate diagnostic,
  NOT a fake green.
- **fake-green rejected** — `WorkerFakeGreen` / `WorkerTestOnlyDiff` / gate-integrity (existing tests).

A convergence FIX (feedback quality, revise budget, gate scope) must land with a `RunDuty`-level test that
pins the emergent outcome ("with the fix, a worker that was stuck at round-budget now greens within it"),
not only a unit test of the changed function. The live rehearsal is confirmation, never the regression net
— the deterministic `RunDuty` test is.

## What the gate does NOT cover (know the limits)

- `*_live_test.go` self-skip without `CORPOS_LIVE=1` + keys, so they never carry the floor — the 95% is
  entirely deterministic Echo/fake-driven tests. A live rehearsal validates end-to-end; it is not a
  regression test and cannot be one (non-deterministic, expensive, model-dependent).
- The floor is one aggregate number over `./internal/...`; it cannot see path/state coverage. These
  categories are the human-owned complement to the line count.
