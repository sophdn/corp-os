package routing

import (
	"context"
	"errors"
	"testing"

	"corpos/internal/profile"
)

// fakeClassifier returns a scripted label per duty (or a fixed error).
type fakeClassifier struct {
	labels map[string]string
	err    error
}

func (f fakeClassifier) Classify(_ context.Context, duty string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.labels[duty], nil
}

func TestRouteMapsLabelToProfile(t *testing.T) {
	r := NewRouter(fakeClassifier{labels: map[string]string{"d": LabelChainExecution}}, nil, "")
	d, err := r.Route(context.Background(), "d")
	if err != nil {
		t.Fatal(err)
	}
	if d.Profile != "task-lifecycle" || d.Label != LabelChainExecution || d.Fellback {
		t.Errorf("decision = %+v, want task-lifecycle / chain-execution / no fallback", d)
	}
}

func TestRouteNoTriggerFallsBack(t *testing.T) {
	r := NewRouter(fakeClassifier{labels: map[string]string{"d": LabelNoTrigger}}, nil, "")
	d, err := r.Route(context.Background(), "d")
	if err != nil {
		t.Fatal(err)
	}
	if d.Profile != "task-lifecycle" || !d.Fellback || d.Label != LabelNoTrigger {
		t.Errorf("no-trigger decision = %+v, want fallback task-lifecycle", d)
	}
}

func TestRouteClassifierErrorFallsBack(t *testing.T) {
	r := NewRouter(fakeClassifier{err: errors.New("classifier down")}, nil, "bug-hunt")
	d, err := r.Route(context.Background(), "d")
	if err == nil {
		t.Error("a classifier error should surface")
	}
	if d.Profile != "bug-hunt" || !d.Fellback || d.Label != "" {
		t.Errorf("error decision = %+v, want fallback bug-hunt with empty label", d)
	}
}

func TestRouteUnknownLabelFallsBack(t *testing.T) {
	r := NewRouter(fakeClassifier{labels: map[string]string{"d": "some-future-label"}}, nil, "")
	d, _ := r.Route(context.Background(), "d")
	if d.Profile != "task-lifecycle" || !d.Fellback {
		t.Errorf("unknown label should fall back, got %+v", d)
	}
}

// panicClassifier fails the test if Classify is called — proves the coding
// heuristic short-circuits before the classifier.
type panicClassifier struct{ t *testing.T }

func (p panicClassifier) Classify(context.Context, string) (string, error) {
	p.t.Fatal("classifier called for a duty the coding heuristic should have handled")
	return "", nil
}

func TestRouteCodingDutyShortCircuitsToCodingProfile(t *testing.T) {
	r := NewRouter(panicClassifier{t}, nil, "")
	for _, duty := range []string{
		"Fix the Abs function in abs.go so the tests pass",
		"Edit /tmp/x/parser.py to handle empty input",
		"implement the retry logic in the http client module",
		"refactor the auth package",
		"get go test ./... green",
	} {
		d, err := r.Route(context.Background(), duty)
		if err != nil {
			t.Fatalf("%q: %v", duty, err)
		}
		if d.Profile != defaultCodingProfile || d.Fellback || d.Label != codingHeuristicLabel {
			t.Errorf("%q routed to %+v, want %s via coding heuristic", duty, d, defaultCodingProfile)
		}
	}
}

func TestRouteNonCodingDutyStillClassifies(t *testing.T) {
	r := NewRouter(fakeClassifier{labels: map[string]string{"summarize the meeting notes": LabelContextHandoff}}, nil, "")
	d, err := r.Route(context.Background(), "summarize the meeting notes")
	if err != nil {
		t.Fatal(err)
	}
	if d.Profile != "synthesis" || d.Label != LabelContextHandoff || d.Fellback {
		t.Errorf("non-coding duty = %+v, want synthesis via the classifier", d)
	}
}

func TestLooksLikeCoding(t *testing.T) {
	for _, d := range []string{
		"fix abs.go", "edit foo.py please", "main.rs has a bug",
		"implement the parser function", "refactor the auth module",
		"debug the failing test", "patch the build error", "run pytest until green",
		// source-tree path segments (phrasing-independent), as a weak orchestrator writes them
		"Investigate go/internal/db/migrations/ and the testutil mirror",
		"create a test under internal/testutil that compares the dirs",
		"add a guard to cmd/foo",
		"update parser_test.go expectations",
	} {
		if !looksLikeCoding(d) {
			t.Errorf("looksLikeCoding(%q) = false, want true", d)
		}
	}
	for _, d := range []string{
		"summarize the findings", "review the design doc",
		"draft an email to the team", "the gopher ran fast",
		"file the quarterly report",
	} {
		if looksLikeCoding(d) {
			t.Errorf("looksLikeCoding(%q) = true, want false", d)
		}
	}
}

func TestHasFileExt(t *testing.T) {
	cases := []struct {
		d, ext string
		want   bool
	}{
		{"abs.go", ".go", true},
		{"see abs.go.", ".go", true},
		{"edit src/abs.go now", ".go", true},
		{"abs.gopher", ".go", false},
		{"the gopher", ".go", false},
		{"report.gov site", ".go", false},
	}
	for _, c := range cases {
		if got := hasFileExt(c.d, c.ext); got != c.want {
			t.Errorf("hasFileExt(%q,%q) = %v, want %v", c.d, c.ext, got, c.want)
		}
	}
}

