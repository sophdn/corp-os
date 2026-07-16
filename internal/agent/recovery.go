package agent

import (
	"context"
	"fmt"
	"time"

	"corpos/internal/model"
	"corpos/internal/router"
)

// recoverAction is what the round loop should do after a model-call fault.
type recoverAction int

const (
	// recoverAbort: the fault is unrecoverable; return the (wrapped) error.
	recoverAbort recoverAction = iota
	// recoverRetry: the loop recovered (compacted / re-prompted / escalated) and
	// should retry the round.
	recoverRetry
	// recoverGraceful: end the turn cleanly without aborting the run (the per-turn
	// deadline is spent; no further call can succeed on this context).
	recoverGraceful
)

// Per-fault recovery budgets within a single turn. Each is small and bounded so a
// persistent fault cannot spin the loop: once a class exhausts its budget the loop
// escalates (lift the floor) or fails with a clear, class-specific message rather
// than the historical bare abort.
const (
	maxOverflowRecoveries  = 4 // compaction passes (each tightens the target) before giving up
	maxMalformedRecoveries = 3 // corrective re-prompts before escalating
	maxTimeoutRecoveries   = 2 // shrink/retry passes before escalating
	maxRateLimitRecoveries = 4 // bounded backoff waits before de-escalating to the floor
)

// Rate-limit backoff bounds. Vars (not consts) so tests can shrink the waits;
// production uses the defaults. The backoff is bounded and ctx-capped, so a
// persistent 429 cannot stall the turn past its deadline.
var (
	rateLimitBaseBackoff = 500 * time.Millisecond
	rateLimitMaxBackoff  = 8 * time.Second
)

// faultRecovery tracks how many times each recoverable fault class has fired in
// the current turn, bounding in-loop recovery.
type faultRecovery struct {
	overflow  int
	malformed int
	timeout   int
	rateLimit int
}

// recoverFromFault classifies a model-call error and attempts bounded in-loop
// recovery, returning the action the round loop should take. It is the single
// place the loop's old "abort the whole run on any model error" behavior is
// replaced: overflow → compact-and-retry (then escalate), malformed tool call →
// bounded re-prompt (then escalate), timeout → shrink/retry/escalate while turn
// budget remains (graceful end once it is spent), rate-limit → bounded backoff then
// de-escalate to the free floor. An unclassified error keeps the historical fatal
// behavior.
func (l *Loop) recoverFromFault(ctx context.Context, err error, fr *faultRecovery, turn int) (recoverAction, error) {
	switch model.ClassifyFault(err) {
	case model.FaultContextOverflow:
		return l.recoverOverflow(ctx, err, fr, turn)
	case model.FaultMalformedToolCall:
		return l.recoverMalformed(ctx, err, fr, turn)
	case model.FaultTimeout:
		return l.recoverTimeout(ctx, fr, turn)
	case model.FaultRateLimit:
		return l.recoverRateLimit(ctx, fr, turn)
	default:
		return recoverAbort, fmt.Errorf("model turn: %w", err)
	}
}

// recoverOverflow handles a context-window overflow: shrink the transcript to fit
// (local-tier-first — don't pay a stronger rung when compaction will do), and only
// when there is no compaction headroom left lift the floor to a larger-window
// rung. Repeated overflows tighten the compaction target each pass.
func (l *Loop) recoverOverflow(ctx context.Context, err error, fr *faultRecovery, turn int) (recoverAction, error) {
	fr.overflow++
	if fr.overflow > maxOverflowRecoveries {
		return recoverAbort, fmt.Errorf("model turn: context overflow unrecoverable after %d recovery attempt(s): %w", fr.overflow-1, err)
	}
	if l.forceCompact(ctx, fr.overflow) != nil {
		return recoverRetry, nil // compacted: retry with a smaller prompt
	}
	// Nothing left to compact (no compactor, or only the goal+recency remain). The
	// floor model cannot fit this prompt — lift to a larger-window rung if one exists.
	// Mandatory (bound-bypassing) escalation: a floor that can't hold the prompt must
	// not be bounced back into by a spent usage bound (bug escalation-bound-exhaustion-
	// falls-back-to-overflowing-floor-and-dies-after-fix-landed).
	if l.escalateForOverflow(ctx, turn) {
		return recoverRetry, nil
	}
	return recoverAbort, fmt.Errorf("model turn: context overflow with no compaction headroom and no higher rung to escalate to: %w", err)
}

