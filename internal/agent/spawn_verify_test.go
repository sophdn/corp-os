package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

// recordingWriteFS is a fake fs provider that records the path of every fs.write/edit it is
// asked to dispatch, so a test can assert WHICH files a worker tried to mutate.
type recordingWriteFS struct{ writes []string }

func (r *recordingWriteFS) Dispatch(_ context.Context, c tool.Call) tool.Result {
	if c.Surface == "fs" && (c.Action == "write" || c.Action == "edit") {
		r.writes = append(r.writes, toolCallPath(c))
	}
	return tool.Result{Call: c, OK: true, Value: map[string]any{"ok": true}}
}

// scriptedCoder plays a fixed sequence of model turns: each entry is one Complete
// response, consumed in order (the last repeats if the loop asks for more).
type scriptedCoder struct {
	turns []model.Response
	n     int
}

func (s *scriptedCoder) Model() string   { return "scripted-coder" }
func (s *scriptedCoder) Available() bool { return true }
func (s *scriptedCoder) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	r := s.turns[s.n]
	if s.n < len(s.turns)-1 {
		s.n++
	}
	r.Model = "scripted-coder"
	return r, nil
}

// codingProfile is the spawn target under test: the atomic-coding-chain shape with an
// orchestrator-owned build/test gate and *_test.go protected (the bug-1073 wiring).
func codingProfile() *profile.JobProfile {
	return &profile.JobProfile{
		Name:            "atomic-coding-chain",
		Tier:            profile.TierLocal,
		Tools:           []profile.SurfaceScope{{Surface: "fs", Actions: []string{"read", "write", "edit"}}},
		VerifyCommand:   []string{"sh", "-c", "go build ./... && go test ./..."},
		VerifyMaxRounds: 4,
		ProtectPaths:    []string{"**/*_test.go"},
	}
}

// editCall is an fs.edit on the given path (the worker's file mutation).
func editCall(id, path string) tool.Call {
	return tool.Call{ID: id, Surface: "fs", Action: "edit", Params: map[string]any{"path": path}}
}

// spawnerWithGate builds a Spawner whose auto-verify gate is driven by run (so the
// test controls RED→GREEN), wiring the recording fs provider as the worker's surface.
func spawnerWithGate(fs tool.Provider, base model.Adapter, run func(int) (int, string)) *Spawner {
	calls := 0
	s := NewSpawner(fs, nil, nil, base)
	s.verifyRun = func(context.Context, []string, string, time.Duration) (int, string) {
		calls++
		return run(calls)
	}
	return s
}

// TestSpawnedCoderSelfRepairsBrokenEdit is the bug-1073 regression: a spawned
// atomic-coding-chain worker that lands a NON-COMPILING edit and then claims done must
// be caught by the loop-owned build/test gate, fed the failure back, and revise to a
// passing edit within the bound — NOT stop with the package broken. Pre-fix the spawner
// attached no gate, so the worker's first done-claim returned immediately and the broken
// edit landed; this test failed.
func TestSpawnedCoderSelfRepairsBrokenEdit(t *testing.T) {
	fs := &recordingWriteFS{}
	// Turn 1: land a BROKEN edit on production code. Turn 2: claim done (triggers the
	// gate → RED). Turn 3: land the FIXED edit. Turn 4: claim done (gate → GREEN).
	coder := &scriptedCoder{turns: []model.Response{
		{ToolCalls: []tool.Call{editCall("e1", "internal/dispatch/dispatch.go")}, StopReason: model.StopToolUse},
		{Text: "done (broken)", StopReason: model.StopEndTurn},
		{ToolCalls: []tool.Call{editCall("e2", "internal/dispatch/dispatch.go")}, StopReason: model.StopToolUse},
		{Text: "fixed it", StopReason: model.StopEndTurn},
	}}
	// Gate is RED on the first check (broken edit), GREEN on the second (fixed edit).
	gateCheck := 0
	sp := spawnerWithGate(fs, coder, func(call int) (int, string) {
		gateCheck = call
		if call == 1 {
			return 1, "internal/dispatch/dispatch.go:42: cannot use args.Params (variable of type any) as []byte value in argument to json.Unmarshal"
		}
		return 0, "ok"
	})
	p := codingProfile()

	res, err := sp.Run(context.Background(), p, "fix the failing dispatch test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.VerifyFailed {
		t.Fatalf("worker should have converged to green within the bound, got VerifyFailed (text=%q)", res.Text)
	}
	if gateCheck < 2 {
		t.Fatalf("the loop-owned gate ran %d time(s); it must run, see RED, and re-check after the fix", gateCheck)
	}
	// The worker revised: it issued the SECOND (fixing) edit, not just the first broken one.
	if len(fs.writes) < 2 {
		t.Fatalf("worker must revise after the RED gate (expected >=2 edits, got %d: %v)", len(fs.writes), fs.writes)
	}
	// Integrity: every edit targeted production code — none touched a *_test.go path.
	for _, w := range fs.writes {
		if strings.HasSuffix(w, "_test.go") {
			t.Fatalf("worker edited a protected test path %q to force green", w)
		}
	}
}

