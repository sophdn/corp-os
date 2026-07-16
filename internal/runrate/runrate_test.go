package runrate

import (
	"testing"
	"time"

	"corpos/internal/session"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := ParseDate(s)
	if err != nil {
		t.Fatalf("ParseDate(%q): %v", s, err)
	}
	return d
}

// TestProjectMethodology exercises the §5 formula end-to-end: paid spend S summed
// across in-period sessions, projected monthly = S / D × 30, with free and
// UNPRICED models distinguished.
func TestProjectMethodology(t *testing.T) {
	sessions := []Session{
		{
			RunID:     "A",
			CreatedAt: mustDate(t, "2026-06-02"),
			Costs: []session.CostRow{
				{Model: "claude-haiku-4-5-20251001", InputTokens: 1000, OutputTokens: 500, USD: 6.0},
				{Model: "qwen2.5-32b", InputTokens: 2000, OutputTokens: 1000, USD: 0}, // free, priced
			},
		},
		{
			RunID:     "B",
			CreatedAt: mustDate(t, "2026-06-05"),
			Costs: []session.CostRow{
				{Model: "claude-haiku-4-5-20251001", InputTokens: 800, OutputTokens: 200, USD: 8.0},
				{Model: "mystery-model", InputTokens: 50, OutputTokens: 10, USD: 0}, // UNPRICED
			},
		},
		{
			RunID:     "C", // out of period — excluded
			CreatedAt: mustDate(t, "2026-07-01"),
			Costs:     []session.CostRow{{Model: "claude-haiku-4-5-20251001", USD: 100.0}},
		},
	}

	rep, err := Project(sessions, mustDate(t, "2026-06-01"), mustDate(t, "2026-06-15"))
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if rep.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2 (C is out of period)", rep.Sessions)
	}
	if rep.ElapsedDays != 14 {
		t.Errorf("ElapsedDays = %v, want 14", rep.ElapsedDays)
	}
	if rep.PaidUSD != 14.0 {
		t.Errorf("PaidUSD (S) = %v, want 14.0", rep.PaidUSD)
	}
	// S / D × 30 = 14 / 14 × 30 = 30.
	if rep.ProjectedMonthlyUSD != 30.0 {
		t.Errorf("ProjectedMonthlyUSD = %v, want 30.0", rep.ProjectedMonthlyUSD)
	}

	// Per-model, most expensive first; haiku rows from both sessions merged.
	if len(rep.PerModel) != 3 {
		t.Fatalf("PerModel len = %d, want 3", len(rep.PerModel))
	}
	haiku := rep.PerModel[0]
	if haiku.Model != "claude-haiku-4-5-20251001" || haiku.USD != 14.0 || haiku.InputTokens != 1800 || haiku.OutputTokens != 700 || !haiku.Priced {
		t.Errorf("haiku rollup = %+v", haiku)
	}
	// The two $0 models tie on USD and break by model name ascending.
	if rep.PerModel[1].Model != "mystery-model" || rep.PerModel[2].Model != "qwen2.5-32b" {
		t.Errorf("zero-cost order = %q, %q", rep.PerModel[1].Model, rep.PerModel[2].Model)
	}
	if len(rep.UnpricedModels) != 1 || rep.UnpricedModels[0] != "mystery-model" {
		t.Errorf("UnpricedModels = %v, want [mystery-model]", rep.UnpricedModels)
	}
}

// TestProjectEmptyPeriod covers a period with no sessions: zero paid spend, zero
// projection, no divide-by-zero (D still positive).
func TestProjectEmptyPeriod(t *testing.T) {
	rep, err := Project(nil, mustDate(t, "2026-06-01"), mustDate(t, "2026-06-08"))
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if rep.Sessions != 0 || rep.PaidUSD != 0 || rep.ProjectedMonthlyUSD != 0 || len(rep.PerModel) != 0 {
		t.Errorf("empty report = %+v", rep)
	}
}

