# The toolkit MCP contract

Corp-OS is the agent **harness**; it does not ship its runtime substrate. At runtime it talks to
**`toolkit-server`** — a separate MCP service that owns the persistent surfaces (the work ledger,
knowledge/memory, benchmarks + classifiers, and optionally filesystem/system access). This document
is the contract from *Corp-OS's side*: the transport, the request/response shape, the surfaces it
consumes, and what a substitute substrate must provide.

The decoupling is deliberate. The agent loop reaches every tool through the `tool.Provider` seam
(`internal/tool`); the MCP client (`internal/mcp`) is just one implementation of that seam. The loop
neither imports nor assumes the substrate — you could point Corp-OS at a different backend that
honors this contract, or serve some surfaces from Corp-OS's own in-process organs (see
[owned organs](#owned-organs-vs-remote-surfaces)).

## Transport

A single bespoke route — not the full MCP stdio/JSON-RPC framing, just an HTTP dispatch table:

```
POST /mcp/<surface>
Content-Type: application/json

{
  "action":    "<action-name>",     // required
  "params":    { … },               // action arguments (object)
  "rationale": "<why>",             // required for write actions
  "project":   "<scope>"            // ledger scope; required for project-scoped writes
}
```

- **Default endpoint:** `http://localhost:3001` (`mcp.DefaultToolkitURL`).
- **Transport seam:** the client is parameterized over
  `Transport func(ctx, path, body map[string]any) (any, error)`, so the HTTP call is injectable —
  tests drive the client with an in-memory transport, and an alternative backend is a drop-in.
- **Project scope:** the client carries a default project (`WithProject`); the substrate requires a
  project on write actions.
- **Errors fold into the transcript:** a call to an unmounted surface (or a substrate error) returns
  a *tool error*, never an out-of-band failure — the loop hands it back to the model like any other
  tool result.

## The tool-spec shape (dispatch table, not one-tool-per-action)

Each surface is exposed to the model as **one tool** whose parameters are
`{action (required), params (object), rationale}`. The model picks a surface tool and names the
action in-band, rather than the substrate flattening hundreds of actions into hundreds of tools.
This keeps the offered tool set small and lets a worker's job-profile scope *which surfaces* it sees.

### Action discovery

A surface answers the reserved call `{"action": "__actions__"}` with `{"actions": [ … ]}` — the live
action list for that surface. This lets Corp-OS discover or lazily load a surface's actions instead
of hardcoding them.

## Surfaces Corp-OS consumes

| Surface | Purpose | Needed for |
|---|---|---|
| **`work`** | Chains, tasks, bugs, roadmap, and `forge` on the work ledger. | Core. The event-sourced work substrate Corp-OS reads and writes. |
| **`knowledge`** | Vault, kiwix, library, and `parse_context`. | Memory injection, context parsing, reference resolution. |
| **`measure`** | Benchmarks (record/query/replay) and the local rubric-classify family (severity, proportionality, **routing-trigger**, …). | The router's duty classifier (`internal/routing`) reads the routing-trigger classifier here. |
| **`fs`** | Owned filesystem: read / write / edit / grep / glob / ls. | Coding work. *Can be served natively* (see below). |
| **`sys`** | Owned system surface: read-only introspection (ps / ports / units / containers) + gated `exec`. | Shell-shaped work. *Can be served natively.* |

Surface names must be **globally unique** across mounted servers — the `Aggregator`
(`internal/mcp/aggregate.go`) routes each call to the server that owns its surface, and a surface
offered by two servers is a configuration error. This is the seam that lets Corp-OS draw `work` /
`knowledge` / `measure` from the toolkit while serving `fs` / `sys` / `web` itself.

## Owned organs vs. remote surfaces

`fs`, `sys`, and `web` are implemented as **native in-process organs** in Corp-OS today
(`internal/fsorgan`, `internal/sysorgan`, `internal/web`) — same tool surface, served locally rather
than over MCP. The remote toolkit form documented here is the alternative. This is the
"cannibalize the rented harness" trajectory: each owned organ removes a substrate dependency, and
the aggregator makes the choice per-surface without the loop noticing.

## What a substitute substrate must provide

To back Corp-OS with a different service, honor this minimum:

1. An HTTP endpoint accepting `POST /mcp/<surface>` with the `{action, params, rationale?, project?}`
   body above, returning a JSON result (or a structured error).
2. Answer `{"action": "__actions__"}` per surface for discovery.
3. Provide at least the **`work`** surface (the ledger Corp-OS is built around). `measure` is needed
   for classifier-driven routing; `knowledge` for memory/context. `fs` and `sys` are optional at the
   substrate level because Corp-OS can serve them from its own organs.