// TestSpawnedCoderCannotEditTestToFakeGreen asserts the verification-integrity guard:
// a worker that tries to edit a *_test.go file (to make the gate pass without fixing the
// code) is DENIED at the dispatch boundary — the protected path never lands.
func TestSpawnedCoderCannotEditTestToFakeGreen(t *testing.T) {
	fs := &recordingWriteFS{}
	// Turn 1: attempt to edit the TEST file (reward hack). Turn 2: fix production code.
	// Turn 3: claim done. The gate is RED until the production fix, then GREEN.
	coder := &scriptedCoder{turns: []model.Response{
		{ToolCalls: []tool.Call{editCall("hack", "internal/dispatch/dispatch_test.go")}, StopReason: model.StopToolUse},
		{ToolCalls: []tool.Call{editCall("fix", "internal/dispatch/dispatch.go")}, StopReason: model.StopToolUse},
		{Text: "done", StopReason: model.StopEndTurn},
	}}
	sp := spawnerWithGate(fs, coder, func(call int) (int, string) {
		if call == 1 {
			return 0, "ok" // gate green once the production edit landed
		}
		return 0, "ok"
	})
	p := codingProfile()

	res, err := sp.Run(context.Background(), p, "fix the dispatch bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.VerifyFailed {
		t.Fatalf("unexpected VerifyFailed: %q", res.Text)
	}
	// The test-file edit was DENIED, so it never reached the provider: only the
	// production edit is recorded.
	for _, w := range fs.writes {
		if strings.HasSuffix(w, "_test.go") {
			t.Fatalf("a *_test.go edit must be denied at the dispatch boundary, but %q landed", w)
		}
	}
	if len(fs.writes) == 0 {
		t.Fatal("the production edit should still have landed")
	}
}

// TestSpawnedNonCodingWorkerHasNoGate confirms the change is scoped: a profile with no
// VerifyCommand (a read-only / non-coding worker) gets NO auto-verify gate and NO
// protect-paths hook — the success path for non-coding workers is unaffected.
func TestSpawnedNonCodingWorkerHasNoGate(t *testing.T) {
	fs := &recordingWriteFS{}
	coder := &scriptedCoder{turns: []model.Response{{Text: "answer", StopReason: model.StopEndTurn}}}
	gateRan := false
	sp := NewSpawner(fs, nil, nil, coder)
	sp.verifyRun = func(context.Context, []string, string, time.Duration) (int, string) {
		gateRan = true
		return 1, "should never run"
	}
	p := &profile.JobProfile{
		Name:  "task-lifecycle",
		Tier:  profile.TierLocal,
		Tools: []profile.SurfaceScope{{Surface: "work"}},
	}
	res, err := sp.Run(context.Background(), p, "read a task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gateRan {
		t.Fatal("a profile with no VerifyCommand must not run an auto-verify gate")
	}
	if res.Text != "answer" || res.VerifyFailed {
		t.Fatalf("non-coding worker answered cleanly without a gate: text=%q failed=%v", res.Text, res.VerifyFailed)
	}
}

