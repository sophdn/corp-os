package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// scriptStep is one programmed model turn: either an error (a fault) or a response.
type scriptStep struct {
	resp model.Response
	err  error
}

// scriptAdapter returns programmed (response, error) pairs across successive
// Complete calls; once its script is exhausted it ends the turn with a plain
// answer. It drives the fault-recovery scenarios deterministically.
type scriptAdapter struct {
	id    string
	steps []scriptStep
	calls int
}

func (s *scriptAdapter) Model() string   { return s.id }
func (s *scriptAdapter) Available() bool { return true }
func (s *scriptAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	i := s.calls
	s.calls++
	if i < len(s.steps) {
		if s.steps[i].err != nil {
			return model.Response{}, s.steps[i].err
		}
		return s.steps[i].resp, nil
	}
	return model.Response{Model: s.id, Text: "done", StopReason: model.StopEndTurn}, nil
}

func answer(id string) model.Response {
	return model.Response{Model: id, Text: "done", StopReason: model.StopEndTurn}
}

// overflowErr / malformedErr / timeoutErr mirror the wrapped sentinels the
// adapters produce, so ClassifyFault routes them through the recovery paths.
var (
	overflowErr  = fmt.Errorf("ctx too big: %w", model.ErrContextOverflow)
	malformedErr = fmt.Errorf("bad json: %w", model.ErrMalformedToolCall)
	timeoutErr   = fmt.Errorf("slow: %w", context.DeadlineExceeded)
	rateLimitErr = fmt.Errorf("429: %w", model.ErrRateLimit)
)

// shrinkBackoff makes rate-limit backoff waits negligible for the duration of a
// test, returning a restore func. Without it the bounded backoff would add real
// wall-clock time to the suite.
func shrinkBackoff(t *testing.T) {
	ob, om := rateLimitBaseBackoff, rateLimitMaxBackoff
	rateLimitBaseBackoff, rateLimitMaxBackoff = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { rateLimitBaseBackoff, rateLimitMaxBackoff = ob, om })
}

// toolThenDone alternates: a single tool call, then (next call) a "done" with no
// tool calls — modeling a worker that makes one edit per revise cycle before
// claiming completion. Used to exercise the per-cycle tool-round budget.
type toolThenDone struct{ calls int }

func (a *toolThenDone) Model() string   { return "ttd" }
func (a *toolThenDone) Available() bool { return true }
func (a *toolThenDone) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	a.calls++
	if a.calls%2 == 1 {
		return model.Response{Model: "ttd", ToolCalls: []tool.Call{{ID: "1", Surface: "fs", Action: "write"}}}, nil
	}
	return model.Response{Model: "ttd", Text: "done", StopReason: model.StopEndTurn}, nil
}

// TestRecoverOverflowEscalatesWhenNoCompaction: with no compactor, a floor model
// hitting a context overflow lifts to the strong rung and converges there instead
// of aborting the run (bugs agent-loop-no-recovery + escalation-ignores-faults).
func TestRecoverOverflowEscalatesWhenNoCompaction(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: overflowErr}}}
	strong := &scriptAdapter{id: "haiku", steps: []scriptStep{{resp: answer("haiku")}}}
	loop := New(router.New(floor, strong), &fakeProvider{}, nil)

	res, err := loop.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("overflow must be recovered, not aborted: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want the strong rung's answer", res.Text)
	}
	if strong.calls == 0 {
		t.Error("escalation never reached the strong rung after an overflow")
	}
}

// TestRecoverOverflowCompactsAndRetries: with a compactor and an oversized
// transcript, a context overflow is absorbed locally — the loop compacts and
// retries on the SAME (floor) rung, never paying the strong rung (local-tier-first).
func TestRecoverOverflowCompactsAndRetries(t *testing.T) {
	// Seed a large prior history so forceCompact has turn-groups to evict; size it
	// into the band (0.75*budget, budget] so the turn-boundary pass does not fire but
	// the tighter overflow pass does.
	var hist []model.ChatMessage
	for i := 0; i < 20; i++ {
		hist = append(hist, usr(fmt.Sprintf("u%d %s", i, strings.Repeat("x", 800))), asst("a"))
	}
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: overflowErr}}}
	sum := &recSummarizer{out: "[compacted]"}
	loop := New(single(floor), &fakeProvider{}, nil,
		WithSystemPrompt("SYS"),
		WithResumed(hist, 20),
		WithCompaction(5000, 2, sum),
	)

	res, err := loop.Run(context.Background(), "next")
	if err != nil {
		t.Fatalf("overflow with a compactor must recover locally: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want the recovered floor answer", res.Text)
	}
	// forceCompact injected a rolling-summary marker (it does not surface on
	// Result.Compaction, which only carries the turn-boundary pass).
	marker := false
	for _, m := range loop.Transcript() {
		if strings.HasPrefix(m.Content, compactionMarker) {
			marker = true
		}
	}
	if !marker {
		t.Error("overflow recovery did not compact the transcript")
	}
}

