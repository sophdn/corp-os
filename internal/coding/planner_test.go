package coding

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/tool"
)

func TestExtractJSONArray(t *testing.T) {
	cases := []struct{ in, want string }{
		{`[{"slug":"a"}]`, `[{"slug":"a"}]`},
		{"```json\n[{\"slug\":\"a\"}]\n```", `[{"slug":"a"}]`},
		{`here is the plan: [{"slug":"a","depends_on":["x"]}] done`, `[{"slug":"a","depends_on":["x"]}]`},
		{`{"slug":"a"}`, ``}, // an object, not an array
		{`no json`, ``},
		{`["a]b"]`, `["a]b"]`}, // a ']' inside a string literal must not close the array
	}
	for _, c := range cases {
		if got := extractJSONArray(c.in); got != c.want {
			t.Errorf("extractJSONArray(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParsePlanTasks(t *testing.T) {
	tasks, err := parsePlanTasks("```json\n[{\"slug\":\"parse\",\"goal\":\"create Parse\",\"assertion\":\"Parse(\\\"42\\\")==42\",\"depends_on\":[]}]\n```")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Slug != "parse" || tasks[0].Assertion != `Parse("42")==42` {
		t.Fatalf("parsed wrong: %+v", tasks)
	}
	if _, err := parsePlanTasks("not json"); err == nil {
		t.Fatal("non-JSON should error")
	}
	if _, err := parsePlanTasks("[]"); err == nil {
		t.Fatal("empty task list should error")
	}
}

// scriptedPlanModel returns scripted responses in order and records every user prompt it
// was sent, so a test can assert the revise loop fed the gate's problems back.
type scriptedPlanModel struct {
	responses []string
	n         int
	userMsgs  []string
}

func (s *scriptedPlanModel) Model() string   { return "scripted-plan" }
func (s *scriptedPlanModel) Available() bool { return true }
func (s *scriptedPlanModel) Complete(_ context.Context, msgs []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	for _, m := range msgs {
		if m.Role == model.RoleUser {
			s.userMsgs = append(s.userMsgs, m.Content)
		}
	}
	r := s.responses[len(s.responses)-1]
	if s.n < len(s.responses) {
		r = s.responses[s.n]
	}
	s.n++
	return model.Response{Text: r, Model: "scripted-plan", StopReason: model.StopEndTurn}, nil
}

// The loop: a first weak plan (loose assertion + a dropped invariant) is rejected by the
// gate; the feedback is fed back; the second plan is clean → the planner converges.
func TestPlanner_RevisesUntilGateWorthy(t *testing.T) {
	weak := `[{"slug":"foo","goal":"create Foo","assertion":"Foo works","depends_on":[]}]`
	clean := `[{"slug":"foo","goal":"create Foo","assertion":"Foo() == 42","depends_on":[]},
	           {"slug":"never-blocks","goal":"keep non-blocking","assertion":"the gate exit 0 even on failure","depends_on":["foo"]}]`
	m := &scriptedPlanModel{responses: []string{weak, clean}}
	p := NewPlanner(m, 4)

	plan, probs, rounds, err := p.Plan(context.Background(), FeatureSpec{
		Slug:       "feat-foo",
		Goal:       "add a Foo helper",
		Invariants: []Invariant{{Name: "never-blocks", Keywords: []string{"exit 0"}}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(probs) != 0 {
		t.Fatalf("planner should have converged; remaining problems:\n%s", FormatPlanProblems(probs))
	}
	if rounds != 2 {
		t.Fatalf("expected convergence in 2 rounds, got %d", rounds)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("converged plan should have 2 tasks, got %d", len(plan.Tasks))
	}
	// The second prompt must carry the gate's rejection feedback (the revise signal).
	if len(m.userMsgs) < 2 || !strings.Contains(m.userMsgs[1], "REJECTED") || !strings.Contains(m.userMsgs[1], "weak-assertion") {
		t.Fatalf("the revise prompt must feed back the gate problems; 2nd prompt:\n%s", lastOr(m.userMsgs))
	}
}

// A planner that never produces a clean plan returns the remaining problems (best-effort)
// after the round budget, not a false success.
func TestPlanner_BudgetSpentReturnsProblems(t *testing.T) {
	weak := `[{"slug":"foo","goal":"g","assertion":"foo works","depends_on":[]}]`
	m := &scriptedPlanModel{responses: []string{weak}} // always weak
	p := NewPlanner(m, 3)
	_, probs, rounds, err := p.Plan(context.Background(), FeatureSpec{Slug: "f", Goal: "g"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(probs) == 0 {
		t.Fatal("a never-converging planner must return remaining problems, not success")
	}
	if rounds != 3 {
		t.Fatalf("should spend the full 3-round budget, got %d", rounds)
	}
}

func lastOr(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return s[len(s)-1]
}

// TestLivePlannerConvergesOnQwen32B is the empirical payoff: the REAL local floor
// (Qwen2.5-32B at :8081) + the deterministic plan-quality gate converge to a gate-worthy
// plan within the round budget — the eval's claim ("good enough behind a quality gate")
// validated live. Gated on CORPOS_LIVE + the local model being reachable.
func TestLivePlannerConvergesOnQwen32B(t *testing.T) {
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 to run the live planner against the local floor")
	}
	qwen := model.NewOpenAICompat("Qwen2.5-32B-Instruct-Q4_K_M.gguf", "http://localhost:8081/v1")
	if !qwen.Available() {
		t.Skip("local Qwen not available")
	}
	p := NewPlanner(qwen, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	plan, probs, rounds, err := p.Plan(ctx, FeatureSpec{
		Slug: "two-tier-green",
		Goal: "Add a 'two-tier green' quality report to the coding gate: after the build/test gate passes, compute a surface-scoped coverage report from the worker's diff (changed PRODUCTION lines; run go test -coverprofile scoped to touched packages; intersect changed lines with covered lines) and grade the green as confirmed vs proposed.",
		Invariants: []Invariant{
			{Name: "advisory-never-blocks", Keywords: []string{"exit 0"}},
			{Name: "production-lines-only", Keywords: []string{"production"}},
		},
	})
	if err != nil {
		t.Fatalf("live planner: %v", err)
	}
	t.Logf("planner used %d round(s); produced %d tasks", rounds, len(plan.Tasks))
	for _, at := range plan.Tasks {
		t.Logf("  - %s: %s  [asserts: %s]", at.Slug, at.Goal, at.Assertion)
	}
	if len(probs) != 0 {
		t.Fatalf("the local floor + gate did not converge to a gate-worthy plan in %d rounds:\n%s", rounds, FormatPlanProblems(probs))
	}
	if !reflect.DeepEqual(CheckPlanQuality(plan), []PlanProblem(nil)) {
		t.Fatal("converged plan must pass CheckPlanQuality")
	}
}