// TestWithSpawnVerifyDir confirms the dir option threads into the gate a spawned coding
// worker runs: a set dir is the gate's working directory; an empty dir is a no-op.
func TestWithSpawnVerifyDir(t *testing.T) {
	fs := &recordingWriteFS{}
	coder := &scriptedCoder{turns: []model.Response{{Text: "done", StopReason: model.StopEndTurn}}}
	var sawDir string
	// A real module root (has a go.mod) so the gate is runnable (Part A's fail-fast does not
	// fire) and we can observe the dir threading into the gate.
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sp := NewSpawner(fs, nil, nil, coder, WithSpawnVerifyDir(target), WithSpawnVerifyDir(""))
	sp.verifyRun = func(_ context.Context, _ []string, dir string, _ time.Duration) (int, string) {
		sawDir = dir
		return 0, "ok"
	}
	if _, err := sp.Run(context.Background(), codingProfile(), "build it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawDir != target {
		t.Fatalf("gate dir = %q, want the set dir %q (empty WithSpawnVerifyDir must be a no-op)", sawDir, target)
	}
}

// TestSpawnedCoderExhaustsRepairBudget asserts the bound: a worker whose gate stays RED
// across the whole revise budget returns an honest VerifyFailed/escalate verdict, never a
// false "done". This is the cap that keeps the self-repair loop terminating.
func TestSpawnedCoderExhaustsRepairBudget(t *testing.T) {
	fs := &recordingWriteFS{}
	coder := &scriptedCoder{turns: []model.Response{
		{ToolCalls: []tool.Call{editCall("e", "internal/dispatch/dispatch.go")}, StopReason: model.StopToolUse},
		{Text: "I think it's done", StopReason: model.StopEndTurn},
	}}
	checks := 0
	sp := spawnerWithGate(fs, coder, func(int) (int, string) { checks++; return 1, "still broken" })
	p := codingProfile()
	p.VerifyMaxRounds = 2

	res, err := sp.Run(context.Background(), p, "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.VerifyFailed {
		t.Fatal("an always-RED gate must exhaust the bound and report VerifyFailed, not a false done")
	}
	if checks < 2 {
		t.Fatalf("gate should be checked across the revise budget, got %d checks", checks)
	}
}

// TestSpawnedCoderCannotAuthorTestToFakeGreen is the bug-1050 regression for the AUTHORING
// vector (distinct from TestSpawnedCoderCannotEditTestToFakeGreen, which covers EDITING an
// existing test). The 1050 worker AUTHORED a brand-new hollow *_test.go via fs.write and left
// production code unchanged. On the atomic-coding-chain profile (ProtectPaths ["**/*_test.go"])
// the protect-paths dispatch denial must block fs.write CREATION of a test file, not just
// fs.edit — so the worker cannot create the file the gate runs.
func TestSpawnedCoderCannotAuthorTestToFakeGreen(t *testing.T) {
	fs := &recordingWriteFS{}
	// Turn 1: AUTHOR a brand-new hollow regression test (fs.write of a NEW *_test.go), the
	// 1050 vector. Turn 2: claim done with production code untouched.
	coder := &scriptedCoder{turns: []model.Response{
		{ToolCalls: []tool.Call{writeCall("internal/fs/read_symbol_mismatch_test.go")}, StopReason: model.StopToolUse},
		{Text: "added a regression test confirming graceful handling", StopReason: model.StopEndTurn},
	}}
	// The gate would go GREEN (the worker's hollow test passes on the raw crash) — but the
	// write must be DENIED before it ever lands, so the test file never reaches the provider.
	sp := spawnerWithGate(fs, coder, func(int) (int, string) { return 0, "ok" })
	p := codingProfile()

	res, err := sp.Run(context.Background(), p, "fix the fs.read crash")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The worker AUTHORED no test that landed: the create write was denied at the boundary.
	for _, w := range fs.writes {
		if strings.HasSuffix(w, "_test.go") {
			t.Fatalf("a *_test.go CREATE (fs.write) must be denied at the dispatch boundary, but %q landed", w)
		}
	}
	if len(fs.writes) != 0 {
		t.Fatalf("the worker mutated only a protected test path, so nothing should have landed; got %v", fs.writes)
	}
	_ = res
}

