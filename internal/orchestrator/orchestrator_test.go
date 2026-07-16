package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/agent"
	"corpos/internal/cost"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/router"
	"corpos/internal/routing"
	"corpos/internal/tool"
)

// fixedClassifier returns a constant routing label (a stand-in for the MCP rubric).
type fixedClassifier struct{ label string }

func (f fixedClassifier) Classify(context.Context, string) (string, error) { return f.label, nil }

// leafRouter routes any duty to the "leaf" profile via a fixed classifier.
func leafRouter() *routing.Router {
	return routing.NewRouter(fixedClassifier{label: routing.LabelChainExecution},
		routing.Table{routing.LabelChainExecution: "leaf"}, "leaf")
}

// noopProvider satisfies tool.Provider; workers in these tests answer directly
// (no tool calls), so it is never actually invoked.
type noopProvider struct{}

func (noopProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: true}
}

// errAdapter fails every completion — to exercise the worker-error path.
type errAdapter struct{}

func (errAdapter) Model() string   { return "err" }
func (errAdapter) Available() bool { return true }
func (errAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{}, errors.New("boom")
}

const leafToml = "name = \"leaf\"\nduty = \"a leaf duty\"\ntier = \"local\"\n"

// testRegistry writes each profile TOML to a temp dir and Loads it (newRegistry is
// unexported; Load is the established loader path).
func testRegistry(t *testing.T) *profile.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leaf.toml"), []byte(leafToml), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	reg, err := profile.Load(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

// leafSpawner builds a Spawner whose workers answer with the given text.
func leafSpawner(answer string) *agent.Spawner {
	worker := model.NewEcho("worker", model.Response{Text: answer, StopReason: model.StopEndTurn})
	return agent.NewSpawner(noopProvider{}, func(*profile.JobProfile) []tool.Spec { return nil }, nil, worker)
}

func spawnCall(params map[string]any) tool.Call {
	return tool.Call{ID: "c1", Surface: Surface, Action: spawnAction, Params: params}
}

func errText(t *testing.T, res tool.Result) string {
	t.Helper()
	v, ok := res.Value.(map[string]any)
	if !ok {
		t.Fatalf("result Value not a map: %+v", res.Value)
	}
	s, _ := v["error"].(string)
	return s
}

func TestDispatchSpawnsWorkerAndReturnsAnswer(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("worker did the thing"), testRegistry(t))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "leaf", "duty": "do the thing"}))
	if !res.OK {
		t.Fatalf("spawn failed: %+v", res.Value)
	}
	v := res.Value.(map[string]any)
	if v["answer"] != "worker did the thing" || v["profile"] != "leaf" || v["duty"] != "do the thing" {
		t.Errorf("unexpected spawn result: %+v", v)
	}
}

func TestEndToEndDecomposeSpawnReconcile(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("subtask answer"), testRegistry(t))
	// The orchestrate agent: turn 1 delegates a duty via agent.spawn; turn 2
	// synthesizes the worker's answer into the final result.
	orch := model.NewEcho("orchestrate",
		model.Response{
			ToolCalls:  []tool.Call{spawnCall(map[string]any{"profile": "leaf", "duty": "do subtask"})},
			StopReason: model.StopToolUse,
		},
		model.Response{Text: "final: subtask answer", StopReason: model.StopEndTurn},
	)
	loop := agent.New(router.New(orch, orch), sp, sp.Specs())
	res, err := loop.Run(context.Background(), "achieve the goal")
	if err != nil {
		t.Fatalf("orchestration: %v", err)
	}
	if res.Text != "final: subtask answer" {
		t.Errorf("reconciled answer = %q", res.Text)
	}
	if len(res.Dispatches) != 1 || !res.Dispatches[0].OK {
		t.Errorf("expected one successful spawn dispatch, got %+v", res.Dispatches)
	}
}

// TestDispatchRejections: a spawn CONFIG/precondition rejection (bad action, missing/unknown
// profile, empty duty) is ClassUsage, NOT ClassTool — no stronger model can fix it, so it must
// not feed the orchestrator's repeated_tool_error escalation (bug 1095). Tally confirms each
// counts as a usage error, not a tool error (the escalation source).
func TestDispatchRejections(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("x"), testRegistry(t))
	cases := []struct {
		name string
		call tool.Call
	}{
		{"unknown-action", tool.Call{Surface: Surface, Action: "nope"}},
		{"missing-profile", spawnCall(map[string]any{"duty": "d"})},
		{"missing-duty", spawnCall(map[string]any{"profile": "leaf"})},
		{"unknown-profile", spawnCall(map[string]any{"profile": "ghost", "duty": "d"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := sp.Dispatch(context.Background(), tc.call)
			if res.OK || res.ErrorClass != tool.ClassUsage {
				t.Errorf("expected usage-class (non-escalatable) failure, got OK=%v class=%s", res.OK, res.ErrorClass)
			}
			if tally := tool.Tally([]tool.Result{res}); tally.ToolErrors != 0 || tally.UsageErrors != 1 {
				t.Errorf("a config rejection must not count as a tool error (escalation source); got ToolErrors=%d UsageErrors=%d", tally.ToolErrors, tally.UsageErrors)
			}
		})
	}
}

