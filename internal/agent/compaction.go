package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"corpos/internal/model"
)

// compactionMarker tags the single rolling-summary system message a Compactor
// injects. It lets a later compaction recognise and replace that message (so
// summaries never stack) and distinguishes it from the seeded system prompt(s),
// which are pinned untouched. See docs/CONTEXT_COMPACTION.md.
const compactionMarker = "[corpos:compacted-context]"

// goalReminderMarker tags the single terse goal-reminder message the loop
// re-surfaces near the transcript tail under long loops. It lets a refresh drop
// the stale reminder before appending a fresh one (so reminders never stack) and
// lets a test/observer recognise it.
const goalReminderMarker = "[corpos:goal-reminder]"

// refreshGoalReminder drops any existing goal-reminder message and appends a fresh
// one at the tail, returning the new transcript. It is a no-op (returns the input)
// when there is no goal anchor. The reminder is a RoleUser message so it stays at
// the tail across every adapter (a RoleSystem message is hoisted to the top-level
// system block by the Anthropic adapter, which would defeat tail salience). It is
// only ever appended after a complete tool-round, so it never separates a tool_use
// from its tool_result.
func refreshGoalReminder(transcript []model.ChatMessage, goalAnchor string) []model.ChatMessage {
	if strings.TrimSpace(goalAnchor) == "" {
		return transcript
	}
	out := make([]model.ChatMessage, 0, len(transcript)+1)
	for _, m := range transcript {
		if strings.HasPrefix(m.Content, goalReminderMarker) {
			continue // drop the stale reminder
		}
		out = append(out, m)
	}
	return append(out, model.ChatMessage{
		Role:    model.RoleUser,
		Content: goalReminderMarker + " Reminder — stay on the ACTIVE GOAL and do not substitute a different target:\n" + goalAnchor,
	})
}

// spawnForceMarker tags the single terse spawn-forcing reminder the loop injects
// near the transcript tail when a spawn-capable (orchestrate) agent has burned its
// pre-spawn read budget without ever calling agent.spawn (bug 1072). It lets a
// refresh drop the stale reminder before appending a fresh one (so reminders never
// stack), lets the post-spawn clear recognise and remove it, and lets a
// test/observer recognise it.
const spawnForceMarker = "[corpos:spawn-now]"

// refreshSpawnForce drops any existing spawn-forcing reminder and appends a fresh
// one at the tail, returning the new transcript. roundsRead is the number of
// read-only rounds spent without a spawn, surfaced so the nudge is concrete. Like
// refreshGoalReminder it is a RoleUser message (so it stays at the tail across every
// adapter) and is only ever appended after a complete tool-round, so it never
// separates a tool_use from its tool_result.
func refreshSpawnForce(transcript []model.ChatMessage, roundsRead int) []model.ChatMessage {
	out := clearSpawnForce(transcript)
	return append(out, model.ChatMessage{
		Role: model.RoleUser,
		Content: fmt.Sprintf("%s You have spent %d tool round(s) reading/grepping and have NOT called agent.spawn. "+
			"Investigation is the WORKER's job, not yours — your own tools cannot edit anything. STOP investigating and "+
			"call agent.spawn NOW: spawn a scoped worker whose duty restates the goal and names the files/symbols you have "+
			"already found. For a code/bug/test duty pass profile=\"atomic-coding-chain\".", spawnForceMarker, roundsRead),
	})
}

