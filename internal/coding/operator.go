package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"corpos/internal/model"
)

// OperatorOp is the operator's intervention vocabulary on a gate failure.
type OperatorOp string

const (
	OpEdit         OperatorOp = "edit"
	OpBranchFix    OperatorOp = "branch_fix"
	OpForceAdvance OperatorOp = "force_advance"
	OpSkip         OperatorOp = "skip"
	OpAbort        OperatorOp = "abort"
)

// allowedOps is the closed set the operator may emit.
var allowedOps = map[OperatorOp]bool{OpEdit: true, OpBranchFix: true, OpForceAdvance: true, OpSkip: true, OpAbort: true}

// OperatorDecision is the single structured intervention an operator composes for
// one gate failure.
type OperatorDecision struct {
	Op            OperatorOp `json:"op"`
	TargetAT      string     `json:"target_at"`
	Goal          string     `json:"goal"`
	CommitSHA     string     `json:"commit_sha"`
	Justification string     `json:"justification"`
	Reason        string     `json:"reason"`
}

// OperatorContext is what the operator sees for one failure: the failed AT, its
// goal, the worker status + diagnostic, the failing gate output, the CURRENT
// package files (Finding 0 — mandatory ground truth), the worker's diff, and a
// deterministic classification hint.
type OperatorContext struct {
	FailedATSlug string
	Goal         string
	WorkerStatus WorkerStatus
	Diagnostic   string
	GateTails    string
	PackageFiles string
	Diff         string
	ClassifyHint string
}

// operatorSystemPrompt states the operator contract in our own terms (clean-room).
// The load-bearing rules: pick exactly one op; pin the fix to symbols that EXIST
// NOW (the current package files are ground truth); never weaken a test to match a
// wrong implementation; reply with only a JSON object.
const operatorSystemPrompt = `You are the OPERATOR of a gate-verified coding chain. A chain is decomposed into atomic tasks (ATs); a local model writes each one; a deterministic GATE (build/test commands) verifies it. When an AT's gate keeps failing, YOU decide the single intervention that unsticks it. You do NOT edit the repo yourself — you emit one structured intervention the orchestrator applies.

Pick exactly one op:
- "branch_fix": spawn a fresh worker attempt for the failed AT with the prior diff + gate error as context. Best when the implementation does not COMPILE and a clean re-attempt with the error in view will fix it. Needs target_at.
- "edit": replace the failed AT's GOAL with a corrected, PRECISE instruction. Needs target_at and goal. The worker REGENERATES THE WHOLE FILE from your goal, so a vague goal makes it play whack-a-mole. Your goal MUST pin the fix down using the CURRENT PACKAGE FILES below as ground truth: (1) name the EXACT symbols/signatures that already exist; do not invent constructors; (2) state an explicit SCOPE boundary — use ONLY symbols that exist NOW; do NOT reference methods/types a later AT will add; (3) include the verbatim corrected code when you can.
- "skip": give up on this AT and advance (rare; only if it is non-load-bearing).
- "force_advance": accept a specific commit as success. Needs commit_sha + justification. Rare.

CRITICAL: never tell the worker to weaken or rewrite a TEST to match a wrong implementation. Fix the implementation, not the assertion.

Respond with ONLY a JSON object, no prose, no code fence:
{"op":"...","target_at":"<slug>","goal":"<for edit only>","commit_sha":"<for force_advance only>","justification":"<for force_advance only>","reason":"<one sentence>"}`

// buildOperatorMessage renders the user message for one failure.
func buildOperatorMessage(c OperatorContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FAILED AT: %s\n", c.FailedATSlug)
	if c.ClassifyHint != "" {
		fmt.Fprintf(&b, "HEURISTIC: %s\n", c.ClassifyHint)
	}
	fmt.Fprintf(&b, "\nAT GOAL (current):\n%s\n", c.Goal)
	fmt.Fprintf(&b, "\nWORKER STATUS: %s\nDIAGNOSTIC:\n%s\n", c.WorkerStatus, orNone(c.Diagnostic))
	fmt.Fprintf(&b, "\nFAILING GATE OUTPUT:\n%s\n", orNone(c.GateTails))
	fmt.Fprintf(&b, "\nCURRENT PACKAGE FILES (ground truth — the symbols that exist NOW; do not reference anything not present here):\n%s\n", orNone(c.PackageFiles))
	fmt.Fprintf(&b, "\nWORKER'S DIFF (the broken attempt):\n%s\n", orNone(c.Diff))
	b.WriteString("\nEmit the single best intervention as JSON.")
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// Operator composes an intervention decision for one gate failure using a model
// adapter (the tier the router selected).
type Operator interface {
	Decide(ctx context.Context, adapter model.Adapter, octx OperatorContext) (OperatorDecision, model.Usage, error)
}

// ModelOperator is the production operator: it asks a model tier for a structured
// decision via a single completion (no tools — the operator emits a decision, it
// does not act in the repo).
type ModelOperator struct{}

// Decide calls the adapter and parses its decision.
func (ModelOperator) Decide(ctx context.Context, adapter model.Adapter, octx OperatorContext) (OperatorDecision, model.Usage, error) {
	msgs := []model.ChatMessage{
		{Role: "system", Content: operatorSystemPrompt},
		{Role: "user", Content: buildOperatorMessage(octx)},
	}
	resp, err := adapter.Complete(ctx, msgs, nil)
	if err != nil {
		return OperatorDecision{}, model.Usage{}, fmt.Errorf("operator completion: %w", err)
	}
	dec, err := parseDecision(resp.Text)
	if err != nil {
		return OperatorDecision{}, resp.Usage, err
	}
	return dec, resp.Usage, nil
}

// jsonObject matches the first {...} blob in a response (operators sometimes wrap
// the JSON in prose or a fence despite instructions).
var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

// parseDecision extracts and validates the operator's JSON decision.
func parseDecision(text string) (OperatorDecision, error) {
	blob := strings.TrimSpace(text)
	if m := jsonObject.FindString(blob); m != "" {
		blob = m
	}
	var dec OperatorDecision
	if err := json.Unmarshal([]byte(blob), &dec); err != nil {
		return OperatorDecision{}, fmt.Errorf("operator emitted unparseable decision: %w; raw: %.200q", err, text)
	}
	dec.Op = OperatorOp(strings.TrimSpace(string(dec.Op)))
	if !allowedOps[dec.Op] {
		return OperatorDecision{}, fmt.Errorf("operator emitted disallowed op %q; raw: %.200q", dec.Op, text)
	}
	return dec, nil
}