func TestDispatchWorkerError(t *testing.T) {
	spawner := agent.NewSpawner(noopProvider{}, func(*profile.JobProfile) []tool.Spec { return nil }, nil, errAdapter{})
	sp := NewSpawnProvider(spawner, testRegistry(t))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "leaf", "duty": "d"}))
	if res.OK || !strings.Contains(errText(t, res), "worker") {
		t.Errorf("expected worker-error failure, got %+v", res.Value)
	}
	// A genuine worker RUNTIME fault (the adapter erroring) stays ClassTool — escalatable,
	// because a stronger model could plausibly help. Only config/precondition errors are
	// demoted to ClassUsage (TestDispatchVerifyGatePreconditionIsUsageClass).
	if res.ErrorClass != tool.ClassTool {
		t.Errorf("a worker runtime fault must stay tool-class (escalatable), got %s", res.ErrorClass)
	}
}

// verifyGatedToml declares a go build/test gate; pointed at a module-less verify-dir its
// VerifyGateRunnable precondition fails — the bug-1095 spawn-config-error shape.
const verifyGatedToml = "name = \"gated\"\nduty = \"d\"\ntier = \"local\"\nverify_command = [\"go\", \"build\", \"./...\"]\n"

func verifyGatedRegistry(t *testing.T) *profile.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gated.toml"), []byte(verifyGatedToml), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	reg, err := profile.Load(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

// TestDispatchVerifyGatePreconditionIsUsageClass is the bug 1095 regression (orchestrator
// side): a spawn whose worker can't even start because the verify gate has no module root
// (the "verify-dir has no go.mod" precondition) is a CONFIG error no model can fix. It must
// come back ClassUsage so the orchestrator does not climb Gemini→Opus retrying an unfixable
// spawn (94.7% of run spend wasted on the frontier in the rehearsal). Tally confirms it is
// not counted as a tool error (the repeated_tool_error escalation source).
func TestDispatchVerifyGatePreconditionIsUsageClass(t *testing.T) {
	moduleless := t.TempDir() // a verify-dir with no reachable/unique go.mod
	worker := model.NewEcho("worker", model.Response{Text: "unreached", StopReason: model.StopEndTurn})
	spawner := agent.NewSpawner(noopProvider{}, func(*profile.JobProfile) []tool.Spec { return nil }, nil,
		worker, agent.WithSpawnVerifyDir(moduleless))
	sp := NewSpawnProvider(spawner, verifyGatedRegistry(t))

	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "gated", "duty": "d"}))
	if res.OK {
		t.Fatalf("expected the verify-gate precondition to fail the spawn, got OK result %+v", res.Value)
	}
	if res.ErrorClass != tool.ClassUsage {
		t.Errorf("a verify-gate precondition error must be usage-class (non-escalatable), got %s", res.ErrorClass)
	}
	if !strings.Contains(errText(t, res), "go.mod") {
		t.Errorf("the actionable precondition message should survive, got %q", errText(t, res))
	}
	if tally := tool.Tally([]tool.Result{res}); tally.ToolErrors != 0 || tally.UsageErrors != 1 {
		t.Errorf("a precondition error must not feed repeated_tool_error; got ToolErrors=%d UsageErrors=%d", tally.ToolErrors, tally.UsageErrors)
	}
}

func TestDispatchDepthGuard(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("x"), testRegistry(t), WithMaxDepth(2))
	ctx := context.WithValue(context.Background(), depthKey{}, 2) // already at the limit
	res := sp.Dispatch(ctx, spawnCall(map[string]any{"profile": "leaf", "duty": "d"}))
	if res.OK || !strings.Contains(errText(t, res), "depth limit") {
		t.Errorf("expected depth-limit refusal, got %+v", res.Value)
	}
}

