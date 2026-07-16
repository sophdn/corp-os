package coding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"corpos/internal/agent"
)

// Orchestrator runs a coding chain: it owns the gate and the write→gate→revise
// loop, routes each AT to its worker, verifies the output contract, and integrates
// green ATs through the Repo seam. It is the start/resume/stage state machine; the
// operator (the router-driven intervention seat) lands on top of it in task 3.
type Orchestrator struct {
	runner        Runner
	repo          Repo
	gateTimeout   time.Duration
	deterministic Worker
	model         Worker
	verifier      Verifier
	newRunID      func() string
	shouldPause   func(runID string) bool
	emitter       Emitter
	// coverageFn, when non-nil, computes the Tier-2 surface-scoped coverage grade after a
	// gate-pass (docs/TWO_TIER_GREEN_DESIGN.md). nil = disabled (the default — unit tests and
	// any caller that did not opt in are unaffected). It is injectable so the wiring is
	// testable without a real `go test` run; WithCoverageGrade installs computeCoverageGrade.
	coverageFn func(ctx context.Context, runner Runner, dir, diff string, timeout time.Duration) CoverageGrade
	// perDutyRespawnCap bounds how many times the worker→gate loop may be (re-)entered for
	// a SINGLE atom across the whole duty (the initial run plus every operator-seat resume).
	// At the cap the loop returns WorkerRespawnCapReached with an honest stuck verdict
	// instead of re-spawning the same non-converging atom toward the cost ceiling (chain 392
	// task 3315). It is distinct from a worker's per-loop revise budget (AT.MaxIterations)
	// and from the tree-wide spawn-count budget (-max-spawns). 0 = unbounded (the default;
	// the live wiring opts in via WithPerDutyRespawnCap).
	perDutyRespawnCap int
	// seededTier, when set, is the model id of the highest tier a PRIOR coding-path
	// invocation in the same run reached — carried in so this fresh-per-invocation organ's
	// atom starts there instead of re-climbing from the local floor (bug 1146, the
	// orchestrate-layer half of the carry that the within-organ ar.HighestTierModel cannot
	// reach because buildCodingPath rebuilds the organ on every re-invocation). It seeds each
	// atom's initial HighestTierModel; the lift is monotonic and capped below the bounded top
	// by the router, so a seed never rests a worker on the frontier. Empty = no carry.
	seededTier string
	// gofmtOnWrite, when true, runs `gofmt -w` over the worker's changed .go files after
	// each attempt and before the gate, so a model's whitespace/indent drift is normalized
	// into a gofmt-clean deliverable (the full gate's stage 1 is gofmt -s). Off by default
	// so unit tests with a counting gate runner are unaffected; the Go-centric live coding
	// path opts in via WithGofmtNormalize.
	gofmtOnWrite bool
}

// Option configures an Orchestrator.
type Option func(*Orchestrator)

// WithRunner sets the owned exec runner the gate (and the deterministic worker)
// use. Defaults to ExecRunner.
func WithRunner(r Runner) Option {
	return func(o *Orchestrator) {
		if r != nil {
			o.runner = r
		}
	}
}

// WithRepo sets the git integration seam. Defaults to a NoopRepo over the chain's
// target repo (the trivial single-dir path).
func WithRepo(r Repo) Option {
	return func(o *Orchestrator) {
		if r != nil {
			o.repo = r
		}
	}
}

// WithModelWorker wires the model worker (a scoped corpos sub-agent via the
// spawner). Without it, model-kind ATs fail with a clear command error.
func WithModelWorker(w Worker) Option {
	return func(o *Orchestrator) {
		if w != nil {
			o.model = w
		}
	}
}

// WithVerifier wires the independent read-only verifier (T5): after a worker AT passes the
// gate, a SEPARATE scoped sub-run re-runs the gate and reports a verdict, and the
// orchestrator requires command-block evidence + spot-checks before crediting success.
// Without it the verify phase is skipped (the prior behavior). A nil verifier is ignored.
func WithVerifier(v Verifier) Option {
	return func(o *Orchestrator) {
		if v != nil {
			o.verifier = v
		}
	}
}

