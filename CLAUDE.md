# corpos — agent conventions (read first)

**Corp-OS** (`corpos`) is a local, owned agent operating system in Go. It owns the agent loop
and drives the existing **toolkit-server over MCP** (client-first); the model stays rented.
Phases 0/A/B/C of the roadmap are **done** — corpos drives the real substrate multi-tool on the
canonical, benchmarked model ladder: **local Qwen (worker) → Gemini-3.1-Flash-Lite (mid/orchestrator)
→ bounded Opus (fallback)**, applied by default for a bare invocation (defined once in
`cmd/corpos/main.go`; Haiku was a bring-up-era stand-in, not the locked fallback). Phase C landed
in chain `corpos-podman-deploy` (id 331, closed 2026-05-31): corpos runs in rootless Podman from a
CGo-free distroless image against a containerized toolkit-server, with Quadlet units committed and
the gate extended to build + smoke the image. **Phase D (one internal tool-call ABI across
providers, validated on three models) is next** — chain `harness-swap-validation` (id 270).

## Build & gate

- Go toolchain at `/usr/local/go/bin` (Go 1.26.3). Work from the repo root.
- **The gate is the law.** `scripts/gate.sh` is the single entrypoint, wired as the pre-commit
  hook via `core.hooksPath=.githooks`. **After cloning, run `./scripts/install-hooks.sh` once.**
  Six stages, all must pass to commit: `gofmt -s` drift · `go vet` · golangci-lint v1.62.2 ·
  govulncheck · `go build` · `go test -race` + a **95% coverage floor on `./internal/...`**.
- Dev tools run pinned via `go run <tool>@<ver>` (golangci `v1.62.2`, x/vuln `v1.3.0`) so `go.mod`
  carries no tool deps.
- **Gotcha:** golangci-lint's `gofmt`/`goimports` linters are **disabled** in `.golangci.yml` —
  v1.62.2 bundles a pre-1.26 gofmt that disagrees with the toolchain. The **toolchain `gofmt -s`
  (gate stage 1) is the single formatting authority.** Run `gofmt -s -w .` before committing.
- The 95% floor runs tight; new code must bring its own tests (sans-IO: injectable transports,
  `httptest`, temp dirs/DBs — the suites show the pattern). Coverage is line-based and cannot see
  which behavioral PATH hit a line — see **`docs/TESTING.md`** for the mandated test categories that
  close that blind spot (the **policy-path matrix**: a router policy gated on `stickyTop` must be
  tested down BOTH the routine and sticky escalation paths AND through the wired spawner, not just
  router isolation — the gap that shipped bug-1165(b)'s broken first cut; and **coding-revise
  convergence** tests via `RunDuty`).

## Invariants (do not break)

- **CGo-free.** `CGO_ENABLED=0`; only pure-Go deps (e.g. `modernc.org/sqlite`, not `mattn`). This
  is what lets Phase C ship on distroless/scratch with no C toolchain. Keep it.
- **The loop dispatches tools through `tool.Provider`** — never hardcode "tools = toolkit-server
  over HTTP". This seam keeps native organs (Phase E), external MCP servers, and multi-agent open.
- **A new model adapter only when the API *shape* differs** (Anthropic Messages ≠ OpenAI chat).
  OpenAI-compatible providers (Qwen, DeepSeek, OpenRouter) share `OpenAICompat` via config (the
  hermes `providers/base.py` lesson).
- **`internal/session` is LOCAL conversation state, NOT the substrate ledger** (flag F4). The
  work/knowledge/fs ledger stays owned by toolkit-server, reached over MCP.

## Layout

`cmd/corpos` (CLI) · `internal/`: `tool` (provider-agnostic dispatch seam) · `mcp` (net/http
client over `POST /mcp/<surface>`) · `model` (Adapter + Echo + OpenAICompat + Anthropic) ·
`router` · `cost` · `hooks` (8-hook surface) · `skills` · `session` (SQLite) · `agent` (the loop).

## Runtime deps (for live smokes)

