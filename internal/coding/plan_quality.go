package coding

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// PlanTask is one proposed atomic task from a PLANNER's decomposition of a feature, BEFORE
// its acceptance test (gate) is authored: a crisp goal, a natural-language ASSERTION of
// what its test will check, and backward dependencies. It is the unit the plan-quality gate
// scores; a task that survives becomes a FeatureTask (its gate authored from the assertion).
type PlanTask struct {
	Slug      string   `json:"slug"`
	Goal      string   `json:"goal"`
	Assertion string   `json:"assertion"` // pinned assertion ("Run(\"21\") == 42", not "Run works")
	DependsOn []string `json:"depends_on"`
	// Package is the repo-relative directory the atom implements (e.g. "internal/routing").
	// For a MULTI-package feature the planner assigns each atom to exactly one of the
	// feature's declared packages; the plan-quality gate validates the assignment. For a
	// single-package feature it is optional (the lone package is assumed).
	Package string `json:"package"`
}

// Invariant is a feature-level property that MUST hold and MUST get its own dedicated atom
// — the failure mode the local-atomization eval surfaced: a planner drops an invariant or
// folds it into an unrelated task's vague clause instead of giving it a focused test (e.g.
// "the coverage advisory never blocks the gate"). Keywords are the distinctive terms a
// covering atom's assertion must mention; the principal authoring the feature supplies them.
type Invariant struct {
	Name     string   // human label, e.g. "advisory-never-blocks"
	Keywords []string // ALL must appear (lower-cased) in a covering atom's assertion
}

// Plan is a planner's full decomposition of a feature: the ordered atomic tasks plus the
// feature's invariants that each demand a dedicated atom.
type Plan struct {
	Slug       string
	Tasks      []PlanTask
	Invariants []Invariant
	// Packages are the repo-relative dirs of the feature's DECLARED packages (from the
	// FeatureSpec). When more than one is declared the plan is multi-package and every
	// atom's Package must name one of them (the task-package check); a single declared
	// package (or none) leaves Package unconstrained.
	Packages []string
}

// PlanProblem is one defect the plan-quality gate found. Kind is a stable machine tag; the
// Detail is the actionable message fed back to the planner (or the principal) to fix.
type PlanProblem struct {
	Task   string // task slug, or "" for a plan-level problem
	Kind   string // structural | weak-assertion | uncovered-invariant | folded-invariants | inconsistent-symbol | task-package | duplicate-assertion
	Detail string
}

