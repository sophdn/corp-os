# Corp-OS (`corpos`)

A local, **owned** agent operating system in Go. Corp-OS owns the agent loop and drives the
existing `toolkit-server` over MCP (client-first), migrating organs to native in-process Go
later. The model stays rented; everything around it is ours.

> Name: **Corpus** (the owned *body* / body-of-work / event-sourced ledger) + **OS**.
>
> **Agents:** read [`CLAUDE.md`](./CLAUDE.md) first — it carries the gate, invariants, runtime
> deps, and design-corpus pointers.

## Status

**Phases 0/A/B/C done.** corpos completes tool-using turns end-to-end against the real
toolkit-server — multi-tool, on local Qwen *and* Anthropic Haiku (same loop, swap `-provider`).
It also runs in rootless Podman: a CGo-free distroless image drives a *containerized*
toolkit-server over MCP, shipped with Quadlet/systemd-user units, and every commit passes the
dialed-to-11 gate (95% coverage floor) — which builds and smoke-tests that image. **Phase D
(one tool-call ABI across providers, validated on three models) is next.**

## Run

```bash
# needs toolkit-server on :3001 (canonical post-flip) and a local OpenAI-compatible model on :8081
go run ./cmd/corpos -prompt "what is the state of chain agent-os-bootstrap?"

# Anthropic (source the key first; never commit it)
. ~/.config/corpos/corpos.env
go run ./cmd/corpos -provider anthropic -prompt "..."
```

## Layout

```
cmd/corpos/        # CLI entrypoint
internal/
  tool/            # provider-agnostic tool-dispatch seam (Call/Result/Spec/Provider)
  mcp/             # toolkit-server client (net/http over POST /mcp/<surface>)
  model/           # Adapter + Echo + OpenAICompat (Qwen/DeepSeek) + Anthropic
  router/          # per-turn model selection (two-call contract + fallback)
  cost/ hooks/ skills/ session/   # ported bridge-harness organs
  agent/           # the loop (the kernel)
```

## Gate

`scripts/gate.sh` is the single entrypoint (pre-commit hook via `core.hooksPath=.githooks`, also
the CI-equivalent): `gofmt -s` · `go vet` · golangci-lint · govulncheck · `go build` ·
`go test -race` with a 95% coverage floor on `./internal/...`. Dev tools run pinned via
`go run <tool>@<ver>`; `go.mod` stays minimal (CGo-free). After cloning:

```bash
./scripts/install-hooks.sh   # wires core.hooksPath=.githooks
```

## Design corpus

In the Obsidian vault under `ideas-to-process/`: the design-orientation doc (locked strategy +
roadmap), the Go-port blueprint, and the Phase-0 deliverables (MCP surface, client choice). See
[`CLAUDE.md`](./CLAUDE.md) for the full list and the work-ledger chains.
