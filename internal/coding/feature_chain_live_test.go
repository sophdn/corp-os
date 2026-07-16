package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"corpos/internal/agent"
	"corpos/internal/fsorgan"
	"corpos/internal/mcp"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/sysorgan"
	"corpos/internal/tool"
)

// TestLiveFeatureChainExecution is Phase 1 of "corpos works through a feature chain":
// PROVE EXECUTION. The principal (me) hand-authors a small feature as a 3-task chain —
// each task a CRISP goal + a pre-placed acceptance test as its OWN gate + a workspace
// allowlist + protected *_test.go — and we drive the WHOLE chain through the operator
// seat. It isolates "can corpos execute a multi-task feature chain (sequencing + the
// accumulating integration branch + per-task gates)" from "can corpos plan one". The
// multi-AT engine + input threading already exist + are tested (TestTrivialChainRuns...);
// this is the first time the REAL operator-seat path drives a multi-task FEATURE chain.
//
// Each AT carries its OWN Gate (a per-package `go test`), so the profile's worker
// self-verify is left empty and the orchestrator's per-AT gate certifies each task. The
// AT's Protected (**/*_test.go) is enforced per-attempt by ModelWorker.Attempt, so the
// worker can never tamper with the principal-authored oracle.
//
// Gated: CORPOS_LIVE=1 + OPENROUTER/ANTHROPIC keys + a reachable toolkit + FC_FIXTURE
// pointing at the feature-chain fixture (3 packages with pre-placed RED acceptance tests).
func TestLiveFeatureChainExecution(t *testing.T) {
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 (and source corpos.env) to run the live feature-chain execution")
	}
	or, an := os.Getenv("OPENROUTER_API_KEY"), os.Getenv("ANTHROPIC_API_KEY")
	if or == "" || an == "" {
		t.Skip("OPENROUTER_API_KEY and ANTHROPIC_API_KEY required for the canonical ladder")
	}
	fixture := os.Getenv("FC_FIXTURE")
	if fixture == "" {
		t.Skip("set FC_FIXTURE to the feature-chain fixture")
	}

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelSetup()
	client := mcp.New(mcp.DefaultToolkitURL, mcp.WithProject("feature-chain"))
	specs := mcp.EnrichedSpecs(setupCtx, client)
	if len(specs) == 0 {
		t.Skip("toolkit-server not reachable / no specs at " + mcp.DefaultToolkitURL)
	}
	// Host-native fs + sys organs as the sole owners of those surfaces (the worker needs
	// real, action-enum-enriched fs/sys, exactly as cmd/corpos mounts them).
	fsOrgan := fsorgan.New()
	sysOrgan := sysorgan.New(client)
	agg, err := mcp.NewAggregator(
		mcp.Server{Name: "toolkit", Provider: client, Specs: dropSurfaces(specs, "fs", "sys")},
		mcp.Server{Name: "fs-native", Provider: fsOrgan, Specs: fsOrgan.Specs()},
		mcp.Server{Name: "sys-native", Provider: sysOrgan, Specs: sysOrgan.Specs()},
	)
	if err != nil {
		t.Fatalf("aggregator: %v", err)
	}
	provider := &countingProvider{inner: agg, specs: agg.Specs()}

	// The real atomic-coding-chain profile for the worker's fs/sys scope + faithful-reporting
	// system prompt + minimal/targeted/stop prompting. Empty VerifyCommand: each AT carries its
	// own gate (below), so the worker self-verify gate is the orchestrator's per-AT gate.
	reg, err := profile.Builtin()
	if err != nil {
		t.Fatalf("builtin profiles: %v", err)
	}
	p, ok := reg.Get("atomic-coding-chain")
	if !ok {
		t.Fatal("atomic-coding-chain profile missing")
	}
	p.VerifyCommand = nil

	qwen := model.NewOpenAICompat("Qwen2.5-32B-Instruct-Q4_K_M.gguf", "http://localhost:8081/v1")
	mid := model.NewOpenAICompat("google/gemini-3.1-flash-lite", "https://openrouter.ai/api/v1",
		model.WithOACKey(or), model.WithOACRequireKey(), model.WithOACUsageAccounting())
	coder := model.NewOpenAICompat("deepseek/deepseek-v3.2", "https://openrouter.ai/api/v1",
		model.WithOACKey(or), model.WithOACRequireKey(), model.WithOACUsageAccounting())
	strong := model.NewAnthropic("claude-opus-4-8", model.WithAnthropicKey(an), model.WithAnthropicMaxTokens(2048))

	spawner := agent.NewSpawner(provider,
		func(pp *profile.JobProfile) []tool.Spec { return mcp.Project(provider.Specs(), scopeOf(pp)) },
		nil, qwen,
		agent.WithMidTier(mid),
		agent.WithStrongTier(strong, 1),
		agent.WithStrongBound(2),
		agent.WithCodingRung(coder),
		agent.WithSpawnVerifyDir(fixture),
	)

	runID := NewRunID()
	wtRoot := filepath.Join(os.TempDir(), "fc-worktrees-"+runID)
	repo := NewGitRepo(ExecRunner{}, fixture, wtRoot)
	orch := New(WithRepo(repo), WithModelWorker(NewModelWorker(spawner, &p)), WithGateTimeout(5*time.Minute))
	seat := NewOperatorSeat(orch, ModelOperator{}, mid, strong, WithK(2), WithCoderRung(coder))

	protectTests := []string{"**/*_test.go"}
	gate := func(pkg string) [][]string {
		return [][]string{{"sh", "-c", "go test ./internal/" + pkg + "/"}}
	}
	// The hand-authored feature chain: 3 crisp, sequenced, separately-gated tasks. parse and
	// double are independent; pipeline composes both (it imports the prior two packages, which
	// the accumulating integration branch makes available).
	chain := Chain{
		Slug:       "feature-parse-double-pipeline",
		TargetRepo: fixture,
		Tasks: []AtomicTask{
			{
				Slug:          "parse",
				Goal:          "Create internal/parse/parse.go in `package parse` with exported `func Parse(s string) (int, error)` that parses a decimal integer string (strconv.Atoi is fine) — return its value, or a non-nil error for a non-numeric string. Make the test in internal/parse pass. Do not modify any _test.go file.",
				Workspace:     []string{"internal/parse/parse.go"},
				Protected:     protectTests,
				Worker:        WorkerConfig{Kind: WorkerModel},
				Gate:          gate("parse"),
				MaxIterations: 4,
			},
			{
				Slug:          "double",
				Goal:          "Create internal/double/double.go in `package double` with exported `func Double(n int) int` returning n*2. Make the test in internal/double pass. Do not modify any _test.go file.",
				Workspace:     []string{"internal/double/double.go"},
				Protected:     protectTests,
				Worker:        WorkerConfig{Kind: WorkerModel},
				Gate:          gate("double"),
				MaxIterations: 4,
			},
			{
				Slug:          "pipeline",
				Goal:          "Create internal/pipeline/pipeline.go in `package pipeline` with exported `func Run(s string) (int, error)` that parses s with featurechain/internal/parse.Parse and, on success, doubles it with featurechain/internal/double.Double — returning the doubled value, or the parse error. Make the test in internal/pipeline pass. Do not modify any _test.go file.",
				Workspace:     []string{"internal/pipeline/pipeline.go"},
				Protected:     protectTests,
				Worker:        WorkerConfig{Kind: WorkerModel},
				Gate:          gate("pipeline"),
				MaxIterations: 4,
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	res, err := RunDuty(ctx, orch, seat, chain, runID)
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}

	t.Logf("=== feature-chain result ===")
	t.Logf("answer: %s", res.Answer)
	t.Logf("operator-seat cost: $%.4f", res.CostUSD)
	t.Logf("worktrees (inspect): %s", wtRoot)
	t.Logf("fs mutations: %d", provider.muts)

	if provider.muts == 0 {
		t.Fatalf("no fs mutations — the chain never produced code")
	}
	if !strings.Contains(res.Answer, "succeeded") {
		t.Fatalf("the feature chain did not carry all tasks to green: %s", res.Answer)
	}
}