// TestRecoverMalformedRePromptsThenAnswers: a malformed tool call triggers a
// bounded corrective re-prompt and the next turn converges, no abort.
func TestRecoverMalformedRePromptsThenAnswers(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: malformedErr}}}
	loop := New(single(floor), &fakeProvider{}, nil)

	res, err := loop.Run(context.Background(), "call a tool")
	if err != nil {
		t.Fatalf("malformed tool call must be recovered: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want the recovered answer", res.Text)
	}
	corrective := false
	for _, m := range loop.Transcript() {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "could not be parsed") {
			corrective = true
		}
	}
	if !corrective {
		t.Error("no corrective re-prompt was injected for the malformed tool call")
	}
}

// TestRecoverMalformedEscalatesAfterBudget: a model that keeps emitting malformed
// tool calls past the re-prompt budget is lifted to the strong rung.
func TestRecoverMalformedEscalatesAfterBudget(t *testing.T) {
	var steps []scriptStep
	for i := 0; i < maxMalformedRecoveries+1; i++ {
		steps = append(steps, scriptStep{err: malformedErr})
	}
	floor := &scriptAdapter{id: "qwen", steps: steps}
	strong := &scriptAdapter{id: "haiku", steps: []scriptStep{{resp: answer("haiku")}}}
	loop := New(router.New(floor, strong), &fakeProvider{}, nil, WithMaxRounds(12))

	res, err := loop.Run(context.Background(), "call a tool")
	if err != nil {
		t.Fatalf("persistent malformed calls should escalate, not abort: %v", err)
	}
	if res.Text != "done" || strong.calls == 0 {
		t.Errorf("expected convergence on the strong rung; strong.calls=%d text=%q", strong.calls, res.Text)
	}
}

// TestRecoverTimeoutGracefulWhenDeadlineSpent: when the per-turn context is already
// done, a timeout ends the turn gracefully (no error, ModelFault set) rather than
// aborting the run.
func TestRecoverTimeoutGracefulWhenDeadlineSpent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the turn deadline is spent before the first call returns
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: timeoutErr}}}
	loop := New(single(floor), &fakeProvider{}, nil)

	res, err := loop.Run(ctx, "slow task")
	if err != nil {
		t.Fatalf("a spent deadline should end the turn gracefully, not error: %v", err)
	}
	if res.ModelFault != string(model.FaultTimeout) {
		t.Errorf("ModelFault = %q, want %q", res.ModelFault, model.FaultTimeout)
	}
}

// TestRecoverTimeoutEscalatesWhileBudgetRemains: a timeout with turn budget still
// available and a stronger rung lifts to it (a faster/stronger model).
func TestRecoverTimeoutEscalatesWhileBudgetRemains(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: timeoutErr}}}
	strong := &scriptAdapter{id: "haiku", steps: []scriptStep{{resp: answer("haiku")}}}
	loop := New(router.New(floor, strong), &fakeProvider{}, nil)

	res, err := loop.Run(context.Background(), "slow task")
	if err != nil {
		t.Fatalf("timeout with budget should escalate, not abort: %v", err)
	}
	if res.Text != "done" || strong.calls == 0 {
		t.Errorf("expected convergence on the strong rung; strong.calls=%d text=%q", strong.calls, res.Text)
	}
}

// TestUnclassifiedModelErrorStillAborts: a non-fault model error preserves the
// historical fatal behavior (no silent swallowing).
func TestUnclassifiedModelErrorStillAborts(t *testing.T) {
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: fmt.Errorf("disk on fire")}}}
	if _, err := New(single(floor), &fakeProvider{}, nil).Run(context.Background(), "x"); err == nil {
		t.Error("an unclassified model error should still abort the turn")
	}
}

// TestForceCompactNoCompactorIsNoop: forceCompact is a no-op without a compactor.
func TestForceCompactNoCompactorIsNoop(t *testing.T) {
	loop := New(single(&scriptAdapter{id: "q"}), &fakeProvider{}, nil)
	if ev := loop.forceCompact(context.Background(), 1); ev != nil {
		t.Errorf("forceCompact with no compactor = %+v, want nil", ev)
	}
}

// TestRecoverRateLimitBacksOffThenRetries: a single 429 on the only rung is
// absorbed by a bounded backoff, then the retry converges — no abort.
func TestRecoverRateLimitBacksOffThenRetries(t *testing.T) {
	shrinkBackoff(t)
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: rateLimitErr}}}
	res, err := New(single(floor), &fakeProvider{}, nil).Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("a transient rate limit must be recovered, not aborted: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want the recovered answer", res.Text)
	}
}

