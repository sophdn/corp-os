---
name: coding-philosophy
description: "Sophi's cross-language coding principles — regression gates, strict typing, structured errors, sans-IO testing, no escape-hatches, docs-as-code. Base skill for language-specific convention skills (rust-conventions, code-standards, future python-conventions) to defer up to."
triggers:
  - coding philosophy
  - convention principles
  - cross-language conventions
  - regression gate
  - check-all
  - strict typing
  - sans-io
  - error handling philosophy
  - escape hatches
  - lint baseline
  - structured errors
  - testing philosophy
  - no mocks
  - structural test layer
  - journey test layer
  - docs-as-code
  - public api boundary
  - default visibility
  - coding standards
  - shared conventions
---

# Coding Philosophy

Cross-language principles that govern code Sophi writes regardless of language. Concrete language-specific mechanisms (cargo / clippy / pyright / eslint / black / ruff) live in per-language convention skills (`rust-conventions`, `code-standards`, future `python-conventions`); this skill carries the *principles* that those mechanisms instantiate.

When a language-specific skill is loaded, it should defer up to this skill for the principle layer — that's the deferral graph documented in the vault decision note ([2026-05-10_extract-coding-philosophy-base-skill.md](../../vault/decisions/2026-05-10_extract-coding-philosophy-base-skill.md)).

## Core

The load-bearing rules, language-agnostic (per-language mechanisms live below + in the per-language skill):

1. **Regression gate ratchets up.** One command runs lint + type-check + test; CI fails on it. Tighten over time; never weaken it to land a commit.
2. **No escape-hatches.** No `#[allow]`, `@ts-ignore`, `: any`, `# type: ignore`, `eslint-disable`, or `panic!()`-to-silence-a-Result. Rewrite so the signal goes away.
3. **Strictest type-checker tier, never weakened.** If a strict check fires, the code shape is wrong, not the checker.
4. **Structured errors, not generic.** One error type per failure domain: greppable identity + machine-readable kind + human Display. No giant per-crate enum, no thrown strings.
5. **Sans-IO core, no mocks.** Pure functions on data; I/O at the boundary. Wanting to mock means the function should take data, not a handle. Split structural-unit vs journey-integration tests.
6. **Three test slots per public fn:** positive, ≥1 negative/error, boundary-where-applicable. Multiple cases go through a data-driven `check()` helper, each with a one-line intent comment.
7. **Deterministic tests.** Pin seeds, freeze time, port 0, no ordering deps. A flaky test is broken — fix or delete it.
8. **Docs-as-code at the public-API boundary.** Doc-comment every public item; semantic failure-mode sections; idiomatic-success examples (never `.unwrap()`).
9. **Minimum surface.** Private by default; expose only what callers need.
10. **Lint baseline is a ratchet** — tighten when adding code, never disable a rule to pass.
11. **Errors are data:** `kind: context`, lowercase, no trailing period, structured fields — not a free-form prose string.
12. **Pre-coding recon at the first edit into unfamiliar code:** read the lint config + 2–3 nearby call sites + relevant vault decisions before writing. Skipping it turns the lint baseline into a cleanup queue.

## What this skill is, and isn't

**This skill carries:** decision rubrics that generalize across languages — when to escalate strictness, what shape errors take, what the testing layers are, what makes a regression gate load-bearing. The 11 principles below are the load-bearing ones; each one is restated in language-specific form by every convention skill that defers up here.

**This skill does not carry:** language-specific syntax, library choices, lint-rule names, or build-tool flags. Those belong in the per-language skill — `rust-conventions` for cargo / clippy / rustfmt / cargo-nextest, `code-standards` for ESLint / Prettier / pyright / black / ruff, and so on. When in doubt: if the rule needs a language to even state it, it's a per-language rule, not a cross-language principle.

## Core principles

### 1. Regression gate ratchets up

Every project has one command that runs all checks together (lint + type-check + test). CI fails commits that fail the gate. The gate's strictness ratchets up over time — start sane, tighten with each major task; never weaken to make a commit go through. Without a regression gate, drift compounds invisibly between commits.

Per-language mechanisms: Rust → `cargo clippy --workspace -- -D warnings && cargo nextest run --workspace`. TS / Expo → `npm run check-all` (lint + type-check + test). Python → `black --check && ruff check && pyright && pytest`. Each language's gate has the same shape; only the tools differ. See [`references/regression-gate.md`](references/regression-gate.md) for worked examples.

### 2. No escape-hatches

Don't suppress lints (`#[allow(...)]`, `// @ts-ignore`, `: any`, `# type: ignore[...]`). Don't add `// eslint-disable` for a one-liner. Don't write `panic!()` to silence a Result. Make the code right; rewrite so the lint's signal goes away. The discipline is identical across languages — escape-hatches drift from local-and-justified to global-and-forgotten if not policed at the commit boundary.

