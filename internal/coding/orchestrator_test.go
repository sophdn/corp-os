package coding

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeWorker returns a scripted attempt result; it counts attempts.
type fakeWorker struct {
	attempts int
	cmdErr   error
}

func (w *fakeWorker) Attempt(_ context.Context, _ AtomicTask, _ string, _ Feedback) AttemptResult {
	w.attempts++
	return AttemptResult{CommandErr: w.cmdErr, Note: "ok"}
}

// fakeRepo / fakeWorkspace exercise the commit + fast-forward + open-error paths
// the NoopRepo does not reach.
type fakeRepo struct {
	head          string
	openErr       error
	ws            *fakeWorkspace
	fastForwarded string
	promoted      bool
	promoteErr    error
}

func (r *fakeRepo) Init(context.Context, *RunState) error   { return nil }
func (r *fakeRepo) HeadSHA(context.Context) (string, error) { return r.head, nil }
func (r *fakeRepo) Open(context.Context, string, *ATRecord) (Workspace, error) {
	if r.openErr != nil {
		return nil, r.openErr
	}
	return r.ws, nil
}
func (r *fakeRepo) FastForward(_ context.Context, sha string) error {
	r.fastForwarded = sha
	return nil
}
func (r *fakeRepo) ResetTo(context.Context, string) error { return nil }
func (r *fakeRepo) Promote(context.Context) error {
	r.promoted = true
	return r.promoteErr
}

type fakeWorkspace struct {
	dir       string
	sha       string
	ok        bool
	commitErr error
	closed    bool
	seeded    map[string]string
	commits   int
	seedErr   error
}

func (w *fakeWorkspace) Dir() string { return w.dir }
func (w *fakeWorkspace) Seed(files map[string]string) error {
	if w.seedErr != nil {
		return w.seedErr
	}
	w.seeded = files
	return nil
}
func (w *fakeWorkspace) Commit(context.Context, string) (string, bool, error) {
	w.commits++
	return w.sha, w.ok, w.commitErr
}
func (w *fakeWorkspace) Close() error { w.closed = true; return nil }

func TestStartValidatesAndSeeds(t *testing.T) {
	o := New(WithRunIDFunc(func() string { return "" }))
	_, err := o.Start(context.Background(), Chain{Slug: "c"}, "")
	if err == nil {
		t.Fatal("invalid chain should error")
	}
	st, err := o.Start(context.Background(), Chain{Slug: "c", Tasks: []AtomicTask{detTask("a")}}, "run1")
	if err != nil {
		t.Fatalf("valid chain: %v", err)
	}
	// An empty chain BaseBranch flows through unchanged; the git Repo seam resolves
	// it to the repo's actual branch at Init time (bug 1077). The NoopRepo used here
	// ignores it, so the seeded state simply carries the empty base.
	if st.RunID != "run1" || len(st.ATs) != 1 || st.ATs[0].Status != ATPending || st.BaseBranch != "" {
		t.Fatalf("bad seed: %+v", st)
	}
}

func TestStartMintsRunID(t *testing.T) {
	o := New()
	st, err := o.Start(context.Background(), Chain{Slug: "c", Tasks: []AtomicTask{detTask("a")}}, "")
	if err != nil || st.RunID == "" {
		t.Fatalf("want minted run id, got %q err=%v", st.RunID, err)
	}
}

// TestTrivialChainRunsWithRealGates is the end-to-end real-exec acceptance: two
// deterministic ATs write files into the target dir; the gate (real `test -f`)
// verifies them; an extraction + a downstream input resolve through a real run.
func TestTrivialChainRunsWithRealGates(t *testing.T) {
	dir := t.TempDir()
	at1 := AtomicTask{
		Slug:           "make-file",
		Worker:         WorkerConfig{Kind: WorkerDeterministic, Command: []string{"sh", "-c", "printf hi > out.txt"}},
		Gate:           [][]string{{"test", "-f", "out.txt"}},
		OutputContract: OutputContract{Extractions: []Extraction{{Name: "greeting", Command: []string{"cat", "out.txt"}}}},
	}
	at2 := AtomicTask{
		Slug:   "use-input",
		Inputs: map[string]InputRef{"greet": {From: "make-file", Field: "greeting"}},
		Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"true"}},
		Gate:   [][]string{{"true"}},
	}
	chain := Chain{Slug: "trivial", TargetRepo: dir, Tasks: []AtomicTask{at1, at2}}

	o := New() // ExecRunner + NoopRepo bound to dir at Start
	st, err := o.Start(context.Background(), chain, "run")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("status = %q, want success (at0 diag: %s)", st.Status, st.ATs[0].Diagnostic)
	}
	if st.ATs[0].Outputs["greeting"] != "hi" {
		t.Fatalf("extraction = %v, want hi", st.ATs[0].Outputs["greeting"])
	}
	if st.ATs[0].WorkerStatus != WorkerSuccess || st.ATs[1].Status != ATSuccess {
		t.Fatalf("ATs not both success: %+v", st.ATs)
	}
}