// WithGateTimeout bounds each gate command's wall-clock time (0 = unbounded).
func WithGateTimeout(d time.Duration) Option {
	return func(o *Orchestrator) {
		if d > 0 {
			o.gateTimeout = d
		}
	}
}

// WithCoverageGrade enables the Tier-2 surface-scoped coverage advisory: after a gate-pass
// the orchestrator grades the green (confirmed vs proposed) and attaches a non-blocking
// FlagCoverageAdvisory on a "proposed" verdict (docs/TWO_TIER_GREEN_DESIGN.md). Off by
// default. Installs the real computeCoverageGrade; tests inject a fake via withCoverageFn.
func WithCoverageGrade() Option {
	return func(o *Orchestrator) {
		o.coverageFn = computeCoverageGrade
	}
}

// WithGofmtNormalize makes the loop run `gofmt -w` over the worker's changed .go files
// after each attempt and before the gate, so a model's whitespace/indent drift becomes
// a gofmt-clean deliverable that survives the full gate's gofmt -s stage. Off by default
// (so unit tests with a counting gate runner are unaffected); the Go-centric live coding
// path opts in. gofmt only reformats — never alters semantics — so it cannot mask a wrong
// fix; it is best-effort (a parse error or a missing gofmt leaves the file untouched).
func WithGofmtNormalize() Option {
	return func(o *Orchestrator) {
		o.gofmtOnWrite = true
	}
}

// WithPerDutyRespawnCap bounds how many times the worker→gate loop may be (re-)entered
// for a single atom across the whole duty, so one non-converging atom stops with an honest
// "escalation exhausted" verdict instead of thrashing to the cost ceiling (chain 392 task
// 3315). A cap < 1 is ignored (left unbounded). It is independent of and complements the
// per-loop revise budget and the tree-wide spawn-count budget, which stay as backstops.
func WithPerDutyRespawnCap(cap int) Option {
	return func(o *Orchestrator) {
		if cap >= 1 {
			o.perDutyRespawnCap = cap
		}
	}
}

// WithSeededTier carries the highest tier a prior coding-path invocation in the same run
// reached into this organ, so a re-spawned coding worker (a fresh organ per orchestrate-layer
// re-invocation) begins at that tier instead of re-climbing from the local Qwen floor (bug
// 1146). It seeds each atom's initial HighestTierModel; the router's monotonic, below-the-
// bounded-top lift means a seed only avoids re-paying the climb and never rests a worker on
// the frontier. An empty model id is a no-op (no prior tier to carry).
func WithSeededTier(modelID string) Option {
	return func(o *Orchestrator) {
		o.seededTier = modelID
	}
}

// WithRunIDFunc overrides run-id minting (tests inject a deterministic id).
func WithRunIDFunc(fn func() string) Option {
	return func(o *Orchestrator) {
		if fn != nil {
			o.newRunID = fn
		}
	}
}

// WithPauseCheck wires a boundary pause check: when it returns true at an AT
// boundary, the chain pauses (resume picks it back up). Without it a chain never
// self-pauses. The full event-ledger pause signal lands in task 6.
func WithPauseCheck(fn func(runID string) bool) Option {
	return func(o *Orchestrator) {
		if fn != nil {
			o.shouldPause = fn
		}
	}
}

// WithEmitter wires the event sink that the chain's state projects over. Without
// it events are discarded (NoopEmitter) and state lives only in memory.
func WithEmitter(e Emitter) Option {
	return func(o *Orchestrator) {
		if e != nil {
			o.emitter = e
		}
	}
}

// New builds an Orchestrator. By default it uses the owned ExecRunner, a NoopRepo,
// the deterministic worker, crypto-random run ids, and a no-op event emitter.
func New(opts ...Option) *Orchestrator {
	o := &Orchestrator{runner: ExecRunner{}, newRunID: randomRunID, emitter: NoopEmitter{}}
	for _, opt := range opts {
		opt(o)
	}
	o.deterministic = NewDeterministicWorker(o.runner)
	if o.repo == nil {
		o.repo = NoopRepo{}
	}
	return o
}

