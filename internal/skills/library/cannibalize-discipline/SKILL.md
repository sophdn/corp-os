---
name: cannibalize-discipline
description: Process for reimplementing a rented Claude Code harness tool (Read/Write/Edit/Grep/Glob/Bash/WebFetch/…) as an owned toolkit-server action, by building a characterization-TDD parity net from the tool's actual source, sanitizing the result to a clean-room behavioral spec, gating on coverage, then layering substrate-native upgrades. The harness-swap counterpart to code-migration-discipline; its distinctive moves are source-derived parity nets, clean-room sanitization, and family-coupled deny-list swaps. Cross-project — lives at ~/.claude/skills/ via the mcp-servers skills manifest.
triggers:
  - cannibalize
  - cannibalize the tool
  - cannibalize Read
  - own the harness tool
  - own the Read tool
  - own the Grep tool
  - own the Glob tool
  - own the Edit tool
  - reimplement the harness
  - reimplement Read as
  - owned tooling
  - owned-tooling-easy-targets
  - harness swap
  - stop using Claude Code
  - parity net
  - characterization net for parity
  - parity floor
  - deny-list swap
  - fit to substrate
---

# Cannibalize Discipline

Reimplement a **rented** Claude Code harness tool as an **owned** toolkit-server
action that we control and can fit to our substrate. The thesis (vault decision
`2026-05-29_own-equals-fit-to-substrate-not-clone`): owning a tool is only worth
it if the owned version answers questions the substrate-blind harness tool
cannot — but you earn that only after a byte-for-byte **parity floor**.

This is `code-migration-discipline` specialized to one source surface (the
Claude Code tool source at `~/dev/claude-code/src/tools/`) with three moves that
migration alone doesn't have: a **source-derived characterization net**,
**clean-room sanitization**, and a **family-coupled deny-list swap**.

The repo-specific seam mechanics (how to wire a new surface/action into
toolkit-server, regenerate corpora, etc.) live in
`docs/OWNING_A_HARNESS_TOOL.md`. This skill is the methodology; that doc is the
checklist.

## When this skill fires

A chain or task to own/replace a Claude Code harness tool — anything shaped
"cannibalize / own / reimplement the harness <tool>", the `owned-tooling-*`
chains, or the broader harness-swap program. Not for porting our own code
between languages (that's code-migration-discipline) or restructuring working
code in place (refactoring-discipline).

## The cannibalize playbook

### 0. Locate the source oracle — and confirm it exists

The authoritative oracle is the tool's implementation in
`~/dev/claude-code/src/tools/<Tool>/` plus its helpers (formatters, readers,
limit configs). **Read the source**, not the live tool's rendered output — live
observation is lossy (it can't exercise edge cases the render hides, and not
every harness tool is even mounted in a given session).

If there is **no counterpart** in the source (e.g. there is no LS tool —
directory listing is Bash/Glob there), the owned tool is **self-defined**, not
parity-matched: you design its contract from first principles and skip the
parity net (steps 1–4 collapse to "spec it, test it"). Say so explicitly.

### 1. Build the characterization net FROM SOURCE first (TDD — the gate)

Before writing the owned implementation, encode the source's exact behavior as a
failing/charactering test net. Cover the full input equivalence space the source
distinguishes: output format (byte-exact), parameter/range semantics, encoding
handling (BOM, CRLF), boundary cases (empty, past-EOF, no-trailing-newline),
size/result caps, and any warning/error strings. The net is the contract; the
implementation is built to pass it. Commit a small fixture tree + golden
assertions so the net is reproducible and guards against later drift.

### 2. Model-agnostic carve-out

**PORT** behavior intrinsic to the tool (output format, range semantics,
encoding, byte/result caps, warnings). **DROP** behavior coupled to Claude's
model. Example: the Read tool's token cap (`countTokensWithAPI`, a Claude-
tokenizer roundtrip) is NOT ported — the owned harness is model-agnostic, so a
byte cap is the size guard. Apply this test to any model-coupled logic.

### 3. Implement to pass the net

Write the owned Go action. Default behavior = byte-for-byte parity (predictability
is itself a feature of an agent tool, and parity is the gate for the deny-list
swap). Substrate-native capability is a LATER, opt-in layer (step 8 / the upgrade
pass), never a change to the default.

### 4. Sanitize to a clean-room behavioral spec (do NOT ship source references)

The net was *derived by reading* the proprietary source, but the **shipped
artifacts must not reference it**. Scrub from the owned code, tests, and docs:
- source file paths (`src/utils/file.ts`, `src/tools/FileReadTool/…`),
- internal symbol names lifted from the source (`addLineNumbers`,
  `readFileInRangeFast`, `FileReadTool`, feature-flag names, etc.),
- copied comments/prose that paraphrase the source's internal narration.

