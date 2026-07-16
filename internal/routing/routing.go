// Package routing turns a duty into the cheapest-capable job-profile to run it
// under — the make-or-break of the cost-via-decomposition thesis (design doc gap
// #3). Wrong routing inverts the economics both ways: over-escalate (assign a
// pricier profile than the duty needs) and the savings evaporate; under-scope
// (assign too lean a profile) and the worker fails, forcing rework that costs
// MORE than routing right the first time.
//
// The routing INPUT is the existing measure.classify_session_routing_trigger
// rubric (a Qwen-local classifier on the toolkit-server measure surface), reached
// through the Classifier seam so this package stays sans-IO. Its session-routing
// label is mapped to a job-profile by Table; an unmapped/no-trigger label falls
// back to a cheap general profile. The mapping is a v1 heuristic bridge — the
// classifier's taxonomy predates corpos profiles — documented in the vault
// decision corpos-duty-to-profile-routing-design and measured by Evaluate.
package routing

import (
	"context"
	"fmt"
	"strings"

	"corpos/internal/profile"
)

// Classifier returns a session-routing label for a duty's text. The production
// implementation (MCPClassifier) dispatches measure.classify_session_routing_trigger;
// tests inject a deterministic fake.
type Classifier interface {
	Classify(ctx context.Context, duty string) (label string, err error)
}

// Table maps a classifier label to the job-profile name that should run a duty of
// that routing intent.
type Table map[string]string

// The session-routing labels the classifier emits (its closed vocabulary).
const (
	LabelRoleInvoke         = "role-invoke"
	LabelContextHandoff     = "context-handoff"
	LabelExecuteDocument    = "execute-document"
	LabelRetirementDispatch = "retirement-dispatch"
	LabelChainExecution     = "chain-execution"
	LabelToolSuggest        = "tool-suggest"
	LabelNoTrigger          = "no-trigger"
)

// DefaultTable is the v1 label→profile bridge. Rationale (per-label, documented
// in the vault decision): chain/document/retirement/tool work maps to the
// ledger-validated LOCAL profiles (the cost win); role-invoke and context-handoff
// — open-ended reasoning/synthesis — map to the MID synthesis seat; no-trigger
// has no mapping and uses the fallback.
var DefaultTable = Table{
	LabelChainExecution:     "task-lifecycle",
	LabelExecuteDocument:    "doc-filing",
	LabelRetirementDispatch: "doc-filing",
	LabelToolSuggest:        "file-sort",
	LabelRoleInvoke:         "synthesis",
	LabelContextHandoff:     "synthesis",
	LabelNoTrigger:          "",
}

// defaultCodingProfile is the coding lane's target: the mechanical, embedded
// "implement one task to a green gate" worker. The classifier's closed label
// vocabulary (role-invoke, chain-execution, …) predates corpos and has NO coding
// label, so a coding duty would otherwise classify into a non-coding lane
// (task-lifecycle / synthesis / doc-filing) and the spawned worker cannot edit
// code (swap-rehearsal task-2 run: an auto-routed coding duty landed on synthesis
// and deleted the file). The looksLikeCoding heuristic routes such duties here
// BEFORE the classifier sees them.
const defaultCodingProfile = "atomic-coding-chain"

// defaultTestAuthoringProfile is the test-authoring lane's target (bug 1089): the
// embedded worker that writes/extends a *_test.go to green while PRODUCTION source is
// protected. atomic-coding-chain protects test files (so a bug-fix can't fake green by
// editing the gate), which is the exact inverse of what a test-authoring task needs — so
// "add/write a test" duties route here instead, ahead of the generic coding lane.
const defaultTestAuthoringProfile = "test-authoring-chain"

// codingHeuristicLabel is the synthetic Decision.Label for a duty routed by the
// coding heuristic rather than the classifier — distinguishable in telemetry from
// the classifier's real labels.
const codingHeuristicLabel = "coding-heuristic"

// testAuthoringHeuristicLabel is the synthetic Decision.Label for a duty routed by the
// test-authoring heuristic — distinguishable in telemetry from the coding heuristic.
const testAuthoringHeuristicLabel = "test-authoring-heuristic"

// Router resolves a duty to a profile via the classifier and the label table.
type Router struct {
	classify             Classifier
	table                Table
	fallback             string // profile used for no-trigger / unmapped / classifier-error
	codingProfile        string // coding-lane target (looksLikeCoding short-circuits here)
	testAuthoringProfile string // test-authoring-lane target (looksLikeTestAuthoring short-circuits here, ahead of coding)
}