func TestRunFailFast(t *testing.T) {
	// at0 fails (gate `false`), at1 must never run.
	at0 := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"true"}}, Gate: [][]string{{"false"}}}
	at1 := detTask("b")
	chain := Chain{Slug: "c", TargetRepo: t.TempDir(), Tasks: []AtomicTask{at0, at1}}
	o := New()
	st, _ := o.Start(context.Background(), chain, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainFailed || st.FailedATSlug != "a" {
		t.Fatalf("want failed at a, got %q / %q", st.Status, st.FailedATSlug)
	}
	if st.ATs[0].WorkerStatus != WorkerGateFailure {
		t.Fatalf("at0 worker status = %q", st.ATs[0].WorkerStatus)
	}
	if st.ATs[1].Status != ATPending {
		t.Fatalf("at1 should not have run, status = %q", st.ATs[1].Status)
	}
}

func TestRunWorkerCommandError(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	fw := &fakeWorker{cmdErr: errors.New("boom")}
	o.model = fw
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"true"}}}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: t.TempDir(), Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.ATs[0].WorkerStatus != WorkerCommandError {
		t.Fatalf("want command error, got %q", st.ATs[0].WorkerStatus)
	}
}

func TestModelWorkerNotWired(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"true"}}}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: t.TempDir(), Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.ATs[0].WorkerStatus != WorkerCommandError {
		t.Fatalf("missing model worker should yield command error, got %q", st.ATs[0].WorkerStatus)
	}
}

func TestModelReviseThenPass(t *testing.T) {
	// gate fails on the first attempt, passes on the second → SUCCESS at iter 2.
	calls := 0
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		calls++
		if calls == 1 {
			return CommandResult{Command: cmd, ExitCode: 1, Stderr: "first miss"}
		}
		return CommandResult{Command: cmd}
	})
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}))
	o.model = &fakeWorker{}
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 3}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: t.TempDir(), Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess || st.ATs[0].Iterations != 2 {
		t.Fatalf("want success at iter 2, got %q iters=%d", st.Status, st.ATs[0].Iterations)
	}
}

func TestModelMaxIterationsExhausted(t *testing.T) {
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		return CommandResult{Command: cmd, ExitCode: 1, Stderr: "always fails"}
	})
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}))
	o.model = &fakeWorker{}
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}, Gate: [][]string{{"g"}}, MaxIterations: 2}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: t.TempDir(), Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.ATs[0].WorkerStatus != WorkerMaxIterationsExhausted || st.ATs[0].Iterations != 2 {
		t.Fatalf("want exhausted at 2 iters, got %q iters=%d", st.ATs[0].WorkerStatus, st.ATs[0].Iterations)
	}
}

func TestCommitAndFastForward(t *testing.T) {
	ws := &fakeWorkspace{dir: t.TempDir(), sha: "deadbeef", ok: true}
	repo := &fakeRepo{head: "base", ws: ws}
	o := New(WithRunner(okRunner()), WithRepo(repo))
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}}, Gate: [][]string{{"g"}}}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("want success, got %q", st.Status)
	}
	if st.ATs[0].CommitSHA != "deadbeef" || repo.fastForwarded != "deadbeef" || st.ATs[0].ParentSHA != "base" {
		t.Fatalf("commit/ff/fork wrong: commit=%q ff=%q fork=%q", st.ATs[0].CommitSHA, repo.fastForwarded, st.ATs[0].ParentSHA)
	}
	if !ws.closed {
		t.Fatal("workspace not closed")
	}
}