// clearSpawnForce removes any spawn-forcing reminder from the transcript, returning
// the filtered slice. Called once the orchestrator actually spawns, so a post-spawn
// synthesis turn is not nagged by a stale nudge.
func clearSpawnForce(transcript []model.ChatMessage) []model.ChatMessage {
	out := make([]model.ChatMessage, 0, len(transcript))
	for _, m := range transcript {
		if strings.HasPrefix(m.Content, spawnForceMarker) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// evictedToolResultMarker tags a tool-result message whose body was elided by the
// within-turn bound (evictToolResults). It marks the result as already shrunk so a
// later pass skips it, and tells the model the data was dropped (and can be
// re-fetched) rather than silently truncated.
const evictedToolResultMarker = "[corpos:evicted-tool-result]"

// minKeepToolResults is the SOFT floor on how many of the most-recent tool results
// the within-turn bound keeps verbatim, so eviction never strips the immediate
// working context out from under the model when there is headroom to spare it.
const minKeepToolResults = 3

// hardKeepToolResults is the HARD floor eviction will never go below — the single
// most-recent tool result (the one the model is actively reasoning over). When even
// the soft `keep` window's results overflow the budget (a narrow floor window can
// physically hold only ~one whole-file read at a time — bug 1066's 3-whole-file-read
// coding carry), the soft floor is abandoned down to this hard floor: a stale read
// the window cannot hold is elided (the model re-fetches if needed) rather than
// wedging the carry into an overflow→Opus escalation. Keeping 0 would orphan the
// just-returned result, so the hard floor is 1.
const hardKeepToolResults = 1

// evictToolResults bounds a transcript DURING a turn (where the turn-group
// compactor cannot help — a single turn is one group with no interior RoleUser
// boundary to cut on) by eliding the BODIES of the oldest tool-result messages,
// which are the bulk of multi-round accumulation (run-6d: 178 results; bug 1066's
// 3-whole-file-read coding carry). Structure is preserved — a result keeps its
// role/id/name, only its content is replaced with a terse marker — so no tool_use is
// orphaned from its tool_result (the Anthropic-400 constraint). The goal anchor and
// assistant reasoning are RoleUser/RoleAssistant messages and are never touched.
//
// It evicts the oldest results in TWO passes against the same ratio-corrected size
// signal the loop's currentSize uses (so eviction targets the budget the floor-fit
// guard actually checks, not an un-ratio'd under-estimate that lets a code-dense
// transcript pass eviction yet overflow the window): first respecting the soft
// `keep` window, then — only if STILL over budget — abandoning the soft floor and
// continuing down to `hardMin`. This guarantees a single capped read + overhead fits
// the floor budget instead of overflowing → escalating to Opus. Returns the number
// of results elided.
func evictToolResults(transcript []model.ChatMessage, budget, overhead, keep, hardMin int, ratio float64) int {
	if keep < 0 {
		keep = 0
	}
	if hardMin < 0 {
		hardMin = 0
	}
	if hardMin > keep {
		hardMin = keep
	}
	if ratio < 1.0 {
		ratio = 1.0
	}
	size := func() int { return overhead + int(float64(estimateTokens(transcript))*ratio) }
	if size() <= budget {
		return 0
	}
	var idxs []int // evictable tool-result positions, oldest first
	for i := range transcript {
		if transcript[i].Role == model.RoleTool && !strings.HasPrefix(transcript[i].Content, evictedToolResultMarker) {
			idxs = append(idxs, i)
		}
	}
	evicted := 0
	// Pass 1 respects the soft keep window; pass 2 (only when still over budget)
	// abandons it down to the hard floor. Both walk oldest-first and stop the moment
	// the size is back under budget, so the hard floor is reached only under real
	// window pressure (a coding carry whose kept reads alone overflow the window).
	for _, lo := range []int{keep, hardMin} {
		evictable := len(idxs) - lo
		for ; evicted < evictable && evicted < len(idxs); evicted++ {
			if size() <= budget {
				return evicted
			}
			i := idxs[evicted]
			name := transcript[i].Name
			if name == "" {
				name = "tool"
			}
			orig := len(transcript[i].Content)
			transcript[i].Content = fmt.Sprintf("%s %s result elided (%d chars) to stay within the context budget; re-run the tool (prefer fs.read with an outline/symbol view or a line range) if you still need this data.", evictedToolResultMarker, name, orig)
		}
		if size() <= budget {
			break
		}
	}
	return evicted
}

// Compactor bounds the agent transcript against a token budget via the hybrid
// strategy in docs/CONTEXT_COMPACTION.md: it pins the head system prompt(s) and
// the active-goal anchor (the first user prompt, kept verbatim), keeps the last
// recencyTurns turn-groups verbatim, and folds everything evicted in between into
// ONE rolling summary system message produced by the summarizer adapter. Eviction
// cuts only on turn boundaries (a RoleUser starts each group), so a tool_result is
// never separated from its tool_use (the Anthropic-400 constraint ResumeState
// documents).
type Compactor struct {
	budget       int           // max context tokens before a compaction fires
	recencyTurns int           // turn-groups kept verbatim at the tail (K)
	summarizer   model.Adapter // produces the rolling summary (a cheap tier)
	// tokenRatio corrects the len/4 transcript estimate for content the real
	// tokenizer counts denser than 4 chars/token (code, especially). It is
	// calibrated per-session from the provider's measured input count (see the
	// loop's observeTokenRatio) and starts at 1.0 (no correction). Applying it in
	// currentSize keeps the budget signal honest without inflating the FIXED
	// tool-spec overhead — the two were conflated before (bug
	// tool-spec-overhead-calibration-conflates-…), which let a code-heavy turn's
	// under-estimate masquerade as unbounded "overhead".
	tokenRatio float64
}

// maxTokenRatio caps the per-session transcript-estimate correction so one
// anomalous call cannot blow the budget signal up without bound.
const maxTokenRatio = 2.5

// observeTokenRatio folds a freshly-measured real/estimated transcript ratio into
// the compactor's running correction. It keeps the conservative MAX (compact a
// touch early rather than overflow the provider) and clamps to [1.0, maxTokenRatio].
func (c *Compactor) observeTokenRatio(r float64) {
	if r > maxTokenRatio {
		r = maxTokenRatio
	}
	if r > c.tokenRatio {
		c.tokenRatio = r
	}
}

// CompactionEvent records one compaction event for telemetry (surfaced on
// Result.Compaction).
type CompactionEvent struct {
	// TurnIndex is the turn at whose boundary the compaction fired.
	TurnIndex int
	// TokensBefore / TokensAfter bracket the TOTAL context size (the fixed tool-spec
	// Overhead plus the transcript estimate) around the event. Both include Overhead,
	// so they are directly comparable to Budget.
	TokensBefore int
	TokensAfter  int
	// GroupsEvicted is the number of turn-groups folded into the rolling summary.
	GroupsEvicted int
	// Budget is the configured -max-context-tokens this event was measured against.
	Budget int
	// Overhead is the fixed tool-spec token cost (offered specs are not part of the
	// transcript and cannot be evicted). TokensAfter > Budget despite a compaction
	// means Overhead alone is crowding the window — raise the budget or narrow the
	// tool surface.
	Overhead int
}

// OverBudget reports whether the transcript was still over budget after this
// compaction — the saturation signal (the un-compactable tool-spec overhead, plus
// the kept recency window, exceeds the budget).
func (e *CompactionEvent) OverBudget() bool { return e.TokensAfter > e.Budget }

// estimateTokens approximates the token size of a transcript without a tokenizer
// (corpos is CGo-free and ships none): ~4 chars/token for content plus a small
// per-message and per-tool-call overhead. It sizes the transcript portion of the
// context; the fixed tool-spec overhead is added separately (see currentSize).
func estimateTokens(msgs []model.ChatMessage) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)/4 + 4
		for _, c := range m.ToolCalls {
			total += (len(c.Surface)+len(c.Action))/4 + paramTokens(c.Params) + 8
		}
	}
	return total
}

