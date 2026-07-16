package coding

import (
	"context"
	"reflect"

	"corpos/internal/tool"
	"testing"
)

// linearChain builds an N-AT deterministic chain that all-pass under okRunner.
func linearChain(slugs ...string) Chain {
	tasks := make([]AtomicTask, len(slugs))
	for i, s := range slugs {
		tasks[i] = AtomicTask{Slug: s, Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}}, Gate: [][]string{{"g"}}}
	}
	return Chain{Slug: "c", TargetRepo: "x", Tasks: tasks}
}

func TestFoldProjectsLiveState(t *testing.T) {
	em := &SliceEmitter{}
	o := New(WithRunner(okRunner()), WithRepo(NoopRepo{Dir: t.TempDir()}), WithEmitter(em))
	live, _ := o.Start(context.Background(), linearChain("a", "b", "c"), "run")
	live = o.RunToCompletion(context.Background(), live)
	if live.Status != ChainSuccess {
		t.Fatalf("setup: want success, got %q", live.Status)
	}
	folded := Fold(em.Events)
	if !reflect.DeepEqual(folded, live) {
		t.Fatalf("Fold(events) != live state\n folded=%+v\n live  =%+v", folded, live)
	}
}

func TestFoldProjectsFailedState(t *testing.T) {
	em := &SliceEmitter{}
	// at "b" fails its gate.
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		if cmd[0] == "gate-b" {
			return CommandResult{Command: cmd, ExitCode: 1}
		}
		return CommandResult{Command: cmd}
	})
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}), WithEmitter(em))
	ch := Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{
		{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}}, Gate: [][]string{{"gate-a"}}},
		{Slug: "b", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}}, Gate: [][]string{{"gate-b"}}},
	}}
	live, _ := o.Start(context.Background(), ch, "run")
	live = o.RunToCompletion(context.Background(), live)
	if live.Status != ChainFailed {
		t.Fatalf("setup: want failed, got %q", live.Status)
	}
	if !reflect.DeepEqual(Fold(em.Events), live) {
		t.Fatalf("Fold != live for a failed chain")
	}
}

func TestResumeReFolds(t *testing.T) {
	// Run to failure, persist the event log, then reconstruct the state purely by
	// re-folding and resume to success after the gate is fixed.
	em := &SliceEmitter{}
	calls := 0
	r := funcRunner(func(cmd []string, _ string) CommandResult {
		calls++
		if calls == 1 {
			return CommandResult{Command: cmd, ExitCode: 1}
		}
		return CommandResult{Command: cmd}
	})
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}), WithEmitter(em))
	live, _ := o.Start(context.Background(), linearChain("a"), "run")
	live = o.RunToCompletion(context.Background(), live)
	if live.Status != ChainFailed {
		t.Fatalf("setup: want failed, got %q", live.Status)
	}

	// Re-fold from the log (simulating a fresh process) and resume.
	reHydrated := Fold(em.Events)
	if reHydrated.Status != ChainFailed || reHydrated.FailedATSlug != "a" {
		t.Fatalf("re-folded state wrong: %+v", reHydrated)
	}
	if err := o.Retry(reHydrated); err != nil {
		t.Fatalf("retry: %v", err)
	}
	reHydrated = o.Resume(context.Background(), reHydrated)
	if reHydrated.Status != ChainSuccess {
		t.Fatalf("resume after re-fold should succeed, got %q", reHydrated.Status)
	}
}

func TestCompactIsFoldEquivalent(t *testing.T) {
	em := &SliceEmitter{}
	o := New(WithRunner(okRunner()), WithRepo(NoopRepo{Dir: t.TempDir()}), WithEmitter(em))
	live, _ := o.Start(context.Background(), linearChain("a", "b", "c", "d"), "run")
	_ = o.RunToCompletion(context.Background(), live)

	compact := Compact(em.Events)
	if len(compact) >= len(em.Events) {
		t.Fatalf("compaction should shrink the log: %d >= %d", len(compact), len(em.Events))
	}
	if !reflect.DeepEqual(Fold(compact), Fold(em.Events)) {
		t.Fatal("Fold(Compact(log)) must equal Fold(log)")
	}
}

func TestBranchFixIsLogFork(t *testing.T) {
	em := &SliceEmitter{}
	// "impl" fails its gate; branch_fix forks the log.
	r := funcRunner(func(cmd []string, _ string) CommandResult { return CommandResult{Command: cmd, ExitCode: 1} })
	o := New(WithRunner(r), WithRepo(NoopRepo{Dir: t.TempDir()}), WithEmitter(em))
	ch := Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{
		{Slug: "impl", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}}, Gate: [][]string{{"g"}}},
	}}
	live, _ := o.Start(context.Background(), ch, "run")
	live = o.RunToCompletion(context.Background(), live)
	if err := o.InterveneBranchFix(context.Background(), live, "impl", "", 0); err != nil {
		t.Fatalf("branch_fix: %v", err)
	}
	folded := Fold(em.Events)
	if !reflect.DeepEqual(folded, live) {
		t.Fatalf("Fold(events) != live after branch_fix\n folded=%+v\n live=%+v", folded, live)
	}
	if folded.findAT("impl-fix1") == nil || folded.findAT("impl").Status != ATSkipped {
		t.Fatalf("log fork not reconstructed: %+v", folded.ATs)
	}
}

func TestNoopEmitterAndFoldEmpty(t *testing.T) {
	NoopEmitter{}.Emit(Event{}) // no panic
	if got := Fold(nil); !reflect.DeepEqual(got, &RunState{}) {
		t.Fatalf("Fold(nil) = %+v, want empty RunState", got)
	}
	// EvATInserted with an out-of-range position appends at the end.
	ev := []Event{
		{Kind: EvChainStarted, Seeds: []ATRecord{{Slug: "a", Position: 0}}},
		{Kind: EvATInserted, AT: ATRecord{Slug: "z", Position: 99}},
	}
	if got := Fold(ev); len(got.ATs) != 2 || got.ATs[1].Slug != "z" {
		t.Fatalf("out-of-range insert should append, got %+v", got.ATs)
	}
}

func TestSliceEmitterRecords(t *testing.T) {
	em := &SliceEmitter{}
	em.Emit(Event{Kind: EvChainStatus, Status: ChainSuccess})
	em.Emit(Event{Kind: EvATStatus})
	if len(em.Events) != 2 {
		t.Fatalf("SliceEmitter should record 2 events, got %d", len(em.Events))
	}
}

func TestMidModelNilAndTierName(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	s := &OperatorSeat{orch: o, strong: strongAdapter()}
	if s.midModel() != "" {
		t.Fatalf("nil mid should yield empty model id, got %q", s.midModel())
	}
	// With no mid configured, an unknown adapter falls back to "mid".
	if s.tierName(decideAdapter{id: "whatever"}) != "mid" {
		t.Fatal("unknown adapter should label as mid")
	}
}

func TestCallPathSpellings(t *testing.T) {
	if callPath(tool.Call{Params: map[string]any{"path": "p"}}) != "p" {
		t.Fatal("path spelling")
	}
	if callPath(tool.Call{Params: map[string]any{"file_path": "fp"}}) != "fp" {
		t.Fatal("file_path spelling")
	}
	if callPath(tool.Call{Params: map[string]any{"other": "x"}}) != "" {
		t.Fatal("no path param → empty")
	}
}
