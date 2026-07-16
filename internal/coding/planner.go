package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"corpos/internal/model"
)

// FeatureSpec is the principal's statement of a feature to be planned: the overall goal,
// the invariants that must EACH get a dedicated atom, and the packages the feature's code
// lives in. The planner turns this into a Plan (a chain of atomic tasks) and refines it
// against the plan-quality gate.
type FeatureSpec struct {
	Slug       string
	Goal       string
	Invariants []Invariant
	// Packages declares where the feature's code lives. One entry = a single-package feature
	// (the legacy default; the planner need not place atoms). More than one = a MULTI-package
	// feature: the planner assigns each atom to one declared package (PlanTask.Package), the
	// plan-quality gate validates the placement, and the assembler authors each atom's oracle
	// in its package — threading cross-package imports along the dependency edges.
	Packages []PackageTarget
}

// packageDirs returns the declared packages' repo-relative dirs (for the plan-quality
// task-package check and the planner prompt).
func (s FeatureSpec) packageDirs() []string {
	dirs := make([]string, 0, len(s.Packages))
	for _, p := range s.Packages {
		dirs = append(dirs, p.Dir)
	}
	return dirs
}

// defaultPlannerRounds bounds the draft → check → revise loop.
const defaultPlannerRounds = 4

// Planner drafts a Plan for a FeatureSpec by prompting a model and REVISING against the
// plan-quality gate (CheckPlanQuality) until the plan is clean or the round budget is
// spent. It is the automated-decomposition half of the planning layer: the local floor
// drafts, the deterministic gate critiques, the floor revises — the loop the atomization
// eval showed is what makes the local model a usable planner (its weakness is checkable,
// not capacity-bound), so no stronger model is required.
type Planner struct {
	model     model.Adapter
	maxRounds int
}

// NewPlanner builds a planner over a model adapter (maxRounds<=0 → defaultPlannerRounds).
func NewPlanner(m model.Adapter, maxRounds int) *Planner {
	return &Planner{model: m, maxRounds: maxRounds}
}

func (p *Planner) rounds() int {
	if p.maxRounds > 0 {
		return p.maxRounds
	}
	return defaultPlannerRounds
}

// Plan drafts and refines a plan: prompt the model → parse → CheckPlanQuality → on problems,
// re-prompt with the formatted problems → repeat. It returns the plan, the REMAINING
// problems (empty iff it converged to a gate-worthy plan), and the number of rounds used.
// A model/parse error short-circuits with the error. A converged plan is ready to hand to
// the gate-authoring step (author a test per assertion) and BuildFeatureChain.
func (p *Planner) Plan(ctx context.Context, spec FeatureSpec) (Plan, []PlanProblem, int, error) {
	var (
		prior []PlanTask
		plan  Plan
		probs []PlanProblem
	)
	for round := 1; round <= p.rounds(); round++ {
		tasks, err := p.draft(ctx, spec, prior, FormatPlanProblems(probs))
		if err != nil {
			return Plan{}, nil, round, fmt.Errorf("planner draft (round %d): %w", round, err)
		}
		plan = Plan{Slug: spec.Slug, Tasks: tasks, Invariants: spec.Invariants, Packages: spec.packageDirs()}
		probs = CheckPlanQuality(plan)
		if len(probs) == 0 {
			return plan, nil, round, nil // converged
		}
		prior = tasks // revise, don't regenerate: anchor the next round to this plan
	}
	return plan, probs, p.rounds(), nil // budget spent — best-effort plan + remaining problems
}

func (p *Planner) draft(ctx context.Context, spec FeatureSpec, prior []PlanTask, feedback string) ([]PlanTask, error) {
	resp, err := p.model.Complete(ctx, []model.ChatMessage{
		{Role: model.RoleSystem, Content: plannerSystemPrompt},
		{Role: model.RoleUser, Content: plannerUserPrompt(spec, prior, feedback)},
	}, nil)
	if err != nil {
		return nil, err
	}
	return parsePlanTasks(resp.Text)
}

