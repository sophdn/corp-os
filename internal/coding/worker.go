package coding

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"corpos/internal/agent"
	"corpos/internal/fsorgan"
	"corpos/internal/hooks"
	"corpos/internal/profile"
)

// WorkerStatus is the outcome the orchestrator assigns an AT's worker run. The
// orchestrator (not the worker) assigns it, because the orchestrator owns the gate
// and the write→gate→revise loop.
type WorkerStatus string

const (
	WorkerSuccess                WorkerStatus = "success"
	WorkerGateFailure            WorkerStatus = "gate_failure"
	WorkerCommandError           WorkerStatus = "command_error"
	WorkerMaxIterationsExhausted WorkerStatus = "max_iterations_exhausted"
	WorkerWorkspaceViolation     WorkerStatus = "workspace_violation"
	// WorkerOutputContractViolation is assigned when the worker passed the gate
	// but the AT's output_contract (assertions/extractions) failed.
	WorkerOutputContractViolation WorkerStatus = "output_contract_violation"
	// WorkerGateIntegrityViolation is assigned when the worker's attempt modified a
	// protected path (e.g. the gate's own oracle test) — the gate is orchestrator-
	// owned and immutable, so such an attempt is rejected, never trusted.
	WorkerGateIntegrityViolation WorkerStatus = "gate_integrity_violation"
	// WorkerFakeGreen is assigned when the gate passed but the worker's newly-added
	// regression test PASSES on the pre-fix tree — a tautological test that asserts the
	// buggy behavior, so the green certifies code that was never fixed. The red-before-
	// green gate (redgreen.go) catches this fake-green mode.
	WorkerFakeGreen WorkerStatus = "fake_green_tautological_test"
	// WorkerTestOnlyDiff is assigned when the gate passed but the worker's diff changed
	// ONLY test files — the production code was never touched, so the green is hollow.
	// The test-only-diff flag (flags.go) catches this; an AT whose job is to add tests
	// opts out with AllowTestOnlyDiff.
	WorkerTestOnlyDiff WorkerStatus = "test_only_diff"
	// WorkerVerifierRejected is assigned when the independent read-only verifier (T5) did
	// not confirm the fix: a non-PASS verdict, a PASS without command-block evidence, or a
	// PASS the orchestrator's own spot-check re-run contradicted.
	WorkerVerifierRejected WorkerStatus = "verifier_rejected"
	// WorkerVerifierError is assigned when the verifier sub-run failed to execute (infra).
	WorkerVerifierError WorkerStatus = "verifier_error"
	// WorkerRespawnCapReached is assigned when the per-duty respawn cap is hit: the
	// worker→gate loop has been re-entered the maximum number of times for this atom
	// across the duty, so the orchestrator stops honestly with an "escalation exhausted"
	// verdict rather than re-spawning toward the cost ceiling (chain 392 task 3315).
	WorkerRespawnCapReached WorkerStatus = "respawn_cap_reached"
)

// Feedback is what the orchestrator hands a worker for one attempt: the resolved
// upstream inputs, the 1-based iteration index, and the prior attempt's gate
// diagnostic (empty on the first attempt). The conversation is reset between
// attempts — prior files remain on disk — so the prompt stays bounded.
type Feedback struct {
	Iteration           int
	Inputs              map[string]any
	PriorGateDiagnostic string
	// CarriedTierModel is the model id of the highest tier a PRIOR respawn of this
	// same atom escalated to. A model worker starts its router there (capped below the
	// bounded top) instead of re-paying the local→mid→coder→strong climb every respawn
	// (chain 392 task 3314). Empty on the first attempt, or when no prior attempt
	// escalated off the floor.
	CarriedTierModel string
}

// AttemptResult is the outcome of one worker turn. A non-nil CommandErr is a hard
// worker failure (a deterministic command exiting non-zero, or a model spawn
// error) that short-circuits the loop — the gate is not consulted. Otherwise the
// orchestrator runs the gate to judge the attempt.
type AttemptResult struct {
	CommandErr error
	Note       string
	// HighestTierModel is the model id of the highest tier this attempt's worker
	// actually reached (read off the spawned loop's router). The orchestrator records
	// it on the AT and carries it into the next respawn's Feedback.CarriedTierModel so
	// the tier this attempt climbed to is not discarded (chain 392 task 3314). Empty
	// for a deterministic worker (it does not spawn a tiered model).
	HighestTierModel string
}

