package coding

import (
	"context"
	"testing"

	"corpos/internal/model"
)

func midAdapter() model.Adapter    { return decideAdapter{id: "google/gemini-3.1-flash-lite"} }
func coderAdapter() model.Adapter  { return decideAdapter{id: "deepseek-chat"} }
func strongAdapter() model.Adapter { return decideAdapter{id: "claude-opus-4-8"} }

// tierUsed reports whether any intervention ran on the named tier.
func tierUsed(res SeatResult, tier string) bool {
	for _, r := range res.Interventions {
		if r.Tier == tier {
			return true
		}
	}
	return false
}

func TestCoderRungAbsorbsAuthoringBelowOpus(t *testing.T) {
	// The gate passes on the 4th call: miss(initial), miss(mid#1), miss(mid#2),
	// pass(coder#3). WITH the coder rung, DeepSeek absorbs the authoring escalation
	// and Opus is never reached.
	o, st := seatHarness(t, 4)
	seat := NewOperatorSeat(o, seatOperator{usage: model.Usage{InputTokens: 200, OutputTokens: 80}},
		midAdapter(), strongAdapter(), WithK(2), WithCoderRung(coderAdapter()))
	res := seat.Run(context.Background(), st)

	if res.FinalStatus != ChainSuccess {
		t.Fatalf("want success, got %q", res.FinalStatus)
	}
	if !tierUsed(res, "coder") {
		t.Fatalf("coder rung should have acted: %+v", res.Interventions)
	}
	if tierUsed(res, "strong") {
		t.Fatalf("Opus should NOT be reached when the coder rung carries: %+v", res.Interventions)
	}
	last := res.Interventions[len(res.Interventions)-1]
	if last.Tier != "coder" || !last.Carried {
		t.Fatalf("the coder rung should carry the point, got %+v", last)
	}
}

func TestBaselineWithoutCoderReachesOpus(t *testing.T) {
	// Same gate schedule, NO coder rung: miss, mid#1, mid#2, strong#3 carries — so
	// Opus IS reached. This is the baseline the coder rung improves on.
	o, st := seatHarness(t, 4)
	seat := NewOperatorSeat(o, seatOperator{usage: model.Usage{InputTokens: 200, OutputTokens: 80}},
		midAdapter(), strongAdapter(), WithK(2))
	res := seat.Run(context.Background(), st)

	if res.FinalStatus != ChainSuccess {
		t.Fatalf("want success, got %q", res.FinalStatus)
	}
	if !tierUsed(res, "strong") {
		t.Fatalf("baseline (no coder) should reach Opus: %+v", res.Interventions)
	}
	if tierUsed(res, "coder") {
		t.Fatalf("no coder rung was configured: %+v", res.Interventions)
	}
}

func TestCoderRungEscalatesToOpusWhenItAlsoFails(t *testing.T) {
	// Gate passes only on the 5th call: the coder rung also misses, so the ladder
	// escalates one more rung to Opus, which carries. Order: mid,mid,coder,strong.
	o, st := seatHarness(t, 5)
	seat := NewOperatorSeat(o, seatOperator{usage: model.Usage{InputTokens: 100, OutputTokens: 40}},
		midAdapter(), strongAdapter(), WithK(2), WithCoderRung(coderAdapter()))
	res := seat.Run(context.Background(), st)

	if res.FinalStatus != ChainSuccess {
		t.Fatalf("want success, got %q", res.FinalStatus)
	}
	tiers := []string{}
	for _, r := range res.Interventions {
		tiers = append(tiers, r.Tier)
	}
	want := []string{"mid", "mid", "coder", "strong"}
	if len(tiers) != len(want) {
		t.Fatalf("tier sequence = %v, want %v", tiers, want)
	}
	for i := range want {
		if tiers[i] != want[i] {
			t.Fatalf("tier sequence = %v, want %v", tiers, want)
		}
	}
}

func TestSeatLadderShape(t *testing.T) {
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}))
	two := NewOperatorSeat(o, seatOperator{}, midAdapter(), strongAdapter())
	if got := two.ladder(); len(got) != 2 || got[0].Model() != "google/gemini-3.1-flash-lite" || got[1].Model() != "claude-opus-4-8" {
		t.Fatalf("2-rung ladder wrong: %v", got)
	}
	three := NewOperatorSeat(o, seatOperator{}, midAdapter(), strongAdapter(), WithCoderRung(coderAdapter()), WithCoderRung(nil))
	got := three.ladder()
	if len(got) != 3 || got[1].Model() != "deepseek-chat" {
		t.Fatalf("3-rung ladder wrong: %v", got)
	}
	if three.tierName(coderAdapter()) != "coder" || three.tierName(midAdapter()) != "mid" || three.tierName(strongAdapter()) != "strong" {
		t.Fatal("tierName mislabeled a rung")
	}
}