// emitAT records an AT's current state as a delta event.
func (o *Orchestrator) emitAT(ar *ATRecord) { o.emitter.Emit(Event{Kind: EvATStatus, AT: *ar}) }

// emitChain records a chain-level transition + the current position.
func (o *Orchestrator) emitChain(state *RunState) {
	o.emitter.Emit(Event{Kind: EvChainStatus, Status: state.Status, FailedATSlug: state.FailedATSlug, Position: state.CurrentPosition})
}

// Start validates the chain, seeds a fresh run, and initializes the integration
// branch via the Repo. It does not execute any AT. A NoopRepo is bound to the
// chain's target dir if no repo was configured.
func (o *Orchestrator) Start(ctx context.Context, chain Chain, runID string) (*RunState, error) {
	if err := chain.Validate(); err != nil {
		return nil, err
	}
	if runID == "" {
		runID = o.newRunID()
	}
	if nr, ok := o.repo.(NoopRepo); ok && nr.Dir == "" {
		o.repo = NoopRepo{Dir: chain.TargetRepo}
	}
	// An empty BaseBranch is left empty here on purpose: the git Repo seam resolves
	// it to the target repo's actual default/current branch (bug 1077). Hardcoding
	// "main" defeated that resolve and broke master-default repos.
	state := &RunState{
		RunID:             runID,
		ChainSlug:         chain.Slug,
		TargetRepo:        chain.TargetRepo,
		BaseBranch:        chain.BaseBranch,
		IntegrationBranch: fmt.Sprintf("coding/runs/%s/integration", runID),
		Status:            ChainPending,
		ATs:               make([]ATRecord, len(chain.Tasks)),
	}
	for i, t := range chain.Tasks {
		// Seed the atom's carried floor from a prior coding-path invocation's reached tier
		// (bug 1146): the fresh-per-invocation organ otherwise starts every atom at the local
		// floor, discarding the orchestrate-layer escalation state. The first worker.Attempt
		// then carries this via Feedback.CarriedTierModel.
		state.ATs[i] = ATRecord{Slug: t.Slug, Spec: t, Position: i, Status: ATPending, HighestTierModel: o.seededTier}
	}
	if err := o.repo.Init(ctx, state); err != nil {
		return nil, fmt.Errorf("init integration branch: %w", err)
	}
	seeds := make([]ATRecord, len(state.ATs))
	copy(seeds, state.ATs)
	o.emitter.Emit(Event{Kind: EvChainStarted, RunID: state.RunID, ChainSlug: state.ChainSlug,
		TargetRepo: state.TargetRepo, BaseBranch: state.BaseBranch, IntegrationBranch: state.IntegrationBranch, Seeds: seeds})
	return state, nil
}

// RunToCompletion drives remaining ATs in order from CurrentPosition. It stops at
// the first AT failure (fail-fast) or a boundary pause; an already-resolved AT is
// advanced without re-running. On a clean sweep the chain reaches SUCCESS.
func (o *Orchestrator) RunToCompletion(ctx context.Context, state *RunState) *RunState {
	state.Status = ChainRunning
	for state.CurrentPosition < len(state.ATs) {
		if o.shouldPause != nil && o.shouldPause(state.RunID) {
			state.Status = ChainPaused
			o.emitChain(state)
			return state
		}
		ar := &state.ATs[state.CurrentPosition]
		if ar.isResolved() {
			state.CurrentPosition++
			continue
		}
		if !o.runOne(ctx, state, state.CurrentPosition) {
			state.Status = ChainFailed
			state.FailedATSlug = ar.Slug
			o.emitChain(state)
			return state
		}
		state.CurrentPosition++
	}
	state.Status = ChainSuccess
	o.emitChain(state)
	return state
}

// Resume continues a paused/failed/pending chain from CurrentPosition.
func (o *Orchestrator) Resume(ctx context.Context, state *RunState) *RunState {
	return o.RunToCompletion(ctx, state)
}

