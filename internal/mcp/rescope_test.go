package mcp

import (
	"context"
	"sync"
	"testing"

	"corpos/internal/tool"
)

func TestScope_Union(t *testing.T) {
	t.Parallel()
	a := Scope{"fs": {"read", "grep"}, "measure": {"classify"}}
	b := Scope{"fs": {"read", "write", "edit"}, "sys": {"exec"}}
	u := a.Union(b)

	// fs is the de-duplicated union of both action lists.
	for _, act := range []string{"read", "grep", "write", "edit"} {
		if !u.Allows("fs", act) {
			t.Errorf("union should grant fs.%s", act)
		}
	}
	// surfaces present in only one side survive.
	if !u.Allows("measure", "classify") || !u.Allows("sys", "exec") {
		t.Error("union should keep one-sided surfaces")
	}
	// inputs are not mutated.
	if len(a["fs"]) != 2 || len(b["fs"]) != 3 {
		t.Errorf("Union mutated an input: a=%v b=%v", a, b)
	}
}

func TestScope_Union_WholeSurfaceWins(t *testing.T) {
	t.Parallel()
	narrow := Scope{"work": {"task_read"}}
	whole := Scope{"work": {}} // whole-surface grant
	if u := narrow.Union(whole); !u.Allows("work", "anything") || len(u["work"]) != 0 {
		t.Errorf("whole-surface grant must win in a union, got %v", u["work"])
	}
	if u := whole.Union(narrow); !u.Allows("work", "anything") || len(u["work"]) != 0 {
		t.Errorf("whole-surface grant must win regardless of order, got %v", u["work"])
	}
}

// reviewToFix mirrors the canonical code-review→bug-fix ladder: a read-only scope
// that can widen to gain fs.write + sys.exec.
func reviewScope() Scope { return Scope{"fs": {"read", "grep", "glob", "ls"}} }
func fixRung() RescopeRung {
	return RescopeRung{Name: "bug-fix", Scope: Scope{
		"fs":   {"read", "write", "edit", "grep", "glob", "ls"},
		"sys":  {"exec"},
		"work": {"bug_read"},
	}}
}

func TestScoped_Rescope_WidensAndRedispatches(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{label: "inner"}
	var gotFrom, gotTo, gotSurface, gotAction string
	var n int
	scoped := NewScoped(inner, reviewScope(),
		WithRescopeLadder("code-review", []RescopeRung{fixRung()}, 1),
		WithRescopeLog(func(from, to, surface, action string) {
			n++
			gotFrom, gotTo, gotSurface, gotAction = from, to, surface, action
		}))

	// fs.write is denied under the review scope, but the ladder grants it → transparent
	// widen + re-dispatch reaches the inner provider with an OK result.
	res := scoped.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "write"})
	if !res.OK {
		t.Fatalf("rescope should have widened and re-dispatched, got %+v", res)
	}
	if len(inner.calls) != 1 || inner.calls[0].Action != "write" {
		t.Fatalf("inner should have received exactly the re-dispatched write, got %+v", inner.calls)
	}
	if n != 1 || gotFrom != "code-review" || gotTo != "bug-fix" || gotSurface != "fs" || gotAction != "write" {
		t.Fatalf("rescope log = (n=%d %s→%s %s.%s), want one code-review→bug-fix fs.write", n, gotFrom, gotTo, gotSurface, gotAction)
	}
	// After widening, an originally-granted call still works (monotonic — no grant lost).
	if r := scoped.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "read"}); !r.OK {
		t.Fatal("widening must not drop the original fs.read grant")
	}
	// A second distinct widening is over budget → the new surface stays denied.
	if r := scoped.Dispatch(context.Background(), tool.Call{Surface: "knowledge", Action: "vault_read"}); r.OK {
		t.Fatal("knowledge.vault_read is in no rung → must stay denied")
	}
}

func TestScoped_Rescope_NoLadderStillDenies(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{label: "inner"}
	scoped := NewScoped(inner, reviewScope()) // no ladder armed
	res := scoped.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "write"})
	if res.OK || len(inner.calls) != 0 {
		t.Fatalf("with no ladder a denied call must not reach inner, got OK=%v calls=%v", res.OK, inner.calls)
	}
}

func TestScoped_Rescope_BudgetBound(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{label: "inner"}
	// Two rungs but budget 1: only the first widening is allowed.
	rungA := RescopeRung{Name: "a", Scope: Scope{"sys": {"exec"}}}
	rungB := RescopeRung{Name: "b", Scope: Scope{"web": {}}}
	scoped := NewScoped(inner, reviewScope(),
		WithRescopeLadder("start", []RescopeRung{rungA, rungB}, 1))

	if r := scoped.Dispatch(context.Background(), tool.Call{Surface: "sys", Action: "exec"}); !r.OK {
		t.Fatal("first widening (sys.exec) should succeed within budget")
	}
	if r := scoped.Dispatch(context.Background(), tool.Call{Surface: "web", Action: "search"}); r.OK {
		t.Fatal("second widening (web) is over budget → must be denied")
	}
}

func TestScoped_Rescope_RungThatDoesNotGrantIsSkipped(t *testing.T) {
	t.Parallel()
	inner := &recordingProvider{label: "inner"}
	// The only rung grants sys, not the requested knowledge surface.
	scoped := NewScoped(inner, reviewScope(),
		WithRescopeLadder("start", []RescopeRung{{Name: "sysrung", Scope: Scope{"sys": {"exec"}}}}, 1))
	if r := scoped.Dispatch(context.Background(), tool.Call{Surface: "knowledge", Action: "vault_read"}); r.OK {
		t.Fatal("no rung grants knowledge → must stay denied (budget untouched)")
	}
	// Budget was not spent on a non-granting rung: a grantable call still widens.
	if r := scoped.Dispatch(context.Background(), tool.Call{Surface: "sys", Action: "exec"}); !r.OK {
		t.Fatal("a grantable call should still widen after a skipped non-granting probe")
	}
}

// okProvider is a stateless, concurrency-safe inner provider (no slice append to
// race on) for the concurrent rescope test.
type okProvider struct{}

func (okProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: true}
}

// TestScoped_Rescope_Concurrent exercises the mutex under -race: concurrent denied
// calls that all widen the same single-rung ladder must not race or over-spend.
func TestScoped_Rescope_Concurrent(t *testing.T) {
	t.Parallel()
	scoped := NewScoped(okProvider{}, reviewScope(),
		WithRescopeLadder("code-review", []RescopeRung{fixRung()}, 1))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scoped.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "write"})
		}()
	}
	wg.Wait()
	if r := scoped.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "write"}); !r.OK {
		t.Fatal("after concurrent widening, fs.write should be granted")
	}
}