func TestNewRouterDefaults(t *testing.T) {
	r := NewRouter(fakeClassifier{}, nil, "")
	if r.fallback != "task-lifecycle" {
		t.Errorf("empty fallback should default to task-lifecycle, got %q", r.fallback)
	}
	if r.table[LabelChainExecution] != "task-lifecycle" {
		t.Errorf("empty table should default to DefaultTable")
	}
	// A custom table + fallback are honored.
	r2 := NewRouter(fakeClassifier{}, Table{LabelChainExecution: "x"}, "y")
	if r2.fallback != "y" || r2.table[LabelChainExecution] != "x" {
		t.Errorf("custom table/fallback not honored: %+v", r2)
	}
}

// tierOf for the measurement: the library tiers of the profiles under test.
func measureTierOf(name string) (profile.Tier, bool) {
	tiers := map[string]profile.Tier{
		"task-lifecycle": profile.TierLocal,
		"doc-filing":     profile.TierLocal,
		"file-sort":      profile.TierLocal,
		"synthesis":      profile.TierMid,
		"code-review":    profile.TierMid,
		"design":         profile.TierMid,
	}
	t, ok := tiers[name]
	return t, ok
}

// routingMeasurementSet is the representative labeled duty set the v1 bridge is
// measured over: each case pairs a duty (with the session-routing label a correct
// classifier should emit) with the oracle's cheapest-capable profile. It exercises
// good fits, over-escalation (classifier over-fires to a mid seat), and mis-scope
// (right tier, wrong profile — the rework risk).
func routingMeasurementSet() (labels map[string]string, cases []Case) {
	rows := []struct{ duty, label, expected string }{
		{"list the open tasks in chain X and mark one complete", LabelChainExecution, "task-lifecycle"},
		{"file this design note into the vault under decisions", LabelExecuteDocument, "doc-filing"},
		{"retire the deprecated benchmark entry", LabelRetirementDispatch, "doc-filing"},
		{"suggest which toolkit surface handles cost queries", LabelToolSuggest, "task-lifecycle"},
		{"synthesize the three review findings into one recommendation", LabelRoleInvoke, "synthesis"},
		{"hand off the current investigation context to a fresh worker", LabelContextHandoff, "synthesis"},
		{"review this Go diff for correctness bugs", LabelNoTrigger, "code-review"},
		{"do deep architecture gap-finding on the router design", LabelRoleInvoke, "design"},
		{"run the chain task to wire the adapter", LabelChainExecution, "task-lifecycle"},
		{"classify and file this bug report", LabelExecuteDocument, "doc-filing"},
		{"append a line to the changelog", LabelRoleInvoke, "doc-filing"},
	}
	labels = map[string]string{}
	for _, r := range rows {
		labels[r.duty] = r.label
		cases = append(cases, Case{Duty: r.duty, Expected: r.expected})
	}
	return labels, cases
}

func TestEvaluateMeasuresRoutingQuality(t *testing.T) {
	labels, cases := routingMeasurementSet()
	r := NewRouter(fakeClassifier{labels: labels}, nil, "")
	q, err := Evaluate(context.Background(), r, cases, measureTierOf)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if q.Total != 11 || q.Correct != 7 || q.OverEscalated != 1 || q.UnderScoped != 3 {
		t.Errorf("quality = %+v, want total 11 / correct 7 / over 1 / under 3", q)
	}
	// Report the rates — the measured routing quality the task asks for.
	t.Logf("routing quality (v1 bridge, expected-label stand-ins): correct=%.1f%% over-escalate=%.1f%% under-scope=%.1f%%",
		q.CorrectRate*100, q.OverEscalateRate*100, q.UnderScopeRate*100)
}

func TestEvaluateStrongTierUnderScope(t *testing.T) {
	tierOf := func(name string) (profile.Tier, bool) {
		m := map[string]profile.Tier{"mid-p": profile.TierMid, "strong-p": profile.TierStrong}
		tr, ok := m[name]
		return tr, ok
	}
	// Routes to a mid profile, but the oracle wants the strong tier → under-scope
	// (the leaner choice risks rework). Exercises the mid↔strong tier ranking.
	r := NewRouter(fakeClassifier{labels: map[string]string{"d": LabelRoleInvoke}}, Table{LabelRoleInvoke: "mid-p"}, "mid-p")
	q, err := Evaluate(context.Background(), r, []Case{{Duty: "d", Expected: "strong-p"}}, tierOf)
	if err != nil {
		t.Fatal(err)
	}
	if q.UnderScoped != 1 || q.OverEscalated != 0 {
		t.Errorf("mid chosen for a strong duty should be under-scope, got %+v", q)
	}
}

func TestEvaluateUnknownProfileErrors(t *testing.T) {
	r := NewRouter(fakeClassifier{labels: map[string]string{"d": LabelChainExecution}}, nil, "")
	_, err := Evaluate(context.Background(), r, []Case{{Duty: "d", Expected: "nonexistent-profile"}}, measureTierOf)
	if err == nil {
		t.Error("an unknown expected profile should error")
	}
}
