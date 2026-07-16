package coding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

func TestParseDecisionValid(t *testing.T) {
	dec, err := parseDecision(`{"op":"edit","target_at":"a","goal":"do x","reason":"r"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if dec.Op != OpEdit || dec.TargetAT != "a" || dec.Goal != "do x" {
		t.Fatalf("decision wrong: %+v", dec)
	}
}

func TestParseDecisionWrappedInProse(t *testing.T) {
	dec, err := parseDecision("Here is my call:\n```json\n{\"op\":\"branch_fix\",\"target_at\":\"impl\"}\n```\nthanks")
	if err != nil {
		t.Fatalf("parse wrapped: %v", err)
	}
	if dec.Op != OpBranchFix {
		t.Fatalf("op = %q", dec.Op)
	}
}

func TestParseDecisionErrors(t *testing.T) {
	if _, err := parseDecision("not json at all"); err == nil {
		t.Fatal("unparseable should error")
	}
	if _, err := parseDecision(`{"op":"delete_repo"}`); err == nil {
		t.Fatal("disallowed op should error")
	}
}

func TestBuildOperatorMessage(t *testing.T) {
	msg := buildOperatorMessage(OperatorContext{
		FailedATSlug: "impl", Goal: "G", WorkerStatus: WorkerGateFailure,
		Diagnostic: "D", GateTails: "GT", PackageFiles: "PF", Diff: "DF", ClassifyHint: "CH",
	})
	for _, want := range []string{"impl", "G", "gate_failure", "D", "PF", "DF", "CH", "ground truth"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q\n%s", want, msg)
		}
	}
	// Empty fields render "(none)".
	if !strings.Contains(buildOperatorMessage(OperatorContext{FailedATSlug: "x"}), "(none)") {
		t.Fatal("empty context should render (none)")
	}
}

// decideAdapter returns a fixed completion (a JSON decision).
type decideAdapter struct {
	id   string
	text string
	err  error
}

func (a decideAdapter) Model() string   { return a.id }
func (a decideAdapter) Available() bool { return true }
func (a decideAdapter) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	if a.err != nil {
		return model.Response{}, a.err
	}
	return model.Response{Text: a.text, Usage: model.Usage{InputTokens: 10, OutputTokens: 5}, StopReason: model.StopEndTurn}, nil
}

func TestModelOperatorDecide(t *testing.T) {
	op := ModelOperator{}
	dec, usage, err := op.Decide(context.Background(), decideAdapter{id: "m", text: `{"op":"edit","goal":"g"}`}, OperatorContext{FailedATSlug: "a"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Op != OpEdit || usage.InputTokens != 10 {
		t.Fatalf("decide wrong: %+v usage=%+v", dec, usage)
	}
}

func TestModelOperatorDecideErrors(t *testing.T) {
	op := ModelOperator{}
	if _, _, err := op.Decide(context.Background(), decideAdapter{id: "m", err: errors.New("down")}, OperatorContext{}); err == nil {
		t.Fatal("completion error should surface")
	}
	if _, _, err := op.Decide(context.Background(), decideAdapter{id: "m", text: "garbage"}, OperatorContext{}); err == nil {
		t.Fatal("unparseable decision should error")
	}
}