// CheckPlanQuality scores a planner's decomposition against the contract that makes its
// atoms into REAL gates — closing the two failure modes the local-floor atomization eval
// found (the planner gets crisp atoms + the dependency DAG right, but writes loose
// assertions and drops/folds invariants):
//
//  1. structural — at least one task, unique slugs, non-empty goal + assertion, and every
//     DependsOn pointing at an EARLIER task (no forward/self refs).
//  2. weak-assertion — every assertion must PIN a concrete expected value; a presence-only
//     or vague assertion ("contains coverage data", "displayed without blocking") is flagged.
//  3. uncovered-invariant — every stated invariant must be covered by some atom's assertion.
//  4. folded-invariants — no single atom may be the cover for MORE THAN ONE invariant; each
//     invariant gets its own dedicated atom.
//  5. inconsistent-symbol — an API symbol referenced as a call (e.g. "Gcd(8,12)") must be
//     spelled the SAME way across every atom; a casing split (one atom "GCD", another "Gcd")
//     means the atoms describe two different functions, so the decomposition is incoherent
//     (the failure the first end-to-end run surfaced — the worker had to define BOTH spellings
//     to pass). Each split is one plan-level problem naming the conflicting spellings.
//  6. task-package — for a MULTI-package feature (more than one declared package), every atom
//     must place itself in exactly one declared package via its Package field; an unplaced or
//     undeclared-package atom is flagged. Inert for a single-package feature.
//
// It returns the problems (empty slice = the plan is gate-worthy). It never panics or
// mutates the plan; it is a pure scorer.
func CheckPlanQuality(p Plan) []PlanProblem {
	var probs []PlanProblem
	if len(p.Tasks) == 0 {
		return []PlanProblem{{Kind: "structural", Detail: "plan has no tasks"}}
	}

	seen := map[string]bool{}
	for i := range p.Tasks {
		t := p.Tasks[i]
		switch {
		case strings.TrimSpace(t.Slug) == "":
			probs = append(probs, PlanProblem{Kind: "structural", Detail: fmt.Sprintf("task at position %d has no slug", i)})
			continue
		case seen[t.Slug]:
			probs = append(probs, PlanProblem{Task: t.Slug, Kind: "structural", Detail: "duplicate task slug"})
		}
		if strings.TrimSpace(t.Goal) == "" {
			probs = append(probs, PlanProblem{Task: t.Slug, Kind: "structural", Detail: "empty goal"})
		}
		if strings.TrimSpace(t.Assertion) == "" {
			probs = append(probs, PlanProblem{Task: t.Slug, Kind: "structural", Detail: "no acceptance assertion — every atom needs a test it can be gated on"})
		}
		for _, dep := range t.DependsOn {
			if dep == t.Slug {
				probs = append(probs, PlanProblem{Task: t.Slug, Kind: "structural", Detail: "depends on itself"})
			} else if !seen[dep] {
				probs = append(probs, PlanProblem{Task: t.Slug, Kind: "structural", Detail: fmt.Sprintf("depends on %q, which is not an EARLIER task (deps must point backward)", dep)})
			}
		}
		if a := strings.TrimSpace(t.Assertion); a != "" && !assertionPinsValue(a) {
			probs = append(probs, PlanProblem{Task: t.Slug, Kind: "weak-assertion",
				Detail: fmt.Sprintf("assertion %q tests presence, not a pinned value — restate it to assert a concrete expected result (an exact value, a comparison, a return, or an exit code)", a)})
		}
		seen[t.Slug] = true
	}

	// Invariant coverage + folding.
	taskCovers := map[string][]string{} // task slug -> invariant names it covers
	for _, inv := range p.Invariants {
		var covering []string
		for _, t := range p.Tasks {
			if coversInvariant(t.Assertion, inv) {
				covering = append(covering, t.Slug)
				taskCovers[t.Slug] = append(taskCovers[t.Slug], inv.Name)
			}
		}
		if len(covering) == 0 {
			probs = append(probs, PlanProblem{Kind: "uncovered-invariant",
				Detail: fmt.Sprintf("invariant %q is not covered by any atom's assertion — add a dedicated atom that tests it (keywords: %s)", inv.Name, strings.Join(inv.Keywords, ", "))})
		}
	}
	for _, slug := range sortedKeys(taskCovers) {
		if names := taskCovers[slug]; len(names) > 1 {
			sort.Strings(names)
			probs = append(probs, PlanProblem{Task: slug, Kind: "folded-invariants",
				Detail: fmt.Sprintf("atom folds %d invariants (%s) — give each invariant its own dedicated atom", len(names), strings.Join(names, ", "))})
		}
	}

	probs = append(probs, checkSymbolConsistency(p)...)
	probs = append(probs, checkTaskPackages(p)...)
	probs = append(probs, checkDuplicateAssertions(p)...)
	return probs
}

