package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/agent"
	"corpos/internal/profile"
)

func TestDeterministicWorkerSuccess(t *testing.T) {
	w := NewDeterministicWorker(okRunner())
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"make"}}}
	res := w.Attempt(context.Background(), at, "/tmp", Feedback{})
	if res.CommandErr != nil {
		t.Fatalf("unexpected command error: %v", res.CommandErr)
	}
	if !strings.Contains(res.Note, "make") {
		t.Fatalf("note = %q", res.Note)
	}
}

func TestDeterministicWorkerCommandError(t *testing.T) {
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		return CommandResult{Command: cmd, ExitCode: 3, Stderr: "nope"}
	})
	w := NewDeterministicWorker(r)
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"make"}}}
	res := w.Attempt(context.Background(), at, "/tmp", Feedback{})
	if res.CommandErr == nil || !strings.Contains(res.CommandErr.Error(), "exited 3") {
		t.Fatalf("want command error mentioning exit 3, got %v", res.CommandErr)
	}
}

// fakeSpawner implements spawnRunner for model-worker tests.
type fakeSpawner struct {
	res     agent.Result
	err     error
	gotDuty string
}

func (f *fakeSpawner) Run(_ context.Context, _ *profile.JobProfile, duty string, _ ...agent.Option) (agent.Result, error) {
	f.gotDuty = duty
	return f.res, f.err
}

func TestModelWorkerSuccess(t *testing.T) {
	fs := &fakeSpawner{res: agent.Result{Text: "did it"}}
	w := &ModelWorker{spawner: fs, profile: &profile.JobProfile{Name: "coding"}}
	at := AtomicTask{Slug: "a", Goal: "build foo", Worker: WorkerConfig{Kind: WorkerModel}}
	res := w.Attempt(context.Background(), at, "/work", Feedback{})
	if res.CommandErr != nil {
		t.Fatalf("unexpected error: %v", res.CommandErr)
	}
	if res.Note != "did it" {
		t.Fatalf("note = %q", res.Note)
	}
	if !strings.Contains(fs.gotDuty, "build foo") {
		t.Fatalf("duty missing goal: %q", fs.gotDuty)
	}
}

func TestModelWorkerSpawnError(t *testing.T) {
	fs := &fakeSpawner{err: errors.New("model down")}
	w := &ModelWorker{spawner: fs, profile: &profile.JobProfile{Name: "coding"}}
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}}
	res := w.Attempt(context.Background(), at, "/work", Feedback{})
	if res.CommandErr == nil || !strings.Contains(res.CommandErr.Error(), "model down") {
		t.Fatalf("want spawn error, got %v", res.CommandErr)
	}
}

func TestModelWorkerFabricationIsCommandError(t *testing.T) {
	// The spawned worker claimed done but the loop's work audit flagged fabrication;
	// the model worker turns that into an honest command error, never a gate-revisable
	// success — so a fabricated "done" cannot reach the gate as if it were real work.
	fs := &fakeSpawner{res: agent.Result{Text: "the test passes", Fabricated: "no-work: zero mutations"}}
	w := &ModelWorker{spawner: fs, profile: &profile.JobProfile{Name: "coding"}}
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}}
	res := w.Attempt(context.Background(), at, "/work", Feedback{})
	if res.CommandErr == nil || !strings.Contains(res.CommandErr.Error(), "fabrication") {
		t.Fatalf("want a fabrication command error, got %v", res.CommandErr)
	}
}

func TestModelWorkerSurfacesHighestTierAndCarriesFloor(t *testing.T) {
	// The model worker surfaces the spawned run's highest reached tier (so the
	// orchestrator can carry it) and, given a carried tier, takes the WithStartRung
	// branch that lifts the spawn's start floor (chain 392 task 3314).
	fs := &fakeSpawner{res: agent.Result{Text: "did it", HighestTierModel: "claude-opus-4-8"}}
	w := &ModelWorker{spawner: fs, profile: &profile.JobProfile{Name: "coding"}}
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}}
	res := w.Attempt(context.Background(), at, "/work", Feedback{CarriedTierModel: "google/gemini-3.1-flash-lite"})
	if res.CommandErr != nil {
		t.Fatalf("unexpected error: %v", res.CommandErr)
	}
	if res.HighestTierModel != "claude-opus-4-8" {
		t.Fatalf("want surfaced highest tier, got %q", res.HighestTierModel)
	}
}

func TestNewModelWorkerWrapsSpawner(t *testing.T) {
	// Compile-time + smoke: *agent.Spawner satisfies spawnRunner.
	sp := agent.NewSpawner(nil, nil, nil, nil)
	if w := NewModelWorker(sp, &profile.JobProfile{Name: "coding"}); w == nil {
		t.Fatal("nil model worker")
	}
}

func TestBuildDutyIncludesEverything(t *testing.T) {
	conv := filepath.Join(t.TempDir(), "conv.md")
	if err := os.WriteFile(conv, []byte("CONVENTION-X"), 0o600); err != nil {
		t.Fatalf("write conv: %v", err)
	}
	at := AtomicTask{
		Slug:           "a",
		Goal:           "GOAL-TEXT",
		Workspace:      []string{"internal/foo/**"},
		Gate:           [][]string{{"go", "build"}},
		ConventionsRef: []string{conv, filepath.Join(t.TempDir(), "missing.md")},
		Worker:         WorkerConfig{Kind: WorkerModel},
	}
	fb := Feedback{Iteration: 2, Inputs: map[string]any{"prev": "VAL"}, PriorGateDiagnostic: "DIAG-Y"}
	duty := buildDuty(at, "/work/tree", fb)
	for _, want := range []string{"GOAL-TEXT", "/work/tree", "internal/foo/**", "prev: VAL", "go build", "CONVENTION-X", "could not load conventions_ref", "DIAG-Y", "smallest edit to the existing code"} {
		if !strings.Contains(duty, want) {
			t.Fatalf("duty missing %q\n---\n%s", want, duty)
		}
	}
}
