---
name: refactoring-discipline
description: "Process for changing the internal shape of working code without changing its observable behavior — same language, same contract, different structure. Seven-step playbook: scope & inventory → exhaustive characterization net (the gate) → audit across the eight axes → first-principles synthesis → triage gate → behavior-preserving execution + parity audit → post-refactor net densification. Counterpart to code-migration-discipline (minus the language boundary; refactoring discovers its own target shape) and bug-fixing-discipline (which changes behavior to correct it). The distinctive spine: the characterization net must be combinatorially complete across input equivalence classes — acceptance × rejection × boundary — before a single line is touched. Cross-project: lives at ~/.claude/skills/ so any repo benefits."
triggers:
  - refactor
  - refactor this
  - refactor the
  - refactoring
  - clean up this code
  - clean up the code
  - tidy up
  - tidy this
  - restructure
  - reorganize
  - reorganise
  - extract function
  - extract method
  - extract class
  - extract module
  - split this file
  - split up this
  - inline this
  - simplify this
  - make this cleaner
  - DRY this up
  - dry this up
  - deduplicate
  - de-duplicate
  - consolidate
  - is this well-structured
  - separation of concerns
  - does this obey conventions
  - is this DRY
  - shared module reuse
  - characterization test
  - characterisation test
  - characterization net
  - behavior-preserving
  - behaviour-preserving
  - pin behavior before
  - pin the behavior
  - first principles rewrite
  - would I write it this way
  - if I wrote it again
  - exhaustive test net
  - while we're here
---

# Refactoring Discipline

Process for taking working code from "its shape could be better" through "restructured, with proof that behavior never changed." Loads the canonical seven-step playbook at [`vault/reference/2026-05-23_refactoring-discipline-playbook.md`](../../vault/reference/2026-05-23_refactoring-discipline-playbook.md) on trigger; this SKILL body carries the discipline summary + acceptance criteria + cross-skill graph.

Cross-project: lives at `~/.claude/skills/` (user-level), so any repo benefits. Sibling to `code-migration-discipline` (translate to another language/arch, behavior preserved) and `bug-fixing-discipline` (change behavior to correct a defect).

## The one-line definition

**Refactoring changes the internal shape of working code without changing its observable behavior.** Same language, same contract, different structure. If behavior changes, it is not a refactor — it is a feature change or a bug fix, and it belongs to a different discipline and a different commit. The discipline's whole job is to keep that line sharp.

## When this skill fires

- A phrase naming a restructure: "refactor this", "clean up", "extract a function", "split this file", "DRY this up", "restructure the module".
- An audit-shaped question about existing code: "is this well-structured", "does this obey conventions", "is this DRY", "separation of concerns".
- A reflex phrase from the discipline: "pin the behavior first", "characterization net", "behavior-preserving", "if I wrote it again".
- An anti-pattern phrase signaling a shortcut about to be taken ("while we're here", "I'll be careful") — the skill fires precisely to interrupt it.

## The seven-step playbook

The deep version (with the combinatorial recipe, the double-edged-axis traps, and the worked example) is in the reference doc. Summary:

1. **Scope & inventory (the trap step).** Pin the boundary precisely; enumerate the full feature set (source-test names + caller grep + API surface + transitive deps); capture the behavioral contract. You cannot refactor what you haven't fully mapped — an incomplete map silently drops behaviors. *(Shared with code-migration step 1.)*

2. **Characterization net — the precondition gate (the load-bearing step).** Before touching a line, pin current behavior in a suite that is **combinatorially complete across input equivalence classes, including rejection cases**: classes → acceptance cross-product → rejection cross-product → boundaries → frozen goldens. **The net is judged against the input space, not the existing test count** — "it has tests" ≠ "it is characterized." If the code isn't exhaustively pinned, you do not proceed; you build the net first. If it's untestable as-is, the first refactor is "make it testable."