// The spawn-count budget (on the SPAWNER — the chokepoint every spawn flows through) caps the
// fan-out: spawns up to the cap succeed, then a further spawn is REFUSED. Through Dispatch the
// refusal surfaces as ClassUsage (a structural bound no stronger model can lift, classified in
// spawnRunFailure), so it does NOT feed the orchestrator's escalation tally, and carries the
// "stop decomposing, synthesize" directive — the over-decomposition bound (Run-42: 34 workers).
func TestDispatchSpawnBudget(t *testing.T) {
	budget := cost.NewSpawnBudget(2)
	worker := model.NewEcho("worker", model.Response{Text: "ans", StopReason: model.StopEndTurn})
	spawner := agent.NewSpawner(noopProvider{}, func(*profile.JobProfile) []tool.Spec { return nil }, nil, worker,
		agent.WithSpawnerSpawnBudget(budget))
	sp := NewSpawnProvider(spawner, testRegistry(t))
	call := spawnCall(map[string]any{"profile": "leaf", "duty": "d"})
	for i := 1; i <= 2; i++ {
		if res := sp.Dispatch(context.Background(), call); !res.OK {
			t.Fatalf("spawn %d within budget must succeed, got %+v", i, res.Value)
		}
	}
	res := sp.Dispatch(context.Background(), call)
	if res.OK {
		t.Fatal("a spawn past the budget must be refused")
	}
	if res.ErrorClass != tool.ClassUsage {
		t.Errorf("budget refusal must be ClassUsage (non-escalatable), got %s", res.ErrorClass)
	}
	if msg := errText(t, res); !strings.Contains(msg, "STOP decomposing") || !strings.Contains(strings.ToLower(msg), "synthesize") {
		t.Errorf("refusal must direct the orchestrator to stop and synthesize, got %q", msg)
	}
}

// A nil/unset budget leaves spawning unbounded (the default — bounded only by depth + cost).
func TestDispatchNoBudgetUnbounded(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("ans"), testRegistry(t))
	call := spawnCall(map[string]any{"profile": "leaf", "duty": "d"})
	for i := 0; i < 30; i++ {
		if res := sp.Dispatch(context.Background(), call); !res.OK {
			t.Fatalf("with no budget, spawn %d must succeed, got %+v", i, res.Value)
		}
	}
}

func TestDispatchAutoRoutesOmittedProfile(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("auto answer"), testRegistry(t), WithRouter(leafRouter()))
	// No profile param: the router classifies the duty and picks one.
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"duty": "do the thing"}))
	if !res.OK {
		t.Fatalf("auto-route spawn failed: %+v", res.Value)
	}
	v := res.Value.(map[string]any)
	if v["profile"] != "leaf" || v["auto_routed"] != true || v["routed_label"] != routing.LabelChainExecution {
		t.Errorf("auto-route result = %+v, want profile leaf / auto_routed / chain-execution label", v)
	}
}

func TestDispatchAutoRouteSentinel(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("a"), testRegistry(t), WithRouter(leafRouter()))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "auto", "duty": "d"}))
	if !res.OK {
		t.Fatalf("'auto' sentinel should route, got %+v", res.Value)
	}
}

func TestDispatchOmittedProfileWithoutRouterErrors(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("x"), testRegistry(t)) // no router
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"duty": "d"}))
	if res.OK || !strings.Contains(errText(t, res), "profile") {
		t.Errorf("an omitted profile with no router should fail, got %+v", res.Value)
	}
}

func TestWithRouterIgnoresNil(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("x"), testRegistry(t), WithRouter(nil))
	if sp.router != nil {
		t.Error("a nil router must be ignored")
	}
}

func TestWithMaxDepthIgnoresNonPositive(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("x"), testRegistry(t), WithMaxDepth(0))
	if sp.maxDepth != defaultMaxDepth {
		t.Errorf("WithMaxDepth(0) should be ignored; maxDepth=%d", sp.maxDepth)
	}
}

func TestSpecsExposeAgentSurface(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("x"), testRegistry(t))
	specs := sp.Specs()
	if len(specs) != 1 || specs[0].Name != Surface {
		t.Errorf("expected one %q spec, got %+v", Surface, specs)
	}
}

// --- live coding path (operator-seat organ) routing -------------------------

const codingToml = "name = \"atomic-coding-chain\"\nduty = \"a coding duty\"\ntier = \"local\"\n"