// TestRecoverRateLimitDeEscalatesToFloor: a paid rung (reached via an initial
// overflow-escalate) that keeps rate-limiting past the backoff budget de-escalates
// to the free floor and converges there instead of aborting.
func TestRecoverRateLimitDeEscalatesToFloor(t *testing.T) {
	shrinkBackoff(t)
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: overflowErr}}} // escalate to strong first
	var rl []scriptStep
	for i := 0; i < maxRateLimitRecoveries+1; i++ {
		rl = append(rl, scriptStep{err: rateLimitErr})
	}
	strong := &scriptAdapter{id: "haiku", steps: rl}
	res, err := New(router.New(floor, strong), &fakeProvider{}, nil).Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("a rate-limited strong rung should de-escalate, not abort: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want the floor's recovered answer after de-escalation", res.Text)
	}
}

// TestRecoverRateLimitGracefulAtFloorWhenExhausted: a 429 on the floor itself, past
// the backoff budget with no lower rung to drop to, ends the turn gracefully
// (ModelFault set) rather than killing the run.
func TestRecoverRateLimitGracefulAtFloorWhenExhausted(t *testing.T) {
	shrinkBackoff(t)
	var steps []scriptStep
	for i := 0; i < maxRateLimitRecoveries+1; i++ {
		steps = append(steps, scriptStep{err: rateLimitErr})
	}
	floor := &scriptAdapter{id: "qwen", steps: steps}
	res, err := New(single(floor), &fakeProvider{}, nil).Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("an exhausted floor rate limit should end gracefully, not error: %v", err)
	}
	if res.ModelFault != string(model.FaultRateLimit) {
		t.Errorf("ModelFault = %q, want %q", res.ModelFault, model.FaultRateLimit)
	}
}

// TestRecoverRateLimitGracefulWhenDeadlineSpentDuringBackoff: if the per-turn
// deadline expires while waiting out the backoff, the turn ends gracefully.
func TestRecoverRateLimitGracefulWhenDeadlineSpentDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the deadline is spent before the backoff wait can elapse
	floor := &scriptAdapter{id: "qwen", steps: []scriptStep{{err: rateLimitErr}}}
	res, err := New(single(floor), &fakeProvider{}, nil).Run(ctx, "x")
	if err != nil {
		t.Fatalf("a spent deadline during backoff should end gracefully: %v", err)
	}
	if res.ModelFault != string(model.FaultRateLimit) {
		t.Errorf("ModelFault = %q, want %q", res.ModelFault, model.FaultRateLimit)
	}
}

// TestBackoffDurationGrowsAndCaps: the backoff doubles per attempt and saturates at
// the cap.
func TestBackoffDurationGrowsAndCaps(t *testing.T) {
	if backoffDuration(1) != rateLimitBaseBackoff {
		t.Errorf("attempt 1 = %v, want base %v", backoffDuration(1), rateLimitBaseBackoff)
	}
	if backoffDuration(2) != 2*rateLimitBaseBackoff {
		t.Errorf("attempt 2 = %v, want 2×base", backoffDuration(2))
	}
	if backoffDuration(100) != rateLimitMaxBackoff {
		t.Errorf("attempt 100 = %v, want cap %v", backoffDuration(100), rateLimitMaxBackoff)
	}
}

// TestVerifyReviseGetsFreshRoundBudgetPerCycle: with maxRounds=2, each cycle spends
// exactly two rounds (one tool call + the claim-done). The per-cycle budget lets
// three failing-then-passing cycles converge; a turn-wide budget would trip the
// guard during the first revise (bug max-tool-rounds-cap-...-starves-verify-revise).
func TestVerifyReviseGetsFreshRoundBudgetPerCycle(t *testing.T) {
	checks := 0
	g := &VerifyGate{Command: []string{"go", "test"}, MaxRounds: 5, run: func(context.Context, []string, string, time.Duration) (int, string) {
		checks++
		if checks < 3 {
			return 1, "fail"
		}
		return 0, "ok"
	}}
	adapter := &toolThenDone{}
	l := New(router.New(adapter, adapter), &fakeProvider{}, nil, WithVerify(g), WithMaxRounds(2))
	res, err := l.Run(context.Background(), "implement it")
	if err != nil {
		t.Fatalf("the per-cycle round budget should let the gate loop converge: %v", err)
	}
	if res.VerifyFailed {
		t.Fatal("should have passed on the third verify cycle")
	}
	if checks != 3 {
		t.Fatalf("verify checks = %d, want 3", checks)
	}
}