3. **Audit across the axes.** The eight questions, producing a **classified findings ledger** (each finding tagged *behavior-preserving* / *behavior-changing* / *taste-only*, with a blast-radius estimate). Conformance (Q2) **delegates** to `coding-philosophy` + the language convention skills — this discipline does not restate conventions. DRY (Q6) and shared-module reuse (Q7) are double-edged — see the reference doc.

4. **First-principles synthesis (Q8 — the integrator).** Sketch the clean-from-scratch shape; the **delta** against current code *is* the candidate refactor backlog. Q1–7 are the evidence; Q8 is the picture. "It would look the same" is a valid output.

5. **Triage gate (refactor vs. leave it).** Prioritize the delta by **value × risk**. **Record rejections with reasons** so the next agent doesn't re-audit and re-propose. "Would look different but the delta isn't worth the churn" is a valid, disciplined outcome. Behavior-changing findings route *out* to bug-fixing/feature work; out-of-scope findings get filed as follow-ups, never broadened into the current refactor.

6. **Behavior-preserving execution + parity audit.** Small steps, the full net green at every step, one transformation per commit. **No assertion is modified to make a refactor pass** — if it must change, behavior changed. Parity = the pre-refactor net green after. *(Shared with code-migration steps 3–4.)*

7. **Post-refactor net densification (the harvest step).** Parity proves behavior was *preserved*; it does **not** cover the new structure. Two gaps remain: **new-branch exhaustiveness** (extractions add branches — nil guards, early returns — that step 6's "coverage must not regress" misses because a new branch has no old baseline; re-run coverage **+ mutation against the new structure** and pin or log-dead each), and the **testability dividend** (the new units — especially sans-IO ones — were the point; characterize each newly-exposed unit boundary across *its own* input-classes, reaching combinations buried end-to-end). **Itself triaged** — skip trivial seams the parity net already covers (with a reason), characterize the ones that expose unpinned classes; close any audit gap flagged "extend if touched" *iff* the refactor touched it. **Strictly additive:** never weakens or modifies a step-2 parity assertion — if one would have to change, step 6 changed behavior. Declaring "done" at parity without densifying the new seams is the silent skip.

### Tooling aids (coverage and mutation testing)

A code-coverage tool **aids** step 2 but is **not** the gate. Coverage measures *which code ran*, not *which behaviors are pinned* — you can hit 100% line coverage with a net that asserts almost nothing and skips every rejection class. So:

- **Coverage is a gap-finder, not a completeness-certifier.** An uncovered branch is a behavior class you haven't pinned (fill it) or dead code (a finding for Q4 — log it, maybe delete it). Green coverage does *not* prove the net is exhaustive.
- **Mutation testing is the real exhaustiveness proxy.** A tool that injects faults and checks your net catches them (Stryker for TS/JS, `cargo-mutants` for Rust, `mutmut`/`cosmic-ray` for Python, `go-mutesting` for Go, PIT for Java) measures whether behavior is actually *pinned*, not merely *executed*. A surviving mutant on a "covered" line is exactly the silent gap a refactor breaks.
- **Coverage must not regress across the refactor** (step 6): a branch that was covered going uncovered means you dropped behavior or the net stopped reaching it.
- **Re-run coverage + mutation against the *new* structure** (step 7): the regress-check can't see a new branch (it has no old baseline) and the parity net never targeted the unit boundaries the extractions exposed. The post-refactor pass is where you pin both.
- **Never let the number become the target** (Goodhart). Coverage is a flashlight that surfaces gaps; the gate is the input-space matrix. Chasing the percentage produces assertion-free tests that touch lines without pinning anything.

## The four load-bearing reflexes

1. **No characterization net, no refactor** — and the net is exhaustive across the *input space*, not the existing test count.
2. **The audit produces a ledger, not a mandate** — every finding triaged; rejections recorded.
3. **Behavior change is not refactoring** — if observable behavior changes, it's a different discipline and a different commit.
4. **First-principles is the synthesizer** — the current-vs-ideal delta is the backlog; "looks the same" is a valid answer.

## Anti-patterns (auto-fail the discipline)

- **Refactor without an exhaustive net** ("I'll be careful"). Careful is not a regression gate.
- **Net judged by test count or coverage %, not input space** — leaves behaviors unpinned that a refactor breaks silently.
- **Big-bang restructure** — change everything, then run tests. A wall of failures can't be triaged.
- **Modifying a test to make the refactor pass** — behavior change wearing a refactor's clothes.
- **DRY-ing coincidental similarity** into the wrong abstraction (duplication is cheaper than the wrong abstraction).
- **Reuse that adds coupling worse than the duplication it removed.**
- **While-we're-here behavior changes** smuggled into a refactor commit.
- **Refactoring hot/volatile code** with no stability — high risk, low payoff.
- **Taste-only refactors** with no articulable improvement; or treating Q8's "it'd look different" as an automatic mandate to rewrite — the triage gate exists to say no.

If you find yourself reaching for any of these, stop and re-read the corresponding playbook step.

## Acceptance: a refactor is "done" only when

- [ ] **Inventory + boundary written.**
- [ ] **Characterization net exists, is exhaustive across the input space** (acceptance × rejection × boundary), and was green before the first refactor commit.
- [ ] **Findings ledger exists and is triaged**; out-of-scope findings filed as follow-ups; rejections recorded with reasons.
- [ ] **Every commit is behavior-preserving**; the pre-refactor net is green after, with no assertion modified to accommodate the change.
- [ ] **Parity holds** against the step-2 goldens (and coverage did not regress).
- [ ] **Post-refactor densification ran** (step 7): coverage + mutation re-run against the new structure, new branches pinned-or-logged-dead, newly-exposed unit boundaries characterized (triaged), parity net still green and *unmodified*. "Done" at parity without densifying the new seams is the silent skip.
- [ ] **The "leave it" decisions are recorded** so the audit isn't re-run from scratch next time.

If a step legitimately doesn't apply, say so explicitly. Silent skips are the anti-pattern.

## Boundary: this skill vs related skills

- **`code-migration-discipline`** — same spine (inventory → characterization → behavior-preserving transformation → parity audit) **minus the language boundary**. Migration is *handed* its target shape ("the same thing in Go"); refactoring *discovers* its target via the audit. Defer to migration for the shared execution/parity mechanics.
- **`bug-fixing-discipline`** — bug-fixing *changes* behavior to correct a defect; refactoring *preserves* behavior. A bug surfaced mid-refactor routes out to bug-fixing. Both share the "while we're here" and "audit before acting" reflexes.
- **`code-review` / `code-auditor` role / `codebase-inspection`** — review evaluates a *proposed diff*; this discipline targets *existing, merged, working* code and outputs a safe-change plan. The audit phase (step 3) *uses* code-auditor / codebase-inspection / coverage tools as instruments.
- **`coding-philosophy` + the language convention skills** (`rust-conventions`, `python-conventions`, `go-conventions`, `code-standards`, `expo-conventions`, `godot-conventions`, `layout-conventions`) — these *define* what good shape is; this discipline is the process that applies that yardstick. Q2 of the audit literally delegates to them — don't restate conventions here.
- **`scratchpad-discipline`** — the findings ledger + rejection log are in-flight working memory; file them at `~/.claude/scratchpads/` for a multi-file refactor that spans a session.
- **`vault-filing-discipline`** — a refactor that surfaces a cross-project structural lesson warrants a vault note (decision or learning).

## Full reference

The canonical seven-step playbook with the combinatorial characterization recipe, the double-edged-axis traps, the per-step anti-patterns, and the `add()` worked example lives at:

[`vault/reference/2026-05-23_refactoring-discipline-playbook.md`](../../vault/reference/2026-05-23_refactoring-discipline-playbook.md)

Read it on first fire of this skill in any new session, or when you hit a step you're unsure how to apply. The summary above is enough for the common case; the full playbook is the deep reference.