// codingRegistry has both the leaf profile and the atomic-coding-chain profile, so
// a duty can resolve to either and exercise the route branch.
func codingRegistry(t *testing.T) *profile.Registry {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"leaf.toml": leafToml, "atomic-coding-chain.toml": codingToml} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write profile: %v", err)
		}
	}
	reg, err := profile.Load(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

// recordingPath is a CodingPath stub: it records that it was called and returns a
// fixed organ answer + cost (the seat outcome the bridge would map back).
type recordingPath struct {
	called bool
	duty   string
	pName  string
}

func (r *recordingPath) fn(_ context.Context, duty string, p *profile.JobProfile) (string, float64, error) {
	r.called = true
	r.duty = duty
	r.pName = p.Name
	return "organ: chain succeeded; integration commit deadbeef", 0.42, nil
}

func TestCodingDutyRoutesToOrgan(t *testing.T) {
	rp := &recordingPath{}
	// The bare spawner answers "bare worker ran" — so if the duty took the bare path
	// instead of the organ, the answer would differ.
	sp := NewSpawnProvider(leafSpawner("bare worker ran"), codingRegistry(t), WithCodingPath(rp.fn))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "atomic-coding-chain", "duty": "fix the bug"}))
	if !res.OK {
		t.Fatalf("organ route failed: %+v", res.Value)
	}
	if !rp.called || rp.duty != "fix the bug" || rp.pName != "atomic-coding-chain" {
		t.Fatalf("organ closure not invoked with the resolved profile: %+v", rp)
	}
	v := res.Value.(map[string]any)
	if v["profile"] != "atomic-coding-chain" || v["duty"] != "fix the bug" {
		t.Errorf("result identity = %+v", v)
	}
	if v["answer"] != "organ: chain succeeded; integration commit deadbeef" {
		t.Errorf("answer not mapped from the organ: %+v", v["answer"])
	}
	if v["cost_usd"] != 0.42 {
		t.Errorf("cost_usd not mapped from the organ: %+v", v["cost_usd"])
	}
}

func TestNonCodingDutyStaysOnBareWorker(t *testing.T) {
	rp := &recordingPath{}
	sp := NewSpawnProvider(leafSpawner("bare worker ran"), codingRegistry(t), WithCodingPath(rp.fn))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "leaf", "duty": "synthesize"}))
	if !res.OK {
		t.Fatalf("bare route failed: %+v", res.Value)
	}
	if rp.called {
		t.Fatal("a non-coding profile must NOT route to the organ")
	}
	if res.Value.(map[string]any)["answer"] != "bare worker ran" {
		t.Errorf("non-coding duty should get the bare worker's answer, got %+v", res.Value)
	}
}

func TestCodingDutyNilPathUsesBareWorker(t *testing.T) {
	// Default nil CodingPath: even the coding profile stays on the bare worker, so
	// the existing behavior is preserved when the organ is not wired.
	sp := NewSpawnProvider(leafSpawner("bare worker ran"), codingRegistry(t))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "atomic-coding-chain", "duty": "fix it"}))
	if !res.OK {
		t.Fatalf("nil-path coding route failed: %+v", res.Value)
	}
	if res.Value.(map[string]any)["answer"] != "bare worker ran" {
		t.Errorf("nil CodingPath should fall back to the bare worker, got %+v", res.Value)
	}
}

func TestCodingPathErrorIsToolFailure(t *testing.T) {
	failPath := func(context.Context, string, *profile.JobProfile) (string, float64, error) {
		return "", 0, errors.New("organ infra fault")
	}
	sp := NewSpawnProvider(leafSpawner("x"), codingRegistry(t), WithCodingPath(failPath))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"profile": "atomic-coding-chain", "duty": "d"}))
	if res.OK || res.ErrorClass != tool.ClassTool {
		t.Errorf("an organ infra error should be a tool-class failure, got OK=%v class=%s", res.OK, res.ErrorClass)
	}
}

func TestCodingDutyAutoRoutedCarriesLabel(t *testing.T) {
	rp := &recordingPath{}
	// An omitted profile on a coding-shaped duty auto-routes to the coding profile
	// (the router's coding heuristic) and reaches the organ — AND the auto_routed /
	// routed_label metadata is carried through the organ path, same as the bare path.
	codeRouter := routing.NewRouter(fixedClassifier{label: routing.LabelChainExecution},
		routing.Table{routing.LabelChainExecution: "atomic-coding-chain"}, "atomic-coding-chain")
	sp := NewSpawnProvider(leafSpawner("bare"), codingRegistry(t), WithRouter(codeRouter), WithCodingPath(rp.fn))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{"duty": "write code to fix the parser bug"}))
	if !res.OK || !rp.called {
		t.Fatalf("auto-routed coding duty should reach the organ: ok=%v called=%v", res.OK, rp.called)
	}
	if rp.pName != "atomic-coding-chain" {
		t.Fatalf("auto-route should resolve the coding profile, got %q", rp.pName)
	}
	v := res.Value.(map[string]any)
	if v["auto_routed"] != true {
		t.Errorf("auto_routed not carried on the organ path: %+v", v)
	}
	if label, _ := v["routed_label"].(string); label == "" {
		t.Errorf("routed_label not carried on the organ path: %+v", v)
	}
}

func TestWithCodingPathIgnoresNil(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("x"), testRegistry(t), WithCodingPath(nil))
	if sp.codingPath != nil {
		t.Error("a nil CodingPath must be ignored")
	}
}