// paramTokens estimates the token size of a tool call's params (its JSON length
// over 4); un-marshalable params fall back to 0 rather than guessing.
func paramTokens(params map[string]any) int {
	if len(params) == 0 {
		return 0
	}
	b, err := json.Marshal(params)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// currentSize is the live TOTAL context size: the fixed tool-spec overhead (the
// offered specs, which the model sees but which are not part of the transcript and
// cannot be evicted) plus the transcript estimate. Computing both in the same
// units — and including overhead consistently before AND after a rewrite — is what
// makes the budget comparison and the before/after notice honest. overhead = 0
// before the loop has calibrated it (no measured call yet) yields a transcript-only
// size.
func (c *Compactor) currentSize(msgs []model.ChatMessage, overhead int) int {
	r := c.tokenRatio
	if r < 1.0 {
		r = 1.0 // uncalibrated (or under-1) ratio is a no-op, never a shrink
	}
	return overhead + int(float64(estimateTokens(msgs))*r)
}

// compact rebuilds the transcript under budget when the live size exceeds it.
// It returns the new transcript, the new rolling-summary body (to carry forward
// so the next event subsumes rather than stacks it), and a non-nil event when a
// compaction actually happened. When nothing is compacted — under budget, or too
// few turn-groups to evict while keeping the recency window — it returns the
// inputs unchanged with a nil event.
func (c *Compactor) compact(ctx context.Context, transcript []model.ChatMessage, goalAnchor, prevSummary string, overhead, turnIndex int) ([]model.ChatMessage, string, *CompactionEvent, error) {
	return c.compactToBudget(ctx, transcript, goalAnchor, prevSummary, overhead, turnIndex, c.budget, c.recencyTurns)
}

// compactToBudget is compact's core, parameterised by the budget and recency
// window it cuts against. The turn-boundary path (compact) passes the Compactor's
// configured budget/recency; the loop's overflow/timeout recovery (forceCompact)
// passes a TIGHTER budget and a smaller recency window so it creates real
// headroom when the model rejected a prompt the configured budget thought fit
// (the token estimate underestimates the real tokenizer). recencyTurns is clamped
// to >=1 so at least the latest turn-group is always preserved.
func (c *Compactor) compactToBudget(ctx context.Context, transcript []model.ChatMessage, goalAnchor, prevSummary string, overhead, turnIndex, budget, recencyTurns int) ([]model.ChatMessage, string, *CompactionEvent, error) {
	if recencyTurns < 1 {
		recencyTurns = 1
	}
	before := c.currentSize(transcript, overhead)
	if before <= budget {
		return transcript, prevSummary, nil, nil
	}

	realSystem, body := splitForCompaction(transcript)

	userIdxs := userMessageIndices(body)
	if len(userIdxs) < 2 {
		// Fewer than two turn-groups: there is no older group to fold into the summary
		// while keeping a verbatim tail, so the turn-group compactor can't help. A single
		// oversized group is trimmed by the within-turn bound (evictToolResults) instead.
		return transcript, prevSummary, nil, nil
	}

	// Size the verbatim recency tail by TOKENS, not a fixed group COUNT (bug 1091): a
	// few oversized frontier (Opus) verify-revise turns used to be kept whole by count
	// and alone exceed the budget, so compaction could never get the transcript under
	// budget however much history it summarized. The tail gets the transcript budget
	// (after the fixed tool-spec overhead) minus the pinned head and a reserve for the
	// rolling summary; recencyTurns stays as the upper CAP so small-turn runs still keep
	// up to N groups unchanged.
	ratio := c.tokenRatio
	if ratio < 1.0 {
		ratio = 1.0
	}
	fit := int(float64(budget-overhead) / ratio)
	tailBudget := fit - estimateTokens(realSystem) - summaryReserveTokens
	if tailBudget < minTailTokens {
		tailBudget = minTailTokens
	}
	recencyStart, keptGroups := recencyStartForBudget(body, userIdxs, recencyTurns, tailBudget)
	middle := body[:recencyStart]
	recency := body[recencyStart:]
	if len(middle) == 0 {
		// The whole body fits the tail budget; the overage is the pinned head / tool-spec
		// overhead, which the turn-group compactor cannot evict. Leave it intact.
		return transcript, prevSummary, nil, nil
	}

	summary, err := c.summarize(ctx, prevSummary, middle)
	if err != nil {
		return transcript, prevSummary, nil, err
	}

	summaryMsg := model.ChatMessage{
		Role:    model.RoleSystem,
		Content: renderSummaryMessage(goalAnchor, summary),
	}

	out := make([]model.ChatMessage, 0, len(realSystem)+1+len(recency))
	out = append(out, realSystem...)
	out = append(out, summaryMsg)
	out = append(out, recency...)

	ev := &CompactionEvent{
		TurnIndex:     turnIndex,
		TokensBefore:  before,
		TokensAfter:   c.currentSize(out, overhead), // same units as before (incl. overhead)
		GroupsEvicted: len(userIdxs) - keptGroups,
		Budget:        budget,
		Overhead:      overhead,
	}
	return out, summary, ev, nil
}

// summaryReserveTokens is the room compactToBudget holds back from the verbatim recency
// tail for the rolling-summary message it produces, so the summary itself doesn't push
// the compacted transcript back over budget (bug 1091). The summary is deliberately
// terse, so a modest fixed reserve suffices.
const summaryReserveTokens = 1024

// minTailTokens floors the token-budgeted recency tail so a large pinned head can't
// drive the tail budget to zero (or negative). The always-keep-the-latest-group rule in
// recencyStartForBudget is the true coherence floor; this only guards the arithmetic.
const minTailTokens = 512

// recencyStartForBudget chooses the body index — a turn-group boundary (RoleUser
// position) — where the verbatim recency tail begins. It keeps the MOST RECENT
// turn-groups that together fit tailBudget estimate-tokens, capped at maxGroups and
// never fewer than one, so a handful of oversized turns can't keep the verbatim tail
// (and thus the whole transcript) permanently over budget (bug 1091). Returns the chosen
// start index and how many groups were kept.
func recencyStartForBudget(body []model.ChatMessage, userIdxs []int, maxGroups, tailBudget int) (start, kept int) {
	n := len(userIdxs)
	// Always keep at least the most recent group, whatever its size — the within-turn
	// bound trims an oversized single group; dropping it would break coherence.
	start = userIdxs[n-1]
	kept = 1
	// Extend the tail to older groups while the whole tail still fits the budget and the
	// group cap is not reached. Walking newest→oldest and re-measuring the full tail keeps
	// the kept span contiguous to the latest turn.
	for i := n - 2; i >= 0 && kept < maxGroups; i-- {
		cand := userIdxs[i]
		if estimateTokens(body[cand:]) > tailBudget {
			break
		}
		start = cand
		kept++
	}
	return start, kept
}

// splitForCompaction partitions a transcript into the pinned head system block
// and the remaining body. The head is the leading run of system prompts; a prior
// rolling-summary message (tagged with compactionMarker) immediately after it is
// dropped from both partitions — its content is carried forward via the
// prevSummary argument so the next summary subsumes it instead of stacking.
func splitForCompaction(transcript []model.ChatMessage) (realSystem, body []model.ChatMessage) {
	i := 0
	for i < len(transcript) && transcript[i].Role == model.RoleSystem && !strings.HasPrefix(transcript[i].Content, compactionMarker) {
		i++
	}
	realSystem = transcript[:i]
	if i < len(transcript) && transcript[i].Role == model.RoleSystem && strings.HasPrefix(transcript[i].Content, compactionMarker) {
		i++ // drop the prior summary message; prevSummary carries its content
	}
	body = transcript[i:]
	return realSystem, body
}

// userMessageIndices returns the positions of the RoleUser messages in body —
// the turn-group boundaries the recency window and eviction span are cut on.
func userMessageIndices(body []model.ChatMessage) []int {
	var idxs []int
	for i, m := range body {
		if m.Role == model.RoleUser {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// renderSummaryMessage builds the single rolling-summary system message: the
// marker, the verbatim active-goal anchor, then the summary of evicted turns.
func renderSummaryMessage(goalAnchor, summary string) string {
	var b strings.Builder
	b.WriteString(compactionMarker)
	b.WriteString("\nThis session's context was compacted to stay within budget.\n\n")
	if goalAnchor != "" {
		b.WriteString("ACTIVE GOAL (verbatim first user prompt):\n")
		b.WriteString(goalAnchor)
		b.WriteString("\n\n")
	}
	b.WriteString("## Summary of earlier turns\n")
	b.WriteString(summary)
	return b.String()
}

// summarize asks the summarizer adapter to fold the evicted span (with any prior
// summary as cumulative prefix) into a terse factual note.
func (c *Compactor) summarize(ctx context.Context, prevSummary string, middle []model.ChatMessage) (string, error) {
	var b strings.Builder
	if prevSummary != "" {
		b.WriteString("Summary so far (extend, do not drop):\n")
		b.WriteString(prevSummary)
		b.WriteString("\n\n")
	}
	b.WriteString("Transcript span to fold into the summary:\n")
	for _, m := range middle {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, " [tool_call %s.%s]", tc.Surface, tc.Action)
		}
		b.WriteByte('\n')
	}
	msgs := []model.ChatMessage{
		{Role: model.RoleSystem, Content: "Summarize the following agent transcript span, preserving decisions made, facts established, files/identifiers touched, and any open threads. Be terse and factual. Output only the summary."},
		{Role: model.RoleUser, Content: b.String()},
	}
	resp, err := c.summarizer.Complete(ctx, msgs, nil)
	if err != nil {
		return "", fmt.Errorf("compaction summary: %w", err)
	}
	return resp.Text, nil
}

// firstUserContent returns the content of the first RoleUser message in a
// transcript (the active-goal anchor), or "" when there is none yet. Used to
// seed the goal anchor for a resumed session whose first prompt predates this
// loop instance.
func firstUserContent(transcript []model.ChatMessage) string {
	for _, m := range transcript {
		if m.Role == model.RoleUser {
			return m.Content
		}
	}
	return ""
}