// runOne runs a single AT: resolve inputs, fork a workspace, drive the worker→gate
// loop, verify the output contract, then commit + fast-forward. It returns false
// on any AT failure (preserving the workspace for operator inspection).
func (o *Orchestrator) runOne(ctx context.Context, state *RunState, idx int) bool {
	ar := &state.ATs[idx]
	ar.Status = ATRunning
	o.emitAT(ar)

	inputs, err := o.resolveInputs(state, ar.Spec)
	if err != nil {
		ar.Status = ATFailed
		ar.WorkerStatus = WorkerCommandError
		ar.Diagnostic = err.Error()
		o.emitAT(ar)
		return false
	}

	if ar.ParentSHA == "" {
		if sha, herr := o.repo.HeadSHA(ctx); herr == nil {
			ar.ParentSHA = sha
		}
	}
	ws, err := o.repo.Open(ctx, ar.ParentSHA, ar)
	if err != nil {
		ar.Status = ATFailed
		ar.WorkerStatus = WorkerCommandError
		ar.Diagnostic = fmt.Sprintf("open workspace: %v", err)
		o.emitAT(ar)
		return false
	}
	// The worktree is PRESERVED on failure (the operator inspects the diff); it is
	// only closed on the success path below.
	dir := ws.Dir()
	ar.WorktreePath = dir

	// Seed the AT's authored acceptance oracle(s) into the worktree and commit them,
	// advancing the diff baseline PAST the seed so the protected oracle the orchestrator
	// just placed is not mistaken for worker tampering (the gate-integrity check in the
	// worker loop flags any changed protected path). The worker then drives the seeded
	// gate red→green.
	if err := o.seedOracles(ctx, ws, ar); err != nil {
		ar.Status = ATFailed
		ar.WorkerStatus = WorkerCommandError
		ar.Diagnostic = fmt.Sprintf("seed oracle: %v", err)
		o.emitAT(ar)
		return false
	}

	status, diag, iters, flags := o.runWorkerLoop(ctx, ar, dir, inputs)
	ar.Iterations = iters
	ar.WorkerStatus = status
	ar.Diagnostic = diag
	ar.Flags = flags
	if status != WorkerSuccess {
		ar.Status = ATFailed
		o.emitAT(ar)
		return false
	}

	// Independent read-only verify phase (T5): the coding worker is an LLM and so is the
	// verifier — the orchestrator decides by requiring command-block evidence and spot-
	// checking. A rejection here fails the AT even though the worker's own gate passed.
	if vstatus, vdiag := o.runVerifyPhase(ctx, ar.Spec, dir); vstatus != WorkerSuccess {
		ar.Status = ATFailed
		ar.WorkerStatus = vstatus
		ar.Diagnostic = vdiag
		o.emitAT(ar)
		return false
	}

	outputs, err := o.verifyContract(ctx, ar.Spec, dir)
	if err != nil {
		ar.Status = ATFailed
		ar.WorkerStatus = WorkerOutputContractViolation
		ar.Diagnostic = err.Error()
		o.emitAT(ar)
		return false
	}
	ar.Outputs = outputs

	sha, ok, err := ws.Commit(ctx, fmt.Sprintf("AT %02d: %s", ar.Position, ar.Slug))
	if err != nil {
		ar.Status = ATFailed
		ar.WorkerStatus = WorkerCommandError
		ar.Diagnostic = fmt.Sprintf("commit: %v", err)
		o.emitAT(ar)
		return false
	}
	if ok {
		ar.CommitSHA = sha
		_ = o.repo.FastForward(ctx, sha)
	} else if head, herr := o.repo.HeadSHA(ctx); herr == nil {
		ar.CommitSHA = head
	}
	_ = ws.Close()
	ar.WorktreePath = ""
	ar.Status = ATSuccess
	o.emitAT(ar)
	return true
}

// seedOracles writes the AT's authored acceptance oracle(s) into the worktree and
// commits them, advancing ar.ParentSHA to the seed commit so the seeded (protected)
// oracle becomes part of the diff baseline rather than appearing as a worker edit.
// A no-oracle AT (every bug-fix task, and a deterministic chain) is left untouched;
// a repo with no fork point (NoopRepo) seeds the file but cannot commit, so the
// baseline stays as-is (the noop path has no protected-diff check to fool).
func (o *Orchestrator) seedOracles(ctx context.Context, ws Workspace, ar *ATRecord) error {
	if len(ar.Spec.Oracles) == 0 {
		return nil
	}
	if err := ws.Seed(ar.Spec.Oracles); err != nil {
		return err
	}
	sha, ok, err := ws.Commit(ctx, fmt.Sprintf("seed acceptance oracle: AT %02d %s", ar.Position, ar.Slug))
	if err != nil {
		return err
	}
	if ok {
		ar.ParentSHA = sha
	}
	return nil
}

