package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"corpos/internal/cost"
	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// The spawner's spawn-count budget refuses a Run once the tree-wide worker count is spent,
// returning ErrSpawnBudgetExhausted with NO worker run (~0 cost) — the single chokepoint that
// bounds BOTH the orchestrator's direct agent.spawn AND the coding organ's operator-seat
// interventions, which all spawn through here (Run-42 over-decomposition: 34 workers for one task).
func TestSpawnerRunSpawnBudget(t *testing.T) {
	adapter := model.NewEcho("w", model.Response{Text: "done", StopReason: model.StopEndTurn})
	budget := cost.NewSpawnBudget(2)
	s := NewSpawner(&fakeProvider{}, nilProject, nil, adapter, WithSpawnerSpawnBudget(budget))
	p := &profile.JobProfile{Name: "coding", Tier: profile.TierLocal}
	for i := 1; i <= 2; i++ {
		if _, err := s.Run(context.Background(), p, "duty"); err != nil {
			t.Fatalf("run %d within budget must succeed, got %v", i, err)
		}
	}
	_, err := s.Run(context.Background(), p, "duty")
	if !errors.Is(err, ErrSpawnBudgetExhausted) {
		t.Fatalf("a spawn past the budget must return ErrSpawnBudgetExhausted, got %v", err)
	}
	if budget.Count() != 2 {
		t.Errorf("a refused spawn must not consume budget; Count = %d, want 2", budget.Count())
	}
}

// ctxAwareAdapter respects its call context: a done context surfaces as a timeout
// fault (the real adapters' behavior when the per-turn deadline is spent), else it
// ends the turn. It records how many live calls it served, so a test can prove a
// spawned worker ran under a LIVE context rather than the parent's spent deadline.
type ctxAwareAdapter struct{ liveCalls int }

func (a *ctxAwareAdapter) Model() string   { return "ctxaware" }
func (a *ctxAwareAdapter) Available() bool { return true }
func (a *ctxAwareAdapter) Complete(ctx context.Context, _ []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	if err := ctx.Err(); err != nil {
		return model.Response{}, err // a spent context is the timeout fault the loop recovers
	}
	a.liveCalls++
	return model.Response{Model: "ctxaware", Text: "done", StopReason: model.StopEndTurn}, nil
}

// workerContext applies the budget as the worker's OWN deadline and preserves the
// parent's values, rather than inheriting the parent's (spent) per-turn deadline.
func TestWorkerContextAppliesOwnBudgetAndKeepsValues(t *testing.T) {
	parent, cancel := context.WithTimeout(WithParentRunID(context.Background(), "run-42"), time.Millisecond)
	defer cancel()
	<-parent.Done() // the parent per-turn deadline is now spent (DeadlineExceeded)

	ctx, stop := workerContext(parent, time.Hour)
	defer stop()

	if err := ctx.Err(); err != nil {
		t.Fatalf("worker ctx should be live despite the spent parent deadline, got %v", err)
	}
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) < 30*time.Minute {
		t.Errorf("worker ctx should carry its OWN ~1h budget, deadline ok=%v in=%v", ok, time.Until(dl))
	}
	if got := ParentRunID(ctx); got != "run-42" {
		t.Errorf("worker ctx lost the parent's values: ParentRunID=%q want run-42", got)
	}
}

// A parent DEADLINE expiry must NOT cancel the worker (that residue is exactly what we
// detach from): the worker keeps its own budget.
func TestWorkerContextIgnoresParentDeadlineExpiry(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	ctx, stop := workerContext(parent, time.Hour)
	defer stop()

	<-parent.Done()                   // parent per-turn deadline expires
	time.Sleep(20 * time.Millisecond) // give any AfterFunc a chance to (wrongly) fire
	if err := ctx.Err(); err != nil {
		t.Fatalf("a parent deadline expiry must not cancel the worker, got %v", err)
	}
}

// A real parent CANCELLATION (operator Ctrl-C / overall-run cancel) DOES propagate to
// the worker — only the deadline is detached, not cancellation.
func TestWorkerContextPropagatesParentCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, stop := workerContext(parent, time.Hour)
	defer stop()

	cancelParent() // an explicit cancel, not a deadline expiry
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatal("a real parent cancel should propagate to the worker ctx")
	}
}

