package coding

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gateDirRunner records the working dir of the `go` gate command and passes everything
// (exit 0), so a test can assert WHERE the organ ran the build/test gate.
type gateDirRunner struct{ goDir string }

func (r *gateDirRunner) Run(_ context.Context, cmd []string, dir string, _ time.Duration) CommandResult {
	if len(cmd) > 0 && cmd[0] == "go" {
		r.goDir = dir
	}
	return CommandResult{Command: cmd} // exit 0 — the gate "passes"
}

// Bug 1094: when the target's Go module is in a subdirectory (go/), the organ must run
// the build/test gate in that module root, not the module-less worktree root.
func TestRunWorkerLoop_GateRunsInResolvedSubdirModule(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go", "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &gateDirRunner{}
	o := New(WithRunner(rec), WithRepo(NoopRepo{Dir: root}), WithGateTimeout(5*time.Second))
	chain := Chain{Slug: "c", TargetRepo: root, Tasks: []AtomicTask{{
		Slug:   "a",
		Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"true"}},
		Gate:   [][]string{{"go", "build", "./..."}},
	}}}
	st, err := o.Start(context.Background(), chain, "r")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("chain status = %q, want success", st.Status)
	}
	if want := filepath.Join(root, "go"); rec.goDir != want {
		t.Fatalf("gate ran in %q, want the resolved module root %q", rec.goDir, want)
	}
}
