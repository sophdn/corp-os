// Package arcreview fires arc-close filing reviews from the corpos loop. It is
// the harness-side firing decision ported from the toolkit's Claude Code Stop
// hooks (hooks/arc-close-detector.sh + arc-close-filing-review-hook.sh): on each
// turn it counts user turns and inspects the last user message for an arc-close
// shape, and when a trigger fires it calls the toolkit's review_arc_for_filing
// action over MCP and dispatches the returned partition — auto-forging
// high-confidence short filings (bug / suggestion), and surfacing body-heavy
// (author) and medium-confidence (confirm) decisions into the next turn's
// context. corpos owns the firing decision; the toolkit keeps review_arc_for_filing
// and the ArcCloseFilingReviewed event as surfaces.
//
// Everything is fail-open: a missing transcript, an unreachable toolkit, a
// non-"fired" status, or a malformed response logs nothing fatal and never
// breaks a turn (mirroring the Stop hook's discipline).
package arcreview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"corpos/internal/hooks"
	"corpos/internal/model"
	"corpos/internal/tool"
)

// DefaultTurnThreshold is the user-turn count that fires a review (trigger A);
// mirrors the Stop hook's TOOLKIT_ARC_REVIEW_TURN_THRESHOLD default.
const DefaultTurnThreshold = 5

// defaultTimeout bounds the review_arc_for_filing call (the Qwen review pair runs
// ~1-2s warm; allow cold-start slack). Mirrors the Stop hook's curl --max-time.
const defaultTimeout = 20 * time.Second

// Dispatcher is the subset of the MCP client this package needs. *mcp.Client
// satisfies it; tests inject a fake.
type Dispatcher interface {
	Dispatch(ctx context.Context, call tool.Call) tool.Result
}

// Reviewer holds one session's arc-close firing state. It is created per session
// (the counter + pending reminders are session-scoped) and exposes two hook
// callbacks: PostTurnHook (detect + fire) and PreUserPromptHook (surface the
// reminders fired reviews queued).
type Reviewer struct {
	disp      Dispatcher
	sessionID string
	threshold int
	timeout   time.Duration

	turnsSinceReview int
	pending          []string // system-reminders queued for the next user turn
}

// Option configures a Reviewer.
type Option func(*Reviewer)

// WithTurnThreshold overrides the user-turn fire threshold (trigger A).
func WithTurnThreshold(n int) Option {
	return func(r *Reviewer) {
		if n > 0 {
			r.threshold = n
		}
	}
}