// End-to-end: a worker spawned under a SPENT parent deadline runs on its own fresh
// budget when WithWorkerTimeout is set (its first call sees a live ctx), instead of
// inheriting the residue and timing out with zero progress (bug 1123).
func TestSpawnerRunGivesWorkerFreshBudget(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-parent.Done() // the orchestrator's per-turn deadline is spent at spawn time

	adapter := &ctxAwareAdapter{}
	s := NewSpawner(&fakeProvider{}, nilProject, nil, adapter, WithWorkerTimeout(time.Hour))
	p := &profile.JobProfile{Name: "coding", Tier: profile.TierLocal}

	res, err := s.Run(parent, p, "duty")
	if err != nil {
		t.Fatalf("worker should run on a fresh budget, not error on the spent parent ctx: %v", err)
	}
	if res.Text != "done" || adapter.liveCalls != 1 {
		t.Errorf("worker did not run on a live ctx: text=%q liveCalls=%d", res.Text, adapter.liveCalls)
	}
}

// Without WithWorkerTimeout the worker inherits the parent ctx unchanged (prior
// behavior): a spent parent deadline makes its first call a timeout fault, which the
// single-tier loop ends gracefully — zero live calls (the bug-1123 failure mode).
func TestSpawnerRunInheritsParentCtxWithoutOption(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-parent.Done()

	adapter := &ctxAwareAdapter{}
	s := NewSpawner(&fakeProvider{}, nilProject, nil, adapter)
	p := &profile.JobProfile{Name: "coding", Tier: profile.TierLocal}

	res, err := s.Run(parent, p, "duty")
	if err != nil {
		t.Fatalf("a spent inherited deadline should end gracefully, not error: %v", err)
	}
	if adapter.liveCalls != 0 || res.Text == "done" {
		t.Errorf("worker should NOT have served a live call on the spent inherited ctx: liveCalls=%d text=%q", adapter.liveCalls, res.Text)
	}
	if res.ModelFault != string(model.FaultTimeout) {
		t.Errorf("expected a timeout ModelFault on the inherited spent ctx, got %q", res.ModelFault)
	}
}

// specRecAdapter records the specs it was asked to complete (to prove projection
// threads through to the worker's model calls) and ends the turn immediately.
type specRecAdapter struct{ specs []tool.Spec }

func (a *specRecAdapter) Model() string   { return "specrec" }
func (a *specRecAdapter) Available() bool { return true }
func (a *specRecAdapter) Complete(_ context.Context, _ []model.ChatMessage, specs []tool.Spec) (model.Response, error) {
	a.specs = specs
	return model.Response{Model: "specrec", Text: "done", StopReason: model.StopEndTurn}, nil
}

func nilProject(*profile.JobProfile) []tool.Spec { return nil }

// TestSpawnDispatchesThroughScopedProvider proves WithScopedProvider routes a
// worker's tool calls through the per-profile scoped provider (the dispatch-time
// capability boundary), not the unscoped shared provider — the worker half of bug
// 1044's fix.
func TestSpawnDispatchesThroughScopedProvider(t *testing.T) {
	shared := &fakeProvider{} // the unscoped provider — must NOT be used
	scoped := &fakeProvider{} // the per-profile wrapper — must receive the call
	var gotProfile *profile.JobProfile
	sp := func(p *profile.JobProfile) tool.Provider {
		gotProfile = p
		return scoped
	}
	adapter := &oneToolThenDone{}
	s := NewSpawner(shared, nilProject, nil, adapter, WithScopedProvider(sp))

	p := &profile.JobProfile{Name: "coding", Tier: profile.TierLocal}
	if _, err := s.Run(context.Background(), p, "duty"); err != nil {
		t.Fatalf("worker Run: %v", err)
	}
	if gotProfile != p {
		t.Fatalf("scope provider not called with the worker's profile")
	}
	if scoped.calls != 1 {
		t.Errorf("scoped provider saw %d calls, want 1", scoped.calls)
	}
	if shared.calls != 0 {
		t.Errorf("unscoped shared provider saw %d calls, want 0 (scope must intercept)", shared.calls)
	}
}

// TestWithScopedProviderNilIgnored confirms a nil factory leaves the worker on the
// shared provider (prior behavior preserved).
func TestWithScopedProviderNilIgnored(t *testing.T) {
	shared := &fakeProvider{}
	adapter := &oneToolThenDone{}
	s := NewSpawner(shared, nilProject, nil, adapter, WithScopedProvider(nil))

	p := &profile.JobProfile{Name: "coding", Tier: profile.TierLocal}
	if _, err := s.Run(context.Background(), p, "duty"); err != nil {
		t.Fatalf("worker Run: %v", err)
	}
	if shared.calls != 1 {
		t.Errorf("with nil scope factory the shared provider should serve; saw %d calls", shared.calls)
	}
}