// TestSpawnedCoderGateGreenOnWorkerAuthoredTestIsFakeGreen is the bug-1050 second-layer
// regression: even if a worker-authored *_test.go write LANDS — a coding profile that has a
// verify gate but NO/incomplete ProtectPaths — a GREEN gate that the worker's own new test
// could be certifying must NOT count as a clean done. The loop refuses it as a fake green
// (VerifyFailed + an Escalate verdict), so the worker-added test is never the gate.
func TestSpawnedCoderGateGreenOnWorkerAuthoredTestIsFakeGreen(t *testing.T) {
	fs := &recordingWriteFS{}
	// Turn 1: author a hollow test (the write LANDS here — no protect-paths on this profile).
	// Turn 2: claim done; production code untouched. The gate is GREEN (the hollow test passes).
	coder := &scriptedCoder{turns: []model.Response{
		{ToolCalls: []tool.Call{writeCall("internal/fs/read_symbol_mismatch_test.go")}, StopReason: model.StopToolUse},
		{Text: "I modified UnmarshalJSON and added a regression test", StopReason: model.StopEndTurn},
	}}
	sp := spawnerWithGate(fs, coder, func(int) (int, string) { return 0, "ok" })
	// A coding profile WITH a gate but WITHOUT ProtectPaths — so the test write is not denied
	// and lands, exercising the detector layer (not the protect-paths layer).
	p := &profile.JobProfile{
		Name:            "coding-no-protect",
		Tier:            profile.TierLocal,
		Tools:           []profile.SurfaceScope{{Surface: "fs", Actions: []string{"read", "write", "edit"}}},
		VerifyCommand:   []string{"sh", "-c", "go test ./..."},
		VerifyMaxRounds: 4,
	}

	res, err := sp.Run(context.Background(), p, "fix the fs.read crash")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The worker-authored test landed (no protect-paths on this profile) ...
	if len(fs.writes) == 0 || !strings.HasSuffix(fs.writes[0], "_test.go") {
		t.Fatalf("precondition: the test-file write should have landed for this profile; got %v", fs.writes)
	}
	// ... so the GREEN gate must be refused as a fake green — not a clean done.
	if !res.VerifyFailed {
		t.Fatalf("a gate greened by a worker-AUTHORED test must be refused as a fake green (VerifyFailed), got clean done: text=%q", res.Text)
	}
	if !strings.Contains(res.Escalate, "fake-green") {
		t.Fatalf("Escalate should carry a fake-green verdict, got %q", res.Escalate)
	}
}

// TestSpawnFailsFastOnNonRunnableGate is the bug-1075 Part-A regression: a spawn whose go
// build/test verify gate is pointed at a dir with NO reachable go.mod must FAIL FAST at spawn
// with an actionable message — NOT silently proceed (and let the worker fabricate the module to
// pass), and NOT run a gate that can only error. Pre-fix the spawn proceeded and the gate ran on a
// module-less dir; this test failed.
func TestSpawnFailsFastOnNonRunnableGate(t *testing.T) {
	fs := &recordingWriteFS{}
	coder := &scriptedCoder{turns: []model.Response{{Text: "done", StopReason: model.StopEndTurn}}}
	gateRan := false
	// A verify-dir with no go.mod (a fresh temp dir).
	noMod := t.TempDir()
	sp := NewSpawner(fs, nil, nil, coder, WithSpawnVerifyDir(noMod))
	sp.verifyRun = func(context.Context, []string, string, time.Duration) (int, string) {
		gateRan = true
		return 0, "ok"
	}
	p := codingProfile() // go build/test gate

	_, err := sp.Run(context.Background(), p, "fix it")
	if err == nil {
		t.Fatal("a go gate pointed at a dir with no go.mod must fail fast at spawn, got nil error")
	}
	if !strings.Contains(err.Error(), "has no go.mod") || !strings.Contains(err.Error(), "module root") {
		t.Fatalf("the fail-fast message must be actionable (name go.mod + the fix), got %q", err.Error())
	}
	if gateRan {
		t.Fatal("the gate must NOT run when the dir is non-runnable — fail fast before the worker")
	}
	if len(fs.writes) != 0 {
		t.Fatalf("the worker must never run on a non-runnable gate, but it wrote %v", fs.writes)
	}
}