// WithTimeout overrides the review-call timeout.
func WithTimeout(d time.Duration) Option {
	return func(r *Reviewer) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// New builds a Reviewer for one session over a Dispatcher (an *mcp.Client in
// production, scoped to the session's project).
func New(disp Dispatcher, sessionID string, opts ...Option) *Reviewer {
	r := &Reviewer{disp: disp, sessionID: sessionID, threshold: DefaultTurnThreshold, timeout: defaultTimeout}
	for _, o := range opts {
		o(r)
	}
	return r
}

// PostTurnHook returns the post_turn callback: count the turn, detect arc-close
// triggers, and fire a review when any trigger holds.
func (r *Reviewer) PostTurnHook() hooks.Func {
	return func(c *hooks.Context) { r.onTurnEnd(c) }
}

// PreUserPromptHook returns the pre_user_prompt callback: drain any reminders a
// prior fire queued into the next turn's system-prompt additions.
func (r *Reviewer) PreUserPromptHook() hooks.Func {
	return func(c *hooks.Context) {
		if len(r.pending) == 0 {
			return
		}
		c.SystemPromptAdditions = append(c.SystemPromptAdditions, r.pending...)
		r.pending = nil
	}
}

// onTurnEnd increments the turn counter and fires when trigger A (the counter
// reaches the threshold) or trigger C (the last user message matches an arc-close
// shape) holds. Both triggers can fire in one turn (the review sees both).
func (r *Reviewer) onTurnEnd(c *hooks.Context) {
	r.turnsSinceReview++
	var triggers []string
	if r.turnsSinceReview >= r.threshold {
		triggers = append(triggers, fmt.Sprintf("counter_user_turns_%d", r.turnsSinceReview))
	}
	if shape := detectShape(lastUserMessage(c.Transcript)); shape != "" {
		triggers = append(triggers, "user_shape_"+shape)
	}
	if len(triggers) == 0 {
		return
	}
	r.fire(c.SessionID, c.Transcript, triggers, r.turnsSinceReview)
	r.turnsSinceReview = 0 // reset on fire (mirrors the detector's counter reset)
}

// fire materialises the transcript, calls review_arc_for_filing, and dispatches
// the partition. Fail-open: any failure returns without surfacing.
func (r *Reviewer) fire(sessionID string, transcript *[]model.ChatMessage, triggers []string, turns int) {
	path, cleanup, err := materialize(transcript)
	if err != nil {
		return
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	res := r.disp.Dispatch(ctx, tool.Call{
		Surface: "work", Action: "review_arc_for_filing",
		Params: map[string]any{
			"session_id":              sessionID,
			"fired_at":                time.Now().UTC().Format(time.RFC3339),
			"triggers":                triggers,
			"user_turns_since_review": turns,
			"transcript_path":         path,
		},
	})
	if !res.OK {
		return // toolkit unreachable / structured error — fail-open
	}
	rr, ok := tool.DecodeValue[reviewResult](res.Value)
	if !ok || rr.Status != "fired" {
		return // debounced / skipped / qwen_unreachable / malformed — fail-open
	}
	r.dispatch(ctx, rr)
}

// dispatch routes one fired review's partition: auto-forge short structured
// filings (bug / suggestion); queue authoring + confirm reminders for the next
// turn. Body-heavy kinds in auto_execute are refused (they belong in
// staged_for_authoring) rather than auto-forged with a draft body.
func (r *Reviewer) dispatch(ctx context.Context, rr reviewResult) {
	for _, d := range rr.Partition.AutoExecute {
		switch d.Action {
		case "forge_bug":
			r.forge(ctx, "bug", d.Payload)
		case "forge_suggestion":
			r.forge(ctx, "suggestion", d.Payload)
		default:
			// forge_vault_note / memory_write must arrive via staged_for_authoring;
			// any other kind is unexpected. Either way: do not auto-forge a body.
		}
	}
	if rem := authoringReminder(rr); rem != "" {
		r.pending = append(r.pending, rem)
	}
	if rem := confirmReminder(rr); rem != "" {
		r.pending = append(r.pending, rem)
	}
}

// forge auto-executes one filing by dispatching work.forge with the decision's
// payload as the row fields, stamping the arc-review attribution (mirrors the
// Stop hook's qwen_task_id stamp).
func (r *Reviewer) forge(ctx context.Context, schema string, payload json.RawMessage) {
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil || len(fields) == 0 {
		return
	}
	fields["qwen_task_id"] = "arc-review-decisions"
	r.disp.Dispatch(ctx, tool.Call{
		Surface: "work", Action: "forge",
		Params:    map[string]any{"schema_name": schema, "slug": deriveSlug(fields), "fields": fields},
		Rationale: "arc-close-filing-review auto-forge (" + schema + ") for session " + r.sessionID,
	})
}

// ---- detection ------------------------------------------------------------

// shapePatterns are the arc-close user-message shapes (ported from
// arc-close-detector.sh detect_user_shape), ordered most-specific first; the
// first match wins.
var shapePatterns = []struct {
	slug string
	re   *regexp.Regexp
}{
	{"session_end", regexp.MustCompile(`\bsession\s+end\b`)},
	{"any_else", regexp.MustCompile(`\bany(thing)?\s+else\b`)},
	{"thats_all", regexp.MustCompile(`\bthat'?s\s+all\b`)},
	{"looks_good", regexp.MustCompile(`\blooks?\s+good\b`)},
	{"wrapping", regexp.MustCompile(`(^|[^/])\bwrapping\b`)},
	{"thanks", regexp.MustCompile(`\bthanks?\b`)},
	{"done", regexp.MustCompile(`\bdone\b`)},
	{"clear_command", regexp.MustCompile(`/clear\b`)},
}

// detectShape returns the arc-close shape slug the text matches, or "".
func detectShape(text string) string {
	if text == "" {
		return ""
	}
	norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
	for _, p := range shapePatterns {
		if p.re.MatchString(norm) {
			return p.slug
		}
	}
	return ""
}

// lastUserMessage returns the content of the last user-role message in the
// transcript, or "" when there is none.
func lastUserMessage(transcript *[]model.ChatMessage) string {
	if transcript == nil {
		return ""
	}
	msgs := *transcript
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

// ---- transcript materialisation ------------------------------------------

// materialize writes the transcript's user/assistant messages to a temp .jsonl in
// the shape the toolkit's snapshot extractor reads ({"role","content"} per line),
// returning the path and a cleanup func. The toolkit's ExtractSnapshot keeps only
// user/assistant rows, so tool/system rows are dropped here.
func materialize(transcript *[]model.ChatMessage) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "corpos-arc-*.jsonl")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	enc := json.NewEncoder(f)
	if transcript != nil {
		for _, m := range *transcript {
			if (m.Role == model.RoleUser || m.Role == model.RoleAssistant) && strings.TrimSpace(m.Content) != "" {
				// map[string]string always marshals — no encode error to handle.
				_ = enc.Encode(map[string]string{"role": m.Role, "content": m.Content})
			}
		}
	}
	_ = f.Close()
	return f.Name(), cleanup, nil
}

// ---- result decoding + reminders -----------------------------------------

// filingDecision mirrors the toolkit's FilingDecision (the fields corpos needs).
type filingDecision struct {
	Action     string          `json:"action"`
	Payload    json.RawMessage `json:"payload"`
	Confidence float64         `json:"confidence"`
	Reasoning  string          `json:"reasoning"`
}

type partition struct {
	AutoExecute        []filingDecision `json:"auto_execute"`
	StagedForAuthoring []filingDecision `json:"staged_for_authoring"`
	SurfaceForConfirm  []filingDecision `json:"surface_for_confirm"`
	Skip               []filingDecision `json:"skip"`
}

// reviewResult mirrors the toolkit's ReviewArcForFilingResult (subset).
type reviewResult struct {
	Status    string    `json:"status"`
	Partition partition `json:"partition"`
	Triggers  []string  `json:"triggers"`
	EventID   string    `json:"event_id"`
	Reason    string    `json:"reason"`
}

// authoringReminder renders the staged_for_authoring decisions as a
// system-reminder asking the in-session agent to author the bodies (Qwen decided
// whether + the kind + a seed title; the agent holds the full arc).
func authoringReminder(rr reviewResult) string {
	staged := rr.Partition.StagedForAuthoring
	if len(staged) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<system-reminder>\nArc-close filing review fired (triggers: %s). Qwen decided %d body-heavy item(s) are worth filing and chose the kind + a seed title for each. YOU hold the full conversation — AUTHOR the body; don't rubber-stamp the draft.\n",
		strings.Join(rr.Triggers, ", "), len(staged))
	for i, d := range staged {
		fmt.Fprintf(&b, "%d. %s — rationale: %s\n   payload: %s\n", i+1, d.Action, d.Reasoning, string(d.Payload))
	}
	fmt.Fprintf(&b, "(staged from arc-review event %s)\n</system-reminder>", rr.EventID)
	return b.String()
}