// checkDuplicateAssertions flags atoms that share an identical (whitespace/case-normalized)
// assertion — a decomposition redundancy. Two atoms testing the EXACT same behavior do
// duplicate work, and worse: the later atom re-edits an implementation the earlier one already
// satisfied, giving the worker a chance to REGRESS it. (Observed live: a redundant
// "tie-breaking" atom whose assertion equalled an earlier "highest-scoring" atom's — the
// worker's re-edit flipped the tie comparison and the gate went red.) Each later duplicate is
// flagged against the first atom carrying that assertion, fed to the planner's revise loop to
// drop it or give it a DISTINCT assertion.
func checkDuplicateAssertions(p Plan) []PlanProblem {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	firstByAssertion := map[string]string{} // normalized assertion -> first task slug carrying it
	var probs []PlanProblem
	for _, t := range p.Tasks {
		a := norm(t.Assertion)
		if a == "" {
			continue
		}
		if first, ok := firstByAssertion[a]; ok {
			probs = append(probs, PlanProblem{Task: t.Slug, Kind: "duplicate-assertion",
				Detail: fmt.Sprintf("assertion is identical to task %q's — two atoms testing the same behavior is redundant and risks the later atom regressing the earlier's work; drop this atom or give it a DISTINCT assertion (a different input/expected value)", first)})
		} else {
			firstByAssertion[a] = t.Slug
		}
	}
	return probs
}

// checkTaskPackages validates per-atom package placement for a MULTI-package feature. When
// the plan declares more than one package, EVERY atom must name exactly one of them in its
// Package field — an unplaced atom (no package) or one naming a package the feature did not
// declare can't be authored against a known dir, and silently defaulting it would seed the
// oracle into the wrong place. A single declared package (or none) is the single-package
// path: Package is unconstrained and assumed to be the lone target. Each violation is a
// per-task structural-grade problem fed back to the planner's revise loop.
func checkTaskPackages(p Plan) []PlanProblem {
	if len(p.Packages) <= 1 {
		return nil
	}
	declared := map[string]bool{}
	for _, d := range p.Packages {
		declared[d] = true
	}
	var probs []PlanProblem
	for _, t := range p.Tasks {
		pkg := strings.TrimSpace(t.Package)
		switch {
		case pkg == "":
			probs = append(probs, PlanProblem{Task: t.Slug, Kind: "task-package",
				Detail: fmt.Sprintf("atom has no `package` — this is a multi-package feature, so assign it one of the declared packages: %s", strings.Join(p.Packages, ", "))})
		case !declared[pkg]:
			probs = append(probs, PlanProblem{Task: t.Slug, Kind: "task-package",
				Detail: fmt.Sprintf("atom's package %q is not a declared package of this feature — use exactly one of: %s", pkg, strings.Join(p.Packages, ", "))})
		}
	}
	return probs
}

// symbolCallRe matches a Go-identifier referenced as a CALL — the identifier immediately
// followed by '(' (allowing whitespace): "Gcd(8,12)", "func Parse(s string)", "Run (x)".
// Scoping detection to call-shaped tokens is what keeps the consistency check free of
// false positives on ordinary prose capitalization ("Parse the input" vs "parse it"): a
// bare word is not a symbol reference, a called identifier unambiguously is.
var symbolCallRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// checkSymbolConsistency flags any API symbol the plan references (as a call) under MORE
// THAN ONE spelling across its atoms — a casing split like "GCD(...)" in one atom and
// "Gcd(...)" in another. It scans each atom's goal AND assertion (a symbol is declared in
// the goal and exercised in the assertion, so a split between the two is just as incoherent
// as one between two atoms), groups every called identifier by its lower-cased form, and
// emits one plan-level problem per group that carries more than one distinct spelling. An
// internally-consistent symbol (even a helper like "strconv.Atoi") never flags, because its
// single spelling yields a one-element group.
func checkSymbolConsistency(p Plan) []PlanProblem {
	spellings := map[string]map[string]bool{} // lower-cased symbol -> set of spellings seen
	for _, t := range p.Tasks {
		for _, text := range []string{t.Goal, t.Assertion} {
			for _, m := range symbolCallRe.FindAllStringSubmatch(text, -1) {
				sym := m[1]
				key := strings.ToLower(sym)
				if spellings[key] == nil {
					spellings[key] = map[string]bool{}
				}
				spellings[key][sym] = true
			}
		}
	}
	var probs []PlanProblem
	for _, key := range sortedSetKeys(spellings) {
		if set := spellings[key]; len(set) > 1 {
			variants := sortedSet(set)
			probs = append(probs, PlanProblem{Kind: "inconsistent-symbol",
				Detail: fmt.Sprintf("the API symbol is spelled %d ways across atoms (%s) — these name different functions; pin ONE spelling and use it in every atom's goal and assertion", len(variants), strings.Join(variants, ", "))})
		}
	}
	return probs
}