// runWorkerLoop drives the worker→gate loop for one AT and returns the final
// worker status, the last gate diagnostic, and the iteration count. The gate is
// run HERE (orchestrator-owned), never by the worker; its diagnostic is fed back
// into the next attempt. A worker command error short-circuits the loop.
func (o *Orchestrator) runWorkerLoop(ctx context.Context, ar *ATRecord, dir string, inputs map[string]any) (WorkerStatus, string, int, []GateFlag) {
	spec := ar.Spec
	parentSHA := ar.ParentSHA
	worker := o.workerFor(spec.Worker.Kind)
	if worker == nil {
		return WorkerCommandError, fmt.Sprintf("no worker wired for kind %q", spec.Worker.Kind), 0, nil
	}
	// Per-duty respawn cap (chain 392 task 3315): refuse re-entering the worker→gate loop
	// once this atom has been (re-)attempted the maximum number of times across the duty.
	// The check is at ENTRY, never mid-loop, so the orchestrator-owned revise budget
	// (AT.MaxIterations) of a running attempt is never truncated. The honest stuck verdict
	// carries the last real gate diagnostic (ar.Diagnostic from the prior attempt).
	if o.perDutyRespawnCap > 0 && ar.RespawnCount >= o.perDutyRespawnCap {
		return WorkerRespawnCapReached, stuckVerdict(spec.Slug, o.perDutyRespawnCap, ar.LastRealDiagnostic), 0, nil
	}
	ar.RespawnCount++
	maxIter, exhausted := attemptBudget(spec)
	// Run the gate in the resolved Go module root: a subdir-module target (module in go/)
	// has no go.mod at the worktree root, so `go build ./...` there can only error. Resolve
	// to the module root so the gate runs where the module lives (bug 1094); the worktree
	// root stays the dir for git/diff/gofmt (the edits span the whole worktree).
	gateDir := dir
	if md, ok := agent.ResolveGoModuleDir(dir); ok {
		gateDir = md
	}
	var lastDiag string
	for iter := 1; iter <= maxIter; iter++ {
		// Carry the highest tier any prior respawn of this atom reached into this
		// attempt's starting floor (chain 392 task 3314): the worker begins there
		// instead of re-climbing from the local floor. ar.HighestTierModel persists
		// across both these iterations and operator-seat resumes of the same atom.
		res := worker.Attempt(ctx, spec, dir, Feedback{Iteration: iter, Inputs: inputs, PriorGateDiagnostic: lastDiag, CarriedTierModel: ar.HighestTierModel})
		// Record the tier this attempt reached so the next respawn starts at >= it. The
		// lift is monotonic (the worker can't serve below its carried floor), so taking
		// the latest reported tier never lowers the carried floor.
		if res.HighestTierModel != "" {
			ar.HighestTierModel = res.HighestTierModel
		}
		if res.CommandErr != nil {
			return WorkerCommandError, res.CommandErr.Error(), iter, nil
		}
		// Normalize the worker's Go edits before the diff/gate: a model frequently emits
		// whitespace/indent drift that go build+test won't catch but gofmt -s (the full
		// gate's stage 1) will, so the deliverable would fail the real gate. gofmt only
		// reformats — never alters semantics — so this yields a gofmt-clean diff without
		// spending a revise round and cannot mask a wrong fix. Opt-in (Go-centric live
		// path); no-op for the default unit-test path.
		if o.gofmtOnWrite {
			o.gofmtChangedGo(ctx, dir)
		}
		// Structured reward-hack flags over the worker's diff (test-file tampering /
		// test-only diff), shared with the gate-integrity check via worktreeDiff.
		diff := o.worktreeDiff(ctx, dir, parentSHA)
		flags := gateFlags(diff, spec)
		// Gate integrity: a worker that modified a protected path (e.g. the gate's
		// own oracle test) is rejected BEFORE the gate is trusted — a tampered gate
		// can't certify success.
		if f, ok := findFlag(flags, FlagProtectedPathEdit); ok {
			return WorkerGateIntegrityViolation, f.Detail, iter, flags
		}
		// Scope the gate's whole-module `go test ./...` to the package(s) the worker edited,
		// so a single-package change on a large module isn't gated on the entire test suite
		// (which times out at codingGateTimeout and hands the worker a useless "timeout"
		// instead of a RED result). go build ./... stays whole-module; scoping is conservative
		// (it can't map every edit). When it CANNOT narrow — no Go edits, or edits outside the
		// module root — the organ must NOT fall back to the whole-module suite (a guaranteed
		// timeout on a large module that yields no actionable RED): skip it and feed back a
		// directive diagnostic instead (bug …organ-gate-runs-whole-module-go-test-on-empty…).
		// Guarded on (a) the gate carrying a whole-module `go test ./...` and (b) a real git
		// worktree (*GitRepo) — scoping reads the git diff (changedGoFiles), which only exists
		// under GitRepo; NoopRepo runs in a shared dir with no git, so it neither needs nor can
		// support scoping. So a non-go gate or a non-git run pays no extra git call.
		gateCmds := spec.Gate
		if gateHasWholeRepoGoTest(spec.Gate) && o.isGitRepo() {
			scoped, skipDiag := scopeOrSkipGate(spec.Gate, o.changedGoFiles(ctx, dir), dir, gateDir, osDirExists)
			if skipDiag != "" {
				lastDiag = skipDiag
				continue
			}
			gateCmds = scoped
		}
		gr := Gate{Commands: gateCmds, Timeout: o.gateTimeout}.Run(ctx, o.runner, gateDir)
		if gr.Passed {
			// Red-before-green: a passing gate is only trustworthy if the worker's
			// newly-added regression tests actually FAIL on the pre-fix tree. A test
			// that passes on unfixed code is a tautology certifying a fake green.
			verdict, err := o.tautologyVerdict(ctx, dir, parentSHA)
			if err != nil {
				return WorkerCommandError, err.Error(), iter, flags
			}
			if verdict != "" {
				return WorkerFakeGreen, verdict, iter, flags
			}
			// Test-only diff: the gate is green but the worker changed only test files —
			// the production code was never touched, so the green is hollow. The principal
			// opts a genuine test-authoring AT out with AllowTestOnlyDiff.
			if f, ok := findFlag(flags, FlagTestOnlyDiff); ok && !spec.AllowTestOnlyDiff {
				return WorkerTestOnlyDiff, f.Detail, iter, flags
			}
			// Oracle-tamper guard (bug 1161): even when a test-only diff is ALLOWED (a
			// test-authoring AT), a green reached by WEAKENING a pre-existing failing test is
			// still a fake-green. The FlagTestOnlyDiff opt-out lets a worker author tests, but
			// it cannot tell "authored a NEW correct test" from "rewrote an EXISTING failing
			// test to assert the buggy output". Since production is protected here, replaying
			// each modified test file's package at the fork point reveals a weakened oracle.
			if _, ok := findFlag(flags, FlagTestOnlyDiff); ok {
				verdict, err := o.oracleTamperVerdict(ctx, dir, parentSHA, diff)
				if err != nil {
					return WorkerCommandError, err.Error(), iter, flags
				}
				if verdict != "" {
					return WorkerFakeGreen, verdict, iter, flags
				}
			}
			// Tier-2 advisory (docs/TWO_TIER_GREEN_DESIGN.md): the green is now trustworthy
			// (not fabricated); grade whether it is SUBSTANTIATED. A "proposed" verdict — a
			// changed production line not exercised by any test, or a skipped touched-package
			// test — attaches a NON-blocking advisory flag. The status stays WorkerSuccess:
			// coverage never hard-fails a real green (a correct fix to already-covered code,
			// or a legitimately-unreachable changed line, must not be blocked).
			if o.coverageFn != nil {
				if g := o.coverageFn(ctx, o.runner, dir, diff, o.gateTimeout); g.Verdict == "proposed" {
					flags = append(flags, GateFlag{Kind: FlagCoverageAdvisory, Detail: g.Advisory})
				}
			}
			return WorkerSuccess, "", iter, flags
		}
		lastDiag = gr.Diagnostic
	}
	// Persist the entry's final gate diagnostic so a later cap-refused entry's stuck
	// verdict can surface it even after an intervening resetAT cleared ar.Diagnostic.
	if lastDiag != "" {
		ar.LastRealDiagnostic = lastDiag
	}
	return exhausted, lastDiag, maxIter, nil
}