// Worker makes one pass at an AT's goal, mutating files under dir. It does NOT run
// the gate — the orchestrator does, after each attempt. This factoring is what
// makes the gate orchestrator-owned and immutable: a worker can neither run nor
// rewrite the gate; it only receives the gate's diagnostic as revision feedback.
type Worker interface {
	Attempt(ctx context.Context, at AtomicTask, dir string, fb Feedback) AttemptResult
}

// DeterministicWorker runs an AT's fixed command once. One-shot, no revision loop.
type DeterministicWorker struct {
	runner Runner
}

// NewDeterministicWorker builds a deterministic worker over the owned exec runner.
func NewDeterministicWorker(runner Runner) DeterministicWorker {
	return DeterministicWorker{runner: runner}
}

// Attempt runs the AT's command in dir. A non-zero exit is a command error that
// the orchestrator surfaces as WorkerCommandError (the gate is not reached).
func (w DeterministicWorker) Attempt(ctx context.Context, at AtomicTask, dir string, _ Feedback) AttemptResult {
	timeout := time.Duration(at.Worker.TimeoutSeconds) * time.Second
	run := w.runner.Run(ctx, at.Worker.Command, dir, timeout)
	if run.ExitCode != 0 {
		return AttemptResult{CommandErr: fmt.Errorf(
			"worker command %q exited %d\nstderr:\n%s",
			strings.Join(at.Worker.Command, " "), run.ExitCode, tail(run.Stderr, GateTailBytes))}
	}
	return AttemptResult{Note: "ran " + strings.Join(at.Worker.Command, " ")}
}

// spawnRunner is the narrow seam onto the corpos worker-spawn primitive. The
// production implementation is *agent.Spawner; tests inject a fake. A model worker
// is thus a scoped corpos sub-agent — it writes files through the agent loop's fs
// tool.Provider (its profile's fs scope), NOT through any prose-parsing protocol.
type spawnRunner interface {
	Run(ctx context.Context, p *profile.JobProfile, duty string, opts ...agent.Option) (agent.Result, error)
}

// ModelWorker drives a model through the corpos spawner in a single attempt; the
// orchestrator loops it against the gate. The worker's job-profile carries its tool
// scope (the fs surface for file writes) and model tier.
type ModelWorker struct {
	spawner spawnRunner
	profile *profile.JobProfile
}

// NewModelWorker builds a model worker over the spawner and the coding job-profile.
func NewModelWorker(spawner *agent.Spawner, p *profile.JobProfile) *ModelWorker {
	return &ModelWorker{spawner: spawner, profile: p}
}

// Attempt composes the AT into a worker duty and spawns a scoped sub-agent to do
// it. A spawn error short-circuits as a command error (a broken model is an infra
// failure, not a gate-revisable condition). The worker runs under the post-turn work
// audit (RequireMutation: a coding AT's job is to change files): a done-claim not backed
// by a real fs mutation, or one narrating a tool-call envelope in prose (run-6c), is an
// honest fabrication failure — never a gate-revisable success. On a sound attempt the
// worker's answer is the note; the files it wrote are judged by the gate.
func (w *ModelWorker) Attempt(ctx context.Context, at AtomicTask, dir string, fb Feedback) AttemptResult {
	duty := buildDuty(at, dir, fb)
	// Sandbox the worker's fs organ to THIS attempt's worktree (bug 1081): a model
	// that emits a relative path would otherwise have it resolved against the process
	// CWD by the raw OS call and escape the worktree into the host repo. WithRoot
	// confines every fs action under dir and rejects an escaping path at the organ
	// boundary. A worktree dir is the worker's whole world; the gate already runs
	// there (WithVerifyDir above). An empty dir leaves the organ unsandboxed.
	ctx = fsorgan.WithRoot(ctx, dir)
	// The worker edits in THIS attempt's worktree (dir), so its self-verify gate + the
	// opportunistic stop-when-green check must run there — not against the spawner's fixed
	// target repo (which never carries this attempt's edits, so the gate would read RED on
	// an unedited tree and never see the fix).
	opts := []agent.Option{
		agent.WithWorkAudit(agent.WorkAudit{RequireMutation: true}),
		agent.WithVerifyDir(dir),
	}
	// Start this respawn at the tier a prior attempt on the same atom escalated to,
	// instead of re-climbing from the local floor (chain 392 task 3314). The router
	// caps the lift below the bounded top, so a carried floor never rests on Opus.
	if fb.CarriedTierModel != "" {
		opts = append(opts, agent.WithStartRung(fb.CarriedTierModel))
	}
	// The principal-owned acceptance/gate test is outside the worker's writable scope:
	// deny any fs.write/edit to a Protected path at the DISPATCH boundary (T4), so the
	// worker cannot author or rewrite the test its verdict depends on (it never lands).
	if len(at.Protected) > 0 {
		opts = append(opts, agent.WithExtraHook(hooks.PreToolUse, "protected-acceptance-path", ProtectedPathGuard(at.Protected)))
	}
	res, err := w.spawner.Run(ctx, w.profile, duty, opts...)
	if err != nil {
		return AttemptResult{CommandErr: fmt.Errorf("model worker spawn: %w", err)}
	}
	if res.Fabricated != "" {
		return AttemptResult{CommandErr: fmt.Errorf("model worker fabrication: %s", res.Fabricated)}
	}
	return AttemptResult{Note: res.Text, HighestTierModel: res.HighestTierModel}
}

