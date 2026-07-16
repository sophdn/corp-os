---
name: bug-fixing-discipline
description: "Methodology for fixing a bug end-to-end — from picking up the bug record through stamping the resolution. Codifies the 12-step root-cause playbook (canonical at vault/reference/2026-05-22_root-cause-diagnosis-playbook.md), the four load-bearing reflexes (two paths disagreeing = invariant violation; audit before fixing; regression test first; stamp on resolve), the anti-patterns list, and the acceptance criteria for declaring a fix done. Decision per author 2026-05-22: full playbook applies to every bug, not only data-divergence shape — non-applicable steps collapse trivially. Cross-project: works in any repo whose `.mcp.json` mounts toolkit-server."
triggers:
  - fix bug
  - fix the bug
  - fix this bug
  - fix a bug
  - fix bugs
  - fixing bug
  - working on bug
  - work on bug
  - work on a bug
  - investigating bug
  - investigate bug
  - diagnose bug
  - diagnose the bug
  - root cause
  - root-cause
  - rootcause
  - patch this bug
  - bug-fixing
  - bug fixing
  - regression test first
  - test before fix
  - audit the pattern
  - audit before fixing
  - audit siblings
  - audit-before-fixing
  - watch it fail
  - watch the test fail
  - fail then pass
  - small frontend bug
  - small visible bug
  - off-by-one
  - two paths disagree
  - two sources disagree
  - sources disagree
  - paths disagree
  - cheap framing
  - hides a backend
  - looks small but
  - before I fix this
  - before fixing this
  - ship the fix
  - what did my fix expose
  - metric worsened
  - metric got worse
  - production smoke
  - agreement check
  - snapshot the db
  - while we're here
  - patch this for now
  - quick patch
  - quick-patch
  - just rename
  - just bump
  - just swap
  - alphabetical accident
  - happens to work
  - undeclared contract
  - bug_resolve
  - bug resolve
  - resolving bug
  - resolve the bug
  - mark the bug fixed
  - stamp the commit
  - stamp commit
  - stamp the sha
  - retro-stamp
  - retro stamp
---

# Bug-Fixing Discipline

Process for taking a bug from "I'm about to fix this" through "stamped, tested, audited, and shipped." Loads the canonical 12-step playbook at [`vault/reference/2026-05-22_root-cause-diagnosis-playbook.md`](../../vault/reference/2026-05-22_root-cause-diagnosis-playbook.md) on trigger; this SKILL body carries the discipline summary + acceptance criteria + cross-skill graph.

Cross-project: lives at `~/.claude/skills/` (user-level), so any repo whose `.mcp.json` mounts toolkit-server benefits. The companion to `bug-filing-discipline` (which covers the file-and-resolve side); this skill covers the fix-it-in-between side.

## When this skill fires

Any time you're about to work on fixing a bug — read it, investigate it, root-cause it, write the patch, run the regression test, or call bug_resolve. Fires on:

- An agent or user phrase that names a bug-fixing action ("fix bug X", "work on bug Y", "diagnose bug Z").
- A symptom-report phrase the playbook calls out as canonical ("small frontend bug", "two sources disagree", "off-by-one count").
- A reflex phrase from the discipline itself ("regression test first", "audit the pattern", "stamp the SHA").
- An anti-pattern phrase that signals a shortcut about to be taken ("just rename", "while we're here", "quick patch") — the skill fires precisely to interrupt these.
- A bug_resolve / bug_reopen call — last chance to verify the discipline was followed before the resolve lands.

If you're invoking `mcp__toolkit-server__work` with action `bug_resolve`, ask yourself the acceptance checklist below before sending.

## Decision: full playbook applies to every bug

Author 2026-05-22: we considered a tier system (full playbook for data-divergence; lite version for paper-cuts). Chose **full playbook for every bug**. Reasons:

- Steps that don't apply collapse trivially (a doc-fix bug has no "second backend path" to compare against — step 1's check is "is the doc on disk the same as the doc rendered?" and resolves in seconds).
- Step 5 (audit the pattern), step 7 (regression test first), step 12 (stamp the SHA) are universally load-bearing. A discipline that lets agents skip them on "small" bugs leaks quality on the bugs we care most about.
- The 12-step framing creates a ratchet: even a 30-second paper-cut fix gets a regression test and an audit grep. This is how the codebase compounds.

**Every step gets a line in the resolution_note** — or the audit failed. If a step genuinely doesn't apply, write the one-sentence acknowledgment ("step 9 N/A — no production data, this is a build-script bug"). Silent skips are the anti-pattern. Bug 549's smoke-test resolve is the model: 12 numbered lines, one per step, including the N/A ones.

