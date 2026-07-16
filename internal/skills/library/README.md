# Core skills library (embedded / bundled)

This directory is corpos's **core skills** tier: universal agent-OS disciplines that
ship *inside the binary* (`//go:embed library` in `../embed.go`), so a fresh install
on any machine injects them with no external skills tree present.

corpos resolves a profile's `skills = [...]` through two layers (`skills.Loader`):

| Tier | Where | What belongs here | Precedence |
|------|-------|-------------------|------------|
| **Core / bundled** | embedded here (`internal/skills/library/<slug>/SKILL.md`) | **universal agent disciplines** — bug filing, vault filing/pull, planning, debugging, review-request, content routing, scratchpad, the cross-language coding philosophy/standards | baseline |
| **User-specific** | on-disk overlay (`-skills-dir`, default `~/.claude/skills`) | **the user's stack** — language conventions (`go-conventions`, `rust-conventions`, `python-conventions`), platform workflows (`github-*`, gitea), framework conventions (`expo-`, `godot-`) | overrides core per slug |

## The rule (enforced, not just documented)

**A builtin profile may reference only a CORE (embedded) skill.** The profilehooks
invariant test (`internal/profilehooks/skills_invariant_test.go`) fails the gate if a
builtin profile names a skill that isn't embedded here. This keeps a fresh corpos
install coherent: its profiles never depend on the particular user's conventions.

User-specific skills are layered in by the *operator*, not baked into corpos:
- drop the skill in the overlay tree (it already lives in `~/.claude/skills`), and
- reference it from a **user profile** (a profile loaded via an on-disk profiles dir),
  not from these builtin profiles.

At runtime `Select` skips an unknown slug, so a user profile that names
`go-conventions` injects it when the overlay provides it and silently no-ops when it
doesn't — no hard dependency.

## Adding a core skill (extensible — no code change)

1. `mkdir internal/skills/library/<slug>` and add its `SKILL.md` (frontmatter `name:`
   MUST equal `<slug>` — the loader keys `Select` on the frontmatter name).
2. Reference it from the relevant builtin profile's `skills = [...]` if a profile
   should inject it.

`//go:embed library` picks up the new directory automatically; the well-formedness
test (`embed_test.go`) and the invariant test cover it with no list to maintain.

## Why these are core (the line)

Core = true for *any* corpos install regardless of what the user works on. Language,
platform, and framework conventions are **not** universal — a Go shop and a Godot shop
need different ones — so they are user-specific by definition and stay in the overlay.

## The `## Core` section (tier-1 terse core — author one for every coding-path skill)

A cheap/mid coding worker runs on a narrow context window, so its skills are injected
under a token budget (`skills.SystemPromptWithin`). A skill whose full body doesn't fit
is injected in a **terse tier** instead. The terse tier prefers the skill's authored
`## Core` section; **only when none exists** does it fall back to the description + a
heading outline — which names what the skill governs but loses the actual rules.

**The convention: every coding-path skill carries a top-level `## Core` section** — the
tier-1 load-bearing rules, the ones a worker must follow as written, terse enough to fit
a floor worker's per-skill share (**~300–500 tokens**). The injector emits it **verbatim**
on a narrow window, so it must stand alone: the rules themselves, not a summary of the
prose and not a table of contents.

Authoring rules:

- The heading is **exactly** `## Core` (case-insensitive; no trailing words — `## Core
  principles` does **not** match the extractor).
- Extraction runs from `## Core` to the next `## ` / `# ` heading, so the section may use
  `### ` sub-headings and lists, but must not contain another `## ` mid-core.
- Keep it **genuinely load-bearing** — the binding rules, not background. Mechanisms,
  worked examples, and rationale stay in the full body below it / in `references/`.
- Size target ~300–500 tok (~1200–2000 chars). Don't grow the floor's skill budget to fit
  a full body; the whole point is a terse core. Un-cored skills keep the outline fallback.

The extraction contract lives in `internal/skills/skills.go` (`coreSection` / `terseBody`);
`skill_core_test.go` asserts the high-traffic coding skills here each carry a real `## Core`
that the injector returns (not the fallback) and that it fits the budget with headroom.
The `~/.claude/skills/_template` copy-target carries a `## Core` placeholder so new skills
author one by default.
