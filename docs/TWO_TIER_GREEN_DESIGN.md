# Two-tier green: a surface-scoped quality report on the coding gate

> Chain `coding-two-tier-green-quality-report`, task
> `design-two-tier-report-shape-and-scoping`. The spec tasks 2–3 implement.

## Why

The coding gate is one bit: `go build && go test` exits 0 (green) or not (red). That
catches a *broken* fix but not an *incomplete* one — a worker can pass the gate without
the changed code being exercised by any test. The existing anti-fake-green machinery
(below) catches *fabricated/hollow* green; it does not catch *untested* green. The
two-tier report adds that axis as an **advisory** grade on top of the hard gate.

## What already exists (extend, do not duplicate)

All orchestrator-owned, all fired around the gate-pass in `internal/coding`:

| Check | Detects | Verdict | Blocks? |
|---|---|---|---|
| protected-path edit (`flags.go` `FlagProtectedPathEdit`) | edited a `*_test.go`/acceptance path | `WorkerGateIntegrityViolation` | **yes** |
| red-before-green (`redgreen.go` `tautologyVerdict`) | new test passes on the pre-fix tree | `WorkerFakeGreen` | **yes** |
| test-only diff (`flags.go` `FlagTestOnlyDiff`) | only test files changed | `WorkerTestOnlyDiff` | **yes** |
| independent verifier (`verifier.go` T5) | verifier non-PASS / no evidence | `WorkerVerifierRejected` | **yes** |
| fake-green / scaffold-fab (`internal/agent`, `StageFakeGreen`) | worker authored the certifying test / wrote a build scaffold | `Result.Escalate` | **yes** |

These all answer "is this green **fabricated**?" and HARD-FAIL. The two-tier report answers
a different question — "is this green **substantive** (the change is exercised)?" — and
**never hard-fails**: it colors the green.

## Tier model

- **Tier 1 — the hard gate (unchanged).** `go build && go test` exit 0. Pass/fail. The law.
- **Tier 2 — the quality report (new).** Computed only when Tier 1 passed, scoped to the
  worker's diff. Produces a verdict that **grades** the green:
  - **confirmed-green** — gate passed AND every changed *production* line is exercised by a
    test (or there are no changed production lines — that case is already the test-only-diff
    blocker's domain).
  - **proposed-green** — gate passed BUT ≥1 changed production line is uncovered (or a test in
    a touched package is `t.Skip`'d). Surfaced as an advisory, **not** a failure.

## Report shape

A pure-data struct (no IO), `internal/coding`:

```go
type CoverageGrade struct {
    Verdict        string             // "confirmed" | "proposed" (| "" when not applicable)
    ChangedLines   int                // changed PRODUCTION lines considered
    ExercisedLines int                // of those, how many a test executed
    Uncovered      map[string][]int   // production file -> changed line numbers with count==0
    SkippedTests   []string           // t.Skip'd tests in touched packages (secondary signal)
    Advisory       string             // one-line human/worker-readable summary, "" when confirmed
}
```

`Verdict == ""` when there are no changed production lines, or coverage could not be measured
(no touched packages, a tooling error) — in that case Tier 2 is a no-op and the chain proceeds
exactly as today.

## Scoping algorithm (diff → surface → coverage)

1. **Diff:** `o.worktreeDiff(ctx, dir, parentSHA)` — the worker's unified diff vs the AT fork
   point (already used for the existing flag checks; guards NoopRepo + empty fork → "" → no-op).
2. **Changed files:** reuse `changedPaths(diff)` (`guard.go:100`). Keep only PRODUCTION Go files
   — drop `*_test.go` and non-`.go`. (Test files are the blockers' concern, not coverage's.)
3. **Changed lines per file:** parse unified-diff hunk headers `@@ -a,b +c,d @@` and collect the
   POST-image (`+c,d`) line numbers that are `+` (added/changed) lines, per file. *(New code —
   no hunk parser exists; follow the `redgreen.go` diff-walk pattern. Pure, sans-IO, table-tested.)*
4. **Touched packages:** map each changed production file's directory to its `./`-relative
   package path; dedupe.
5. **Coverage run:** reuse `ExecRunner` to run, in the worktree dir,
   `go test -coverprofile=<tmp> -covermode=atomic -coverpkg=<touched pkgs> ./<touched pkgs>`.
   This is a SECOND, scoped run (the gate already proved green; this measures coverage). It is
   cheap — a narrow, warm-cache `go test` is sub-second (measured 0.38s on the t3b fixture).
   The profile is written to a temp file (not committed into the worktree).
6. **Parse the profile:** hand-parse `mode:` + `file:sl.sc,el.ec numStmt count` rows (no
   `x/tools/cover` dep). A block with `count==0` is uncovered; `count>0` is covered. Map each
   profile block to its file + line range.
7. **Intersect:** for each changed production line (step 3), is it inside a covered block
   (`count>0`) for that file? `ExercisedLines` counts the hits; `Uncovered[file]` collects the
   misses. `SkippedTests` is parsed from `go test -v` output (`--- SKIP`) for touched packages.
8. **Verdict:** `Uncovered` empty AND `SkippedTests` empty AND `ChangedLines>0` → `confirmed`.
   Any uncovered changed line OR any skipped test → `proposed` with a one-line `Advisory`
   (e.g. `gate green, but changed lines internal/x/y.go:12,13 are not exercised by any test`).

## Wiring (advisory, never blocking)

- **Owner:** `internal/coding` (the orchestrator owns the gate; coverage is a gate-derived
  metric). New file `internal/coding/coverage.go` (pure diff/profile parsing + the runner-driven
  coverage run). The agent loop is NOT the owner — it stays generic.
- **Insertion point:** `orchestrator.go` `runWorkerLoop`, inside the `gr.Passed` block, AFTER the
  existing post-gate checks (tautology, test-only-diff) and BEFORE `return WorkerSuccess`. The
  status returned is **still `WorkerSuccess`** — Tier 2 never produces a failure.
- **Surfacing:** append a new `GateFlag{Kind: FlagCoverageAdvisory, Detail: grade.Advisory}` to
  the attempt's `flags` (rides on `ar.Flags`, emitted with the AT event — structured, never
  swallowed). On a `confirmed` grade, no flag is added.
- **Feed-back to the worker:** add `PriorCoverageAdvisory string` to `coding.Feedback`; `buildDuty`
  appends it as a clearly-NON-blocking note ("gate passed; coverage advisory: … — add a test if
  these lines should be exercised, otherwise report done"). It only reaches the worker on a
  subsequent attempt (a revise driven by some other signal); a `proposed` green alone never forces
  a revise — it informs.

## Hard invariants

1. **Coverage is advisory.** Tier 1 (gate exit 0) is the only hard pass/fail. A `proposed` green
   still commits. A correct one-line fix to already-covered code, or a legitimately-unreachable
   changed line, must never be blocked.
2. **Orchestrator-computed, not worker-narrated.** Like every other verdict, Tier 2 is computed by
   the loop from the diff + profile, never from anything the worker says.
3. **Scoped, not whole-module.** Coverage runs only the touched packages — cheap and relevant.
   Whole-module coverage % is noise for a bug fix.
4. **Pure where possible.** Hunk parsing and profile parsing are sans-IO and table-tested; only the
   coverage `go test` run touches the runner seam.

## Out of scope for v1 (future extensions)

Branch/condition coverage; coverage-delta vs the pre-fix tree; flaky/timing-dependent tests;
import/signature-consistency checks. The struct + insertion point are designed so these slot in as
additional `CoverageGrade` fields without re-wiring.