const plannerSystemPrompt = `You decompose a software feature into a chain of ATOMIC coding tasks for an automated coding agent. Each task MUST be:
- CRISP: one single, localized change (one function / one file / one focused behavior) — never "implement the whole thing".
- INDIVIDUALLY VERIFIABLE by exactly ONE acceptance test, and you MUST state that test's assertion so it PINS A CONCRETE EXPECTED VALUE — a comparison, a return value, an exit code, or an exact literal. NEVER a presence-only assertion ("contains X", "works", "is generated", "without error").
- ORDERED with explicit backward dependencies: a task may only depend on EARLIER tasks (by slug).
- CONSISTENT in naming: use ONE exact spelling for every API symbol (function / type name) across ALL tasks — if one task names "Gcd", no other task may write "GCD" or "gcd"; differing casings name DIFFERENT functions.
- PLACED, when the feature spans multiple packages: set "package" to the ONE listed package directory the atom implements (omit it when only one package is in play).
Every stated INVARIANT must get its OWN dedicated task whose assertion tests exactly that invariant (e.g. an invariant "never blocks" gets a task asserting "exit code 0").
Output ONLY a JSON array — no prose, no markdown fences:
[{"slug":"kebab-slug","goal":"one line","assertion":"the pinned assertion","depends_on":["earlier-slug"],"package":"repo/rel/dir — only when packages are listed below"}]`

func plannerUserPrompt(spec FeatureSpec, prior []PlanTask, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FEATURE: %s\n", spec.Goal)
	if len(spec.Packages) > 1 {
		b.WriteString("\nPACKAGES — this feature spans these directories; set each task's \"package\" to EXACTLY ONE of them:\n")
		for _, p := range spec.Packages {
			fmt.Fprintf(&b, "- %s (package %s)\n", p.Dir, p.PackageName)
		}
	}
	if len(spec.Invariants) > 0 {
		b.WriteString("\nINVARIANTS — each REQUIRES its own dedicated task whose assertion tests it:\n")
		for _, inv := range spec.Invariants {
			fmt.Fprintf(&b, "- %s — its task's assertion must mention: %s\n", inv.Name, strings.Join(inv.Keywords, ", "))
		}
	}
	if len(prior) > 0 && feedback != "" {
		// Revise, don't regenerate: hand back the current plan and ask to fix ONLY the
		// flagged problems, keeping every other task. Without this anchor the model
		// re-decomposes from scratch each round and COLLAPSES the plan (dropping feature
		// tasks to clear flagged problems) — the failure the first live run surfaced.
		if js, err := json.MarshalIndent(prior, "", "  "); err == nil {
			fmt.Fprintf(&b, "\nYOUR CURRENT PLAN:\n%s\n", js)
		}
		fmt.Fprintf(&b, "\nIt was REJECTED. %s\nReturn the FULL corrected JSON array: fix every problem above, KEEP every other task unchanged, and do NOT drop or merge tasks to reduce the count.\n", feedback)
	}
	return b.String()
}

// parsePlanTasks extracts the JSON task array from a model response (tolerating markdown
// fences or surrounding prose a local model may add) and unmarshals it.
func parsePlanTasks(text string) ([]PlanTask, error) {
	s := extractJSONArray(text)
	if s == "" {
		return nil, fmt.Errorf("no JSON array found in planner response")
	}
	var tasks []PlanTask
	if err := json.Unmarshal([]byte(s), &tasks); err != nil {
		return nil, fmt.Errorf("parse plan JSON: %w", err)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("planner produced an empty task list")
	}
	return tasks, nil
}

// extractJSONArray returns the substring from the first '[' to its matching ']' (bracket-
// depth + string-literal aware), or "" if there is no balanced array — so a model that
// wraps the array in ```json fences or pads it with prose still parses.
func extractJSONArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