// recoverMalformed handles a malformed/truncated tool call from the model: a
// bounded corrective re-prompt asking it to re-emit valid JSON, then escalation to
// a stronger rung once local re-prompts are exhausted.
func (l *Loop) recoverMalformed(ctx context.Context, err error, fr *faultRecovery, turn int) (recoverAction, error) {
	fr.malformed++
	if fr.malformed > maxMalformedRecoveries {
		if l.escalateForFault(ctx, model.FaultMalformedToolCall, turn) {
			return recoverRetry, nil
		}
		return recoverAbort, fmt.Errorf("model turn: model kept emitting malformed tool calls after %d re-prompt(s): %w", fr.malformed-1, err)
	}
	// No assistant message was recorded for the failed call (the decode failed
	// before we could append it), so a plain corrective user turn is well-formed.
	l.transcript = append(l.transcript, model.ChatMessage{
		Role: model.RoleUser,
		Content: "Your previous tool call could not be parsed: its function arguments were not valid JSON " +
			"(it was likely truncated). Re-issue the tool call with a single complete, well-formed JSON object " +
			`for the arguments, e.g. {"action":"<action>","params":{...}}. Do not cut it off.`,
	})
	return recoverRetry, nil
}

// recoverTimeout handles a model-call deadline. When the per-turn context deadline
// itself is spent, no further call can succeed on it, so the turn ends gracefully
// (the run is not killed). While turn budget remains, shrink the prompt (a smaller
// context is a faster local call) and retry; once the shrink budget is spent, lift
// to a faster/stronger rung, falling back to a graceful end rather than a hard
// abort.
func (l *Loop) recoverTimeout(ctx context.Context, fr *faultRecovery, turn int) (recoverAction, error) {
	if ctx.Err() != nil {
		// Intentional: a spent per-turn deadline is not a run-fatal error — the turn
		// ends gracefully so a REPL/-resume can continue.
		return recoverGraceful, nil //nolint:nilerr
	}
	fr.timeout++
	if fr.timeout > maxTimeoutRecoveries {
		if l.escalateForFault(ctx, model.FaultTimeout, turn) {
			return recoverRetry, nil
		}
		return recoverGraceful, nil // out of options but turn budget remains; don't hard-abort
	}
	// Try to make the next local call faster by shrinking the prompt; if there is
	// nothing to compact, escalate to a faster rung when one exists, else retry once.
	if l.forceCompact(ctx, fr.timeout) != nil {
		return recoverRetry, nil
	}
	if l.escalateForFault(ctx, model.FaultTimeout, turn) {
		return recoverRetry, nil
	}
	return recoverRetry, nil
}

// forceCompact compacts the transcript to a budget TIGHTER than the configured one
// (and a smaller recency window), creating real headroom after the model rejected a
// prompt the configured budget thought fit — the token estimate underestimates the
// real tokenizer. attempt (1-based) tightens the target geometrically across
// repeated faults in a turn. Returns the event, or nil when there is no compactor
// or nothing left to evict. The calibrated tool-spec overhead persists across the
// rewrite.
func (l *Loop) forceCompact(ctx context.Context, attempt int) *CompactionEvent {
	if l.compactor == nil {
		return nil
	}
	if l.goalAnchor == "" {
		l.goalAnchor = firstUserContent(l.transcript)
	}
	// Target = budget * (3/4)^attempt, floored so we never aim below the un-evictable
	// tool-spec overhead plus a little room (that case is handled by escalation).
	target := l.compactor.budget
	for i := 0; i < attempt; i++ {
		target = target * 3 / 4
	}
	if floor := l.toolSpecTokens + 256; target < floor {
		target = floor
	}
	// Shrink the recency window too on later attempts so a large kept tail cannot
	// keep the transcript over the tightened target.
	recency := l.compactor.recencyTurns - (attempt - 1)
	newT, newSummary, ev, err := l.compactor.compactToBudget(ctx, l.transcript, l.goalAnchor, l.summaryBody, l.toolSpecTokens, l.turns, target, recency)
	if err != nil || ev == nil {
		return nil
	}
	l.transcript = newT
	l.summaryBody = newSummary
	return ev
}

