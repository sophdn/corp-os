---
name: scratchpad-discipline
description: "Persistent agent working memory for chain-task sessions and bug investigations. File markdown at ~/.claude/scratchpads/sessions/<chain-slug>.md (for chains driven via mcp__toolkit-server__work) or ~/.claude/scratchpads/bugs/<bug-slug>.md (for bug investigation across more than a few tool calls). Holds todos, hypotheses, repro details, anchors, things-tried, resume hints — anything intra-session/intra-bug that would otherwise live only in the conversation buffer. Scratchpad is canonical for chain-level / cross-session state; updates land at concrete checkpoints (after every commit, after every plan-changing discovery, before any >15-min subtask, before significant user status messages) — NOT just at session boundaries. **Task* (TaskCreate/TaskUpdate/TodoWrite) is DENIED** via `permissions.deny` in settings.json — it was the harness's over-firing-reminder trigger (see §Task* note), so the scratchpad is now the sole in-session tracking surface. Scratchpads are NOT vault content — see Contrast-with-vault section. Cross-project: scratchpads live in the shared ~/.claude/ tree, readable by any future session."
triggers:
  - scratchpad
  - session scratchpad
  - bug scratchpad
  - working memory
  - agent memory
  - investigation notes
  - bug hypotheses
  - things tried
  - what did I try
  - resume work
  - continue from last session
  - pick up where I left off
  - chain session
  - bug session
  - don't use TaskCreate
  - don't use TaskUpdate
  - skip TaskCreate
  - task tools reminder
  - task* nag
  - phantom obligation
  - todo list for this session
  - tracking progress
---

# Scratchpad discipline

Two scratchpad surfaces under `~/.claude/scratchpads/`:

- **`sessions/<chain-slug>.md`** — when working a chain via
  `mcp__toolkit-server__work` (chain_status, task_start, task_complete,
  etc.).
- **`bugs/<bug-slug>.md`** — when investigating a bug across more than a
  few tool calls.

Both are persistent agent working memory written as plain markdown.
They survive conversation compaction; they survive session end; any
future session can `Read` them and pick up. The conversation buffer
and the harness's local Task* list do neither.

## When to create

### Session scratchpad (chain work)

Create when:
- The user asks you to work a chain via `mcp__toolkit-server__work`.
- You're about to track multi-task progress and would otherwise reach
  for `TaskCreate`.

Path: `~/.claude/scratchpads/sessions/<chain-slug>.md`.

Check for an existing file first — prior sessions on the same chain
may have left context. If one exists, `Read` it before any other work
and append to it rather than overwriting.

### Bug scratchpad (bug investigation)

Create when:
- The user points you at a bug (by slug or via `bug_read`) and the
  investigation will take more than a few tool calls.
- You're forming hypotheses, accumulating repro evidence, or trying
  multiple fixes you'd otherwise lose to compaction.

Path: `~/.claude/scratchpads/bugs/<bug-slug>.md`.

Same continuation rule: check first, append on continuation.

### Cumulative-session tripwires (mandatory)

Per-step "this cluster looks small" judgment composes into chain-
shaped work without crossing any tripwire — by the time the session
is 50 edits deep, the conversation buffer is doing the job of a chain
and nothing got tracked. The user has feedback memory
`tracking_needs_forcing_function` saying soft as-you-go cadence loses
to conversation-buffer momentum.

Fix: count cumulatively, not per-step. If ANY of these fire during a
session that has not yet created a scratchpad or proposed a chain,
**pause and propose tracking** before continuing the next batch:

1. **≥3 batched edits to the same artifact** in one session — propose
   a bug scratchpad (if you're chasing one defect) or a chain (if
   the work is feature-shaped).
2. **≥15 triage decisions over a single document** (item-by-item
   sweep of a list/intake doc) — propose a session scratchpad and
   capture the decision-rule in it; consider whether the sweep is
   chain-shaped.
3. **≥40 tool calls** in one logical workstream (one doc, one repo
   area, one bug) without a tracking surface — propose a scratchpad
   regardless of subjective per-step smallness.
4. **≥2 vault notes written or ≥1 in-doc roadmap authored** in one
   session — that's chain-shaped output; propose a chain via
   `mcp__toolkit-server__work` (action=`forge`, kind=`chain`).
5. **A system reminder fires for the 3rd time** in one session AND
   none of the six update-triggers below have fired since the last
   acknowledgment — the reminder is correctly identifying drift;
   create the scratchpad now even if the next subtask "looks small".

"Propose tracking" means: tell the user what surface you'd open
(scratchpad vs chain) and why, then act on their answer. If they
declined chain creation earlier in the session, still open the
scratchpad — it's lighter weight and survives compaction.

Counts reset per session; carry them in working memory (not literally
on disk). The point is to have a numeric tripwire to check against,
not a soft vibe-check.