func TestOpenWorkspaceError(t *testing.T) {
	repo := &fakeRepo{openErr: errors.New("worktree add failed")}
	o := New(WithRunner(okRunner()), WithRepo(repo))
	at := detTask("a")
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainFailed || st.ATs[0].WorkerStatus != WorkerCommandError {
		t.Fatalf("open error should fail the AT, got %q / %q", st.Status, st.ATs[0].WorkerStatus)
	}
}

func TestOutputContractAssertionFails(t *testing.T) {
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		if cmd[0] == "assert" {
			return CommandResult{Command: cmd, ExitCode: 1, Stderr: "assertion boom"}
		}
		return CommandResult{Command: cmd}
	})
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}))
	at := AtomicTask{
		Slug:           "a",
		Worker:         WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}},
		Gate:           [][]string{{"g"}},
		OutputContract: OutputContract{Assertions: []Assertion{{Name: "post", Command: []string{"assert"}}}},
	}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.ATs[0].WorkerStatus != WorkerOutputContractViolation {
		t.Fatalf("want output_contract_violation, got %q", st.ATs[0].WorkerStatus)
	}
}

func TestOutputContractJSONExtractionAndParseError(t *testing.T) {
	// JSON extraction success.
	good := funcRunner(func(cmd []string, _ string) CommandResult {
		if cmd[0] == "emit" {
			return CommandResult{Command: cmd, Stdout: `{"k":1}` + "\n"}
		}
		return CommandResult{Command: cmd}
	})
	o := New(WithRunner(good), WithRepo(NoopRepo{Dir: t.TempDir()}))
	at := AtomicTask{
		Slug:           "a",
		Worker:         WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}},
		Gate:           [][]string{{"g"}},
		OutputContract: OutputContract{Extractions: []Extraction{{Name: "obj", Command: []string{"emit"}, Format: FormatJSON}}},
	}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("json extraction should succeed, got %q (%s)", st.Status, st.ATs[0].Diagnostic)
	}

	// JSON extraction parse error.
	bad := funcRunner(func(cmd []string, _ string) CommandResult {
		if cmd[0] == "emit" {
			return CommandResult{Command: cmd, Stdout: "not json"}
		}
		return CommandResult{Command: cmd}
	})
	o2 := New(WithRunner(bad), WithRepo(NoopRepo{Dir: t.TempDir()}))
	st2, _ := o2.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st2 = o2.RunToCompletion(context.Background(), st2)
	if st2.ATs[0].WorkerStatus != WorkerOutputContractViolation {
		t.Fatalf("bad json should violate contract, got %q", st2.ATs[0].WorkerStatus)
	}
}

func TestExtractionCommandFails(t *testing.T) {
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		if cmd[0] == "emit" {
			return CommandResult{Command: cmd, ExitCode: 5, Stderr: "no"}
		}
		return CommandResult{Command: cmd}
	})
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}))
	at := AtomicTask{
		Slug:           "a",
		Worker:         WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}},
		Gate:           [][]string{{"g"}},
		OutputContract: OutputContract{Extractions: []Extraction{{Name: "x", Command: []string{"emit"}}}},
	}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.ATs[0].WorkerStatus != WorkerOutputContractViolation {
		t.Fatalf("failing extraction command should violate contract, got %q", st.ATs[0].WorkerStatus)
	}
}

func TestResolveInputsBranchAware(t *testing.T) {
	o := New()
	state := &RunState{ATs: []ATRecord{
		{Slug: "orig", Status: ATSkipped, Outputs: map[string]any{"f": "stale"}},
		{Slug: "orig-fix1", Status: ATSuccess, ParentATSlug: "orig", Outputs: map[string]any{"f": "fresh"}},
	}}
	spec := AtomicTask{Slug: "down", Inputs: map[string]InputRef{"in": {From: "orig", Field: "f"}}}
	got, err := o.resolveInputs(state, spec)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["in"] != "fresh" {
		t.Fatalf("branch-aware resolution = %v, want fresh (branch supersedes original)", got["in"])
	}
}

func TestResolveInputsMissingUpstream(t *testing.T) {
	o := New()
	state := &RunState{ATs: []ATRecord{{Slug: "a", Status: ATFailed}}}
	spec := AtomicTask{Slug: "b", Inputs: map[string]InputRef{"in": {From: "a", Field: "f"}}}
	if _, err := o.resolveInputs(state, spec); err == nil {
		t.Fatal("want error: no successful upstream")
	}
}