// NewRouter builds a Router. An empty table uses DefaultTable; an empty fallback
// uses "task-lifecycle" (the cheapest validated general worker). The coding lane
// defaults to defaultCodingProfile.
func NewRouter(c Classifier, table Table, fallback string) *Router {
	if len(table) == 0 {
		table = DefaultTable
	}
	if fallback == "" {
		fallback = "task-lifecycle"
	}
	return &Router{classify: c, table: table, fallback: fallback, codingProfile: defaultCodingProfile, testAuthoringProfile: defaultTestAuthoringProfile}
}

// Decision is the outcome of routing one duty.
type Decision struct {
	Profile  string // the chosen job-profile name
	Label    string // the classifier's session-routing label ("" on classifier error)
	Fellback bool   // true when the fallback profile was used (no mapping / error)
}

// Route classifies the duty and resolves it to a profile. On a classifier error,
// or a label that maps to nothing, it returns the fallback profile (Fellback
// true) and surfaces the classifier error (if any) so the caller can log it — the
// routing decision is still usable.
func (r *Router) Route(ctx context.Context, duty string) (Decision, error) {
	// Test-authoring lane, checked BEFORE the coding lane: "add/write a test" duties
	// would otherwise route to atomic-coding-chain, which protects test files and so
	// DENIES the worker's own *_test.go deliverable (bug 1089). A genuine test-authoring
	// duty (authoring verb + test noun, no production-fix intent) goes to the inverted
	// profile that protects production instead.
	if r.testAuthoringProfile != "" && looksLikeTestAuthoring(duty) {
		return Decision{Profile: r.testAuthoringProfile, Label: testAuthoringHeuristicLabel, Fellback: false}, nil
	}
	// Coding lane: the classifier has no coding label, so detect implementation
	// duties here and route them to the coding worker BEFORE asking the classifier
	// (which would misfile them into a non-coding lane). This is the make-or-break
	// for spawned coding work — a coding duty on a synthesis/task-lifecycle worker
	// cannot edit code.
	if r.codingProfile != "" && looksLikeCoding(duty) {
		return Decision{Profile: r.codingProfile, Label: codingHeuristicLabel, Fellback: false}, nil
	}
	label, err := r.classify.Classify(ctx, duty)
	if err != nil {
		return Decision{Profile: r.fallback, Label: "", Fellback: true}, err
	}
	if p, ok := r.table[label]; ok && p != "" {
		return Decision{Profile: p, Label: label, Fellback: false}, nil
	}
	return Decision{Profile: r.fallback, Label: label, Fellback: true}, nil
}

// codingFileExts are source-file extensions whose mention in a duty is a strong
// signal of an implementation task. hasFileExt matches each only as a real suffix
// (bounded by a word boundary), so "abs.go" matches but "abs.gopher" does not.
var codingFileExts = []string{
	".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs", ".java", ".rb",
	".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".kt", ".swift", ".scala", ".sh", ".sql",
}

// codingTestRunners are standalone strong signals — a duty that names a test
// runner is an implementation/verify task regardless of phrasing.
var codingTestRunners = []string{"go test", "unit test", "pytest", "cargo test", "npm test", "go build", "go vet"}

// codingPathSegments are source-tree directory/path markers. A duty that names such
// a path is operating on code regardless of its verb ("Investigate go/internal/…",
// "Create a test under …/migrations/") — a phrasing-independent signal that a weak
// orchestrator's varied duty wording would otherwise slip past the verb×noun test.
var codingPathSegments = []string{"internal/", "src/", "/pkg/", "/cmd/", "/migrations", "_test."}

// codingVerbs and codingNouns are the verb×noun heuristic for duties that name no
// file, test runner, or source path: an implementation verb together with a code
// noun routes to the coding lane. The verbs include the produce family (create/
// add/write) because spawned duties often read "create a test that …"; each is
// still gated by a code noun, so "write an email" / "add a note" stay out.
var codingVerbs = []string{"implement", "refactor", "debug", "patch", "rewrite", "fix ", "edit ", "modify", "create", "add ", "added", "adding", "write ", "writing"}
var codingNouns = []string{"function", "method", "test", "bug", "compile", "build error", "code", "module", "package", "import", "struct", "class", "endpoint", "regression", "guard"}