// gofmtChangedGo runs `gofmt -w` over the .go files the worker changed in dir, so a
// model's whitespace/indent drift is normalized before the gate and the commit. Only
// changed files are formatted, so an untouched file never gains a spurious diff. Best-
// effort: a gofmt failure (a syntactically-broken file the gate will reject anyway, or
// gofmt missing from PATH) is ignored, leaving the file as the worker wrote it.
func (o *Orchestrator) gofmtChangedGo(ctx context.Context, dir string) {
	files := o.changedGoFiles(ctx, dir)
	if len(files) == 0 {
		return
	}
	_ = o.runner.Run(ctx, append([]string{"gofmt", "-w"}, files...), dir, o.gateTimeout)
}

// changedGoFiles lists the .go files with uncommitted changes in dir (modified, added,
// or untracked; deletions excluded — nothing to format), parsed from `git status
// --porcelain`. Returns nil on any git error (best-effort normalization).
func (o *Orchestrator) changedGoFiles(ctx context.Context, dir string) []string {
	run := o.runner.Run(ctx, []string{"git", "status", "--porcelain", "--untracked-files=all"}, dir, o.gateTimeout)
	if run.ExitCode != 0 {
		return nil
	}
	var files []string
	for _, ln := range strings.Split(run.Stdout, "\n") {
		if len(ln) < 4 { // shortest meaningful entry is "XY p"
			continue
		}
		if strings.Contains(ln[:2], "D") { // a deleted path has nothing to format
			continue
		}
		path := strings.TrimSpace(ln[2:])
		if i := strings.Index(path, " -> "); i >= 0 { // rename/copy: take the new path
			path = path[i+len(" -> "):]
		}
		path = strings.Trim(path, "\"") // porcelain quotes paths with unusual chars
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
	}
	return files
}

