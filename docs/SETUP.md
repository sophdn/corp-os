# corpos — fresh-machine setup

A from-scratch runbook for standing up **corpos** and the services it needs on a
new machine. Written to be executable by a human or by a local Claude Code agent
helping with the install. corpos is a CLI/REPL agent-OS — it has no GUI of its
own; the optional dashboard is an observability surface for the *toolkit*, not
for corpos.

> **Status of this doc:** authored during collaborator onboarding. The exact
> hosted-LLM flag wiring (§4, Option A) should be confirmed on a first run —
> corpos announces any ladder deviation at startup. File issues against anything
> that drifts.

---

## 1. The stack — what actually has to run

corpos drives a separate **toolkit-server** over HTTP and calls an
**OpenAI-compatible LLM endpoint**. Four repos, all under the `shared/` org on
the homelab Gitea:

| Repo | Role | Needed to run corpos? | Toolchain |
|------|------|----------------------|-----------|
| `shared/corpos` | the agent-OS CLI/REPL itself | **yes** — it's the thing | Go 1.26.4 |
| `shared/corpos-toolkit` | `toolkit-server` backend (the ledger/tool surface corpos calls) | **yes** | Go 1.26.4 (module is in `go/`) |
| `shared/corpos-toolkit-dashboard` | React/Vite observe UI | optional (observability only) | Node 22 |
| `shared/llama-server` | local LLM (llama.cpp + Qwen on `:8081`) | only for the **local-LLM** path | shell + podman |

```
corpos (CLI)  ──HTTP POST /mcp/<surface>──▶  toolkit-server  (:3001 container / :3000 native)
      │                                            owns SQLite ledger
      └──HTTP /v1/chat/completions──▶  LLM endpoint (:8081 local  OR  hosted)
dashboard (browser, optional) ──HTTP/SSE──▶ toolkit-server
```

---

## 2. Prerequisites

- **Go 1.26.4+** at `/usr/local/go` (both corpos and corpos-toolkit pin this in
  `go.mod`; 1.26.3 trips two govulncheck findings). If your distro ships older
  Go, install the official tarball or set `GOTOOLCHAIN=auto` to let Go fetch it.
