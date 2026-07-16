---
name: code-standards
description: "Cross-language coding standards — TypeScript/frontend conventions, script authoring (Python/Bash/Rust routing), code audit framework, and repo patterns."
triggers:
  - script
  - scripts
  - one-shot
  - code audit
  - code review
  - audit framework
  - repo bootstrap
  - scaffold
  - new repo
  - language routing
  - python script
  - bash script
  - coding standards
  - typescript
  - react
  - solidjs
  - component pattern
  - frontend conventions
  - tsx
  - props
  - hooks
  - createSignal
---

# Code Standards (Cross-Language)

Sophi's non-Rust-specific coding standards. For Rust-specific conventions, see the `rust-conventions` skill. These cover TypeScript/frontend, script authoring, code audits, and repo scaffolding.

## Core

TypeScript / frontend / scripts (defers up to `coding-philosophy` for the cross-language principles):

- **Defer up first.** Any rule whose principle isn't language-specific lives in `coding-philosophy` — read it for regression-gate, strict-typing, structured-errors, sans-IO, no-escape-hatches.
- **TS strict.** `strict: true` + `isolatedModules` + `noUnusedLocals`/`noUnusedParameters`. No `any` in production. Named exports only; no `export *` from internal modules.
- **Errors:** a custom `Error` subclass per failure domain (`ApiError`, `ValidateError`) with a `name` field + additive context; never throw bare strings.
- **Components:** match the nearby shape — props destructuring, named export, design-token references (no hardcoded colors/spacing). Structural tests via testing-library; journeys via Playwright; no Mock Service Worker.
- **Scripts route by job:** Bash for glue (`set -euo pipefail`), Python for logic (ruff + pyright + black), Rust for anything that must not break. One-shot scripts clean up after themselves.
- **Audits:** every finding carries severity + location + what's wrong + what to do.
- **Pre-coding recon (first-touch):** before the first new line in an unfamiliar package, skim the eslint/tsconfig/prettier (or ruff/pyright) config, read 2–3 nearby call sites, and `vault_search` non-trivial topics. Cost is minutes; skipping it costs hour-plus lint-thrash arcs.

## Boundary: this skill vs `coding-philosophy`

`coding-philosophy` carries cross-language *principles* — regression gates, strict typing tiers, structured errors, sans-IO testing layers, no escape-hatches. **Read `coding-philosophy` first for any rule whose underlying principle is not language-specific.** This skill restates the principles in TypeScript- and Python-/Bash-script mechanism form, plus carries cross-language *application* concerns (the audit framework, script lifecycle conventions) that don't fit cleanly under any single language skill.

The deferral graph: `code-standards → coding-philosophy`, with `expo-conventions → code-standards → coding-philosophy` for the Expo / RN layer one hop further. See [`~/.claude/vault/decisions/2026-05-10_extract-coding-philosophy-base-skill.md`](../../vault/decisions/2026-05-10_extract-coding-philosophy-base-skill.md) for the full graph + rationale.

The audit framework's cross-applicability question (it covers Rust, Python, TS, Shell, Markdown — could in principle live in `coding-philosophy`) is open; for now it stays here. If Rust-side audits start needing it surfaced from `rust-conventions`, the framework can move up.

## Pre-coding recon (first-touch trigger)

**Fires** the first time this session you're about to author TS / TSX / JS / Python-script / Bash-script code in a package or directory you haven't already read, OR you reach for an unfamiliar third-party library.

**Minimum recon, before the first line of new code:**

1. Skim the relevant lint/format config: TS → `.eslintrc*` or `eslint.config.{js,mjs,ts}` + `tsconfig.json` + `.prettierrc`; Python script → `pyproject.toml [tool.ruff]` + `[tool.pyright]` if present; Bash → existing scripts' `set -euo pipefail` posture and shellcheck directives.
2. Read 2-3 nearby files that exercise the API you're about to use. Component shape (props destructuring, named vs default export), error class shape, and design-token references should match what's already there.
3. If the topic is non-trivial (state management, async/Promise discipline, design tokens, accessibility, retry policy, observability hook), `vault_search` for prior decisions before designing.

**Why:** lint thrash, hand-rolled reimplementations of utilities the codebase already exports, and design-token drift (hardcoded colors, ad-hoc spacing) all cluster around skipped first-touch recon. The cost of recon is minutes; the cost of skipping is hour-plus iteration arcs. See toolkit bug #700 for a worked example.

The trigger condition matters more than the depth — match recon to the change shape. A copy-tweak in a familiar component doesn't need recon; a new component or new API call in an unfamiliar package does.

## References

- `references/typescript-style.md` — TypeScript conventions: strict mode, interface-first, naming, JSDoc, no-any, error handling, lint rules, formatting
- `references/frontend-components.md` — Framework-aware component patterns for React and SolidJS: props, hooks/signals, state, testing, API layers, design tokens
- `references/script-conventions.md` — Language routing (Python/Bash/Rust), lifecycle discipline, cleanup rules
- `references/code-audit.md` — Audit structure: every finding has severity, location, what's wrong, what to do