## Task* — denied (do not reach for it)

`TaskCreate` / `TaskUpdate` / `TodoWrite` are **denied** via
`permissions.deny` in `~/.claude/settings.json`, so they are not in the
tool set — do not try to use them. They were the harness's native
in-session task list, and their "task tools haven't been used recently"
reminder over-fired every ~10 turns because the firing heuristic counts
assistant turns since *those* tools were last used and is blind to the
canonical `mcp__toolkit-server__work` surface. Denying the tools trips
the reminder's tool-presence gate (`getTaskReminderAttachments` returns
early when `TaskUpdate` is absent from the tool set), stopping the nag at
the source.

**Consequence for this skill:** the scratchpad is the *sole* in-session
tracking surface. For in-flight subtask visibility within one chain task
(the old "5+ subtasks" case), use a `## Tasks` checklist in the
scratchpad — it survives compaction and session end, which Task* never
did. Chain-task-level state stays in `mcp__toolkit-server__work`
(chain_status / task_start / task_complete), as always.

Fix record + verification status:
`~/.claude/scratchpads/bugs/task-tools-reminder-overfires-when-mcp-work-tools-are-active.md`,
plus the vault learning note on the source-verified diagnosis.

## What to write

Useful content:

- **Todos** — chain task slugs + status; bug investigation steps.
- **Anchors** — repo paths, commit SHAs, frequently-rerun command
  incantations, env values that affect reproduction.
- **Hypotheses** (bugs especially) — "I think it's X because Y;
  next check Z."
- **Things tried + outcome** — counter-evidence is the most
  compaction-fragile thing. Persist it.
- **Open questions** — anything you'd ask the user but haven't yet.
- **Resume hints** — at session end, a "next steps" block so the next
  session picks up cleanly without re-deriving context.

What does NOT go in:

- **Cross-project insights** → vault (`vault-filing-discipline`).
- **Project-internal documentation** → project repo.
- **Code** → actual code files.
- **Bug records themselves** → toolkit DB via `forge(bug, …)`.
- **Chain/task lifecycle state** → toolkit-server's work surface is
  canonical for that (chain_status, task_start, task_complete). The
  scratchpad mirrors a subset for the agent's convenience; it doesn't
  replace the work surface.

## File format

Plain markdown. Suggested skeleton — adapt freely:

```
# <slug> scratchpad

Started: YYYY-MM-DD
Last updated: YYYY-MM-DD

## Tasks / Investigation steps
- [x] <step> — <result / commit SHA>
- [ ] <step> — <status>

## Anchors
- Repo: <path>
- Key files: <paths>
- Commands: <incantations>
- Env: <values>

## Hypotheses / Notes
- <hypothesis> — <evidence / counter-evidence>

## Things tried
- <thing> → <outcome>

## Open questions
- <question>

## Resume hints
- <what the next session should do first>
```

Bug scratchpads lean on `## Hypotheses` and `## Things tried`. Session
scratchpads lean on `## Tasks`. Use whatever sections fit; the skeleton
is a starting point, not a contract.

## Lifecycle

- **Create** at task/bug-start (or session-start if the topic is clear).
- **Update at concrete checkpoints** (see Update triggers below) —
  NOT "as you go." Aspirational "update as you go" guidance reliably
  leaves the scratchpad untouched mid-flow because the conversation
  buffer is the path of least resistance. Specific trigger conditions
  are the forcing function.
- **At session end**: leave the scratchpad in place. Update the
  "Last updated" timestamp + write a "Resume hints" block. Do not
  delete. Do not clean up. Stale-looking scratchpads are themselves
  a signal (the work was abandoned, or the bug closed, etc.).

Old scratchpads accumulate. That's fine — they're cheap text files
and they constitute a record of agent investigation history that's
useful for retros and pattern analysis.

### Update triggers (mandatory)

Append to the scratchpad at each of these points. Each trigger is a
moment the conversation buffer would otherwise be the sole record of
something durable.

1. **After every commit.** One line: SHA + what shipped + which
   subtask it closed. Especially load-bearing because commits are
   the natural checkpoint between subtask phases.
2. **After every plan-changing discovery.** Examples: "lint config
   forbids `any` outside internal/db — refactoring to typed payloads";
   "jsonschema-go rejects typed structs per issue #23 — need bytes-
   roundtrip"; "T1's catalog under-specifies the actual handler
   surface — needs extension." The kind of finding that, if lost to
   compaction or session end, would force a future you (or future
   session) to re-derive.
3. **After every blocker.** What stuck, what was tried, what's
   needed to unstick. Future-you reads this before retrying.
4. **Before any subtask likely to take >15 min** without a natural
   commit boundary. Write the plan + the expected outcome before
   diving in, so resume hints stay fresh if the session breaks
   mid-subtask.
5. **Before sending the user a significant status message.** If the
   content is worth telling the user, it's worth persisting. The
   conversation buffer compacts; the scratchpad doesn't.
