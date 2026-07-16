package coding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// countingProvider wraps the real toolkit provider and tallies every substantive fs
// mutation dispatch (write/edit/move/remove) the worker actually issues. It is the
// direct observable for bug 1078: the defect was a coding worker that claimed "done"
// with ZERO such dispatches. A non-zero count is the fix's primary success signal.
type countingProvider struct {
	inner tool.Provider
	specs []tool.Spec
	muts  int
	calls []string // EVERY dispatch (surface.action ok=…) — the worker-behavior trace
}

func (c *countingProvider) Specs() []tool.Spec { return c.specs }

// dropSurfaces returns specs without the named surfaces (the harness counterpart to
// cmd/corpos's dropSurfaceSpec — used to strip the toolkit's fs/sys before the native
// organs take over those surfaces).
func dropSurfaces(specs []tool.Spec, drop ...string) []tool.Spec {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	out := make([]tool.Spec, 0, len(specs))
	for _, s := range specs {
		if !skip[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

func (c *countingProvider) Dispatch(ctx context.Context, call tool.Call) tool.Result {
	r := c.inner.Dispatch(ctx, call)
	c.calls = append(c.calls, fmt.Sprintf("%s.%s ok=%v", call.Surface, call.Action, r.OK))
	if call.Surface == "fs" {
		switch call.Action {
		case "write", "edit", "move", "remove":
			c.muts++
		}
	}
	return r
}

// TestLiveDogfoodT3bOperatorSeat is the bug-1078 end-to-end validation: drive the REAL
// operator-seat organ (the live coding path) on the canonical ladder
// (Qwen floor → Gemini mid → DeepSeek coder rung → bounded Opus) against the seeded
// bug-1070 dispatch regression, and confirm the coding worker now DISPATCHES real
// fs.write/edit mutations (the diff carried to the gate) instead of fabricating a
// no-work "done". Mirrors cmd/corpos buildCodingPath; the only deviations are a counting
// provider (to observe the mutation signal) and a narrowed gate (the dispatch package
// only — the full-module suite is 4min and pre-broken in an isolated copy).
//
// Gated: CORPOS_LIVE=1 + OPENROUTER_API_KEY + ANTHROPIC_API_KEY + a reachable toolkit +
// T3B_FIXTURE pointing at the seeded-RED go-module-root copy.
func TestLiveDogfoodT3bOperatorSeat(t *testing.T) {
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 (and source corpos.env) to run the live t3b operator-seat dogfood")
	}
	or, an := os.Getenv("OPENROUTER_API_KEY"), os.Getenv("ANTHROPIC_API_KEY")
	if or == "" || an == "" {
		t.Skip("OPENROUTER_API_KEY and ANTHROPIC_API_KEY required for the canonical ladder")
	}
	fixture := os.Getenv("T3B_FIXTURE")
	if fixture == "" {
		t.Skip("set T3B_FIXTURE to the seeded bug-1070 go-module-root copy")
	}

	// Real toolkit provider (fs surface writes land on disk), wrapped to count mutations.
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelSetup()
	client := mcp.New(mcp.DefaultToolkitURL, mcp.WithProject("dogfood-t3b"))
	specs := mcp.EnrichedSpecs(setupCtx, client)
	if len(specs) == 0 {
		t.Skip("toolkit-server not reachable / no specs at " + mcp.DefaultToolkitURL)
	}
	// Mount the host-native fs + sys organs as the sole owners of those surfaces, exactly as
	// cmd/corpos's mountNativeFS/mountNativeSys do: strip the toolkit's fs/sys specs (the
	// toolkit's fs comes back THIN — no action enum — so an action-scoped profile would drop
	// it on projection, and its sys enum lacks "exec") and substitute the native organs, whose
	// enriched specs carry the action enum and whose dispatch does real in-process host file
	// I/O + exec. Without this the worker is handed ZERO tools and "done" is its only move.
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

	// The real atomic-coding-chain profile (its fs/sys scope + faithful-reporting system
	// prompt + protected *_test.go), with ONLY the gate narrowed to the dispatch package.
	reg, err := profile.Builtin()
	if err != nil {
		t.Fatalf("builtin profiles: %v", err)
	}
	p, ok := reg.Get("atomic-coding-chain")
	if !ok {
		t.Fatal("atomic-coding-chain profile missing from the builtin registry")
	}
	// Narrow gate to the package under test. DOGFOOD_VERIFY_CMD overrides the whole gate
	// (e.g. a `-run` filter when the package's full suite is slow); else DOGFOOD_VERIFY_PKG
	// names the package (default the bug-1070 dispatch package).
	gateCmd := os.Getenv("DOGFOOD_VERIFY_CMD")
	if gateCmd == "" {
		verifyPkg := os.Getenv("DOGFOOD_VERIFY_PKG")
		if verifyPkg == "" {
			verifyPkg = "./internal/dispatch"
		}
		gateCmd = "go build " + verifyPkg + "/... && go test " + verifyPkg + "/"
	}
	p.VerifyCommand = []string{"sh", "-c", gateCmd}

	// DIAGNOSTIC: what tool specs does the worker actually SEE? If the toolkit returned thin
	// specs (no action enum), projectActions fails closed and drops fs/sys → the worker has no
	// tools and "done" is its only move (the lever-1 latent risk). Log the projected surface set.
	projected := mcp.Project(provider.Specs(), scopeOf(&p))
	t.Logf("DIAGNOSTIC: enriched specs from toolkit: %d", len(provider.specs))
	t.Logf("DIAGNOSTIC: projected to atomic-coding-chain worker: %d surfaces", len(projected))
	for _, s := range projected {
		hasEnum := false
		if props, ok := s.InputSchema["properties"].(map[string]any); ok {
			if act, ok := props["action"].(map[string]any); ok {
				_, hasEnum = act["enum"]
			}
		}
		t.Logf("DIAGNOSTIC:   worker-visible surface %q (action-enum present=%v)", s.Name, hasEnum)
	}

	// Canonical ladder (identical models to cmd/corpos buildCodingPath).
	qwen := model.NewOpenAICompat("Qwen2.5-32B-Instruct-Q4_K_M.gguf", "http://localhost:8081/v1")
	mid := model.NewOpenAICompat("google/gemini-3.1-flash-lite", "https://openrouter.ai/api/v1",
		model.WithOACKey(or), model.WithOACRequireKey(), model.WithOACUsageAccounting())
	coder := model.NewOpenAICompat("deepseek/deepseek-v3.2", "https://openrouter.ai/api/v1",
		model.WithOACKey(or), model.WithOACRequireKey(), model.WithOACUsageAccounting())
	strong := model.NewAnthropic("claude-opus-4-8", model.WithAnthropicKey(an), model.WithAnthropicMaxTokens(2048))

	// Spawner with the worker's own escalation ladder + its self-verify gate, exactly as
	// cmd/corpos wires it: floor Qwen → mid Gemini → DeepSeek coder rung → bounded Opus.
	spawner := agent.NewSpawner(provider,
		func(pp *profile.JobProfile) []tool.Spec { return mcp.Project(provider.Specs(), scopeOf(pp)) },
		nil, qwen,
		agent.WithMidTier(mid),
		agent.WithStrongTier(strong, 1),
		agent.WithStrongBound(2),
		agent.WithCodingRung(coder),
		agent.WithSpawnVerifyDir(fixture),
	)

	// Full organ: orchestrator + operator SEAT (mid → coder → strong, K=2), exactly as
	// cmd/corpos buildCodingPath wires it. The seat's branch_fix worktree-collision bug
	// (operator-seat-branch-fix-readds-same-worktree-branch-git-exit-255) is fixed, so the
	// seat now escalates past branch_fix to a capable rung instead of aborting at exit 255.
	// This exercises the whole bug-1078 surface end-to-end: the worker DISPATCHES fs mutations
	// (acting, not fabricating), and on a stuck floor the seat authors on the coder/Opus rung.
	runID := NewRunID()
	wtRoot := filepath.Join(os.TempDir(), "t3b-worktrees-"+runID) // persistent (inspectable)
	repo := NewGitRepo(ExecRunner{}, fixture, wtRoot)
	orch := New(WithRepo(repo), WithModelWorker(NewModelWorker(spawner, &p)), WithGateTimeout(10*time.Minute), WithCoverageGrade())
	// Bump the per-AT revise budget so a fabricated first attempt has room to be re-prompted
	// AND respawned with the gate diagnostic.
	p.VerifyMaxRounds = 5
	seat := NewOperatorSeat(orch, ModelOperator{}, mid, strong, WithK(2), WithCoderRung(coder))
	// The duty (DOGFOOD_DUTY, default the bug-1070 dispatch brief) — points the worker at
	// the seeded regression's failing behaviour without handing it the patch.
	duty := os.Getenv("DOGFOOD_DUTY")
	if duty == "" {
		duty = dogfoodT3bDuty
	}
	chain := BridgeChain(runID, duty, fixture, &p)

	// A full operator-seat run escalates mid → coder → bounded Opus across several worker
	// attempts + branch_fix interventions — dozens of real API calls — so the wall-clock is
	// dominated by model latency (the in-worktree gate itself is sub-second). 8m clipped a
	// legitimately-converging run mid-build (exit 124); give escalation room to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	res, err := RunDuty(ctx, orch, seat, chain, runID)
	if err != nil {
		t.Fatalf("RunDuty: %v", err)
	}

	t.Logf("=== t3b dogfood result ===")
	t.Logf("answer: %s", res.Answer)
	t.Logf("worktrees (inspect): %s", wtRoot)
	t.Logf("fs mutation dispatches by the worker: %d", provider.muts)
	t.Logf("FULL dispatch trace (%d calls): %v", len(provider.calls), provider.calls)

	// PRIMARY acceptance signal (bug 1078): the worker dispatched at least one real fs
	// mutation — it acted instead of fabricating a no-work "done".
	if provider.muts == 0 {
		t.Fatalf("REGRESSION: worker dispatched ZERO fs mutations — the no-work fabrication is NOT fixed")
	}
}

// dogfoodT3bDuty points the worker at the seeded regression's failing behaviour without
// handing it the patch (the production fix) or the acceptance test body.
const dogfoodT3bDuty = `A regression was introduced in this Go module (package toolkit, module root is the repo root). ` +
	`In internal/dispatch, a ` + "`project`" + ` value nested inside an action's params map is silently dropped: ` +
	`the dispatcher only consults the envelope-level project, so a call like ` +
	`{action, params:{project:"corpos", ...}} wrongly resolves to the server's default project instead of "corpos". ` +
	`Fix the PRODUCTION code in internal/dispatch so that a non-empty params-nested project is honored when the ` +
	`top-level project is empty (the envelope-level project must still win when it is set). ` +
	`The failing test in internal/dispatch must pass. Do NOT modify any _test.go file.`
