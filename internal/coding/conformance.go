package coding

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"corpos/internal/model"
)

// ConformanceCritic adversarially checks an implementation against the PROSE intent it was
// built from — the half a behavioral acceptance gate cannot cover. An authored oracle pins
// ONE example per behavior, so a worker can satisfy the LETTER of its tests while violating
// the spec: it can hard-code a return that the single example happens to expect, pick the
// first match where the spec says the best, or compare case-sensitively where the spec says
// otherwise (all three observed building the profile-matcher core). The critic is handed the
// prose goal and the implementation source and charged to FIND a concrete input where the
// code's behavior contradicts the goal — the move a human reviewer makes. It is necessarily
// model-driven: the source of truth is prose, so this cannot be a deterministic gate. A found
// counterexample is a real defect (or, rarely, a critic false positive) to be turned into a
// new acceptance case the worker must then satisfy.
type ConformanceCritic struct {
	model model.Adapter
}

// NewConformanceCritic builds a critic over a reasoning model adapter.
func NewConformanceCritic(m model.Adapter) *ConformanceCritic {
	return &ConformanceCritic{model: m}
}

// ConformanceVerdict is the parsed result of a conformance probe.
type ConformanceVerdict struct {
	// Conforms is true when the critic found no input on which the implementation contradicts
	// the spec.
	Conforms bool
	// Counterexample is the critic's described input + expected-vs-actual divergence, set only
	// when Conforms is false. It is the actionable revision feedback (and, as a regression test,
	// the case the strengthened oracle should pin).
	Counterexample string
}

// conformanceRe extracts the critic's verdict line: `CONFORMANCE: CONFORMS|VIOLATION`
// (case-insensitive, `:` or `=`).
var conformanceRe = regexp.MustCompile(`(?i)CONFORMANCE\s*[:=]\s*(CONFORMS|VIOLATION)`)

// Probe asks the critic to find an input on which impl contradicts goal. It returns the
// verdict, or an error when the model call fails or its reply carries no parseable verdict (an
// unparseable critic reply must NOT be read as conformance — a missing verdict is an infra
// failure for the caller to surface, not a silent pass).
func (c *ConformanceCritic) Probe(ctx context.Context, goal, implSource string) (ConformanceVerdict, error) {
	if strings.TrimSpace(goal) == "" || strings.TrimSpace(implSource) == "" {
		return ConformanceVerdict{}, fmt.Errorf("conformance probe: goal and implementation are both required")
	}
	resp, err := c.model.Complete(ctx, []model.ChatMessage{
		{Role: model.RoleSystem, Content: conformanceSystemPrompt},
		{Role: model.RoleUser, Content: conformanceUserPrompt(goal, implSource)},
	}, nil)
	if err != nil {
		return ConformanceVerdict{}, fmt.Errorf("conformance probe: %w", err)
	}
	verdict, ok := parseConformanceReport(resp.Text)
	if !ok {
		return ConformanceVerdict{}, fmt.Errorf("conformance probe: no parseable `CONFORMANCE: CONFORMS|VIOLATION` verdict in reply")
	}
	return verdict, nil
}

// parseConformanceReport extracts the verdict from a critic reply. ok is false when no verdict
// line is present. On a VIOLATION the whole reply is carried as the counterexample (it holds
// the input + expected-vs-actual the critic described). Pure (no IO) so it is unit-testable.
func parseConformanceReport(text string) (ConformanceVerdict, bool) {
	m := conformanceRe.FindStringSubmatch(text)
	if m == nil {
		return ConformanceVerdict{}, false
	}
	if strings.EqualFold(m[1], "CONFORMS") {
		return ConformanceVerdict{Conforms: true}, true
	}
	return ConformanceVerdict{Conforms: false, Counterexample: strings.TrimSpace(text)}, true
}

const conformanceSystemPrompt = `You are an adversarial code reviewer. You are given a feature's PROSE SPECIFICATION and an IMPLEMENTATION that already passes its automated tests. Those tests pin only one example per behavior, so the implementation may satisfy the tests while still CONTRADICTING the specification.

Your job: find ONE concrete input on which the implementation's actual behavior differs from what the specification requires. Reason specifically about the ways a minimal implementation cheats a thin test:
- MAGNITUDE: a value the spec says to COMPUTE (a count, a sum) that the code instead hard-codes or caps.
- SELECTION/ORDER: the spec says pick the best / highest / earliest, but the code returns the first / last / an arbitrary one.
- CASE & FORMAT: the spec says case-insensitive / trimmed / whole-word, but the code compares raw.
- BOUNDARIES: empty, zero, ties, duplicates, missing.

Trace the code on candidate inputs until you either find a contradiction or convince yourself none exists.

Reply with EXACTLY one verdict line:
- ` + "`CONFORMANCE: CONFORMS`" + ` if the implementation fully satisfies the specification, OR
- ` + "`CONFORMANCE: VIOLATION`" + ` followed by: the concrete input, what the SPEC requires for it, and what the IMPLEMENTATION actually produces.`

func conformanceUserPrompt(goal, implSource string) string {
	var b strings.Builder
	b.WriteString("SPECIFICATION:\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\nIMPLEMENTATION:\n```go\n")
	b.WriteString(strings.TrimSpace(implSource))
	b.WriteString("\n```\n")
	return b.String()
}
