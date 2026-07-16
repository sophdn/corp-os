# The gate-authoring bridge — point corpos at a feature chain

The bridge that turns a **feature** (a chain of work-ledger tasks, each with prose
`acceptance_criteria`) into something corpos's **gate-driven executor** can carry to green.

## Why a bridge is needed

corpos's coding worker only knows it is done when an acceptance test goes **red → green**
(it cannot be trusted to self-report — `*_test.go` is protected so it can't fake-green). For
a **bug fix** that test pre-exists (the codebase already has the oracle). For a **feature**,
the code — and its test — don't exist yet. So every feature task needs its acceptance test
**authored**. That authoring is the planning intelligence; everything else is plumbing.

We proved the executor handles a multi-task feature chain end-to-end
(`TestLiveFeatureChainExecution`: a 3-task `parse → double → pipeline` carried to green in
~72s, output-threaded across the accumulating integration branch). So the remaining work is
**planning**, and the bridge is where it plugs in.

## The flow

```
work-ledger chain (tasks: problem_statement + acceptance_criteria + ...)
      │
      │  ── PLANNING ──  (principal now → automated later)
      │     • atomize: each task must be CRISP/localized (the worker's reliable ceiling;
      │       intricate atoms miss — see the bug-987 finding). Split a task that isn't.
      │     • AUTHOR a red-before-green acceptance test per atom (the gate).
      ▼
[]FeatureTask{ Slug, Goal, Gate (authored), Workspace, Inputs, MaxIterations }
      │
      │  BuildFeatureChain()  — STRUCTURAL half (internal/coding/feature_chain.go)
      │     • enforces the gate-authoring contract: every task MUST carry a non-empty
      │       Goal + an authored Gate (else it would fake-green).
      │     • defaults Protected = **/*_test.go (worker satisfies the oracle, never edits it).
      │     • validates (unique slugs, backward-only input refs).
      ▼
coding.Chain  ──►  RunDuty(orch, seat, chain)  ──►  corpos executes each atom to green,
                                                     threading outputs, escalating per atom.
```

`BuildFeatureChain` is built and tested (this commit). The PLANNING box is the staged work
below; today the principal (Claude/Sophi) fills it.

## The gate-authoring playbook (the principal's discipline, for now)

Per ledger task, turn its `acceptance_criteria` prose into a concrete gate:

1. **Atomize first.** If the task is not a single localized change, split it until each atom
   is crisp. A crisp atom = one file / one function / one focused behavior — the shape corpos
   carries reliably. (An irreducibly intricate atom is a signal to reserve it for the strong
   rung or human authorship.)
2. **Author the oracle.** Write a real acceptance test that asserts the atom's observable
   behavior, place it in the target repo, and confirm it is **RED** on the current tree
   (red-before-green — a test that passes un-built is a tautology). The worker may not edit
   it (Protected `**/*_test.go`).
3. **Prefer behavioral gates where a unit test is awkward** — `go build ./pkg/...` succeeds,
   a CLI command exits 0, an output file matches. The gate is any argv vector(s) that exit 0.
4. **Scope the workspace.** Set `Workspace` to the impl file(s) the atom may touch — it
   reinforces a minimal change and keeps the worker out of unrelated code.
5. **Thread dependencies.** When an atom consumes an earlier atom's output, use `Inputs`
   (backward refs only). Often the accumulating integration branch is enough (a later atom
   simply imports an earlier atom's package).

## Staged automation (the roadmap this opens)

- **Now — principal-authored.** Claude/Sophi runs the playbook; `BuildFeatureChain` + the
  operator seat execute. This is a working feature path *today*.
- **Next — ledger-connected.** A reader that pulls a chain's tasks from the work surface
  (`chain_state`/`task_list`) into `FeatureTask` skeletons, so you literally pass a chain
  slug. (Gates still principal-authored; the structural mapping is automated.)
- **Later — automated planning.** A planner model atomizes a feature and authors the
  per-atom gates. The open question — whether a *local* model is competent enough at accurate
  atomization to run this on the cheap tier — is under evaluation (the atomization model
  scan); if not, the planning rung borrows the strong tier.

## Invariants

- The authored gate is the **definition of done**; corpos satisfies it, never rewrites it.
- Atoms must be **crisp** — the bridge does not make an intricate task tractable; atomization
  does.
- Verdicts stay **orchestrator-owned** (red-before-green, the two-tier coverage advisory, the
  fake-green guards) — the bridge composes with them, it does not bypass them.