// buildDuty assembles the model worker's duty: the goal, resolved upstream inputs,
// the workspace write-allowlist, the gate it must satisfy, any conventions, and —
// on a revision — the prior gate diagnostic. The worker writes files through its fs
// tool scope, constrained to the workspace allowlist and the worktree dir.
func buildDuty(at AtomicTask, dir string, fb Feedback) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n", at.Goal)
	fmt.Fprintf(&b, "\nWrite files using your fs tools, under the worktree root %q.\n", dir)
	if len(at.Workspace) > 0 {
		fmt.Fprintf(&b, "Workspace allowlist — write ONLY paths matching these patterns:\n  - %s\n",
			strings.Join(at.Workspace, "\n  - "))
	}
	if len(fb.Inputs) > 0 {
		b.WriteString("\nUpstream inputs (from earlier tasks in the chain):\n")
		for name, val := range fb.Inputs {
			fmt.Fprintf(&b, "  %s: %v\n", name, val)
		}
	}
	if len(at.Gate) > 0 {
		b.WriteString("\nThe gate that determines success (you do not run it; the orchestrator does):\n")
		for _, cmd := range at.Gate {
			fmt.Fprintf(&b, "  - %s\n", strings.Join(cmd, " "))
		}
	}
	for _, ref := range at.ConventionsRef {
		content, err := os.ReadFile(ref)
		if err != nil {
			fmt.Fprintf(&b, "\n(could not load conventions_ref %s: %v)\n", ref, err)
			continue
		}
		fmt.Fprintf(&b, "\n--- Conventions from %s ---\n%s\n--- end conventions ---\n", ref, content)
	}
	b.WriteString("\nMake the smallest edit to the existing code that turns the gate green; do not add new files unless unavoidable, and stop once the fix is in place.\n")
	// Test-already-exists directive (bug 1163): for a production-fix AT the acceptance/regression test
	// ALREADY exists — it is the orchestrator-owned gate, protected from edits — whether it was
	// pre-seeded (a rehearsal) or authored by the OracleAuthor before this worker ran (the live path).
	// Yet the orchestrator's own duty text frequently says "add a test case / then add the test" (its
	// framing leaks the bug's regression-test acceptance criterion past the groundTruthDirective), which
	// sends a cheap worker to spend its budget authoring — or fighting the protected-path guard over — a
	// test that already exists, instead of on the one production fix. This deterministic worker-level
	// directive overrides that: when tests are protected AND this is NOT a test-authoring AT, state
	// plainly that the test exists and only production code needs changing. Captured live: a bug-1020
	// worker read the acceptance test, then burned rounds because its duty told it to "add the test".
	if protectsTestFiles(at.Protected) && !at.AllowTestOnlyDiff {
		b.WriteString("\nThe acceptance/regression test for this fix ALREADY EXISTS — it is the gate above, and it is READ-ONLY to you (you cannot edit any *_test.go file, and you do not need to). Do NOT author, add, or modify any test. If the goal text mentions adding a test, that test is already present as the gate; make ONLY the production-code change that turns it green.\n")
	}
	if fb.PriorGateDiagnostic != "" {
		fmt.Fprintf(&b, "\nPRIOR ATTEMPT FAILED THE GATE (files remain on disk; emit the corrected versions):\n%s\n",
			fb.PriorGateDiagnostic)
	}
	return b.String()
}

// protectsTestFiles reports whether any protected glob targets Go test files, i.e. the acceptance
// test is present and off-limits to the worker (the default for a production-fix atom, which sets
// Protected to **/*_test.go). It keys the "the test already exists, make only the prod fix" directive.
func protectsTestFiles(protected []string) bool {
	for _, g := range protected {
		if strings.Contains(g, "_test.go") {
			return true
		}
	}
	return false
}
