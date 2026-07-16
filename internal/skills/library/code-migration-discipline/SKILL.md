---
name: code-migration-discipline
description: Process for translating an existing surface from one language or architecture to another without behavioral regression. Five-step playbook: feature inventory → test layer separation → per-feature TDD loop → parity audit → promotion + archive. Counterpart to bug-fixing-discipline for the planned-translation case. Cross-project — lives at ~/.claude/skills/ so any repo benefits.
triggers:
  - migrate X to Y
  - migrate to go
  - migrate to rust
  - port from
  - port to
  - rewrite in
  - language migration
  - code migration
  - rust retirement
  - retire crate
  - retire X in favor of Y
  - reimplement in
  - translate to
  - move from
  - extract to
  - same shape in another language
  - parity port
---

# Code Migration Discipline

Process for translating an existing surface — a crate, a binary, a service, a module — from one language or architecture to another without losing behavior. Companion to `bug-fixing-discipline` (which covers the unplanned-bug case); this covers the planned-translation case.

The discipline exists because **most migrations don't fail at the translation step; they fail at "we thought we had all the features."** The 2026-05-22 status-data regression in mcp-servers is the canonical worked example: T2's Option-A backfill migrated entity state to a new projection layer without an entity-count parity audit, and ~1500 closed bugs/tasks/chains silently regressed to open/pending. Today's restoration session was, in retrospect, the parity audit that should have been a precondition of the original migration.

## When this skill fires

- Any phrase naming a planned translation: "migrate X to Y", "port from Rust to Go", "rewrite in TypeScript", "retire crate in favor of Go package".
- Any chain or task whose acceptance criteria asserts "behavior preserved across language/arch change".
- Any time you're about to delete a file/crate/binary that has been superseded — the discipline's parity audit should already have run; if it didn't, the delete is premature.
- Any moment where you'd reach for "I'll just port this verbatim" — the skill fires precisely to interrupt that shortcut.

## The five-step playbook

### 1. Feature inventory (the trap step)

The hardest step. Most migrations fail here because "I read the source, I know what it does" misses tacit behaviors. Don't guess; enumerate.

- **Source's own test suite.** Treat each test fn name as a feature; tests describe what the author thought mattered. Capture fn names, not just `#[test]` / `@Test` occurrences — `grep -A1 #[test] | grep "fn "` (or language equivalent) gives you the actual feature labels. Filter worktree / .git / build-artifact noise.
- **Caller grep.** For every public symbol of the source, find every call site in the broader codebase. Some "features" are dead code (no callers — skip them). Some live behaviors don't appear in the source (handled by surrounding glue — must port too).
- **Public API surface enumeration.** List every exported function, type, constant. Each is a contract.
- **Transitive dependency inventory.** For each source being migrated, enumerate (a) what other to-be-migrated crates/modules consume this one, AND (b) what this one consumes from those other to-be-migrated crates. The union must come along or be vendored. Without this, a "self-contained" extract turns out to drag its dependency cone with it. (Example: in the mcp-servers Rust retirement, benchmarks/ taps deep into shared-db's events surface — moving benchmarks alone broke. Surfaced in T3 audit 2026-05-22.)
- **Characterization tests on edge inputs.** Feed the source representative + adversarial inputs; capture outputs. These captured outputs become ground truth for the target's test suite — especially for behaviors no one knew were behaviors (off-by-one quirks, nil handling, error message wording that downstream code depends on).
- **For data-touching migrations: row-level invariants.** Count entities in each state in the source schema; the target must produce the same counts on the same input. **Without this step, you get the 2026-05-22 incident.**

Output: a written feature list + transitive-dep table, scoped to what the migration must preserve.

### 2. Test layer separation

Not every feature deserves a unit test. Layer the suite:

- **Contract tests** (API shape, error envelopes, return types) — always write first. Cheap, catches the most regressions.
- **Behavior / characterization tests** (golden inputs → expected outputs) — always write first. The captured-output tests from step 1 land here.
- **Implementation tests** (internal data structures, private helpers) — usually skip. They bind the new implementation to the old one's structure, which defeats half the point of migrating.
- **Integration smoke** (at least one per surface, end-to-end) — write first as the "the whole thing wires up" gate.

The skill is in *what NOT to test*. Implementation tests written in the source language don't port; they keep the new code shaped like the old code.

### 3. Per-feature TDD loop (not big-bang)

Write feature N's tests → translate feature N (in the target language's idiom) → tests green → move to feature N+1.

