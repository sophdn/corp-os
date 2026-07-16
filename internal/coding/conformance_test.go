package coding

import (
	"context"
	"strings"
	"testing"
)

func TestParseConformanceReport(t *testing.T) {
	if v, ok := parseConformanceReport("analysis…\nCONFORMANCE: CONFORMS\n"); !ok || !v.Conforms {
		t.Fatalf("CONFORMS not parsed: ok=%v v=%+v", ok, v)
	}
	// case-insensitive verdict keyword + `=` separator.
	if v, ok := parseConformanceReport("conformance = conforms"); !ok || !v.Conforms {
		t.Fatalf("lenient CONFORMS not parsed: ok=%v v=%+v", ok, v)
	}
	viol := "CONFORMANCE: VIOLATION\ninput Match(\"a b\", …) — spec wants score 2, code returns 1"
	if v, ok := parseConformanceReport(viol); !ok || v.Conforms || !strings.Contains(v.Counterexample, "score 2") {
		t.Fatalf("VIOLATION not parsed with counterexample: ok=%v v=%+v", ok, v)
	}
	if _, ok := parseConformanceReport("I think it looks fine, no verdict line"); ok {
		t.Fatal("a reply with no verdict line must not parse")
	}
}

func TestConformanceCritic_Probe(t *testing.T) {
	const goal, impl = "Match counts keywords.", "func Match() int { return 1 }"

	// CONFORMS reply → conforming verdict.
	okModel := &scriptedPlanModel{responses: []string{"traced it; CONFORMANCE: CONFORMS"}}
	if v, err := NewConformanceCritic(okModel).Probe(context.Background(), goal, impl); err != nil || !v.Conforms {
		t.Fatalf("want conforming verdict, got v=%+v err=%v", v, err)
	}
	// The probe hands the critic BOTH the spec and the implementation.
	if len(okModel.userMsgs) != 1 || !strings.Contains(okModel.userMsgs[0], goal) || !strings.Contains(okModel.userMsgs[0], impl) {
		t.Fatalf("probe prompt must carry the spec + impl; got %q", okModel.userMsgs)
	}

	// VIOLATION reply → non-conforming with the counterexample carried.
	badModel := &scriptedPlanModel{responses: []string{"CONFORMANCE: VIOLATION\nMatch with 2 keywords returns 1, spec wants 2"}}
	v, err := NewConformanceCritic(badModel).Probe(context.Background(), goal, impl)
	if err != nil || v.Conforms || !strings.Contains(v.Counterexample, "2 keywords") {
		t.Fatalf("want a violation carrying the counterexample, got v=%+v err=%v", v, err)
	}

	// Unparseable reply → error (a missing verdict must NOT read as conformance).
	if _, err := NewConformanceCritic(&scriptedPlanModel{responses: []string{"looks good to me"}}).Probe(context.Background(), goal, impl); err == nil {
		t.Fatal("an unparseable critic reply must error, not silently conform")
	}
	// Model error → error.
	if _, err := NewConformanceCritic(errModel{}).Probe(context.Background(), goal, impl); err == nil {
		t.Fatal("a model error must propagate")
	}
	// Missing inputs → error.
	if _, err := NewConformanceCritic(okModel).Probe(context.Background(), "", impl); err == nil {
		t.Fatal("empty goal must error")
	}
}
