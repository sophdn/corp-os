# Demo 1 — bug-fix (red → green)

**Scenario:** a one-line bug. `calc.Add` returns `a - b`; the pre-existing test
asserts `Add(2,3) == 5`. corpos must find and fix the bug so `go test ./calc/`
passes — **without** editing the test (the oracle is protected).

**What to watch in the [transcript](./transcript.txt):**

1. **The risk gate is real.** Run with the default `-risk-gate enforce`, corpos
   makes the correct edit but is **blocked from running `go test`** via
   `sys.exec`, so it cannot verify. It does **not** claim success — it halts with
   *"no final answer"* rather than declaring a green it never observed. That
   refusal is the anti-fake-green machinery working. (See
   [`transcript-enforce.txt`](./transcript-enforce.txt).)
2. **With `-risk-gate build-test`** (the correct mode for coding), corpos runs
   the test, the fix lands (`a + b`), and `go test` passes.

**The honest gap:** on the cheap floor the run can still thrash on the edit-tool
format and halt on the frontier-usage bound even though the repo ends green — a
tracked convergence gap, visible right in the transcript. The *fix itself is
correct*; the *clean termination* is the unreliable part. This is exactly the
cheap-tier convergence frontier the top-level README calls out.

**Cost:** cents (the ladder touches the paid mid/strong rungs; the local worker
is the free floor but the `bug-fix` profile starts at the mid rung).

## Run it yourself

```bash
cd scenario
git init -q -b main && git add -A && git commit -qm "initial (with bug)"
go test ./calc/          # RED: Add(2,3) = -1, want 5
../run.sh                # drives corpos against this repo
```