// confirmReminder renders the surface_for_confirm decisions as a system-reminder
// for the agent to file (or skip) on the next turn.
func confirmReminder(rr reviewResult) string {
	conf := rr.Partition.SurfaceForConfirm
	if len(conf) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<system-reminder>\nArc-close filing review fired (triggers: %s). Qwen returned %d filing decision(s) for confirm:\n",
		strings.Join(rr.Triggers, ", "), len(conf))
	for i, d := range conf {
		fmt.Fprintf(&b, "%d. [confidence=%.2f] %s — %s\n   payload: %s\n", i+1, d.Confidence, d.Action, d.Reasoning, string(d.Payload))
	}
	b.WriteString("Execute via mcp__toolkit-server__work.forge(...) with the matching schema, or skip if covered.\n</system-reminder>")
	return b.String()
}

// ---- slug derivation ------------------------------------------------------

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// deriveSlug picks a forge slug from the payload: a kebab name wins, else the
// kebab-cased title, else a stable fallback. Capped at 80 chars.
func deriveSlug(fields map[string]any) string {
	if n, ok := fields["name"].(string); ok && n != "" {
		return cap80(n)
	}
	if t, ok := fields["title"].(string); ok && t != "" {
		return cap80(kebab(t))
	}
	return "arc-review-filing"
}

func kebab(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func cap80(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