Replace each with a **behavioral** description in our own terms ("numbered lines,
unpadded, split on newline, no trailing newline") and our own symbol names. The
owned tool must stand on its own as if specified from its observable contract.
This is clean-room hygiene (IP + independence): the owned substrate cannot
depend on, or advertise, the source it replaced. Keep any "derived from reading
<external source>" provenance in a **non-shipped** scratch note or the chain
record, not in the committed artifacts.

### 5. Coverage gate

The owned tool must carry high code coverage (the characterization net plus
direct unit tests). Run `go test -cover` (or the project's coverage gate) and
hold a high bar — every branch the source distinguishes should be exercised by a
test. Low coverage on a parity floor means the net is incomplete; go back to
step 1. A cannibalization is not "done" while branches are untested.

### 6. Wire the surface

Follow `docs/OWNING_A_HARNESS_TOOL.md` for the repo seams (new surface vs new
action on an existing surface, the gate-enforced lists, corpus regeneration,
doc.go four-field block). doc.go and action-doc prose are shipped artifacts —
apply step 4 sanitization to them too.

### 7. Deploy lifecycle

Own + test in a worktree → `worktree-merge.sh --no-reap` from the main checkout →
post-merge advisor rebuilds + restarts the daemon → `/mcp reconnect` → confirm
the live MCP picked up the new binary (`admin.server_version` vs `git HEAD`) and
the new surface/action works on a deployed binary. A Go change in a linked
worktree does NOT deploy until merged to main.

### 8. Deny-list swap — LAST, family-coupled, global + path-scoped

The harness tool is denied only after the owned replacement is live. Two hard
constraints (learned the hard way; full detail in `OWNING_A_HARNESS_TOOL.md` §3
and feedback memory
`deny-list-swap-mechanics-global-path-scoped-and-read-edit-write-coupled`):

- **Mechanism: a GLOBAL, path-scoped rule** in `~/.claude/settings.json`, e.g.
  `"deny": ["Read(~/dev/mcp-servers/**)"]`. Project-local settings resolve from
  the session's *launch* dir, not the repo, so a repo-local deny silently never
  loads. The global path-scoped rule is cwd-independent, repo-scoped, and
  hot-reloads.
- **Read/Edit/Write are a coupled FAMILY — never deny one alone.** The harness
  Edit requires a prior harness Read, and the owned read does NOT satisfy that
  guard, so denying Read alone breaks the Read→Edit loop for existing files. Own
  the whole read/write/edit family, then deny them together at the END.
- The agent **cannot revert its own deny** (auto-mode self-modification guard) —
  only the user can. Land a deny deliberately.
- **Land a complementary ALLOW for the owned surface together with the deny.** A
  family-coupled harness deny must sanction the owned MCP surface (e.g.
  `mcp__toolkit-server__fs`) in the same change — otherwise the auto-mode
  classifier reads the harness→owned tool-switch as deny-circumvention and blocks
  the owned tools on the very deny-scoped path. The confirmed allow shape is
  **tool-level** (`mcp__toolkit-server__fs`); MCP tools don't accept a path-glob
  the way `Read(<glob>)` does.
- **Land that complementary ALLOW from the USER or a FRESH session — never
  self-edit it mid-build.** Editing your own `permissions.allow` mid-session is
  self-permission-widening: the auto-mode guard then puts THAT session into a
  conservative posture that blocks write-shaped Bash (build/test/`rm`) on the
  deny-scoped repo for the rest of the session. It's session-local and clears on
  a fresh session; the deny edit alone does NOT trigger it — only the allow
  (widening) does. So have the user land the allow, or hand off to a fresh
  session to build/test/commit — do not widen your own perms and then keep
  working in that repo. See bug
  `self-editing-permissions-allow-degrades-the-editing-session`.

### 9. Upgrade pass (the point of owning)

Default stays parity; layer substrate-native capability as OPT-IN modes (event
log, knowledge_pointers, doc.go intended-use, CODEMAP, projections, go/ast for
symbol work since no LSP exists in-repo). Scope each upgrade only AFTER its
parity tech lands — design against concrete code, not a guess.

## The load-bearing reflexes

1. **Source is the oracle; the net comes first.** Never build the owned tool from
   the chain's prose description or live observation — read the source and write
   the net before the implementation.
2. **Parity before cleverness.** The default must match byte-for-byte before any
   substrate upgrade; parity is the deny-list gate.
3. **Sanitize before shipping.** No source paths or lifted symbol names in
   committed artifacts. Provenance lives in scratch/chain records.
4. **Coverage proves the net.** High coverage is the evidence the characterization
   net is complete.
5. **Deny is a family-wide, deliberate, last step.** Never per-tool for a coupled
   family; never from the session that can't un-deny itself.

## Anti-patterns (auto-fail the discipline)

- Building from the live tool's output or the task's prose instead of the source.
- Shipping the owned code/tests/docs with `src/...` paths or the source's internal
  symbol names (clean-room violation).
- Declaring parity "done" with low coverage / an incomplete net.
- Denying a harness tool before its owned replacement is merged + deployed, or
  denying one member of the read/write/edit family alone.
- Changing the default output to add substrate context (upgrades are opt-in).

## Acceptance: a cannibalization is "done" only when

- A source-derived characterization net exists and passes, covering the full
  behavior space (format, params, encodings, boundaries, caps, warnings).
- The owned default is byte-for-byte parity (or, for a self-defined tool, meets
  its first-principles spec).
- Shipped artifacts are sanitized — no source references or lifted symbol names.
- Coverage clears the project bar.
- The surface is wired, gate-green, merged, and verified live on a deployed
  binary.
- (Deny + upgrades may be deferred per the family-coupling and upgrade-after-
  parity rules, but are tracked.)

## Boundary: this skill vs related skills

- **code-migration-discipline** — porting OUR code between languages/architectures.
  Cannibalize is its specialization to the Claude-Code-tool source surface, plus
  sanitization + the deny-list swap.
- **refactoring-discipline** — restructuring working code in place, no behavior
  change. Cannibalize changes the *implementer*, preserving behavior at the floor.
- **OWNING_A_HARNESS_TOOL.md** — the repo-specific seam checklist this skill's
  step 6/7 delegate to.