// looksLikeCoding reports whether a duty is an implementation task that needs a
// coding worker (one that can edit files + run a gate). Sufficient signals: a
// source-file extension, a test-runner invocation, or a source-tree path segment.
// Otherwise a coding verb AND a code noun together. Kept conservative on the
// verb×noun path to avoid pulling prose/synthesis duties into the coding lane — a
// missed coding duty falls through to the classifier (the prior behavior), so the
// heuristic only ever adds coverage, never removes a previously-correct route.
func looksLikeCoding(duty string) bool {
	d := strings.ToLower(duty)
	for _, ext := range codingFileExts {
		if hasFileExt(d, ext) {
			return true
		}
	}
	if containsAny(d, codingTestRunners...) || containsAny(d, codingPathSegments...) {
		return true
	}
	return containsAny(d, codingVerbs...) && containsAny(d, codingNouns...)
}

// testAuthoringVerbs author a NEW or EXTENDED test — the deliverable IS the test file.
var testAuthoringVerbs = []string{"add ", "added", "adding", "write ", "writing", "author", "create", "creating", "extend", "extending"}

// testRevisionVerbs strengthen an EXISTING test (bug 1101): the deliverable is still the
// *_test.go, so a revision duty needs the test-authoring profile (which can edit the test),
// not the coding lane (which protects *_test.go). The orchestrator emits these as follow-up
// duties when a first-pass test is judged too weak ("improve/strengthen the test"); they
// carry no create/add/write verb, so the bug-1089 authoring list alone missed them and the
// duty thrashed protect-path denials to Opus. Substring matching covers the -ed/-ing forms
// (improve→improving, strengthen→strengthening, …). Kept conservative and gated by a test
// noun below so prose-revision duties ("improve the docs") stay out.
var testRevisionVerbs = []string{"improve", "strengthen", "expand", "enhance", "harden", "broaden", "flesh out"}

// testWeaknessSignals flag an existing test as inadequate WITHOUT a leading verb — the
// rehearsal's "the test ... is too simple" shape (bug 1101). Gated by a test noun + the
// production-fix exclusion, they read as "this test needs strengthening" → test-authoring.
var testWeaknessSignals = []string{"too simple", "too weak", "too shallow", "too thin", "too basic"}

// testNouns name the test artifact. "test" as a substring also covers "unit test",
// "test case", "_test.go", and "tests".
var testNouns = []string{"test", "coverage"}

// productionFixSignals mark a duty as a production bug-FIX even when it mentions a test
// (e.g. "fix the failing test", "make the test pass"): those must protect test files and
// fix production, so they belong in the atomic-coding lane, NOT the test-authoring lane.
// The diagnosis family ("investigate", "why ...", "failed to", "root-cause", "diagnose") is
// included because an orchestrator RE-WORDS "fix bug X" into an investigation ("investigate
// why X failed ... ensure it checks ... create a regression test") that drops the "fix"
// keyword — and that reworded fix duty must still reach the coding lane, not test-authoring,
// or the worker (production-protected) tampers the failing test to green (bug 1161).
var productionFixSignals = []string{"fix ", "fixing", "bug", "failing", "make it pass", "make the test pass", "debug", "passing", "repair", "broken", "investigate", "failed to", "why ", "root-cause", "root cause", "diagnose"}

// productionImplSignals mark a duty that must CREATE or CHANGE production code — a greenfield
// "implement the package/function so the test passes". Even when such a duty ALSO asks to
// author a test, it must NOT route to test-authoring-chain: that profile PROTECTS production,
// so it could never create the impl the gate needs and the run thrashes to timeout (bug 1150).
// It falls through to the coding lane instead — a less-bad partial deliverable (the impl, with
// the test vacuously green) than a thrash. The real remedy is the orchestrator DECOMPOSING a
// mixed "write a test AND implement it" goal into an implement atom THEN a test-authoring atom
// (orchestrate.toml), because neither single profile can deliver both a new test and its
// production: atomic-coding-chain protects *_test.go, test-authoring-chain protects **/*.go.
var productionImplSignals = []string{"implement", "implementation"}

// looksLikeTestAuthoring reports whether a duty's deliverable is a test file — either
// AUTHORING a new/extended test (an authoring verb + a test noun) or REVISING an existing
// one (a revision verb, or a "too simple"-style weakness signal, + a test noun), and in
// all cases WITHOUT a production-fix signal. It is checked ahead of looksLikeCoding so the
// duty reaches the profile that can actually edit the test (production-protected) rather
// than atomic-coding-chain (test-protected, which would deny the deliverable). Conservative
// on purpose: anything fix/bug-flavoured falls through to the coding lane (its prior route),
// and every signal is gated by a test noun, so this only ever adds coverage.
func looksLikeTestAuthoring(duty string) bool {
	d := strings.ToLower(duty)
	if containsAny(d, productionFixSignals...) {
		return false
	}
	// A duty that must also IMPLEMENT production (a mixed "write a test AND implement it")
	// cannot converge in test-authoring-chain (production-protected); send it to the coding
	// lane, and rely on the orchestrator to decompose it into two atoms (bug 1150).
	if containsAny(d, productionImplSignals...) {
		return false
	}
	if !containsAny(d, testNouns...) {
		return false
	}
	return containsAny(d, testAuthoringVerbs...) ||
		containsAny(d, testRevisionVerbs...) ||
		containsAny(d, testWeaknessSignals...)
}

