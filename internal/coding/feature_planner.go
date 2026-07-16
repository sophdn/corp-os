package coding

import (
	"context"
	"fmt"
	"strings"
)

// FeaturePlanner closes the FULL automated path from a prose FeatureSpec to a runnable,
// oracle-gated Chain — the capstone of the gate-authoring bridge:
//
//	FeatureSpec → Planner.Plan (decompose, gate-certify) → OracleAuthor.Author (per atom:
//	prose assertion → seeded red-now acceptance gate) → BuildFeatureChain → operator seat.
//
// It is pure orchestration over the two proven halves: the planner converges a gate-worthy
// decomposition, and the author turns each atom's prose assertion into an executable,
// protected oracle. The result is a chain the operator-seat path executes.
type FeaturePlanner struct {
	planner *Planner
	author  *OracleAuthor
}

// NewFeaturePlanner wires a planner and an oracle author into the assembler.
func NewFeaturePlanner(planner *Planner, author *OracleAuthor) *FeaturePlanner {
	return &FeaturePlanner{planner: planner, author: author}
}

// AssembleReport carries the planning + authoring telemetry for inspection. PlanProblems
// is non-empty only on a non-convergence error (Assemble returns the partial report so a
// caller can see WHERE the pipeline stopped).
type AssembleReport struct {
	Plan         Plan
	PlanRounds   int
	PlanProblems []PlanProblem
	Oracles      []AuthoredOracle
}

// Assemble runs the planner to a gate-worthy plan, authors each atom's acceptance oracle in
// its package (the lone declared package for a single-package feature, or the package the
// planner placed the atom in for a multi-package one — threading the upstream packages an
// atom depends on as importable into its oracle), and builds the feature chain. It errors if
// spec declares no package, if the plan does not converge (an ungate-worthy plan cannot have
// trustworthy gates authored from it), if an atom is placed in an undeclared package, or if
// any oracle cannot be authored + validated. The returned report is always populated up to
// the point of failure.
func (fp *FeaturePlanner) Assemble(ctx context.Context, spec FeatureSpec, chainSlug, targetRepo string) (Chain, AssembleReport, error) {
	if len(spec.Packages) == 0 {
		return Chain{}, AssembleReport{}, fmt.Errorf("feature spec declares no package — Assemble needs at least one PackageTarget to author oracles into")
	}
	plan, probs, rounds, err := fp.planner.Plan(ctx, spec)
	report := AssembleReport{Plan: plan, PlanRounds: rounds, PlanProblems: probs}
	if err != nil {
		return Chain{}, report, fmt.Errorf("plan: %w", err)
	}
	if len(probs) > 0 {
		return Chain{}, report, fmt.Errorf("plan did not converge to a gate-worthy decomposition in %d rounds (%d problems) — cannot author trustworthy gates:\n%s",
			rounds, len(probs), FormatPlanProblems(probs))
	}

	byDir := make(map[string]PackageTarget, len(spec.Packages))
	for _, p := range spec.Packages {
		byDir[p.Dir] = p
	}
	slugPkg := make(map[string]string, len(plan.Tasks)) // task slug -> its package dir
	for _, t := range plan.Tasks {
		slugPkg[t.Slug] = taskPackageDir(spec, t)
	}

	fts := make([]FeatureTask, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		target, ok := byDir[slugPkg[task.Slug]]
		if !ok {
			return Chain{}, report, fmt.Errorf("author oracle for task %q: placed in package %q, which the feature did not declare", task.Slug, slugPkg[task.Slug])
		}
		oracle, err := fp.author.Author(ctx, task, target, upstreamTargets(task, slugPkg, byDir, target.Dir))
		if err != nil {
			return Chain{}, report, fmt.Errorf("author oracle for task %q: %w", task.Slug, err)
		}
		report.Oracles = append(report.Oracles, oracle)
		fts = append(fts, oracle.ToFeatureTask(task))
	}

	chain, err := BuildFeatureChain(chainSlug, targetRepo, fts)
	if err != nil {
		return Chain{}, report, err
	}
	return chain, report, nil
}

// taskPackageDir resolves the package dir an atom belongs to: its assigned Package for a
// multi-package feature, or the sole declared package's dir for a single-package one (where
// Package is optional and the lone target is assumed).
func taskPackageDir(spec FeatureSpec, t PlanTask) string {
	if len(spec.Packages) == 1 {
		return spec.Packages[0].Dir
	}
	return strings.TrimSpace(t.Package)
}

// upstreamTargets resolves the distinct packages an atom may import: the packages of its
// backward dependencies, excluding its own (a same-package dep needs no import). These are
// handed to the oracle author so a downstream atom's test can call an upstream atom's API by
// its real import path. Order follows DependsOn for a deterministic prompt; duplicates and
// the atom's own package are dropped.
func upstreamTargets(task PlanTask, slugPkg map[string]string, byDir map[string]PackageTarget, ownDir string) []PackageTarget {
	var out []PackageTarget
	seen := map[string]bool{ownDir: true}
	for _, dep := range task.DependsOn {
		dir := slugPkg[dep]
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if t, ok := byDir[dir]; ok {
			out = append(out, t)
		}
	}
	return out
}