## The four load-bearing reflexes

Internalise these; everything else is downstream.

1. **Surface vs source mismatch = invariant violation.** Three specific shapes to watch for:
   - *Data-divergence shape:* "Two paths to the same data return different answers" (MCP says X, dashboard says Y). Treat as a bug in the data layer until proven otherwise.
   - *Silent-effect shape:* "A tool returned ok but you can't verify the effect actually landed" (bug_resolve returned `{ok:true}` but the resolution_note was silently dropped). Treat as a bug at the API boundary until proven otherwise — likely a paraphrase mismatch, alias gap, or unrecognized-param drop.
   - *Stale-record shape:* "The bug report describes a problem that no longer exists." The bug record itself is the surface; the code / runtime is the source. Common when a fix landed but never got stamped, or when a related arc rewrote the cited code path. **Always perform a staleness check before believing the bug body** — grep for the cited symbol, run the cited repro, query the live system. If the symptom is gone, the right disposition is `bug_resolve(commit_sha=<git-log-found-sha>)` not a new fix attempt. Two retro-stamps surfaced via this check in the discipline's first run (#671, #505); see [`vault/learnings/general/2026-05-22_identifying-retro-stamps-in-bug-reports.md`](../../vault/learnings/general/2026-05-22_identifying-retro-stamps-in-bug-reports.md) for the full retro-stamp workflow.
   All three shapes share the principle: when the surface report doesn't match what the backend / code / runtime actually does, the bug is at the boundary between them, not in the surface.
2. **Audit before fixing.** Once you've named the mechanism, grep the codebase for siblings. Fix the pattern, not the instance. The instance you saw is almost always 1 of N. Sibling actions (e.g. bug_resolve → suggestion_resolve) are the most common audit hit; sibling tables and sibling endpoints follow. *Sub-reflex (parity hunt):* when the audit reveals a JSON / spec / source-of-truth paired with code that should mirror it (e.g. `runtime-affecting-paths.json` ↔ advisor case statement), grep for an existing parity test (`scripts/test-*-parity.sh`, `*.parity.test.ts`, etc.) before writing a new contract test. Extending an existing pin is stronger than adding a fresh one — the existing test already wired into the regression gate.
3. **Regression test first; watch it fail.** Write the test against the visible bug, run it, confirm it fails. Then fix. The fail-then-pass cycle proves three things at once: the test catches the bug, the fix addresses what the test catches, the test will catch the bug if it returns. *Bonus:* the failure mode of the test often reveals where the data actually lives (bug 549's test failed with `NULL scan error` on `json_extract`, exposing that resolution_note lives in the event payload, not the projection column — saved the wrong assertion).
4. **Stamp on resolve.** The bug record names every commit in the resolution. Future agents reading the bug get the full diagnostic chain in one read.

## Scope-creep watch (sibling discipline to reflex #2)

The "fix the pattern, not the instance" principle from step 6 can pull you to expand a fix beyond what was filed. Counterbalance: **if the audit found paraphrases, variations, or wider-class extensions beyond the filed scope, file a follow-up rather than broadening the current fix.**

Concrete test: would the broadening change the bug's title? If yes, it's out of scope — file the variant separately. The current fix should close the bug as filed; the follow-up captures the wider design call for separate consideration.

Worked example (bug 549): audit found `notes` paraphrase (filed scope) AND `resolution_summary` paraphrase (out of scope — the bug only named `notes`). Added the `notes` alias, closed #549, filed `bug-resolve-also-silently-drops-resolution-summary-paraphrase-not-just-notes` as the follow-up. The wider paraphrase set becomes a separate design decision rather than scope creep in the current commit.

Apply this watch specifically when reflex #2 surfaces siblings — distinguish "same-action-other-instance" (in scope) from "same-bug-class-different-instance" (out of scope, follow-up).

## Anti-patterns (auto-fail the discipline)

From the playbook, repeated here so the skill body alone is enough to catch them:

- **Patching the symptom.** "Just rename X so it sorts right" hides the contract; the next refactor reintroduces the bug.
- **Skipping the audit.** Fixing one instance and leaving the sibling instances to break later wastes the diagnostic context you just built up.
- **Skipping the production smoke.** Unit tests prove the local fix; only production data proves the systemic fix. Many bugs are bugs only at scale.
- **Rolling back when the metric worsens after a fix.** That metric got worse because your fix is right and exposed a sibling bug. Diagnose forward.
- **Single mega-commit for layered fixes.** Each commit should carry one diagnosis + one fix + one regression test. The git history becomes a debugging document.

If you find yourself reaching for any of these, stop and re-read the playbook step that addresses the underlying urge.

## Acceptance: a fix is "done" only when

Before calling bug_resolve, confirm:

- [ ] **Root cause is named in prose** (not just "patched"). The resolution_note should say *what was broken* and *why*, not just *what file was edited*.
- [ ] **Regression test was written before the fix**, watched to fail, then passes post-fix. Test path + test name belong in resolution_note.
- [ ] **Audit-the-pattern step ran.** Even if no siblings were found, the resolution_note should say "audited `grep <pattern>` across <scope>; N siblings found, all addressed" or "no siblings found".
- [ ] **Bug stamped with all commit SHAs** in the resolution (multi-commit fixes are normal per step 10; stamp each one).
- [ ] **Production-data smoke confirmed the bug's symptom at scale** (where measurable). For data-divergence bugs: agreement-check SQL pre-fix N rows, post-fix 0. For API-ergonomics / silent-drop bugs: SQL count of historical events / rows showing the symptom shape, with post-fix expected to decrease the rate going forward (historical NULLs often can't be backfilled — write-once events, immutable audit rows). For **self-consistent fixes** (where the fix lives in the verification / build / classification pipeline itself — e.g. the post-commit advisor classifies its own diff, the pre-commit hook gates the fix to its own gate config): the post-fix commit's own behavior under the new system IS the smoke. Cite that explicitly in the resolution_note ("commit X classified by the fixed advisor as Y — correct under the new rule"). For bugs with no measurable surface: write the N/A acknowledgment.
- [ ] **If the fix is non-trivial:** 1-2 vault notes filed per playbook step 12's spirit (the diagnostic chain is worth preserving for the next agent who hits a similar shape).