// A profile's SystemPrompt is projected onto the spawned worker as a leading system
// message (T6: the faithful-reporting posture reaches the worker, not just the duty).
func TestSpawnProjectsProfileSystemPrompt(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("base"))
	p := &profile.JobProfile{Name: "p", Tier: profile.TierLocal, SystemPrompt: "Report faithfully; narration is not execution."}
	w := s.Spawn(p)
	var sys string
	for _, m := range w.Transcript() {
		if m.Role == model.RoleSystem {
			sys = m.Content
		}
	}
	if sys != p.SystemPrompt {
		t.Fatalf("worker system message = %q, want the profile prompt", sys)
	}
}

// An empty profile SystemPrompt seeds no system message.
func TestSpawnNoSystemPromptWhenProfileEmpty(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("base"))
	w := s.Spawn(&profile.JobProfile{Name: "p", Tier: profile.TierLocal})
	for _, m := range w.Transcript() {
		if m.Role == model.RoleSystem {
			t.Fatalf("unexpected system message %q for a profile with no SystemPrompt", m.Content)
		}
	}
}

func TestSpawnProjectsScopeThreadsProfileAndRuns(t *testing.T) {
	var gotProfile *profile.JobProfile
	wantSpecs := []tool.Spec{{Name: "work"}, {Name: "fs"}}
	proj := func(p *profile.JobProfile) []tool.Spec {
		gotProfile = p
		return wantSpecs
	}
	rec := &specRecAdapter{}
	s := NewSpawner(&fakeProvider{}, proj, nil, rec)

	p := &profile.JobProfile{Name: "task-lifecycle", Tier: profile.TierLocal}
	w := s.Spawn(p)
	if w.Profile() != p {
		t.Fatalf("profile not threaded onto worker loop")
	}
	if gotProfile != p {
		t.Fatalf("projection not called with the profile")
	}
	res, err := w.Run(context.Background(), "do the duty")
	if err != nil {
		t.Fatalf("worker Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("worker answer = %q, want done", res.Text)
	}
	if len(rec.specs) != len(wantSpecs) || rec.specs[0].Name != "work" || rec.specs[1].Name != "fs" {
		t.Errorf("projected specs not threaded to the model: %+v", rec.specs)
	}
}

func TestRouterForLocalEscalatesOnToolError(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("base"),
		WithStrongTier(model.NewEcho("strong"), 1))
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}})
	if rt.State() != router.StateCheap {
		t.Fatalf("worker should start on the cheap tier, got %s", rt.State())
	}
	rt.Observe(router.Signals{ToolErrors: 1})
	if rt.State() != router.StateEscalated {
		t.Errorf("escalation not wired: state %s after a tool error", rt.State())
	}
}

func TestRouterForNoEscalateSignalStaysSingleTier(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("base"),
		WithStrongTier(model.NewEcho("strong"), 1))
	// A profile with EscalateOn that does NOT include tool_error stays single-tier.
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"timeout"}})
	rt.Observe(router.Signals{ToolErrors: 9})
	if rt.State() != router.StateCheap {
		t.Errorf("worker without a tool_error escalation signal should not escalate")
	}
}

func TestRouterForStrongTierRunsOnStrong(t *testing.T) {
	base := &recAdapter{}
	strong := &recAdapter{}
	s := NewSpawner(&fakeProvider{}, nilProject, nil, base, WithStrongTier(strong, 1))
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierStrong})
	var want model.Adapter = strong
	if rt.NextAdapter() != want {
		t.Errorf("strong-tier worker should run on the strong adapter")
	}
}

func TestRouterForNoStrongTierIsSingleTier(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("base"))
	// Even an escalate-on-tool-error profile runs single-tier with no strong adapter.
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}})
	rt.Observe(router.Signals{ToolErrors: 5})
	if rt.State() != router.StateCheap {
		t.Errorf("with no strong tier the worker must stay single-tier")
	}
}

