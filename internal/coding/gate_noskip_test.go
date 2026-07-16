package coding

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recordingRunner records every command and returns empty/success. `git status` comes back
// empty (no changed files), so the gate-scoping path sees an empty diff.
type recordingRunner struct{ commands [][]string }

func (r *recordingRunner) Run(_ context.Context, cmd []string, _ string, _ time.Duration) CommandResult {
	r.commands = append(r.commands, cmd)
	return CommandResult{Command: cmd} // exit 0, empty stdout
}

func (r *recordingRunner) ran(substr string) bool {
	for _, c := range r.commands {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

// Bug …organ-gate-runs-whole-module-go-test-on-empty-unscopable-diff-times-out: when the
// worker lands no Go edits, the organ must NOT run the whole-module `go test ./...` (which
// only times out on a large module). It feeds back a "no edits" RED diagnostic instead, and
// every attempt skips the gate fast rather than burning a 10m timeout.
func TestRunWorkerLoop_SkipsWholeModuleGateWhenNoEdits(t *testing.T) {
	// A *GitRepo makes the scope branch fire; parentSHA "" keeps worktreeDiff out of the
	// real-git DiffWorktree path, so a zero-value repo is sufficient.
	rec := &recordingRunner{}
	o := New(WithRepo(&GitRepo{}), WithRunner(rec), WithModelWorker(&fakeWorker{}), WithGateTimeout(time.Second))
	spec := AtomicTask{
		Slug:          "fix",
		Gate:          [][]string{{"sh", "-c", "go build ./... && go test ./..."}},
		Worker:        WorkerConfig{Kind: WorkerModel},
		MaxIterations: 2,
	}
	status, diag, iters, _ := o.runWorkerLoop(context.Background(), &ATRecord{Spec: spec}, t.TempDir(), nil)

	if rec.ran("go test ./...") {
		t.Fatalf("the whole-module `go test ./...` gate must never run on an empty diff; commands: %v", rec.commands)
	}
	if status != WorkerMaxIterationsExhausted {
		t.Fatalf("status = %q, want WorkerMaxIterationsExhausted", status)
	}
	if !strings.Contains(diag, "no Go edits detected") {
		t.Fatalf("diagnostic = %q, want the actionable no-edits message", diag)
	}
	if iters != 2 {
		t.Fatalf("iters = %d, want 2 (both attempts skipped the gate, none timed out)", iters)
	}
}