// workerFor selects the worker for a kind (reading spec data, not branching on
// behavior). The model worker may be nil when none is wired.
func (o *Orchestrator) workerFor(kind WorkerKind) Worker {
	if kind == WorkerModel {
		return o.model
	}
	return o.deterministic
}

// attemptBudget returns the worker→gate loop ceiling and the status assigned on
// exhaustion: a deterministic worker is one-shot (gate failure on miss); a model
// worker loops up to its max_iterations (exhaustion on miss).
func attemptBudget(spec AtomicTask) (int, WorkerStatus) {
	if spec.Worker.Kind == WorkerDeterministic {
		return 1, WorkerGateFailure
	}
	return spec.maxIterations(), WorkerMaxIterationsExhausted
}

// stuckVerdict renders the honest per-duty-respawn-cap verdict: the atom was re-attempted
// the maximum number of times and escalation is exhausted, so the orchestrator stops here
// rather than re-spawning toward the cost ceiling (chain 392 task 3315). It surfaces the
// last real gate diagnostic so the caller sees WHY the atom never converged, not just that
// it stopped.
func stuckVerdict(slug string, cap int, lastDiag string) string {
	if strings.TrimSpace(lastDiag) == "" {
		lastDiag = "(no gate diagnostic)"
	}
	return fmt.Sprintf("stuck on %q: per-duty respawn cap (%d) reached — escalation exhausted, halting instead of re-spawning. Last gate diagnostic: %s", slug, cap, lastDiag)
}

// randomRunID mints a 12-hex-char run id (crypto/rand; CGo-free).
func randomRunID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