func TestRouterForMidWorkerRestsOnMidEscalatesToStrong(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithStrongTier(model.NewEcho("strong"), 1))
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierMid, EscalateOn: []string{"tool_error"}})
	if got := rt.NextAdapter().Model(); got != "mid" {
		t.Fatalf("a tier=mid worker should rest on the mid rung, got %s", got)
	}
	rt.Observe(router.Signals{ToolErrors: 1})
	if got := rt.NextAdapter().Model(); got != "strong" {
		t.Errorf("a tier=mid worker should escalate to strong, got %s", got)
	}
}

func TestRouterForLocalWorkerClimbsThroughMid(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithStrongTier(model.NewEcho("strong"), 1))
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}})
	rt.Observe(router.Signals{ToolErrors: 1}) // local → mid
	if got := rt.NextAdapter().Model(); got != "mid" {
		t.Fatalf("a local worker's first escalation should climb to mid, got %s", got)
	}
	rt.Observe(router.Signals{ToolErrors: 1}) // mid → strong
	if got := rt.NextAdapter().Model(); got != "strong" {
		t.Errorf("a local worker's second escalation should climb to strong, got %s", got)
	}
}

func TestRouterForBoundsTheStrongRung(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithStrongTier(model.NewEcho("strong"), 1),
		WithStrongBound(1))
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierMid, EscalateOn: []string{"tool_error"}})
	rt.Observe(router.Signals{ToolErrors: 1}) // mid → strong
	if got := rt.NextAdapter().Model(); got != "strong" {
		t.Fatalf("first escalation should reach strong, got %s", got)
	}
	if got := rt.NextAdapter().Model(); got != "mid" {
		t.Errorf("the strong rung's bound should refuse a second turn, dropping to mid, got %s", got)
	}
	if rt.BoundBlocked() != 1 {
		t.Errorf("bound-blocked = %d, want 1", rt.BoundBlocked())
	}
}

func TestRouterForMidWorkerWithoutStrongRunsSingleTierOnMid(t *testing.T) {
	// A tier=mid worker resting on the top configured rung (no strong) is single-tier.
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")))
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierMid, EscalateOn: []string{"tool_error"}})
	rt.Observe(router.Signals{ToolErrors: 9})
	if got := rt.NextAdapter().Model(); got != "mid" {
		t.Errorf("a mid worker atop the ladder should stay on mid, got %s", got)
	}
}

func TestWithMidTierIgnoresNil(t *testing.T) {
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("base"), WithMidTier(nil))
	if s.hasMid {
		t.Errorf("a nil mid adapter must be ignored")
	}
}

func TestSpawnerRunBuildsHooksAndReturnsResult(t *testing.T) {
	hookBuilt := false
	bh := func(*profile.JobProfile) *hooks.Surface {
		hookBuilt = true
		return hooks.NewSurface()
	}
	m := model.NewEcho("q", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	s := NewSpawner(&fakeProvider{}, nilProject, bh, m)
	res, err := s.Run(context.Background(), &profile.JobProfile{Name: "x", Tier: profile.TierLocal}, "duty")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Run answer = %q", res.Text)
	}
	if !hookBuilt {
		t.Errorf("buildHooks was not invoked for the worker")
	}
}

func TestSpawnNilBuildHooksReturningNilIsSafe(t *testing.T) {
	// A buildHooks that returns nil must not wire an empty surface or panic.
	bh := func(*profile.JobProfile) *hooks.Surface { return nil }
	m := model.NewEcho("q", model.Response{Text: "ok", StopReason: model.StopEndTurn})
	s := NewSpawner(&fakeProvider{}, nilProject, bh, m)
	if _, err := s.Run(context.Background(), &profile.JobProfile{Name: "x", Tier: profile.TierLocal}, "duty"); err != nil {
		t.Fatalf("Run with nil-returning buildHooks: %v", err)
	}
}

func TestWithEscalationContractWiresFullConfigAndEmitter(t *testing.T) {
	em := &recEmitter{id: "w-evt"}
	// The worker router uses the full toolkit config: parse_failure (enabled at
	// threshold 2 in DefaultConfig) is a trigger WithEscalation would NOT enable.
	s := NewSpawner(errProvider{}, nilProject, nil, toolErrorThenAnswer("local"),
		WithStrongTier(model.NewEcho("strong", model.Response{Text: "s", StopReason: model.StopEndTurn}), 1),
		WithEscalationContract(em, router.DefaultConfig()))

	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}})
	e := rt.Observe(router.Signals{ParseFailures: 2})
	if e.Direction != router.EdgeEscalate || e.Trigger != router.TriggerParseFailure {
		t.Fatalf("worker router should run the full config (parse_failure): %+v", e)
	}

	// The emitter threads onto the spawned worker loop: a tool-error turn escalates
	// (repeated_tool_error pinned to the worker's threshold of 1) and emits.
	if _, err := s.Run(context.Background(),
		&profile.JobProfile{Name: "w", Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}}, "duty"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.calls) == 0 {
		t.Error("a worker escalate edge should emit via the contract emitter")
	}
}

