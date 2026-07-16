package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// vProvider is a no-op tool provider (the verify tests drive final-answer turns
// with no tool calls).
type vProvider struct{}

func (vProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: true}
}

func TestVerifyGateCheck(t *testing.T) {
	pass := &VerifyGate{Command: []string{"x"}, run: func(context.Context, []string, string, time.Duration) (int, string) { return 0, "ok" }}
	if ok, out := pass.check(context.Background()); !ok || out != "ok" {
		t.Fatalf("pass: ok=%v out=%q", ok, out)
	}
	fail := &VerifyGate{Command: []string{"x"}, run: func(context.Context, []string, string, time.Duration) (int, string) { return 1, "boom" }}
	if ok, out := fail.check(context.Background()); ok || out != "boom" {
		t.Fatalf("fail: ok=%v out=%q", ok, out)
	}
}

func TestMaxRoundsOrDefault(t *testing.T) {
	if (&VerifyGate{}).maxRoundsOrDefault() != defaultVerifyMaxRounds {
		t.Fatal("zero → default")
	}
	if (&VerifyGate{MaxRounds: 5}).maxRoundsOrDefault() != 5 {
		t.Fatal("explicit honored")
	}
}

func TestTailString(t *testing.T) {
	if tailString("abc", 10) != "abc" {
		t.Fatal("short passthrough")
	}
	big := strings.Repeat("x", verifyOutputTail+50)
	got := tailString(big, verifyOutputTail)
	if !strings.HasPrefix(got, "…truncated…\n") || len(got) != len("…truncated…\n")+verifyOutputTail {
		t.Fatalf("truncation wrong: len=%d", len(got))
	}
}

func TestCleanGitEnv(t *testing.T) {
	t.Setenv("GIT_DIR", "/x/.git")
	t.Setenv("GIT_WORK_TREE", "/x")
	t.Setenv("CORPOS_KEEP_ME", "1")
	env := cleanGitEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") {
			t.Fatalf("git var leaked: %q", kv)
		}
	}
	found := false
	for _, kv := range env {
		if kv == "CORPOS_KEEP_ME=1" {
			found = true
		}
	}
	if !found {
		t.Fatal("non-git env must be preserved")
	}
}

func TestExecVerifyRealExec(t *testing.T) {
	ctx := context.Background()
	if c, _ := execVerify(ctx, []string{"true"}, t.TempDir(), 0); c != 0 {
		t.Fatalf("true → %d", c)
	}
	if c, _ := execVerify(ctx, []string{"false"}, t.TempDir(), 0); c != 1 {
		t.Fatalf("false → %d", c)
	}
	if c, _ := execVerify(ctx, nil, t.TempDir(), 0); c != 127 {
		t.Fatalf("empty → %d", c)
	}
	if c, _ := execVerify(ctx, []string{"corpos-no-such-bin-zzz"}, t.TempDir(), 0); c != 127 {
		t.Fatalf("missing → %d", c)
	}
	if c, _ := execVerify(ctx, []string{"sleep", "5"}, t.TempDir(), 30*time.Millisecond); c != 124 {
		t.Fatalf("timeout → %d", c)
	}
	if c, out := execVerify(ctx, []string{"sh", "-c", "echo hi"}, t.TempDir(), 0); c != 0 || !strings.Contains(out, "hi") {
		t.Fatalf("capture: %d %q", c, out)
	}
}

// verifyLoop builds a loop whose model always claims done (echo, no tool calls)
// and whose verify gate is driven by the supplied run func.
func verifyLoop(maxRounds int, run func(int) (int, string)) (*Loop, *int) {
	calls := 0
	adapter := model.NewEcho("m", model.Response{Text: "done", StopReason: model.StopEndTurn})
	g := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: maxRounds, run: func(context.Context, []string, string, time.Duration) (int, string) {
		calls++
		return run(calls)
	}}
	l := New(router.New(adapter, adapter), vProvider{}, nil, WithVerify(g))
	return l, &calls
}

func TestLoopVerifyPassesFirstTry(t *testing.T) {
	l, calls := verifyLoop(3, func(int) (int, string) { return 0, "ok" })
	res, err := l.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.VerifyFailed || *calls != 1 || res.Text != "done" {
		t.Fatalf("want clean pass once: failed=%v calls=%d text=%q", res.VerifyFailed, *calls, res.Text)
	}
}