- **git**, **jq**, **gcc/build-essential** (the gate's `go test -race` needs cgo).
- *(local-LLM path only)* an **NVIDIA GPU with ≥24 GB VRAM**, NVIDIA driver +
  CUDA, and either a CUDA-built `llama-server` binary or rootless **podman** for
  the container image. *(hosted path needs none of this.)*
- *(dashboard only)* **Node 22** + npm.
- Network reachability to the Gitea over Tailscale: `https://example-host.tailnet.ts.net/git/`.

---

## 3. Clone the repos

```bash
mkdir -p ~/dev && cd ~/dev
BASE=https://github.com/sophdn
git clone $BASE/corpos.git
git clone $BASE/corpos-toolkit.git
# optional:
git clone $BASE/corpos-toolkit-dashboard.git
git clone $BASE/llama-server.git           # only for the local-LLM path
```

---

## 4. Choose your LLM (do this first — it shapes the rest)

corpos's default model ladder is **worker → mid → strong**:

| Rung | Default | Provider | Key env |
|------|---------|----------|---------|
| worker (low) | `Qwen2.5-32B-Instruct-Q4_K_M.gguf` on `http://localhost:8081/v1` | local llama.cpp | none (keyless) |
| mid | `google/gemini-3.1-flash-lite` | OpenRouter | `OPENROUTER_API_KEY` |
| strong | `claude-opus-4-8` (bounded, `-strong-bound 2`) | Anthropic | `ANTHROPIC_API_KEY` |

### Option A — Hosted (recommended if you don't have a 24 GB NVIDIA GPU)

The whole stack is endpoint-agnostic: corpos talks to **any** OpenAI-compatible
`/v1/chat/completions`. To skip the local box entirely, repoint the **worker**
rung at a hosted endpoint and supply keys for the cloud rungs:

```bash
# in ~/.config/corpos/corpos.env  (chmod 600), then `source` it before a run
export OPENROUTER_API_KEY=...      # mid rung
export ANTHROPIC_API_KEY=...       # strong rung
export OPENAI_API_KEY=...          # only if your hosted worker endpoint needs a key
```

Run corpos pointing the worker rung at a hosted OpenAI-compatible endpoint:

```bash
go run ./cmd/corpos \
  -provider openai -model-url https://<your-openai-compatible-host>/v1 -model <model-id> \
  -prompt "..."
```

(You can also drive a single hosted provider for everything with
`-provider openrouter -model <model>` — corpos announces this as a deviation
from the benchmarked ladder. Confirm context-window detection: hosted gateways
that omit llama.cpp's `/models` `n_ctx` need `-context-window <n>`.)

### Option B — Local LLM (free worker tier; needs the GPU)

```bash
cd ~/dev/llama-server
# 1. Get the model (~19 GB) — see README for the exact wget + sha256 verify:
#    Qwen2.5-32B-Instruct-Q4_K_M.gguf -> /mnt/data1/models/  (or edit the path
#    in the unit/Containerfile/Quadlet if you mount models elsewhere)
# 2. Serve it on :8081 (container path is primary; see llama-server/README.md):
#    bash scripts/build-image.sh && bash scripts/install-quadlet-units.sh
# 3. Verify:  curl http://localhost:8081/v1/models
```

Then corpos's defaults (`-model-url http://localhost:8081/v1`) work as-is, and
you still set `OPENROUTER_API_KEY` + `ANTHROPIC_API_KEY` for the mid/strong rungs
(or override them — see `-mid-*` / `-strong-*` flags).

---

## 5. Stand up the toolkit-server (required)

corpos calls it for its tool surface; without it corpos degrades to thin static
specs. The Go module lives in **`go/`**, not the repo root.

```bash
cd ~/dev/corpos-toolkit
mkdir -p ~/.local/share/toolkit/data            # DB dir is not auto-created
make -C go build                                 # produces go/bin/toolkit-server
bash scripts/install-hooks.sh                    # wire the pre-commit gate

# Run the HTTP daemon (the --db flag is REQUIRED; server exits 2 without it):
HTTP_PORT=3000 TOOLKIT_DB=~/.local/share/toolkit/data/toolkit.db \
  TOOLKIT_DEFAULT_PROJECT=corpos-toolkit go/launch.sh
```

- **Port:** native daemon defaults to **`:3000`**. The *containerized* deployment
  publishes **`:3001`** (the canonical client port). Note which one you run —
  corpos defaults to `-mcp-url http://localhost:3001`, so for the native `:3000`
  daemon pass `-mcp-url http://localhost:3000`.
- ⚠️ `go/launch.sh` currently hardcodes `/home/user/...` paths and an old
  `--default-project seed-packet` default — override `TOOLKIT_DB` /
  `TOOLKIT_DEFAULT_PROJECT` as above, or edit the script for your machine.
- For Claude Code (not corpos) to use it as an MCP server you'd also run
  `bash scripts/install-proxy.sh`; corpos itself needs only the HTTP daemon.

---

## 6. Build and configure corpos

```bash
cd ~/dev/corpos
go build ./cmd/corpos          # or: go run ./cmd/corpos ...
bash scripts/install-hooks.sh  # wires core.hooksPath=.githooks (the gate)
gofmt -s -w .                  # gofmt -s is the sole formatting authority
```

**Secrets** (convention): `~/.config/corpos/corpos.env`, `chmod 600`, holding
`export ANTHROPIC_API_KEY=…` etc.; `source` it before a run. Never commit it.

**Useful flags** (full list: `go run ./cmd/corpos -h`):
`-prompt` (empty = interactive REPL), `-mcp-url` (toolkit, default `:3001`),
`-project` (defaults to `corpos-toolkit`, the current ledger project),
`-provider` / `-model` / `-model-url`, `-timeout`, `-max-cost-usd`,
`-risk-gate enforce|build-test|off`, `-inspect` / `-run-rate` (telemetry).

---

## 7. Run it

```bash
source ~/.config/corpos/corpos.env
# Smoke test (no services needed):
go run ./cmd/corpos -version
# Real run (toolkit on :300x + an LLM endpoint up):
go run ./cmd/corpos -mcp-url http://localhost:3000 -prompt "say hello"
```

Empty `-prompt` drops into an interactive REPL.

---

## 8. (Optional) the dashboard

```bash
cd ~/dev/corpos-toolkit-dashboard
npm ci
cp .env.example .env.local      # set VITE_API_BASE_URL to your toolkit:
                                #   http://localhost:3000 (native) or :3001 (container)
npm run dev                     # http://localhost:5180
```

No backend of its own — it just reads the toolkit's HTTP API. For a UI demo with
no backend: `node dev-mock.mjs --dummy` (serves sample data on `:3001`).

---

## 9. Known landmines (save yourself the debugging)

- **Go version:** 1.26.4 required; 1.26.3 fails govulncheck. Use the official
  tarball or `GOTOOLCHAIN=auto`.
- **toolkit module is in `go/`**, not the repo root.
- **`:3000` vs `:3001`:** native toolkit = 3000, container = 3001. corpos and the
  dashboard both default to one of these — set `-mcp-url` / `VITE_API_BASE_URL`
  to match what you actually ran.
- **`go/launch.sh`** hardcodes `/home/user` paths + a stale `seed-packet`
  default — override the env vars above.
- **dashboard has no Node-version pin** (CI/container use 22; pin yours to 22).
- **dashboard `gen-types.sh`** defaults to a wrong toolkit dir — set
  `TOOLKIT_GO_DIR=~/dev/corpos-toolkit/go` if you regenerate API types.
- **corpos-toolkit's `.github/workflows/ci.yml` is dead** (references a retired
  polyglot layout) — the live CI is `.gitea/workflows/ci.yaml`. Ignore the GitHub one.
- **No GPU?** Use the hosted LLM path (§4 Option A) — do not try to run the 32B
  model on CPU.
- **Run corpos from inside the repo you point `-verify-dir` at.** The coding
  worker edits files relative to the process working directory, but the
  build-test gate runs in `-verify-dir`. If those are *disjoint* trees (neither
  contains the other), the worker fixes one tree while the gate checks another —
  the run can never go green and just burns spend climbing the escalation ladder.
  corpos now **refuses to start** in that case with a FATAL message; the fix is to
  `cd` into the repo and pass `-verify-dir .` (or a subdir of the working
  directory). `CWD == verify-dir` and `verify-dir` under CWD are the normal,
  supported shapes.

---

## 10. For a local Claude orchestrating this

Recommended order: (1) install Go 1.26.4 + jq + gcc; (2) clone `corpos` +
`corpos-toolkit`; (3) decide LLM path with the operator — **default to hosted**
unless they have a ≥24 GB NVIDIA GPU; (4) stand up toolkit-server on `:3000` with
its own `TOOLKIT_DB`; (5) put keys in `~/.config/corpos/corpos.env`; (6)
`go build ./cmd/corpos` + `install-hooks.sh`; (7) `go run ./cmd/corpos -version`
to smoke-test, then a real `-prompt` run with `-mcp-url`/`-project` set correctly.
Treat §9 as the failure-mode checklist when a step misbehaves.
