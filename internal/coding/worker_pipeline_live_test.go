package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"corpos/internal/agent"
	"corpos/internal/fsorgan"
	"corpos/internal/mcp"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

// scopeOf builds the projection scope (surface → allowed actions) from a profile.
func scopeOf(p *profile.JobProfile) mcp.Scope {
	scope := make(mcp.Scope, len(p.Tools))
	for _, t := range p.Tools {
		scope[t.Surface] = t.Actions
	}
	return scope
}

// liveModelWorker wires a real corpos ModelWorker: local Qwen over llama-server,
// writing files through the toolkit-server fs surface (a scoped agent.Spawner). It
// skips unless CORPOS_LIVE=1 and the toolkit + llama endpoints are reachable.
func liveModelWorker(t *testing.T) *ModelWorker {
	t.Helper()
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 to run the live worker-pipeline integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := mcp.New(mcp.DefaultToolkitURL, mcp.WithProject("qwen-floor-probe"))
	specs := mcp.EnrichedSpecs(ctx, client)
	if len(specs) == 0 {
		t.Skip("toolkit-server not reachable / no specs at " + mcp.DefaultToolkitURL)
	}
	// Mount the host-native fs organ and STRIP the raw toolkit fs spec (mirrors cmd/corpos
	// mountNativeFS). RAW-FS TRAP (bug 1080): the toolkit fs spec is enum-less, so projecting
	// an action-scoped fs[read,write,edit,…] over it fails CLOSED and drops the whole surface —
	// the coding worker would then be handed ZERO file tools and the spawn now fails loud with
	// ErrWorkerToolsUnrunnable. The native organ carries the action enum AND does real local
	// I/O (confined to the worker's dir via fsorgan.WithRoot), so fs survives projection.
	organ := fsorgan.New()
	kept := make([]tool.Spec, 0, len(specs))
	for _, s := range specs {
		if s.Name != fsorgan.Surface {
			kept = append(kept, s)
		}
	}
	provider, err := mcp.NewAggregator(
		mcp.Server{Name: "toolkit", Provider: client, Specs: kept},
		mcp.Server{Name: "fs-native", Provider: organ, Specs: organ.Specs()},
	)
	if err != nil {
		t.Fatalf("aggregator: %v", err)
	}
	p := &profile.JobProfile{
		Name: "atomic-coding-worker",
		Duty: "write Go code to satisfy a gate",
		Tier: profile.TierLocal,
		Tools: []profile.SurfaceScope{
			{Surface: "fs", Actions: []string{"read", "write", "edit", "ls"}},
		},
	}
	qwen := model.NewOpenAICompat("Qwen2.5-32B-Instruct-Q4_K_M.gguf", "http://localhost:8081/v1")
	spawner := agent.NewSpawner(provider,
		func(pp *profile.JobProfile) []tool.Spec { return mcp.Project(provider.Specs(), scopeOf(pp)) },
		nil, qwen)
	return NewModelWorker(spawner, p)
}

// initCalcWorkerTarget seeds a git Go module whose tests already exist on main, so a
// worker need only write the impl that satisfies the (orchestrator-owned) gate.
func initCalcWorkerTarget(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(cleanGitEnv(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	must := func(rel, content string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must("go.mod", "module calcworker\n\ngo 1.26\n")
	// The reference test ships on main; the worker writes the impl to satisfy it.
	must("internal/calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"Add(2,3)=%d, want 5\", Add(2, 3))\n\t}\n\tif Add(-1, 1) != 0 {\n\t\tt.Fatalf(\"Add(-1,1)=%d, want 0\", Add(-1, 1))\n\t}\n}\n")
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "base: go.mod + calc reference test")
	return dir
}

// TestLiveWorkerPipelineRunsChain is the keystone live integration (task 7): the
// local-Qwen worker, driven through the corpos agent.Spawner, writes Go code via the
// toolkit fs surface into a per-AT worktree, and the orchestrator-owned go-test gate
// verifies it — end-to-end, natively, gate-green. Proves the organ runs a coding
// chain with the real worker pipeline, not a stand-in.
func TestLiveWorkerPipelineRunsChain(t *testing.T) {
	worker := liveModelWorker(t)
	repoDir := initCalcWorkerTarget(t)
	r := NewGitRepo(ExecRunner{}, repoDir, t.TempDir())
	o := New(WithRunner(ExecRunner{}), WithRepo(r), WithModelWorker(worker))

	chain := Chain{Slug: "calc-worker", TargetRepo: repoDir, BaseBranch: "main", Tasks: []AtomicTask{
		{
			Slug:          "impl-add",
			Goal:          "Create the file internal/calc/calc.go with `package calc` and an exported function `func Add(a, b int) int` that returns the sum a + b. Do not modify any test file.",
			Workspace:     []string{"internal/calc/calc.go"},
			Protected:     []string{"**/*_test.go"},
			Worker:        WorkerConfig{Kind: WorkerModel},
			Gate:          [][]string{{"go", "test", "./internal/calc"}},
			MaxIterations: 3,
		},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := o.Start(ctx, chain, "calc-worker-run")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(ctx, st)
	t.Logf("chain status=%s  AT status=%s worker=%s iters=%d diag=%.300s",
		st.Status, st.ATs[0].Status, st.ATs[0].WorkerStatus, st.ATs[0].Iterations, st.ATs[0].Diagnostic)
	if st.Status != ChainSuccess {
		t.Fatalf("live worker pipeline should drive the chain to SUCCESS; got %q", st.Status)
	}
}
