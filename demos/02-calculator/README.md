# Demo 2 — calculator (feature from a prose goal)

**Scenario:** an empty Go module. corpos is asked to *build* a `calc` package
(Add/Sub/Mul/Div, with Div erroring on divide-by-zero), write a table-driven
test, and make `go test ./calc/` pass — a small feature from a one-paragraph
goal, not a pre-existing bug.

**What the [transcript](./transcript.txt) shows:**

- **Sub-orchestration.** No profile matched, so corpos ran under `orchestrate`
  and used `agent.spawn` to delegate the coding to scoped child workers — the
  decompose-and-delegate primitive, live.
- **The verification integrity, again.** A worker produced *correct logic* and a
  full table-driven test, and `go test` passes — **but it wrote the
  implementation inside `calc_test.go`** (see [`produced-calc_test.go`](./produced-calc_test.go)).
  The only file changed is a `*_test.go` file, so the green is *hollow*: a real
  importer of the package gets nothing (test-only build scope). This is precisely
  the **test-only-diff / hollow-green** pattern the gate targets — and corpos did
  **not** declare done. It halted "still stuck" rather than accept it.

**The honest gap:** the correct next move is to recover the implementation into a
real `calc.go`; instead the run thrashed through the strong-bound (the cheap-tier
convergence frontier). The gate's *refusal* is right; the *recovery* is the
unreliable part. **Cost:** $0.21 (mostly the bounded Opus turns).

## Run it yourself

```bash
cd scenario
git init -q -b main && git add -A && git commit -qm "empty scaffold"
../run.sh
```
