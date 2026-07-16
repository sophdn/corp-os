package coding

import (
	"fmt"
	"strings"
)

// FeatureTask is the principal's authored spec for ONE task of a feature chain: a crisp
// goal plus the red-before-green acceptance gate that DEFINES "done" for it. It is the
// unit the gate-authoring bridge consumes (docs/GATE_AUTHORING_BRIDGE.md).
//
// The distinction that makes this its own type: a BUG-FIX task's gate is the codebase's
// EXISTING test suite (someone already wrote the oracle). A FEATURE task implements code
// that does not exist yet, so there is NO pre-existing oracle — the acceptance test that
// proves the task is done must be AUTHORED (red on the un-built tree, green once built) and
// supplied here. Leaving it unauthored is the fake-green trap: the worker would "pass"
// `go build && go test` vacuously (unrelated tests stay green) without the feature being
// exercised. The bridge refuses an ungated feature task for exactly this reason.
type FeatureTask struct {
	// Slug uniquely identifies the task within the chain.
	Slug string
	// Goal is the crisp duty handed to the coding worker (a single, localized change —
	// finely-atomized enough to be within the worker's reliable ceiling).
	Goal string
	// Gate is the AUTHORED acceptance gate: ordered argv vectors that must all exit 0,
	// red before the task's code exists and green after (e.g. a `go test -run <oracle>`
	// over the seeded acceptance test). REQUIRED — an empty gate is rejected.
	Gate [][]string
	// Oracles are the authored acceptance-test files (repo-relative path → Go source)
	// the gate runs against. The bridge SEEDS them into the worktree (committed,
	// protected) before the worker runs — this is how the prose→argv seam closes: the
	// authored test is carried WITH the task, not assumed to pre-exist on the tree.
	// Every oracle path must be a *_test.go file (auto-protected by the default set).
	Oracles map[string]string
	// Workspace is the write-allowlist (the impl file(s) this task may create/edit),
	// reinforcing a minimal, targeted change.
	Workspace []string
	// Inputs are named references to EARLIER tasks' extracted outputs (threaded by the
	// orchestrator); refs must point backward.
	Inputs map[string]InputRef
	// MaxIterations bounds the write→gate→revise loop (0 → DefaultMaxIterations).
	MaxIterations int
}

// BuildFeatureChain assembles a corpos coding.Chain from principal-authored FeatureTask
// specs, enforcing the gate-authoring contract: every task MUST carry a non-empty Goal and
// a non-empty authored Gate (the red-before-green oracle a feature has no pre-existing
// substitute for). It defaults each task's Protected set to **/*_test.go — the worker must
// SATISFY the authored oracle, never rewrite it (the fake-green guard) — and runs the full
// chain validation (unique slugs, backward-only input refs, valid worker kinds). It returns
// the executable chain, or the first contract/validation violation.
//
// This is the structural half of the gate-authoring bridge: the principal (or, later, an
// automated planner) authors the per-task gates from a feature's acceptance criteria; this
// turns them into a chain the operator-seat path executes (proven by
// TestLiveFeatureChainExecution).
func BuildFeatureChain(slug, targetRepo string, tasks []FeatureTask) (Chain, error) {
	if len(tasks) == 0 {
		return Chain{}, fmt.Errorf("feature chain %q has no tasks", slug)
	}
	ats := make([]AtomicTask, 0, len(tasks))
	for i, ft := range tasks {
		if strings.TrimSpace(ft.Slug) == "" {
			return Chain{}, fmt.Errorf("feature chain %q: task at position %d has no slug", slug, i)
		}
		if strings.TrimSpace(ft.Goal) == "" {
			return Chain{}, fmt.Errorf("feature chain %q: task %q has an empty goal", slug, ft.Slug)
		}
		if len(ft.Gate) == 0 {
			return Chain{}, fmt.Errorf("feature chain %q: task %q has no authored gate — a feature task must carry its red-before-green acceptance test, since there is no pre-existing oracle to verify against (an ungated task would fake-green)", slug, ft.Slug)
		}
		for path := range ft.Oracles {
			if !strings.HasSuffix(path, "_test.go") {
				return Chain{}, fmt.Errorf("feature chain %q: task %q oracle %q must be a *_test.go file (the feature's acceptance oracle is a Go test, auto-protected by the bridge)", slug, ft.Slug, path)
			}
		}
		ats = append(ats, AtomicTask{
			Slug:          ft.Slug,
			Goal:          ft.Goal,
			Gate:          ft.Gate,
			Oracles:       ft.Oracles,
			Workspace:     ft.Workspace,
			Protected:     []string{"**/*_test.go"},
			Inputs:        ft.Inputs,
			Worker:        WorkerConfig{Kind: WorkerModel},
			MaxIterations: ft.MaxIterations,
		})
	}
	chain := Chain{Slug: slug, TargetRepo: targetRepo, Tasks: ats}
	if err := chain.Validate(); err != nil {
		return Chain{}, err
	}
	return chain, nil
}