Per-language mechanisms: Rust forbids `#[allow(clippy::...)]` per `rust-conventions`. TS forbids `explicit any` in production code. Python's `# type: ignore[...]` is similarly disallowed in production code (test code gets a small grace period). Tests get a slightly larger grace because mocks need flexibility — the rule there is "tolerated, not encouraged."

### 3. Strict typing tier

Use the language's strictest available type-checker tier. Never weaken with overrides. The strict tier is non-negotiable; if a strict-tier check fires, the code shape is wrong, not the type-checker. The principle generalizes whether the language has nominal types (Rust), gradual types (TS, Python), or anything in between.

Per-language mechanisms: Rust uses default `cargo` (effectively strict by construction) plus `RUSTFLAGS=-D warnings`. TS demands `strict: true` plus `isolatedModules` plus `noUnusedLocals` plus `noUnusedParameters`. Python demands `pyright`'s strict mode in the production tree (lenient mode tolerated only in tests). All three: never weaken.

### 4. Structured errors over generic

Errors are domain-shaped: distinct failure domain → distinct type with greppable identity, machine-readable kind, and human-readable Display. The old monolithic `Error` enum or thrown-string pattern is retired across languages. Callers need to match on the kind they care about; debuggers need to read the source chain.

Per-language mechanisms: Rust uses struct-with-kind-enum + `Display` + `source()` chain (one error type per fallible operation; not one giant enum per crate). TS uses custom `Error` subclasses per failure domain (`ApiError`, `ValidateError`, etc.). Python uses custom `Exception` subclasses likewise. The shape principle is identical — the syntax differs.

### 5. Sans-IO + no-mocks + structural-vs-journey testing layers

Two principles enforced together:

- **Sans-IO core.** Pure functions on data; callers handle I/O at the boundary. The testable surface is the pure core. Mocking is the wrong tool — if you find yourself wanting to mock, ask whether the function should take *data* instead of a *handle*.
- **Structural unit + journey integration split.** Unit tests assert structural guarantees (shapes, derived state, error class identity, retry classification, prop unions). Journey tests assert end-to-end flows. Don't try to simulate journeys with unit-test tooling; don't try to test structure with browser-driven journey tooling.

Per-language mechanisms: Rust — `lib.rs` is sans-IO, `main.rs` is the I/O shell; integration tests in `tests/` directory exercise the real public API; no `mockall`. TS / Expo — Jest + testing-library for structural; Playwright for journeys; no Mock Service Worker. Python — pytest unit tests against pure functions; integration tests in `tests/integration/` with real fixtures; no `unittest.mock` for production code paths.

### 6. Testing coverage shape

Every public function needs three test slots: positive path, at least one negative / error path, and boundary conditions where applicable. Multiple cases for the same logic go through a data-driven `check()` helper, not copy-pasted test bodies. Each test fn carries a one-line intent comment.

Per-language mechanisms: Rust — table-driven `check(input, expected)` + `expect_test` for snapshot output. TS — `it()` blocks with `for (const c of cases) { … }` patterns. Python — `pytest.mark.parametrize` for table-driven. Coverage requirement is the same shape; the harness differs.

### 7. Determinism in tests

Pin random seeds, freeze time, avoid port races (use port 0 / OS-assigned), no test-ordering dependencies. A flaky test is broken — fix it or delete it. Test runs that depend on environment state (filesystem layout, current time, ambient services) are integration tests and should be marked / gated as such.

Per-language mechanisms: Rust — `tempfile` for filesystem isolation, `rand::SeedableRng::from_seed`, `std::sync::OnceLock` for shared fixtures. TS — `jest.useFakeTimers()`, port-0 in test servers. Python — `tmp_path` / `tmp_path_factory` pytest fixtures, `freezegun` for time, port-0 sockets. The discipline is identical.

### 8. Docs-as-code at the public-API boundary

Every public function / class / module gets a doc comment. Worked example in the crate-level / module-level intro. Semantic sections for failure modes (`# Errors`, `# Panics`, `# Safety` in Rust; JSDoc `@throws`; Python docstring `Raises:`). Intra-doc links over prose references where the language supports them. Examples use the language's idiomatic-success shape (`?` in Rust, `try/catch` in TS, etc.) — never `.unwrap()` or `// don't actually do this`.

Per-language mechanisms: Rust — `//!` for crate / module, `///` for items, `cargo doc` builds the surface. TS — JSDoc with `@param` / `@returns` / `@throws`. Python — docstrings (Google or NumPy style) with `Args:` / `Returns:` / `Raises:`.

### 9. Default visibility / minimum surface

API is opt-in, not opt-out. Default to private; expose only what callers need. Cross-module reach within a crate is `pub(crate)`. Cross-crate reach is `pub`. Same for TS `export` discipline (named exports, barrel files, no implicit re-exports) and Python `__all__` / leading-underscore convention.