func TestLoopVerifyFailsThenPasses(t *testing.T) {
	// Fail the first check, pass the second → the agent gets one revise turn.
	l, calls := verifyLoop(3, func(n int) (int, string) {
		if n == 1 {
			return 1, "FAIL: TestX"
		}
		return 0, "ok"
	})
	res, err := l.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.VerifyFailed || *calls != 2 {
		t.Fatalf("want pass after one revise: failed=%v calls=%d", res.VerifyFailed, *calls)
	}
	// The failure was fed back into the transcript as a revise prompt.
	found := false
	for _, m := range l.transcript {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "Automated verification failed") {
			found = true
		}
	}
	if !found {
		t.Fatal("verify failure should be fed back as a user turn")
	}
}

func TestLoopVerifyExhaustsBudget(t *testing.T) {
	l, calls := verifyLoop(2, func(int) (int, string) { return 1, "always fails" })
	res, err := l.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.VerifyFailed {
		t.Fatal("want VerifyFailed after exhausting the revise budget")
	}
	if *calls != 3 { // checks at rounds: fail(1<2→revise), fail(2<2? no→exhaust)… = 2 revises + final = 3 checks
		t.Fatalf("verify calls = %d, want 3", *calls)
	}
}

func TestWithVerifyIgnoresEmpty(t *testing.T) {
	adapter := model.NewEcho("m", model.Response{Text: "x", StopReason: model.StopEndTurn})
	l := New(router.New(adapter, adapter), vProvider{}, nil, WithVerify(nil), WithVerify(&VerifyGate{}))
	if l.verify != nil {
		t.Fatal("nil gate and empty-command gate must be ignored")
	}
}

// countingDone always claims done (no tool calls) and counts its Complete calls,
// so a per-rung call count reveals which rung the loop used.
type countingDone struct {
	id    string
	calls int
}

func (a *countingDone) Model() string   { return a.id }
func (a *countingDone) Available() bool { return true }
func (a *countingDone) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	a.calls++
	return model.Response{Model: a.id, Text: "done", StopReason: model.StopEndTurn}, nil
}

// TestVerifyStuckFloorEscalatesToStrongRung is the regression for
// escalation-does-not-fire-on-persistently-stuck-floor: a verify gate that stays
// RED across repeated revise cycles must lift the ladder to the strong rung before
// the revise budget is spent, instead of the floor burning the whole budget.
func TestVerifyStuckFloorEscalatesToStrongRung(t *testing.T) {
	floor := &countingDone{id: "qwen"}
	strong := &countingDone{id: "haiku"}
	redGate := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 6,
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 1, "FAIL: still red" }}
	l := New(router.New(floor, strong), vProvider{}, nil, WithVerify(redGate), WithVerifyStuckEscalation(2))

	res, err := l.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.VerifyFailed {
		t.Fatal("an always-RED gate must exhaust the revise budget (VerifyFailed)")
	}
	if floor.calls == 0 {
		t.Fatal("local-tier-first: the floor should get the first revise attempts")
	}
	if strong.calls == 0 {
		t.Fatal("regression: a persistently stuck floor never escalated to the strong rung")
	}
}

// TestVerifyStuckEscalationDisabled confirms the knob: n<=0 keeps the prior
// behavior (floor only, strong rung untouched). Using a negative value also
// exercises the option's clamp.
func TestVerifyStuckEscalationDisabled(t *testing.T) {
	floor := &countingDone{id: "qwen"}
	strong := &countingDone{id: "haiku"}
	redGate := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 3,
		run: func(context.Context, []string, string, time.Duration) (int, string) { return 1, "FAIL" }}
	l := New(router.New(floor, strong), vProvider{}, nil, WithVerify(redGate), WithVerifyStuckEscalation(-1))

	res, err := l.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.VerifyFailed {
		t.Fatal("always-RED gate should still exhaust the budget")
	}
	if strong.calls != 0 {
		t.Fatalf("disabled stuck-escalation must NOT touch the strong rung, got %d strong calls", strong.calls)
	}
}