**Translate behavior, not syntax.** Go developers don't write Result<T,E>; Rust developers don't write `if err != nil { return }`. The test suite catches semantic drift; the target-language idiom keeps the new code maintainable. Porting line-for-line produces un-idiomatic target code that becomes a maintenance burden — every future change has to navigate the source language's shape through the target language's grammar.

Resist the urge to translate everything before running any tests. A 100-test failing wall is paralyzing; per-feature loops keep momentum and surface bugs while you still remember the feature's nuance.

**Target-language type-safety conventions shape the port.** When the source uses a permissive shape (Rust's `serde_json::Value`, Python's `dict[str, Any]`) and the target's conventions forbid it (Go's forbidigo blocking bare `any`, TypeScript's `strict` mode), the port produces a typed struct hierarchy where the source had a hashmap. Net: clearer + safer code; slightly higher upfront cost. The skill is to recognize this isn't drift — it's the target language asking for the model the source had implicitly. (Surfaced 2026-05-22 during T5 Phase 3 of the mcp-servers benchmarks port: smokeRecord/smokeResponse/smokeChoice typed structs replaced Rust's serde_json::Value traversal.)

### 4. Parity audit before promotion

Before declaring the migration done, run a side-by-side gate. Choose the audit shape that fits the migration:

- **Live diff** (default): same input → source + target → diff outputs. For pure-function migrations this is straightforward.
- **Test-suite cross-coverage** (when source + target both have robust parallel test coverage of the same behaviors): the audit can collapse to "verify both test suites cover equivalent scenarios" — surface a per-feature mapping table (source test → target test) showing every source behavior is also tested in the target. This is *sufficient evidence* when both sides demonstrate the behavior via passing tests, no live-input round-trip required.
- **Data-touching migrations: entity-count invariants.** "Source has 756 fixed bugs; target must too." "Source has 1694 closed tasks; target must too." Per-status, per-project, per-table.
- **Schema-touching migrations: schema-equivalence test.** Same input fixture, both schemas, both populated; diff the row sets.

**Parity isn't surface-name equality.** Two patterns surface differently but achieve equivalent outcomes:
  - **Mock idiom translation**: Rust's typed `MockModel { responses: Mutex<Vec<...>>, calls: Arc<Mutex<...>> }` ↔ Go's `httptest.NewServer(handler)` + per-test handler closures. Different patterns, identical test scenarios. Parity check looks for "can the consumer write the same test scenarios in the target's idiom" — not "does the same Mock type exist."
  - **Config-in-code vs config-in-files**: source embeds N variants as enum/struct definitions ↔ target externalizes N variants as config files loaded at startup. Parity check compares (a) source's variant count vs target's config-file count + (b) per-variant behavior match. Both axes must hold.

The parity audit is the *precondition for archive*, not an optional follow-up. If you skip it and ship anyway, you're betting the migration is perfect. The 2026-05-22 incident is what that bet looks like when it loses.

### 5. Promotion + archive ritual

- **Source stays running until target proves itself.** Dual-run for a calibration period if the migration is load-bearing. The Rust toolkit-server stayed live alongside the Go port for the first weeks of the T-series migration; that overlap was the safety net.
- **Cutover atomic where possible.** A single commit flips the routing or the imports, with a clear rollback (`git revert <SHA>`).
- **Source archived, not deleted.** Move to `tools/archive/<surface>-<date>/` (mcp-servers convention) or equivalent. Future archaeology often needs the source's original shape.
- **Rollback path named explicitly in the migration commit message.** "If we need to roll back: revert <SHA>; previous binary at <path>; data path <description>."
- **The parity audit's results are linked in the chain closure** as evidence the migration met its acceptance criteria.