Per-language mechanisms: Rust — `fn` is private by default; `pub(crate)` for internal cross-module; `pub` for crate API. TS — named export only, barrel `index.ts` files, no `export *` from internal modules. Python — leading-underscore prefix for internals; `__all__` declares the public surface in `__init__.py`. The principle: defer-and-curate-the-surface.

### 10. Lint baseline as a meta-discipline

Beyond the per-language lint rules, the meta-discipline is: keep the lint baseline tight, ratchet it up when adding code, never disable a rule to make a commit go through. The lint baseline is the codified version of what "we don't write code that does X" means — it should track the team's evolving sense of taste, not stagnate at "what was lintable in 2018."

Per-language mechanisms: Rust — `clippy` workspace-wide with `-D warnings`; new lints get evaluated, not allow'd. TS — ESLint flat config with `@typescript-eslint` rule set; `no-explicit-any` warning, `prefer-const` error, etc. Python — `ruff` with the `ALL` rule set (per-rule opt-out only when justified) + `black` formatting (no per-line escape).

### 11. Error-context shape

Errors carry context that distinguishes one instance from another: greppable, machine-friendly, lowercase-no-trailing-period, structured (kind + context fields), not just a free-form message string. The error message is for the human reading the log; the kind is for the program reading the response. Both have to be there.

Per-language mechanisms: Rust — `Display` style of `"{kind}: {context}"`, lowercase, no trailing period. TS — `ApiError extends Error { statusCode, … }` with `name` field and additive context fields. Python — custom `Exception` with structured `__init__(self, kind, context, …)` parameters and `__str__()` that renders the same shape. The principle: errors are data, not prose.

### 12. Pre-coding recon at the boundary into unfamiliar code

Before the first edit in a code area new to this session, do a brief survey. The trigger is concrete; the depth is judgment.

**Trigger** — fire when the next planned edit is the first time this session you:
- write or edit in a particular package / crate / module
- define types in a particular language
- invoke a particular third-party library

When the trigger has already fired earlier this session for the same target, don't re-survey — incremental work in already-surveyed code rides the prior recon.

**What to survey** (depth scales with stakes, not a fixed checklist):
- the language's convention skill (which forwards back here for cross-language principles).
- the project's lint config (`.golangci.yml`, `clippy.toml`, `eslint.config.*`, `ruff.toml`, etc.) — its accumulated taste, costly to discover via failed gate runs.
- two or three nearby call sites for the operation you're about to repeat.
- `vault_search` for prior decisions on the surface.

**Lagging indicator that this was skipped:** an iterative-lint session in a previously-unfamiliar package — three passes of *wrote → lint flagged → rewrote* instead of one pass of *checked → wrote → passed*. If the next edit is harder than the language usually feels, that's the signal.

This is distinct from #10 (lint baseline). #10 names *what counts as acceptable*. This principle names *how to learn that baseline before writing against it* — skipping it converts the baseline from a design constraint into a cleanup queue.

## References

- [`references/regression-gate.md`](references/regression-gate.md) — worked examples for principle #1 across Rust, TS, and Python. Shows the `cargo clippy / nextest` shape, the `npm run check-all` shape, and the `black + ruff + pyright + pytest` shape side-by-side. The first reference because regression-gate is the most-load-bearing principle (per chain `convention-skills-consolidation-readiness-audit` T1's framing).

Future references can be authored as a deep-dive becomes useful. Candidates flagged by the audit chain:

- `references/structured-errors.md` — Rust struct+kind, TS custom Error subclass, Python Exception subclass side-by-side.
- `references/sans-io-core.md` — sans-IO design pattern across the three languages with worked examples.
- `references/no-escape-hatches.md` — the per-language disallowance list and the test-grace-period exception.

These don't ship in the initial extraction (T1 surfaced 11 principles; only regression-gate gets a worked-examples reference now). When an agent first needs one of the deferred references, author it in place rather than scaffolding empty docs upfront.

## How language-specific skills relate to this one

Each language-specific convention skill should:

1. **Open with a `## Boundary: this skill vs `coding-philosophy`** section** that names the deferral. The body of the section says: cross-language principles live here; my-language-specific mechanisms live in the body of this skill.
2. **State language-specific mechanisms** for each principle (regression gate command, error shape, lint rule names, etc.) in the skill's body or `references/` subdir.
3. **Not restate the principle text** — just the mechanism. If a future skill author needs the principle, they read this skill.

Today's deferring skills:
- `rust-conventions` (Rust mechanisms)
- `code-standards` (TS-general + script routing + audit framework — note that audit framework is cross-language by design but lives in code-standards rather than here; that's an open question from the audit chain T1)
- `expo-conventions` (Expo / RN-specific layer; defers to `code-standards` first, which defers up to here — two-hop)

Future skills (per chain `convention-skills-consolidation-readiness-audit` T3):
- `python-conventions` — overdue. voice-trainer/backend has 62 Python files with full pyright + ruff + black + pytest tooling that already follows this philosophy; the skill formalizes the mechanisms.