If a step legitimately doesn't apply, **say so explicitly in the resolution_summary**. Skipping silently is the anti-pattern.

## Boundary: this skill vs related skills

- **`bug-filing-discipline`** — the filing side (severity rubric, surface tags, resolve-state taxonomy). bug-fixing-discipline picks up after a bug is filed and runs through to resolve.
- **`systematic-debugging`** — the 4-phase diagnosis methodology. bug-fixing-discipline wraps it: diagnosis is steps 1-5 of the 12-step playbook. Use systematic-debugging when the bug-fixing flow hits the "I don't yet know what's wrong" phase.
- **`coding-philosophy`** — cross-language principles (regression-gate ratchets, structured errors, no escape-hatches, sans-IO testing). bug-fixing-discipline is one specific application of those principles. Defer up for any rule whose underlying principle is language-agnostic.
- **`vault-filing-discipline`** — post-fix outputs go through this skill. The 1-2 vault learnings per significant fix follow the standard vault-note frontmatter + routing rules.
- **The per-language convention skills** (`go-conventions`, `rust-conventions`, `code-standards`, `python-conventions`, `expo-conventions`, `godot-conventions`) — each carries the "Pre-coding recon (first-touch trigger)" section that applies when a fix touches a package you haven't read this session. Fire alongside this skill on new-package bug-fixes.

## Boundary: this skill vs the filing-vs-fixing decision

The vault decision [`decisions/2026-05-19_filing-vs-fixing-boundary-for-autonomous-agent-action.md`](../../vault/decisions/2026-05-19_filing-vs-fixing-boundary-for-autonomous-agent-action.md) governs **when** an agent may autonomously fix vs surface-for-confirm — filing actions are permissive (auto-execute OK at high confidence), fixing actions are conservative (surface for confirm regardless of confidence). This SKILL governs **how** to fix once authorized — by user direction, by a chain task, or by the rare counter-pattern (formatter, codemod-with-tests, generated-file regen) where the verification surface lets fixing auto-execute safely.

In short: that decision says *whether* to act; this skill says *how* to act. An agent that has been directed to fix a bug should load this skill; an agent considering whether to fix autonomously should consult the decision first, and only then load this skill if the answer is yes.

## Full reference

The canonical 12-step playbook with the worked example, the "when this playbook applies" framing, and the per-step anti-patterns lives at:

[`vault/reference/2026-05-22_root-cause-diagnosis-playbook.md`](../../vault/reference/2026-05-22_root-cause-diagnosis-playbook.md)

Read it on first fire of this skill in any new session, OR when you hit a step you're unsure how to apply. The summary above is enough for the common case; the full playbook is the deep reference.