// TestProjectNonPositivePeriod rejects D ≤ 0 (the projection divides by D).
func TestProjectNonPositivePeriod(t *testing.T) {
	day := mustDate(t, "2026-06-01")
	if _, err := Project(nil, day, day); err == nil {
		t.Error("want an error when since == until (D = 0)")
	}
	if _, err := Project(nil, mustDate(t, "2026-06-02"), mustDate(t, "2026-06-01")); err == nil {
		t.Error("want an error when since is after until (D < 0)")
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		want    string // RFC3339 of the parsed instant, when not an error
	}{
		{in: "2026-06-01", want: "2026-06-01T00:00:00Z"},
		{in: "2026-06-01T13:30:00Z", want: "2026-06-01T13:30:00Z"},
		{in: "not-a-date", wantErr: true},
		{in: "2026/06/01", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseDate(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseDate(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDate(%q): %v", c.in, err)
			continue
		}
		if got.Format(time.RFC3339) != c.want {
			t.Errorf("ParseDate(%q) = %s, want %s", c.in, got.Format(time.RFC3339), c.want)
		}
	}
}

// TestRollupTiersSharesAndOrder verifies per-model spend folds onto the ladder
// in cheap→frontier order with correct token and USD shares, and that the
// frontier (strong/Opus) share is the one-glance gate number.
func TestRollupTiersSharesAndOrder(t *testing.T) {
	per := []ModelSpend{
		{Model: "claude-opus-4-8", InputTokens: 100, OutputTokens: 100, USD: 4.0},
		{Model: "google/gemini-3.1-flash-lite", InputTokens: 700, OutputTokens: 100, USD: 6.0},
		{Model: "qwen2.5-32b", InputTokens: 8000, OutputTokens: 1000, USD: 0},
	}
	tiers := RollupTiers(per)
	if len(tiers) != 3 {
		t.Fatalf("got %d tiers, want 3: %+v", len(tiers), tiers)
	}
	// Ladder order: local, mid, strong.
	wantOrder := []string{"local", "mid", "strong"}
	for i, w := range wantOrder {
		if string(tiers[i].Tier) != w {
			t.Errorf("tier[%d] = %q, want %q", i, tiers[i].Tier, w)
		}
	}
	// Total USD = 10; total tok = 10000.
	strong := tiers[2]
	if got := strong.USDShare; got < 0.399 || got > 0.401 {
		t.Errorf("strong USDShare = %.4f, want ~0.40", got)
	}
	if got := strong.TokenShare; got < 0.019 || got > 0.021 {
		t.Errorf("strong TokenShare = %.4f, want ~0.02", got)
	}
	usd, tok := FrontierShare(tiers)
	if usd != strong.USDShare || tok != strong.TokenShare {
		t.Errorf("FrontierShare = (%.4f,%.4f), want (%.4f,%.4f)", usd, tok, strong.USDShare, strong.TokenShare)
	}
}

// TestRollupTiersEmptyAndNoStrong covers the zero-total guard (no divide) and a
// rollup with no frontier rung (FrontierShare returns zeros).
func TestRollupTiersEmptyAndNoStrong(t *testing.T) {
	if got := RollupTiers(nil); len(got) != 0 {
		t.Errorf("empty rollup = %+v, want none", got)
	}
	// All-free local-only window: totalUSD == 0, shares stay 0 (no NaN).
	tiers := RollupTiers([]ModelSpend{{Model: "qwen2.5-32b", InputTokens: 10, OutputTokens: 5}})
	if len(tiers) != 1 || tiers[0].Tier != "local" {
		t.Fatalf("want single local tier, got %+v", tiers)
	}
	if tiers[0].USDShare != 0 {
		t.Errorf("USDShare on free window = %v, want 0", tiers[0].USDShare)
	}
	if tiers[0].TokenShare < 0.999 {
		t.Errorf("sole tier TokenShare = %v, want ~1.0", tiers[0].TokenShare)
	}
	usd, tok := FrontierShare(tiers)
	if usd != 0 || tok != 0 {
		t.Errorf("FrontierShare with no strong rung = (%v,%v), want (0,0)", usd, tok)
	}
}

// TestProjectPopulatesPerTier confirms Project rolls the per-model spend onto
// the ladder so the window report carries the tier view.
func TestProjectPopulatesPerTier(t *testing.T) {
	sessions := []Session{{
		RunID:     "T",
		CreatedAt: mustDate(t, "2026-06-02"),
		Costs: []session.CostRow{
			{Model: "claude-opus-4-8", InputTokens: 10, OutputTokens: 10, USD: 1.0},
			{Model: "qwen2.5-32b", InputTokens: 100, OutputTokens: 100, USD: 0},
		},
	}}
	rep, err := Project(sessions, mustDate(t, "2026-06-01"), mustDate(t, "2026-06-03"))
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if len(rep.PerTier) != 2 {
		t.Fatalf("PerTier = %+v, want 2 tiers", rep.PerTier)
	}
	usd, _ := FrontierShare(rep.PerTier)
	if usd < 0.999 {
		t.Errorf("frontier USD share = %v, want ~1.0 (opus is the only paid spend)", usd)
	}
}