// TestSpawnedCoderScaffoldFabricationRefusesGreen is the bug-1075 Part-B regression: a worker that
// makes a go gate pass by WRITING a go.mod (a build-scaffold) into the verify-dir — manufacturing
// the gate's input instead of fixing real code — must have the green REFUSED (VerifyFailed + an
// Escalate verdict), not counted as a clean done. The verify-dir HAS a go.mod (so Part A's
// fail-fast does not fire) and the gate goes GREEN, isolating the scaffold-fabrication detector.
func TestSpawnedCoderScaffoldFabricationRefusesGreen(t *testing.T) {
	fs := &recordingWriteFS{}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Turn 1: write a go.mod INTO the verify-dir (the fabrication). Turn 2: claim done.
	scaffold := filepath.Join(dir, "go.mod")
	coder := &scriptedCoder{turns: []model.Response{
		{ToolCalls: []tool.Call{writeCall(scaffold)}, StopReason: model.StopToolUse},
		{Text: "added a go module so the build passes", StopReason: model.StopEndTurn},
	}}
	// The gate goes GREEN (the fabricated module builds) — the detector, not the gate, must refuse.
	sp := NewSpawner(fs, nil, nil, coder, WithSpawnVerifyDir(dir))
	sp.verifyRun = func(context.Context, []string, string, time.Duration) (int, string) { return 0, "ok" }
	// A coding profile with a go gate but NO ProtectPaths (so the go.mod write is not denied and
	// lands, exercising the scaffold detector rather than the protect-paths layer).
	p := &profile.JobProfile{
		Name:            "coding-no-protect",
		Tier:            profile.TierLocal,
		Tools:           []profile.SurfaceScope{{Surface: "fs", Actions: []string{"read", "write", "edit"}}},
		VerifyCommand:   []string{"sh", "-c", "go build ./..."},
		VerifyMaxRounds: 4,
	}

	res, err := sp.Run(context.Background(), p, "make the build pass")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Precondition: the go.mod write landed.
	if len(fs.writes) == 0 || !strings.HasSuffix(fs.writes[0], "go.mod") {
		t.Fatalf("precondition: the go.mod scaffold write should have landed; got %v", fs.writes)
	}
	if !res.VerifyFailed {
		t.Fatalf("a green won by fabricating a go.mod in the verify-dir must be refused (VerifyFailed), got clean done: text=%q", res.Text)
	}
	if !strings.Contains(res.Escalate, "build-scaffold") {
		t.Fatalf("Escalate should carry the scaffold-fabrication verdict, got %q", res.Escalate)
	}
}

// TestSpawnedCoderNoScaffoldUnaffected confirms the guard is narrow: a normal carry (a worker that
// fixes production code and writes no build-scaffold into the verify-dir) greens cleanly — the
// scaffold guard does not fire on legitimate work.
func TestSpawnedCoderNoScaffoldUnaffected(t *testing.T) {
	fs := &recordingWriteFS{}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	coder := &scriptedCoder{turns: []model.Response{
		{ToolCalls: []tool.Call{editCall("e", filepath.Join(dir, "internal", "foo.go"))}, StopReason: model.StopToolUse},
		{Text: "fixed the bug in production code", StopReason: model.StopEndTurn},
	}}
	sp := NewSpawner(fs, nil, nil, coder, WithSpawnVerifyDir(dir))
	sp.verifyRun = func(context.Context, []string, string, time.Duration) (int, string) { return 0, "ok" }
	p := codingProfile()

	res, err := sp.Run(context.Background(), p, "fix the bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.VerifyFailed {
		t.Fatalf("a clean production-code carry must green, got VerifyFailed: escalate=%q", res.Escalate)
	}
}