// escalateForFault lifts the router one rung in response to a model-call fault the
// loop could not absorb locally, emitting the EscalationProposed event + local row
// (the same telemetry path the turn-boundary fold uses). Returns true when it
// climbed (the next round will route to the stronger rung), false when already at
// the top rung. This is what makes the escalation ladder rescue model-call faults
// (bug escalation-ladder-ignores-model-call-faults), not just repeated_tool_error.
func (l *Loop) escalateForFault(ctx context.Context, f model.FaultKind, turn int) bool {
	edge := l.router.EscalateForFault(f)
	if edge.Direction != router.EdgeEscalate {
		return false
	}
	l.recordEscalationEdge(ctx, turn, edge)
	return true
}

// escalateForOverflow lifts the router for a MANDATORY floor context-overflow,
// BYPASSING the usage bound — unlike escalateForFault, because a floor that cannot
// physically hold the prompt must not be bounced back into by a spent bound (the
// run-12 death: bound spent → bounce to floor → overflow → EscalateForFault saw cur
// already at top → no climb → die). Returns true when it climbed or unstuck the
// bound, false only when there is genuinely no higher rung to reach.
func (l *Loop) escalateForOverflow(ctx context.Context, turn int) bool {
	edge := l.router.EscalateForOverflow()
	if edge.Direction != router.EdgeEscalate {
		return false
	}
	l.recordEscalationEdge(ctx, turn, edge)
	return true
}

// recoverRateLimit handles a provider throttling (429/529) on the current rung. A
// rate limit is transient and recoverable, so rather than aborting (the historical
// behavior that killed run-3's green attempt) the loop honors the limit with a
// bounded backoff and retries the same rung; once the backoff budget is spent it
// DE-escalates to the free local floor for the rest of the turn (a throttled paid
// rung is pointless to retry, and the floor is free + not org-rate-limited). If it
// is already at the floor, or the per-turn deadline is spent while waiting, the
// turn ends gracefully — a transient limit is never run-fatal. Bug
// model-call-rate-limit-429-not-recoverable-aborts-run.
func (l *Loop) recoverRateLimit(ctx context.Context, fr *faultRecovery, turn int) (recoverAction, error) {
	fr.rateLimit++
	if fr.rateLimit > maxRateLimitRecoveries {
		if l.deEscalateToFloor(ctx, turn) {
			return recoverRetry, nil
		}
		return recoverGraceful, nil // already at the floor; don't hard-abort a transient limit
	}
	if !sleepCtx(ctx, backoffDuration(fr.rateLimit)) {
		return recoverGraceful, nil // per-turn deadline spent during backoff
	}
	return recoverRetry, nil
}

// deEscalateToFloor drops the router to the free local floor (a rate-limited paid
// rung shouldn't be retried this turn), recording the de-escalate edge on the same
// telemetry path as an escalate. Returns true when it dropped, false when already
// at the floor.
func (l *Loop) deEscalateToFloor(ctx context.Context, turn int) bool {
	edge := l.router.DeEscalateToFloor()
	if edge.Direction != router.EdgeDeescalate {
		return false
	}
	l.recordEscalationEdge(ctx, turn, edge)
	return true
}

// backoffDuration returns an exponentially growing, capped wait for the attempt-th
// (1-based) rate-limit retry: base, 2·base, 4·base, … up to rateLimitMaxBackoff.
func backoffDuration(attempt int) time.Duration {
	d := rateLimitBaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= rateLimitMaxBackoff {
			return rateLimitMaxBackoff
		}
	}
	return d
}

// sleepCtx waits for d or until ctx is done, whichever comes first. It returns
// true if the full wait elapsed, false if ctx was cancelled/expired meanwhile.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