## TypeScript Conventions

### Core Config
- `strict: true` always. Plus `isolatedModules`, `noUnusedLocals`, `noUnusedParameters`.
- Interface-first for object shapes; `type` for unions and computed types. No `I-` prefix on interfaces.
- No `explicit any` in production code. `unknown` + narrowing instead. `any` tolerated in tests.

### Naming
- Components: `PascalCase`. Functions/hooks: `camelCase`. Constants: `SCREAMING_SNAKE` or `camelCase`.
- Hooks: `use` prefix. Props: `ComponentNameProps`. Hook returns: `UseHookNameReturn`.
- Files: `PascalCase.tsx` for components, `camelCase.ts` for utilities.

### Framework-Specific

**React (19+):** ref-as-prop on reusable components (React 19 deprecates `React.forwardRef`). Props destructured with defaults. Named export only. Custom hooks return typed objects (not tuples). Component-per-directory pattern (`ComponentName/index.tsx` + `styles.ts`).

**SolidJS:** Named function exports (not arrow). NO props destructuring (`props.x` preserves reactivity). `createSignal` for local state, `createResource` for async. Co-located `.test.tsx` files.

### Shared
- Design tokens as single source of truth (theme object or CSS custom properties). No hardcoded colors/spacing.
- i18n: all user-facing strings through `t()`. No bare strings in JSX.
- Typed API layer: thin wrappers over transport (fetch/invoke), one file per domain.
- Formatting: Prettier with semis, double quotes, trailing commas, 80-width, 2-space indent.

See `references/typescript-style.md` and `references/frontend-components.md` for full detail.

## Script Authoring Conventions

### Language Routing

Three buckets:

- **Python** — One-shot scripts, API calls, data munging, batch processing. Anything that doesn't persist as a tool. Examples: embedding runs, CSV transforms, API smoke tests, migration scripts.
- **Bash** — Glue between existing tools. File operations, pipeline orchestration, bulk renames, git batch ops, CI helpers.
- **Rust** — Anything that becomes a workspace crate, MCP tool, or pre-commit enforcer. At that point, `rust-conventions` applies — it's no longer a "script."

Rule of thumb: if it runs once and you throw it away, Python. If it runs on every commit or becomes an MCP tool, Rust. Bash is for glue.

### Script Location

Scripts live in `scripts/` at the project root. Create the directory when the first script is written.

### Script Lifecycle

Every script has an implicit or explicit lifecycle:
- **delete-after-use** — default for undocumented scripts. Delete when the task is done.
- **keep** — permanent utility.
- **keep-until: \<condition\>** — keep until a concrete trigger is met, then delete.

Scripts without documentation are assumed delete-after-use. This prevents `scripts/` from accumulating without a lifecycle lever.

## Code Audit Framework

### Finding Structure

Every finding has exactly 4 fields:
1. **Severity** — `blocking` (must fix before merge), `debt` (should fix, not urgent), `nitpick` (style/preference)
2. **Location** — exact file and line range
3. **What's wrong** — the problem, stated precisely
4. **What to do** — the fix, stated concretely

### Language-Specific Focus Areas

**Rust:** ownership/lifetime issues, unnecessary clones, unwrap on fallible paths, missing error propagation, clippy violations, dependency bloat.

**Python:** type hint gaps, bare excepts, mutable default arguments, import organization, missing docstrings on public functions.

**TOML/Config:** schema drift, deprecated keys, inconsistent naming.

**Shell/CI:** unquoted variables, missing `set -euo pipefail`, hardcoded paths, missing error handling.

**Technical Markdown:** broken links, stale references to moved/renamed files, code blocks with wrong language tags.

### Reuse Scan

Flag duplicated code blocks (>10 lines of near-identical logic in two or more locations). Name both locations and suggest extraction.

### Voice

Dry, precise, slightly sardonic. Findings are actionable, not diplomatic. "This will break" not "this might potentially cause issues in some scenarios."

## Authoring a new conventions skill

When a new language or framework needs its own convention skill (e.g. a future `python-conventions`, `go-conventions`, `tauri-conventions`), copy the template at [`~/.claude/skills/_template/`](../_template/) and fill it in. The template carries the required-section skeleton from chain `convention-skills-consolidation-readiness-audit` T2: frontmatter, title, intro, References list, plus optional sections (Boundary, Headline rule, body sections, Pitfalls, Drift catalog) wrapped in include/delete decision-rule comment guards.

```bash
cp -r ~/.claude/skills/_template ~/.claude/skills/<new-skill-name>
```

Defer up to `coding-philosophy` for any cross-language principle the new skill restates. The deferral note + graph at this skill's `## Boundary` section is the model.

## Pitfalls

- Don't over-classify: if a finding is clearly blocking, say blocking. Hedging everything as "debt" to be polite defeats the severity system.
- Script lifecycle is a convention, not a gate. Its purpose is to reduce confusion when someone encounters old scripts, not to slow down script writing.
- Language routing is about lifecycle destiny: will this code persist? If yes → Rust. If no → Python. If it's glue → Bash. Don't overthink it.