func TestResolveInputsMissingField(t *testing.T) {
	o := New()
	state := &RunState{ATs: []ATRecord{{Slug: "a", Status: ATSuccess, Outputs: map[string]any{"other": 1}}}}
	spec := AtomicTask{Slug: "b", Inputs: map[string]InputRef{"in": {From: "a", Field: "f"}}}
	_, err := o.resolveInputs(state, spec)
	if err == nil {
		t.Fatal("want error: missing field")
	}
}

func TestResolveInputsNone(t *testing.T) {
	o := New()
	got, err := o.resolveInputs(&RunState{}, AtomicTask{})
	if err != nil || got != nil {
		t.Fatalf("no inputs should be (nil,nil), got %v %v", got, err)
	}
}

func TestStagingRetrySkipAbort(t *testing.T) {
	o := New()
	mk := func() *RunState {
		return &RunState{Status: ChainFailed, CurrentPosition: 0, FailedATSlug: "a",
			ATs: []ATRecord{{Slug: "a", Status: ATFailed, Iterations: 3, Diagnostic: "x"}, {Slug: "b", Status: ATPending}}}
	}

	st := mk()
	if err := o.Retry(st); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if st.Status != ChainPending || st.ATs[0].Status != ATPending || st.ATs[0].Iterations != 0 || st.FailedATSlug != "" {
		t.Fatalf("retry did not reset: %+v", st.ATs[0])
	}

	st = mk()
	if err := o.Skip(st); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if st.ATs[0].Status != ATSkipped || st.CurrentPosition != 1 || st.Status != ChainPending {
		t.Fatalf("skip wrong: %+v pos=%d", st.ATs[0].Status, st.CurrentPosition)
	}

	st = mk()
	o.Abort(st)
	if st.Status != ChainAborted {
		t.Fatalf("abort wrong: %q", st.Status)
	}
}

func TestStagingNotStageable(t *testing.T) {
	o := New()
	st := &RunState{Status: ChainRunning, ATs: []ATRecord{{Slug: "a"}}}
	if err := o.Retry(st); err == nil {
		t.Fatal("retry on running chain should error")
	}
	if err := o.Skip(st); err == nil {
		t.Fatal("skip on running chain should error")
	}
}

func TestStagingNoCurrentAT(t *testing.T) {
	o := New()
	st := &RunState{Status: ChainFailed, CurrentPosition: 5, ATs: []ATRecord{{Slug: "a"}}}
	if err := o.Retry(st); err == nil {
		t.Fatal("retry with no current AT should error")
	}
	if err := o.Skip(st); err == nil {
		t.Fatal("skip with no current AT should error")
	}
}

func TestResumeAfterRetry(t *testing.T) {
	// A failed chain, retried, then resumed to success with a now-passing gate.
	calls := 0
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		calls++
		if calls == 1 {
			return CommandResult{Command: cmd, ExitCode: 1}
		}
		return CommandResult{Command: cmd}
	})
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}))
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}}, Gate: [][]string{{"g"}}}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainFailed {
		t.Fatalf("want initial failure, got %q", st.Status)
	}
	if err := o.Retry(st); err != nil {
		t.Fatalf("retry: %v", err)
	}
	st = o.Resume(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("resume after retry should succeed, got %q", st.Status)
	}
}

func TestRunResolvedATAdvances(t *testing.T) {
	// A pre-SUCCESS AT is advanced without re-running (no worker invocation).
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	fw := &fakeWorker{}
	o.model = fw
	st := &RunState{Status: ChainPending, ATs: []ATRecord{{Slug: "a", Status: ATSuccess, Spec: AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerModel}}}}}
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess || fw.attempts != 0 {
		t.Fatalf("resolved AT should advance without running; status=%q attempts=%d", st.Status, fw.attempts)
	}
}

func TestPauseAtBoundary(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}), WithPauseCheck(func(string) bool { return true }))
	at := detTask("a")
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainPaused {
		t.Fatalf("want paused, got %q", st.Status)
	}
}

func TestWithGateTimeoutOption(t *testing.T) {
	o := New(WithGateTimeout(50 * time.Millisecond))
	if o.gateTimeout != 50*time.Millisecond {
		t.Fatalf("gate timeout not set: %v", o.gateTimeout)
	}
}
