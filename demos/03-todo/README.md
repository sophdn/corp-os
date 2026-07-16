# Demo 3 — todo app (multi-file feature, backend + frontend)

**Scenario:** an empty Go module. corpos is asked to build a *multi-part*
feature — a `todo` backend (in-memory store + `net/http` API + tests) **and** a
minimal static frontend (`index.html` + `app.js`) — and make `go test ./...`
pass. This is the largest scenario: it needs cross-file, backend+frontend work.

**What the [transcript](./transcript.txt) shows:**

- **Orchestration on a larger goal.** corpos ran under `orchestrate` and
  delegated the build to a scoped worker via `agent.spawn`.
- **Partial, honest delivery.** It produced a **real, working frontend** —
  [`produced/index.html`](./produced/index.html) + [`produced/app.js`](./produced/app.js)
  (add + list todos via `fetch`). But it wrote the **entire backend**
  (`Todo`, `Store`, `NewStore`, the HTTP handlers) **inside `todo_test.go`**
  (`package todo_test`) — see [`produced/todo_test.go`](./produced/todo_test.go).
  So `go test` passes, but there's no backend package for the frontend to talk
  to: hollow green again. corpos **did not declare done** — it halted "still
  stuck."

**The honest gap:** the bigger the task, the more visible the frontier. corpos
engaged the right machinery (orchestration, spawn, the gate), delivered *part*
of the feature for real, and refused to bless an incomplete/hollow result — but
it did **not** converge to a complete, correctly-structured app. That last mile
is exactly the active work the top-level README names. **Cost:** $0.10.

## Run it yourself

```bash
cd scenario
git init -q -b main && git add -A && git commit -qm "empty scaffold"
../run.sh
```
