# AGENTS.md — working in this repo

Guidance for an LLM (or a human) making changes to Corp-OS. Start with the
[README](./README.md) for *what this is* and [ARCHITECTURE.md](./ARCHITECTURE.md) for *how it
fits together*; this file is the operating manual for changing it.

## Orientation

- **`cmd/corpos`** — the CLI / REPL / inspect entrypoint and composition root (where adapters,
  organs, and profiles get wired).
- **`internal/`** — the kernel and everything around it. The [README package map](./README.md#package-map-internal)
  lists all 33 packages by area; every package leads with a doc comment stating its role — read that
  first.
- **`docs/`** — design docs (the gate, the two-tier green report, the gate-authoring bridge) and the
  MCP substrate contract.

## Build & gate

- Go toolchain, Go 1.26+. Work from the repo root.
- **The gate is the law.** `scripts/gate.sh` is the single entrypoint, wired as the pre-commit hook
  via `core.hooksPath=.githooks`. **After cloning, run `./scripts/install-hooks.sh` once.** Six
  stages, all must pass to commit: `gofmt -s` drift · `go vet` · `golangci-lint` · `govulncheck` ·
  `go build` · `go test -race`, plus a **95% coverage floor on `./internal/...`**.
- Dev tools run pinned via `go run <tool>@<ver>`, so `go.mod` carries no tool deps.
- **Formatting authority:** the toolchain's `gofmt -s` (gate stage 1) is the single source of truth
  — golangci-lint's bundled `gofmt`/`goimports` linters are intentionally disabled (a bundled
  pre-1.26 gofmt disagrees with the toolchain). Run `gofmt -s -w .` before committing.
- The 95% floor runs tight: **new code must bring its own tests.** The suites show the sans-IO
  pattern — injectable transports, `httptest`, temp dirs/DBs.

## Invariants (do not break)

- **CGo-free.** `CGO_ENABLED=0`; pure-Go deps only (e.g. `modernc.org/sqlite`, not `mattn`). This is
  what lets the container image ship on distroless/scratch with no C toolchain.
- **The loop dispatches tools through `tool.Provider`** — never hardcode "tools = the MCP substrate
  over HTTP". This seam keeps native organs, external MCP servers, and multi-agent open.
- **A new `model.Adapter` only when the API *shape* differs** (Anthropic Messages ≠ OpenAI chat).
  OpenAI-compatible providers (Qwen, DeepSeek, OpenRouter) share `OpenAICompat` via config.
- **`internal/session` is LOCAL conversation state, not the substrate ledger.** The work / knowledge
  / fs ledger stays owned by the MCP substrate, reached over MCP; never conflate the two.
- **Coding verdicts are orchestrator-owned, computed from the diff** — never trust a worker's
  self-report that it fixed something. See [ARCHITECTURE.md §5](./ARCHITECTURE.md).

## Running it locally

Corp-OS drives an MCP substrate (`toolkit-server`, a separate service) and a model backend:

- **Substrate:** an MCP HTTP daemon (default `http://localhost:3001`, `POST /mcp/<surface>` with a
  `{action, params}` body). See [`docs/`](./docs) for the contract.
- **Local model:** an OpenAI-compatible endpoint (default `http://localhost:8081/v1`), e.g. Qwen on
  llama.cpp.
- **Hosted model:** set `ANTHROPIC_API_KEY` in your environment (keep it in a file *outside* the
  repo; never commit a key or bake it into an image), then run with `-provider anthropic`.

```bash
go run ./cmd/corpos -prompt "…"                    # local model (default)
go run ./cmd/corpos -provider anthropic -prompt "…"
```

## Git & branch discipline

- Set `origin` to your own remote. End commit messages with a `Co-Authored-By:` trailer crediting any
  agent that did the work. **Every commit gates.**
- **Worktrees only — the main checkout stays on `main`.** A hard pre-commit guard
  (`scripts/guard-worktree-discipline.sh`) refuses any commit on a non-`main` branch in the main
  checkout. Start work with `scripts/worktree-new.sh <slug>` (a linked worktree on its own branch),
  merge back with `scripts/worktree-merge.sh <branch>` (conflict-check → gate → reap), and clear
  stragglers with `scripts/worktree-reap.sh`.
