package coding

import (
	"context"
	"os"
	"os/exec"
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

// TestLiveFeaturePipelineEndToEnd is the capstone proof of the gate-authoring bridge: from
// a PROSE FeatureSpec, the WHOLE automated pipeline runs unattended —
//
//	Planner.Plan (Qwen decomposes + the plan-quality gate certifies)
//	  → OracleAuthor.Author (Qwen writes each atom's acceptance test; go/parser + the
//	    git red-now checker validate it)
//	  → FeaturePlanner.Assemble → BuildFeatureChain
//	  → the operator seat executes the chain to green on the canonical ladder.
//
// No human authored a task, a gate, or a test. The principal supplied only the prose goal.
//
// Gated: CORPOS_LIVE=1 + OPENROUTER/ANTHROPIC keys + a reachable toolkit + local Qwen.
func TestLiveFeaturePipelineEndToEnd(t *testing.T) {
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 (and source corpos.env) to run the end-to-end feature pipeline")
	}
	or, an := os.Getenv("OPENROUTER_API_KEY"), os.Getenv("ANTHROPIC_API_KEY")
	if or == "" || an == "" {
		t.Skip("OPENROUTER_API_KEY and ANTHROPIC_API_KEY required for the canonical ladder")
	}
	qwen := model.NewOpenAICompat("Qwen2.5-32B-Instruct-Q4_K_M.gguf", "http://localhost:8081/v1")
	if !qwen.Available() {
		t.Skip("local Qwen not available")
	}

	// A fresh Go-module fixture with a single EMPTY feature package — the feature is fully
	// absent, so every authored oracle is red against HEAD.
	const pkgDir = "internal/intmath"
	fixture := initEmptyPkgModuleTarget(t, pkgDir)
	head := gitHead(t, fixture)

	// --- Plan + author (assemble the chain) -------------------------------------------
	planRepo := NewGitRepo(ExecRunner{}, fixture, t.TempDir())
	planner := NewPlanner(qwen, 5)
	author := NewOracleAuthor(qwen, NewGitRedChecker(planRepo, head, 90*time.Second), 4)
	fp := NewFeaturePlanner(planner, author)

	asmCtx, cancelAsm := context.WithTimeout(context.Background(), 8*time.Minute)
	chain, report, err := fp.Assemble(asmCtx, FeatureSpec{
		Slug:     "intmath",
		Goal:     "Add an integer-math utility to the package: GCD(a, b int) int returning the greatest common divisor (always non-negative), and LCM(a, b int) int returning the least common multiple of a and b. LCM must be defined in terms of GCD.",
		Packages: []PackageTarget{{Dir: pkgDir, PackageName: "intmath"}},
	}, "feat-intmath", fixture)
	cancelAsm()

	t.Logf("=== plan (%d rounds, %d atoms) ===", report.PlanRounds, len(report.Plan.Tasks))
	for _, at := range report.Plan.Tasks {
		t.Logf("  - %s: %s [asserts: %s]", at.Slug, at.Goal, at.Assertion)
	}
	for _, o := range report.Oracles {
		t.Logf("--- oracle %s (%s) ---\n%s", o.TestFunc, o.TestPath, o.TestSource)
	}
	if err != nil {
		t.Fatalf("assemble (plan→author) failed: %v", err)
	}

	// --- Execute the assembled chain on the canonical ladder --------------------------
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelSetup()
	client := mcp.New(mcp.DefaultToolkitURL, mcp.WithProject("feature-pipeline"))
	specs := mcp.EnrichedSpecs(setupCtx, client)
	if len(specs) == 0 {
		t.Skip("toolkit-server not reachable / no specs at " + mcp.DefaultToolkitURL)
	}
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

	reg, err := profile.Builtin()
	if err != nil {
		t.Fatalf("builtin profiles: %v", err)
	}
	p, ok := reg.Get("atomic-coding-chain")
	if !ok {
		t.Fatal("atomic-coding-chain profile missing")
	}
	p.VerifyCommand = nil // each AT carries its own authored gate

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
	wtRoot := filepath.Join(os.TempDir(), "fp-worktrees-"+runID)
	repo := NewGitRepo(ExecRunner{}, fixture, wtRoot)
	orch := New(WithRepo(repo), WithModelWorker(NewModelWorker(spawner, &p)), WithGateTimeout(5*time.Minute))
	seat := NewOperatorSeat(orch, ModelOperator{}, mid, strong, WithK(2), WithCoderRung(coder))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := RunDuty(ctx, orch, seat, chain, runID)
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}

	t.Logf("=== end-to-end result ===")
	t.Logf("answer: %s", res.Answer)
	t.Logf("operator-seat cost: $%.4f", res.CostUSD)
	t.Logf("worktrees (inspect): %s", wtRoot)
	t.Logf("fs mutations: %d", provider.muts)

	if provider.muts == 0 {
		t.Fatalf("no fs mutations — the pipeline never produced code")
	}
	if !strings.Contains(res.Answer, "succeeded") {
		t.Fatalf("the assembled feature chain did not carry all tasks to green: %s", res.Answer)
	}

	// Direct proof: the authored acceptance tests pass on the INTEGRATED result. The integrated
	// code lands on the orchestrator's integration branch, so materialize that commit into a
	// fresh tree and run go test THERE. Running against `fixture` (the base working tree, where
	// the feature package is still empty) would pass VACUOUSLY as "ok, no test files".
	integrated := materializeIntegration(t, fixture, res.Answer)
	verify := exec.Command("go", "test", "-count=1", "./"+pkgDir+"/")
	verify.Dir = integrated
	verify.Env = cleanGitEnv()
	if out, verr := verify.CombinedOutput(); verr != nil {
		t.Fatalf("integrated feature does not pass its own authored acceptance tests: %v\n%s", verr, out)
	}
}