6. **When changing direction.** "Was going to do X, but discovered
   Y, so now doing Z instead." The pivot record is more important
   than either the original plan or the new one in isolation.

If a reminder fires ("task tools haven't been used recently") and
you're tempted to discharge it: check whether any of the six triggers
above have fired since the last scratchpad update. If yes, write the
overdue update. If no, the reminder is genuine noise — acknowledge
and continue.

## Continuation

At the start of any session that might continue prior work:

1. If chain work: `Read ~/.claude/scratchpads/sessions/<chain-slug>.md`.
2. If bug work: `Read ~/.claude/scratchpads/bugs/<bug-slug>.md`.
3. If a scratchpad exists, read it before any other tool call. Its
   "Resume hints" block should tell you where to start.
4. Update it with this session's date in "Last updated" before
   beginning new work.

Listing options:
- `ls ~/.claude/scratchpads/sessions/` — all chains touched.
- `ls ~/.claude/scratchpads/bugs/` — all bugs investigated.
- `grep -r "Resume hints" ~/.claude/scratchpads/` — find scratchpads
  with explicit handoff notes.

## Contrast with adjacent persistence surfaces

Three other persistence surfaces sit alongside scratchpads under
`~/.claude/`. They look superficially similar but each has a
distinct purpose:

| Surface | What goes here | Lifetime model |
|---|---|---|
| **scratchpad** (`~/.claude/scratchpads/`) | Intra-session / intra-bug working memory: todos, hypotheses, things-tried, anchors | File persists, content is session/bug-scoped by intent |
| **vault** (`~/.claude/vault/`) | Cross-project insights: decisions, learnings, reference | Durable, queryable across every project mounting toolkit-server |
| **auto-memory** (`~/.claude/projects/<project>/memory/`) | User preferences + Claude-behavior rules (the harness writes) | Durable, applies to future sessions of the same user |
| **toolkit DB** (via `forge`) | Chain/task/bug records, library entries — work-shaped state | Durable, canonical for work artifacts |

Quick test for which surface a piece of content belongs to:

- *Would another project's agent benefit?* → vault.
- *Would only the same chain or bug's next session benefit?* → scratchpad.
- *Is it a preference about Claude that the user expressed?* → auto-memory (the harness handles it; don't manually write).
- *Is it the existence/state of a work artifact?* → toolkit DB.

Anti-patterns:

- **Don't write scratchpad content into the vault.** Scratchpads
  fail the cross-project test by design; mixing them into the vault
  dilutes the cross-project signal that `vault_search` relies on.
- **Don't write vault content into scratchpads.** Scratchpads are
  not discoverable via `vault_search` and don't participate in
  cross-project queries.
- **Don't write memory-shaped content (user preferences,
  Claude-behavior rules) into scratchpads.** Memory is harness-
  managed. Scratchpads are agent-managed working state.
- **Don't duplicate toolkit-DB state into scratchpads.** Chain /
  task / bug lifecycle lives in the work surface. Scratchpads may
  mirror a subset of task slugs as an agent-convenience todo list,
  but the work surface remains the source of truth.

If something in a scratchpad turns out to generalize cross-project,
copy the insight to vault (via `forge(vault-note, …)`) and cite the
scratchpad as source. Most scratchpad content stays scratchpad
content forever — that's the design intent.

## Interaction with bug-filing-discipline

Bug investigation in a scratchpad is the agent's working memory.
**The bug record itself stays canonical in the toolkit DB** — file,
update, and resolve bugs via `forge(bug, …)` / `bug_resolve` per
`bug-filing-discipline`. The scratchpad is *intermediate* state.

At resolution time, the relevant findings from the scratchpad should
be summarized into the bug's `resolution_note`. The scratchpad is the
exhaustive record; the bug's resolution_note is the durable summary.

A scratchpad without a resolved bug is fine (investigation in
progress). A resolved bug whose scratchpad still has unresolved
hypotheses is a smell — either the bug isn't actually resolved, or
the scratchpad should be updated to mark hypotheses ruled out.

## When NOT to use

- **Single-tool-call work** — no need to set up working memory for one
  command.
- **Casual conversation / planning / exploration** — the conversation
  buffer is fine when there's no execution arc to track.
- **Cross-project content** — see `vault-filing-discipline`.
- **Project-internal documentation** — put it in the project repo.

## Composition

- **Pairs with `bug-filing-discipline`** — that skill governs bug
  records and resolution; this one governs the working memory used
  during investigation.
- **Pairs with `vault-filing-discipline`** — that skill is for
  cross-project content; this one is the explicit intra-session
  complement. Together they cover the persistence surface that the
  harness's ephemeral Task* list does not.
- **Pairs with `content-routing`** — when content-routing says
  "agent's intra-session working memory," this skill is the surface.
