# Choosing your tiers

corpos's economic thesis is **cost-via-decomposition**: route each duty to the
cheapest model capable of it, and escalate only on evidence of failure. That only
works if there's a *ladder* of models to route across. This guide is how you pick
the rungs.

The default ladder has three rungs, cheapest → strongest:

| Rung | Job | Default | Provider | Key |
|------|-----|---------|----------|-----|
| **worker** (floor) | the bulk of turns — cheap, high-volume | `Qwen2.5-32B-Instruct-Q4_K_M` | local llama.cpp `:8081` | none (local) |
| **mid** | escalation when the floor stalls | `google/gemini-3.1-flash-lite` | OpenRouter | `OPENROUTER_API_KEY` |
| **strong** (frontier) | last-resort, **usage-bounded** | `claude-opus-4-8` | Anthropic | `ANTHROPIC_API_KEY` |

A worker rests on the floor and climbs **one rung per escalation edge**,
descending back toward the floor on clean turns. The strong rung is
`-strong-bound 2` by default — escalation-only, never a per-turn habit — so the
expensive model is a fallback, not the default driver.

> **The honest tradeoff up front.** The model is *rented by design*. Seeing the
> real cost-via-decomposition behavior means running an actual multi-rung ladder,
> and that costs real API money on the mid/strong rungs — there is no fully-free
> way to exercise the whole thesis. What you **can** do for free is run the worker
> tier locally (Option A below) and collapse the ladder, or watch the committed
> [demo transcripts](../demos/) of real runs without spending anything.

---

## The bottom rung: local or remote

### Option A — local worker (free per-token, needs an NVIDIA GPU)

Run [`llama-server`](https://github.com/sophdn/llama-server) on `:8081` (see its
README + [SETUP.md §4B](./SETUP.md)). Then corpos's defaults already point at it:

```bash
go run ./cmd/corpos \
  -model-url http://localhost:8081/v1 -model Qwen2.5-32B-Instruct-Q4_K_M \
  -project corpos-toolkit -mcp-url http://localhost:3000 \
  -prompt "..."
```

The worker tier is now free per-token; you only pay for mid/strong *if* a duty
escalates.

### Option B — remote worker (no GPU, pay per token)

The worker rung is just an OpenAI-compatible endpoint, so point it at any hosted
lower-tier model (a cheap OpenRouter/OpenAI/Together/etc. model):

```bash
go run ./cmd/corpos \
  -provider openai -model-url https://<host>/v1 -model <cheap-model-id> \
  -project corpos-toolkit -mcp-url http://localhost:3000 \
  -prompt "..."
```

Pick a genuinely *cheap* model here — the whole point is that the floor is where
most turns land. Hosted gateways that don't report llama.cpp's `/models` `n_ctx`
need `-context-window <n>`.

---

## The upper rungs

Set each rung's provider/model/endpoint independently:

```bash
# mid rung (escalation target)
-mid-provider openrouter -mid-model google/gemini-3.1-flash-lite
# strong rung (frontier, bounded)
-strong-provider anthropic -strong-model claude-opus-4-8 -strong-bound 2
```

Supply the keys the rungs you enable need, in `~/.config/corpos/corpos.env`
(`chmod 600`, `source` before a run):

```bash
export OPENROUTER_API_KEY=...   # mid
export ANTHROPIC_API_KEY=...    # strong
```

### Collapsing the ladder

You don't have to run all three rungs:

- **`-mid-provider ""`** — no mid rung; the ladder collapses to worker ↔ strong.
- **`-strong-provider ""`** — single-tier, no escalation at all (cheapest, but a
  stuck worker has nowhere to climb).
- **One provider for everything** — `-provider openrouter -model <model>` drives
  every rung from one hosted model. corpos announces this at startup as a
  deviation from the hand-selected ladder; it's the simplest way to try corpos with
  a single API key, at the cost of not exercising the routing thesis.

---

## Which should I pick?

| You have… | Do this |
|-----------|---------|
| a 24 GB+ NVIDIA GPU | Option A (local worker) + keys for mid/strong — the full ladder, worker tier free |
| no GPU, want the real ladder | Option B (cheap hosted worker) + mid + strong — all paid, but the thesis is visible |
| no GPU, just kicking the tires | single-provider (`-provider openrouter -model <model>`) — one key, one model |
| don't want to spend anything | read the [demo transcripts](../demos/) — real runs, captured |

corpos announces its effective ladder at startup, so you always see which rungs
are wired and where any deviation from the hand-selected defaults sits.