func TestWithEscalationContractNilConfigKeepsSingleTrigger(t *testing.T) {
	// A zero (nil-Triggers) config leaves hasEscConfig false — workers keep the
	// single repeated_tool_error trigger.
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("base"),
		WithStrongTier(model.NewEcho("strong"), 1),
		WithEscalationContract(nil, router.Config{}))
	if s.hasEscConfig {
		t.Error("a nil-Triggers config must not set hasEscConfig")
	}
	rt := s.routerFor(&profile.JobProfile{Tier: profile.TierLocal, EscalateOn: []string{"tool_error"}})
	if e := rt.Observe(router.Signals{ParseFailures: 9}); e.Direction != router.EdgeNone {
		t.Error("without a full config, parse_failure must not be a trigger")
	}
}

func TestWithStrongTierIgnoresNilAndClampsThreshold(t *testing.T) {
	base := model.NewEcho("base")
	sNil := NewSpawner(&fakeProvider{}, nilProject, nil, base, WithStrongTier(nil, 5))
	if sNil.hasStrong {
		t.Errorf("a nil strong adapter must be ignored")
	}
	sClamp := NewSpawner(&fakeProvider{}, nilProject, nil, base, WithStrongTier(model.NewEcho("strong"), 0))
	if sClamp.escalateAfter != 1 {
		t.Errorf("escalateAfter below 1 should clamp to 1, got %d", sClamp.escalateAfter)
	}
}

// Bug 1080: a file-mutating (coding) profile whose projection DROPS the fs surface (the
// enum-less raw fs spec fails closed under fs[write,edit] action-scoping while sys survives)
// must be REFUSED at spawn with ErrWorkerToolsUnrunnable — not handed zero file tools, where it
// can only fabricate a zero-fs-dispatch "done". The spawn boundary classifies it ClassUsage.
func TestSpawnerRun_RefusesMutatorWithDroppedFS(t *testing.T) {
	adapter := model.NewEcho("w", model.Response{Text: "done", StopReason: model.StopEndTurn})
	coder := &profile.JobProfile{Name: "atomic-coding-chain", Tier: profile.TierLocal, Tools: []profile.SurfaceScope{
		{Surface: "fs", Actions: []string{"read", "write", "edit"}},
		{Surface: "sys", Actions: []string{"exec"}},
	}}

	// Projection drops fs (enum-less → fail-closed) but sys survives → the 1080 trap.
	dropFS := func(*profile.JobProfile) []tool.Spec { return []tool.Spec{{Name: "sys"}} }
	s := NewSpawner(&fakeProvider{}, dropFS, nil, adapter)
	if _, err := s.Run(context.Background(), coder, "fix the bug"); !errors.Is(err, ErrWorkerToolsUnrunnable) {
		t.Fatalf("a coding profile projected with no fs surface must error ErrWorkerToolsUnrunnable, got %v", err)
	}

	// fs survives projection → the guard must NOT fire (the worker runs).
	keepFS := func(*profile.JobProfile) []tool.Spec { return []tool.Spec{{Name: "fs"}, {Name: "sys"}} }
	s2 := NewSpawner(&fakeProvider{}, keepFS, nil, adapter)
	if _, err := s2.Run(context.Background(), coder, "fix the bug"); errors.Is(err, ErrWorkerToolsUnrunnable) {
		t.Fatalf("fs survived projection — the guard must NOT fire; got %v", err)
	}

	// A read-only profile with fs dropped is NOT a mutator → exempt (it can answer from the
	// substrate; only a file-mutating profile is a misconfiguration when it loses fs).
	readonly := &profile.JobProfile{Name: "code-review", Tier: profile.TierLocal, Tools: []profile.SurfaceScope{
		{Surface: "fs", Actions: []string{"read", "grep"}},
	}}
	if _, err := s.Run(context.Background(), readonly, "review the diff"); errors.Is(err, ErrWorkerToolsUnrunnable) {
		t.Fatalf("a read-only profile is exempt from the mutator-fs guard; got %v", err)
	}
}
