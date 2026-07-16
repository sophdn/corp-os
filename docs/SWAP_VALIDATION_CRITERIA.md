# Harness-Swap Validation Criteria — bar LOCKED 2026-05-31 · window NOT yet open

> **Status: the BAR is locked; the WINDOW has NOT opened.** The economic knobs in
> §3 (run-rate threshold, decision rule, 14-day duration) were set by Sophi on
> 2026-05-31 (see the sign-off block at the bottom) and remain the committed bar.
> **But the validation window is readiness-gated, not calendar-gated — it has not
> started and no dates bind it.** It opens only when corpos is a true daily-driver
> replacement for Claude Code (see §0). This doc is the deliverable for
> `harness-swap-validation` (chain 270) task 1, `lock-acceptance-criteria`.

Drafted 2026-05-31 by Claude Code (corpos session). Substrate work this draft
depends on is already landed: the multi-turn REPL (chain 332), substrate-sourced
tool specs with per-action purpose (DefaultSpecs fix + suggestion 46 + the
chain_status doc correction), and the cost-aware router + run-rate telemetry
(`feat/cost-aware-router`).

## 0. Entry gate — when this window may open (READINESS, not calendar)

**The window has not opened.** It is a category error to "be in the validation
period" while corpos cannot yet do everything Claude Code does for daily work —
that would force split-brain operation (some work in corpos, the rest still in
Claude Code), and a run-rate measured across a split brain is meaningless. Worse,
a cheap number achieved only because corpos carried the easy slice is a *Rollback*
signal by the two-signal rule (§6), not a Keep.

So the window is **downstream** of corpos reaching daily-driver parity, not a
period that runs in parallel with the build. It opens only when ALL of these hold:

- the corpos-replacement build band has landed — `corpos-sub-orchestration`,
  `toolkit-decomposition`, and `corpos-context-compaction` (roadmap positions
  7–9);