// LooksLikeTestAuthoring is the exported test-authoring detector, used by the spawn
// guardrail to keep a test-authoring duty off a profile that protects *_test.go even
// when the orchestrator names that profile EXPLICITLY (bypassing Route's auto-routing —
// the rehearsal mis-assignment where the model picked atomic-coding-chain for a
// test-authoring duty and the worker could never write its deliverable).
func LooksLikeTestAuthoring(duty string) bool { return looksLikeTestAuthoring(duty) }

// TestAuthoringProfile is the router's configured test-authoring lane target (the
// profile the guardrail redirects a mis-assigned test-authoring duty to). Empty when
// the lane is disabled.
func (r *Router) TestAuthoringProfile() string { return r.testAuthoringProfile }

// hasFileExt reports whether ext appears in d as a file suffix — i.e. followed by
// a word boundary (end of string or a non-alphanumeric), so ".go" matches
// "abs.go" and "abs.go." but not "abs.gopher".
func hasFileExt(d, ext string) bool {
	from := 0
	for {
		i := strings.Index(d[from:], ext)
		if i < 0 {
			return false
		}
		end := from + i + len(ext)
		if end == len(d) || !isAlphanum(d[end]) {
			return true
		}
		from += i + len(ext)
	}
}

func isAlphanum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// tierRank orders the model tiers for over/under-scope comparison.
func tierRank(t profile.Tier) int {
	switch t {
	case profile.TierLocal:
		return 0
	case profile.TierMid:
		return 1
	case profile.TierStrong:
		return 2
	default:
		return 0
	}
}

// Case is one labeled routing case for Evaluate: a duty and the profile an oracle
// says is the cheapest-capable choice.
type Case struct {
	Duty     string
	Expected string // the oracle's correct profile
}

// Quality is the measured routing quality over a case set (design doc gap #3 /
// task acceptance): the correct-profile rate, plus the two failure modes that
// invert the economics — over-escalation (chose a pricier tier than the oracle)
// and under-scope (chose a leaner tier, the rework risk).
type Quality struct {
	Total            int
	Correct          int
	OverEscalated    int
	UnderScoped      int
	CorrectRate      float64
	OverEscalateRate float64
	UnderScopeRate   float64
}

// TierOf reports a profile's model tier; the bool is false for an unknown profile.
type TierOf func(profileName string) (profile.Tier, bool)

// Evaluate routes every case and scores the router against the oracle: a hit when
// the chosen profile equals Expected; otherwise an over-escalation when the chosen
// profile's tier outranks the expected one, or an under-scope when it is leaner.
// tierOf resolves a profile's tier (from the registry). A classifier error on a
// case routes to the fallback and is scored like any other choice.
func Evaluate(ctx context.Context, r *Router, cases []Case, tierOf TierOf) (Quality, error) {
	q := Quality{Total: len(cases)}
	for _, c := range cases {
		d, _ := r.Route(ctx, c.Duty) // a classifier error still yields the fallback decision
		switch {
		case d.Profile == c.Expected:
			q.Correct++
		default:
			chosen, ok1 := tierOf(d.Profile)
			want, ok2 := tierOf(c.Expected)
			if !ok1 || !ok2 {
				return Quality{}, fmt.Errorf("unknown profile in case %q (chosen=%q expected=%q)", c.Duty, d.Profile, c.Expected)
			}
			if tierRank(chosen) > tierRank(want) {
				q.OverEscalated++
			} else {
				// Same-or-lower tier but wrong profile: a under-scope/mis-scope that
				// risks rework (the worker may lack the capability the duty needs).
				q.UnderScoped++
			}
		}
	}
	if q.Total > 0 {
		q.CorrectRate = float64(q.Correct) / float64(q.Total)
		q.OverEscalateRate = float64(q.OverEscalated) / float64(q.Total)
		q.UnderScopeRate = float64(q.UnderScoped) / float64(q.Total)
	}
	return q, nil
}