### Effort estimates: agent-pace vs human-pace

A planned port executed by an agent with the dependency map already in hand (per step 1's transitive inventory) runs **~30-60× faster than human-pace estimates**. The 2026-05-22 mcp-servers benchmarks port estimated 8-13 working days; Phases 1-3 took ~45 minutes in execution.

Plans should label estimates explicitly as **agent-pace** or **human-pace**; mixing them produces "this took 30 minutes instead of 1 day" surprises that erode trust in the plan's other estimates. The acceleration comes from: (a) step 1's transitive inventory eliminates re-discovery, (b) target equivalents that already exist make the port glue-work not new-code-writing, (c) the discipline prevents backtrack thrash, (d) agents read + write code at machine speed regardless of LOC count.

When estimating a planned port, factor in: **was the target's equivalent already shipped?** If yes, divide your gut human estimate by ~30. If no (target needs to be written too), keep the human estimate — the port becomes new-implementation work that doesn't accelerate.

## The four load-bearing reflexes

Internalize these; everything else is downstream.

1. **Feature inventory is the work.** If you skip step 1, the migration ships with silent gaps. Don't shortcut by reading the source; combine source-test names + caller grep + API enumeration + characterization tests.
2. **Translate behavior, not syntax.** The test suite proves behavior; the target language's idiom keeps the code maintainable. Don't port `Result<T,E>` into Go.
3. **Parity audit is a precondition, not a follow-up.** If you ship without it, you've made an untested bet. The 2026-05-22 incident is the worked example.
4. **Source archived, never deleted (until far past promotion).** Reversibility costs nothing in disk; irreversibility costs real recovery time.

## Anti-patterns (auto-fail the discipline)

- **Port verbatim.** Produces target code that reads like source code; future maintenance must navigate two languages' idioms simultaneously. Worse: agents reading the target later assume the source idioms were intentional design choices.
- **Rewrite without tests.** Silent regression risk. This is exactly the 2026-05-22 shape — T2's Option-A backfill produced row state without producing the events that would have made the state survive a rebuild.
- **Big-bang translate-all-then-run-tests.** 100 failing tests is paralyzing; you can't tell which failures are real regressions vs which are tests-not-yet-passing. Per-feature loop avoids the wall.
- **Trust the source's tests as complete coverage.** Most test suites have gaps. Characterization tests fill them. If the source has 30 tests and you only write 30 tests for the target, you've inherited the source's blind spots.
- **Skip the parity audit because "it compiles."** Compilation proves syntax; tests prove unit behavior; parity audit proves systemic behavior. They're three different gates. (The 2026-05-22 benchmarks-port Phase 3 surfaced a fold-hook bug precisely because the parity audit insisted on "rows visible in the projection post-run, not just events in the events table." Without the audit, the binary would have silently shipped working events + empty projections.)
- **Delete the source the moment the target promotes.** Reversibility window matters. Archive, don't delete.
- **Trust the target's runtime defaults match the source's.** Event-sourced architectures often have per-process hooks (projection folds, telemetry sinks, dispatch policies) that the original server registers in main() but standalone CLI binaries don't. Porting a binary that emits events into a different process context can silently break the downstream pipeline. Always verify the runtime hook surface for the target process type, not just the source code.

If you find yourself reaching for any of these, stop and re-read the corresponding playbook step.

## Acceptance: a migration is "done" only when

Before declaring a code migration complete, confirm:

- [ ] **Feature inventory exists in writing** — a list (in the chain's design_decisions, or a vault note, or a docs/ markdown) of what the migration must preserve. Combines source-test names + caller-grep findings + API enumeration + characterization-test inputs.
- [ ] **Test suite covers the inventory** at the contract + behavior layers (not implementation). Tests are written in the target language and use target-language conventions.
- [ ] **Per-feature TDD loop was followed** — each feature's tests went red → green before the next feature started. Git history shows the alternation.
- [ ] **Parity audit ran and passed** — side-by-side run on same inputs, diff is empty (or differences are explicitly accepted and documented). For data migrations: row counts per status/per-table match.
- [ ] **Source is archived, not deleted** — at the time of promotion. (Deletion can come later, after the calibration period; the archive sticks around longer.)
- [ ] **Cutover commit names the rollback path** — single-line in the message: how to revert if the migration turns out wrong.
- [ ] **Chain closure cites the parity-audit evidence** — not just "tests pass" but "source produced N rows of state X; target produces N rows of state X; diff = 0."

If a step legitimately doesn't apply (rare — usually means the migration is small enough that it's really a refactor), say so explicitly in the chain closure. Skipping silently is the anti-pattern.

## Boundary: this skill vs related skills

- **`bug-fixing-discipline`** — the unplanned-bug case. Same shape (12-step playbook with anti-patterns), different trigger. A bug surfaced by a parity audit during migration becomes a bug-fixing-discipline problem; the migration discipline's job is to expose it.
- **`vault-filing-discipline`** — feature-inventory documents + parity-audit results often warrant a vault note (decision or learning) so the next migration starts from this one's lessons.
- **`coding-philosophy`** — cross-language principles like "no escape hatches" still apply within the target language. Migration discipline says WHAT to test; coding-philosophy says HOW to write the resulting code.
- **`systematic-debugging`** — fires when characterization tests surface behavior that doesn't match the source's docstrings. Use it to figure out what the source actually does before deciding what the target should do.
- **The per-language convention skills** (`go-conventions`, `rust-conventions`, etc.) — apply to the target language's resulting code. Migration discipline + go-conventions together cover "what does Go code that replaces Rust code look like."

## Boundary: code migration vs data migration

This skill scopes to **code** migration: source code → target code, same behavior. **Data** migration (one schema's data → another schema's data) has its own discipline, with extra steps:

- Snapshot the source data before migrating.
- Run the migration in a transaction; commit only after row-count invariants pass.
- Keep both schemas readable during a calibration window; auto-cutover only after live verification.
- Have an explicit rollback path that can restore from the pre-migration snapshot.

When a migration is **both** (code AND data, like the toolkit-server Rust→Go + CRUD-tables → projections), both disciplines fire. The 2026-05-22 incident was a data-migration discipline failure inside a code-migration arc — the code was ported fine; the data backfill was the silent gap. Without both disciplines explicitly named, the gap is easy to miss.

If you're about to retire a database table, drop a CRUD path, or change a schema, the data-migration discipline applies on top of this one. (Sibling skill TBD; for now, treat the data-migration steps above as the floor.)

## Worked example: the 2026-05-22 mcp-servers incident

What went wrong (chronologically):
1. **T2 (Option-A backfill)** populated proj_current_bugs/tasks/chains directly from the old CRUD tables. Skipped step 1.4 (data parity audit at the event-emission layer) — no synthetic `BugResolved` / `TaskCompleted` / `TaskCancelled` events were emitted for pre-T2 terminal state. Migration "looked done" because row state was correct.
2. **T6 dropped the CRUD tables.** Removed the fallback source of truth. Migration's parity audit (had it existed) would have noticed: "events table has 50 BugResolved entries; source had 756 fixed bugs; gap of 706."
3. **A `rebuild-projections` invocation** on May 22 truncated projections and folded from events alone. The 706 missing terminal events surfaced as 706 regressed bugs.
4. **Restoration took ~3 hours** to identify, scope, and execute 11 passes of synthetic-event replay.

What the skill would have caught: step 1.4 (entity-count invariant: "source has N closed bugs; target's events table must have N BugResolved events"). One SQL query at migration-close time. Would have surfaced the gap before T6 dropped the CRUD safety net.

This is why **the parity audit is a precondition, not a follow-up.** When you skip it and ship, you're hoping the events table is complete. When you skip it and then ship something that DEPENDS on the events table being complete (T6), you've removed the safety net while making an untested bet.