- the substrate gaps that block real daily work are closed — at minimum the exec
  substrate (no `/bin/sh` in the deployed toolkit-server image) and an owned
  `fs.move`/`rm` primitive (design doc §7 gap #6);
- corpos can plausibly carry the large majority of Sophi's daily agent work
  *without* routine fallback to Claude Code.

When those hold, Sophi opens the window deliberately and the 14-day clock (§3)
starts *then*. The swap happens at a logical readiness point, not an arbitrary
calendar date. Until then: keep building; do NOT describe any period as "the
validation window," and do not report a swap decision as pending-by-date.

## 1. Purpose

Run a bounded period in which **corpos drives Sophi's daily agent work in place of
Claude Code**, with its cost router pushing the bulk to free local Qwen and cheap
APIs and reserving expensive models for rare emergencies. `internal/cost`
accumulates a real API run-rate. At the end, decide **keep / rollback / extend**
against the economic bar below. **This period runs only once the §0 readiness
entry gate is met — it is not running now**, and a Max5x renewal boundary is a
re-evaluation checkpoint, never a deadline that forces it open early.

## 2. The bar (ECONOMIC — locked in chain 270 body 2026-05-31)

The swap is a **clear net win** only if corpos can do the daily work such that the
**projected monthly API run-rate is comfortably below the Max5x cost (~$200/mo)** —
not a wash. "Comfortably below" is made concrete by the threshold in §3.

## 3. Criteria values — LOCKED 2026-05-31

| Knob | Value | Notes |
|---|---|---|
| **Run-rate threshold** | projected **≤ $100/mo** | Half of Max5x — a win with tolerance for escalation spend. The period must land *comfortably* under this, not graze it. |
| **Period length** | **14 days** — duration LOCKED; **start = the §0 readiness entry gate**, NOT a calendar date | The 14-day duration is fixed; the *start* is whenever §0 is met. A Max5x renewal boundary is a natural re-evaluation checkpoint, never a deadline that forces the window open early. |
| **Daily-driver commitment** | **corpos-first for ALL agent work** | Heaviest signal: corpos is first-reach for every agent task in the window. Made feasible by the coverage escape hatch in §3a. |
| **Emergency-Opus policy** | Opus only by **explicit manual invocation**, logged | Keeps Opus out of the bulk; each use is a counted, deliberate exception. |

### 3a. Coverage escape hatch (load-bearing for "corpos-first for ALL work")

corpos cannot yet carry every task — most notably **shell-shaped work** (sysadmin,
discovery, build/run, git, verification), because it has no owned exec /
system-introspection surface and its `fs` is read/write/edit only (filed:
suggestion `corpos-needs-an-owned-exec-system-introspection-surface-fs-ls-grep-glob-to`).
So "corpos-first for ALL work" means: **try corpos first on every task; when it
genuinely can't carry one, fall back to Claude Code and LOG the fallback** — what
the task was and which missing capability forced it.

This turns the gaps into data. The decision (§6) then weighs **two** signals, not
one:

- **Run-rate** — projected monthly API spend vs the §3 threshold (the economic bar).
- **Coverage** — the fraction of real tasks corpos carried without falling back,
  plus the ranked list of capabilities that forced fallbacks.

A low run-rate with poor coverage is *not* a clean keep — it likely means corpos
only carried the cheap-and-easy slice. Coverage gaps are the named follow-on
drivers chain 270 condition (f) calls for. Keep the fallback log in
`docs/SWAP_VALIDATION_LOG.md` (or a session-DB query) over the period.

## 4. Recommended corpos configuration for the period

Cost routing is now real (see `feat/cost-aware-router`). Recommended invocation
for daily driving:

```sh
corpos \
  -strong-provider anthropic -strong-model claude-haiku-4-5-20251001 \
  -escalate-after 1 \
  -session-dir ~/.local/state/corpos/sessions
```

- **Cheap tier (bulk):** local Qwen (free) drives every turn by default.
- **Strong tier (escalation):** Haiku, engaged only after a tool-error turn, then
  de-escalating after 2 clean turns. Needs `ANTHROPIC_API_KEY`; if absent, the
  router degrades escalations back to Qwen (announced at startup).
- **Emergency Opus (rare, manual):** `corpos -provider anthropic -model claude-opus-4-8 …`
  for the occasional turn that genuinely needs it. Counts as an exception (§3).
- Persisting `-session-dir` keeps every session's run-rate on disk for the report.

## 5. Measurement methodology

Each session prints a run-rate summary at exit (`printCostSummary`): total USD +
per-model token/USD breakdown (free / priced / UNPRICED) + router fallback counts.
For the period:

1. Keep `-session-dir` stable so every session DB is retained.
2. Sum **paid** spend (non-free models) across all sessions in the period → period
   API spend `S` over `D` elapsed days.
3. **Projected monthly run-rate** = `S / D × 30`.
4. Compare to the §3 threshold.

Prices come from `internal/cost` (`priceTable`); confirm they still match current
list prices at lock time. Any `[UNPRICED]` model in a summary means the table
needs an entry before the number is trustworthy.

## 6. Decision rule (weighs run-rate AND coverage — see §3a)

- **Keep** — projected run-rate ≤ $100/mo **AND** coverage is high (corpos carried
  the large majority of real tasks; fallbacks were few and concentrated in
  capabilities already queued as follow-on work). → queue Max5x cancellation; top
  up the API console for emergency Opus; record the verdict + stats as a decision
  event on the write-side ledger.
- **Rollback** — projected run-rate > $100/mo, **OR** coverage was poor (a low
  run-rate achieved only because corpos carried just the cheap-and-easy slice while
  real work kept falling back to Claude Code). → renew Max5x; **name the specific
  drivers** — the ranked fallback-forcing capabilities (the exec/introspection
  surface is the known first one), routing quality, local-model tool-use
  reliability, ML/ONNX offload.
- **Extend** — signal is promising but the period was too short / unrepresentative,
  or a near-term capability (e.g. the exec surface) would materially change
  coverage. → name what to watch and the new decision date.

The two-signal rule is the load-bearing guard: **a low run-rate is only a Keep if
coverage backs it up.** Cheap-because-narrow is a Rollback, not a win.

In all cases: a decision doc records the verdict with the measured run-rate vs the
locked bar, and a decision event lands on the write-side ledger (chain 270
completion conditions d + g).

## 7. Out of scope for the bar (do not block the decision on these)

Per the chain 270 roadmap note, the swap **rides the current client-first arch** —
it is NOT gated on Phase E (native organs), the ml/ONNX sidecar, the
containerization flip, or `--inspect`. Those are separate threads.

## 8. Sign-off — the BAR is locked (the window is not a date)

- Run-rate threshold: **≤ $100 /mo** (projected, comfortably under) — LOCKED
- Period **duration**: **14 days** — LOCKED
- Period **start**: the **§0 readiness entry gate**, NOT a calendar date — the
  original 2026-05-31 → 2026-06-14 dates are VOID (they implied a live window
  while corpos was still being built; see the 2026-06-04 correction below)
- Daily-driver commitment: **corpos-first for ALL agent work**, with the §3a
  coverage escape hatch (fall back to Claude Code when corpos can't carry a task;
  log the fallback)
- Locked by: **Sophi** · Date: **2026-05-31** (bar values set in-session)

_The run-rate threshold, decision rule, and 14-day duration are the committed bar.
Changing them re-opens the lock and should be recorded here with a dated note._

> **2026-06-04 correction (Sophi).** The window is NOT active and was never meant
> to run on a fixed calendar while corpos is still missing daily-driver
> capabilities (fs.move/rm, exec substrate, git-process, sub-orchestration,
> compaction, …). Operating split-brained between Claude Code and corpos is not a
> validation. The window is readiness-gated per §0 — it opens at a logical point,
> not an arbitrary date. The original calendar dates are void.