// sortedSet returns the elements of a string set, sorted.
func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedSetKeys returns the keys of a set-of-sets map, sorted (for deterministic problem order).
func sortedSetKeys(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pinnedMarkers are tokens that indicate an assertion names a concrete expected result.
var pinnedMarkers = []string{
	"==", "!=", ">=", "<=", "equals", "equal to", "matches expected", "returns", "return ",
	"exit", "true", "false", "nil", "error", "exactly", "byte-identical", "len(", "count is",
}

// weakMarkers are tokens that indicate a presence-only / vague assertion (the local-floor
// failure mode: testing that something exists/works rather than equals a value).
var weakMarkers = []string{
	"contains", "present", "exists", "works", "without ", "is displayed", "is generated",
	"successfully", "is created", "is added", "appears", "is shown", "handled", "as expected,",
}

// assertionPinsValue reports whether an assertion names a concrete expected result. It is
// true when the assertion carries a pinned marker (or a digit or a quoted literal) — even
// if also somewhat vague — and false only when it is purely presence/vague language.
func assertionPinsValue(assertion string) bool {
	a := strings.ToLower(assertion)
	for _, m := range pinnedMarkers {
		if strings.Contains(a, m) {
			return true
		}
	}
	if hasDigit(a) || strings.ContainsAny(a, "\"'`") {
		return true
	}
	// No pinned signal: weak only if it also reads as presence/vague; an assertion with
	// neither signal is left to the structural/empty check, not flagged as weak here.
	for _, m := range weakMarkers {
		if strings.Contains(a, m) {
			return false
		}
	}
	return true // neither pinned nor obviously weak — don't over-flag.
}

// coversInvariant reports whether an assertion covers an invariant. Matching is at the WORD
// level, not raw substring: every WORD of every keyword must appear as a token in the
// assertion. So the keyword "exit 0" is satisfied by "the gate exit code is 0" (both "exit"
// and "0" are tokens) — a robustness the live planner forced (it wrote a semantically-correct
// "exit code is 0" that an exact-substring check wrongly rejected, sending it into a thrash).
// Tokenizing (split on non-alphanumerics) also avoids spurious hits — "0" matches the token
// "0" but not the "0" inside "10". Empty keyword sets never match.
func coversInvariant(assertion string, inv Invariant) bool {
	if len(inv.Keywords) == 0 {
		return false
	}
	toks := tokenSet(assertion)
	for _, kw := range inv.Keywords {
		for _, w := range tokenize(kw) {
			if !toks[w] {
				return false
			}
		}
	}
	return true
}

// tokenize splits s into lower-cased alphanumeric word tokens.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// tokenSet is the set of word tokens in s.
func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokenize(s) {
		out[t] = true
	}
	return out
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FormatPlanProblems renders the problems as a numbered, actionable list to feed back to
// the planner (or the principal) for a revise. Empty input yields "".
func FormatPlanProblems(probs []PlanProblem) string {
	if len(probs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The plan does not yet meet the gate-quality contract; fix each:\n")
	for i, p := range probs {
		where := "plan"
		if p.Task != "" {
			where = "task " + p.Task
		}
		fmt.Fprintf(&b, "%d. [%s] %s: %s\n", i+1, p.Kind, where, p.Detail)
	}
	return b.String()
}