- **toolkit-server** HTTP daemon at `http://localhost:3001` (canonical post-flip; native :3000 retired)
  (`POST /mcp/<surface>` with `{action, params, rationale?, project?}`). Surfaces: work / knowledge /
  fs / measure / ml / admin. Endpoint defined once at `mcp.DefaultToolkitURL`; see `corpos-toolkit/docs/TOPOLOGY.md`.
- **Local model**: Qwen on llama.cpp at `http://localhost:8081/v1` (OpenAI-compatible).
- **Anthropic key**: `~/.config/corpos/corpos.env` (`export ANTHROPIC_API_KEY=…`, chmod 600).
  `source` it before an anthropic smoke; **never commit a key or bake it into an image.**
- Run: `go run ./cmd/corpos -prompt "…"` (defaults to Qwen) or `… -provider anthropic`.

## Git

Remote = gitea `https://github.com/sophdn/corp-os.git` (CA configured in `~/.gitconfig`;
the credential helper has the token). End commit messages with the
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer. Every commit gates.

**Worktrees only — the main checkout stays on `main`.** Never `git checkout -b` here;
a hard pre-commit guard (`scripts/guard-worktree-discipline.sh`, wired into
`.githooks/pre-commit`) refuses any commit on a non-`main` branch in the main checkout.
Start work with `scripts/worktree-new.sh <slug>` (a linked worktree on its own branch,
gate-only hooks); merge back with `scripts/worktree-merge.sh <branch>` (conflict-check →
gate → reap); clear stragglers with `scripts/worktree-reap.sh`. After cloning, run
`scripts/install-hooks.sh` once. Full discipline: skill **`worktree-workflow`**.

## Design corpus (the WHY) — in the Obsidian vault `~/Documents/files/ideas-to-process/`

- `2026-05-30_design-orientation_own-the-agent-runtime-claude-code-as-client.md` — the LOCKED
  strategy + the Phase 0→F+ roadmap (start here).
- `2026-05-30_corpos-go-port-blueprint_from-bridge-harness.md` — the Go-port blueprint + flags F1–F7.
- `2026-05-30_corpos-T2_toolkit-server-mcp-client-surface.md`, `…_T3_go-mcp-client-choice.md` — Phase-0 deliverables.

## Work ledger (`mcp__toolkit-server__work`, project `corpos-toolkit`)

<!-- The substrate ledger project was renamed mcp-servers -> corpos-toolkit on 2026-06-09
     (finish-sophdn-repo-split T10). corpos's own chains under the separate `corpos` project
     are unchanged. The corpos-* chains below were filed under the old mcp-servers project and
     now resolve under corpos-toolkit. -->


- `agent-os-bootstrap` (Phases 0/A/B) — **CLOSED**.
- `corpos-podman-deploy` (id 331, **Phase C**) — **CLOSED** 2026-05-31. Shipped the CGo-free
  distroless image, rootless `corpos-net` + podman-secret key handling, Quadlet/systemd-user
  units, and the gate's `image-smoke` stage. Chain **315** (`auto-startup-dev-services`), which
  containerized toolkit-server + llama-server, is closed too. Known gaps were filed, not blocking:
  bug `corpos-mcp-defaultspecs-too-thin`; suggestion `make-local-llama-reachable-from-corpos-net`.
  Deferred out of C and since overtaken: the flip making the container canonical landed on the
  toolkit side (it now serves `:3001`). Still open from C's deferral list: the ml→onnx-serve
  sidecar and an interactive corpos REPL.
- `harness-swap-validation` (id 270, **Phase D**) — next. One internal tool-call ABI normalizing
  the providers' differing schemas, run on three models against the containerized stack.

## Landed

- Conversation-message persistence + `-resume` (chain 332).
- The loop persists cost / tool_call / turn rows to the session store (best-effort) and drives
  the escalation contract — wired by the escalation + orchestration-tree work.
- Session inspection: `-inspect <run-id>` renders the sub-orchestration telemetry TREE (per-worker
  cost, escalation edges); `-inspect-session <run-id>` dumps one session's transcript + cost +
  tool_calls detail; `-run-rate` projects the monthly API spend over `-session-dir` within
  `[-since,-until]` (`docs/SWAP_VALIDATION_CRITERIA.md` §5; `internal/runrate`).
