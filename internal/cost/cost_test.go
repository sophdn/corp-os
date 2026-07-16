package cost

import "testing"

func TestPriceKnownFreeAndUnknown(t *testing.T) {
	if got := Price("claude-haiku-4-5-20251001", 1000, 1000); got == 0 {
		t.Error("a priced model should cost > 0")
	}
	if got := Price("qwen2.5-32b", 1000, 1000); got != 0 {
		t.Errorf("free model = %v, want 0", got)
	}
	if got := Price("unknown-model", 1000, 1000); got != 0 {
		t.Errorf("unknown model = %v, want 0", got)
	}
}

func TestIsPriced(t *testing.T) {
	if !IsPriced("claude-opus-4-8") {
		t.Error("opus should be priced")
	}
	if IsPriced("unknown-model") {
		t.Error("unknown model should be unpriced")
	}
}

func TestGeminiFlashLitePriced(t *testing.T) {
	// The default mid rung (§4.6) must be priced so its run-rate is measurable.
	if !IsPriced("google/gemini-3.1-flash-lite") {
		t.Error("the mid-tier Gemini-Flash-Lite seat should be priced")
	}
	if got := Price("google/gemini-3.1-flash-lite", 1000, 1000); got <= 0 {
		t.Errorf("Gemini-Flash-Lite price = %v, want > 0", got)
	}
}

func TestCodingRungPriced(t *testing.T) {
	// The atomic-coding-chain intermediate rung (deepseek/deepseek-v3.2) must be
	// priced so a coding/test-authoring run-rate is not partly UNPRICED and the
	// swap gate's spend total is trustworthy (bug 1104).
	const codingRung = "deepseek/deepseek-v3.2"
	if !IsPriced(codingRung) {
		t.Fatalf("coding rung %q should be priced (else -run-rate flags it UNPRICED)", codingRung)
	}
	if got := Price(codingRung, 1000, 1000); got <= 0 {
		t.Errorf("coding rung price = %v, want > 0", got)
	}
	usd, src := PriceUsage(codingRung, Usage{InputTokens: 1000, OutputTokens: 1000})
	if src != PricedTable {
		t.Errorf("coding rung price source = %q, want %q", src, PricedTable)
	}
	if usd <= 0 {
		t.Errorf("coding rung PriceUsage = %v, want > 0", usd)
	}
	if Classify(codingRung) != TierMid {
		t.Errorf("coding rung tier = %q, want %q", Classify(codingRung), TierMid)
	}
}

func TestLedgerAccumulatesAndSorts(t *testing.T) {
	l := NewLedger()
	l.Add("claude-haiku-4-5-20251001", Usage{InputTokens: 1000, OutputTokens: 1000})
	l.Add("claude-haiku-4-5-20251001", Usage{InputTokens: 1000})
	l.Add("unknown-model", Usage{InputTokens: 5000, OutputTokens: 5000})

	if l.TotalUSD() <= 0 {
		t.Error("total should be > 0")
	}
	b := l.Breakdown()
	if len(b) != 2 {
		t.Fatalf("breakdown len = %d, want 2", len(b))
	}
	if b[0].USD < b[1].USD {
		t.Error("breakdown should be sorted most-expensive-first")
	}
	for _, m := range b {
		if m.Model == "unknown-model" {
			if m.Priced {
				t.Error("unknown model should be flagged unpriced")
			}
			if m.PricedFrom != PricedUnpriced {
				t.Errorf("unknown model PricedFrom = %q, want unpriced", m.PricedFrom)
			}
		}
		if m.Model == "claude-haiku-4-5-20251001" && m.PricedFrom != PricedTable {
			t.Errorf("table-priced model PricedFrom = %q, want table", m.PricedFrom)
		}
	}
}

// TestProviderReportedCostWins is the bug-1046 core: a call carrying a provider
// cost is billed at that exact figure, not the static table — closing the ~4x
// over-report on a cache-heavy run.
func TestProviderReportedCostWins(t *testing.T) {
	// 17.9M input as in run-6d. Table would bill ~$4.48; the provider reported $1.11.
	usage := Usage{InputTokens: 17_912_133, OutputTokens: 37_254, ProviderCostUSD: 1.11, ProviderReported: true}

	usd, src := PriceUsage("google/gemini-3.1-flash-lite", usage)
	if src != PricedProvider {
		t.Fatalf("price source = %q, want provider", src)
	}
	if usd != 1.11 {
		t.Errorf("provider-reported cost = %v, want exactly 1.11", usd)
	}

	tableUSD, _ := PriceUsage("google/gemini-3.1-flash-lite", Usage{InputTokens: 17_912_133, OutputTokens: 37_254})
	if tableUSD <= usd*2 {
		t.Errorf("table estimate %v should be the much-higher number the provider cost replaces (>2x %v)", tableUSD, usd)
	}
}

// TestCacheReadBilledCheaper checks the fallback path bills prompt-cache reads at
// the cache rate (~0.1x), so the table no longer over-counts a cache-heavy run.
func TestCacheReadBilledCheaper(t *testing.T) {
	allFresh, _ := PriceUsage("claude-opus-4-8", Usage{InputTokens: 100_000})
	mostlyCached, _ := PriceUsage("claude-opus-4-8", Usage{InputTokens: 100_000, CachedInputTokens: 90_000})
	if mostlyCached >= allFresh {
		t.Errorf("cache-heavy call (%v) should cost less than all-fresh (%v)", mostlyCached, allFresh)
	}
	// 90% cached at 0.1x ≈ 0.19x of all-fresh; assert a real discount, not a rounding nudge.
	if mostlyCached > allFresh*0.3 {
		t.Errorf("90%% cached should be ~0.19x of fresh, got %v vs %v", mostlyCached, allFresh)
	}
}

// TestOpusRecalibrated guards the recalibration: Opus 4.8 fresh input is $5/M
// (the prior table billed $15/M — a 3x over-report).
func TestOpusRecalibrated(t *testing.T) {
	usd := Price("claude-opus-4-8", 1_000_000, 0)
	if usd != 5.0 {
		t.Errorf("opus-4-8 input/M = $%v, want $5 (recalibrated from the stale $15)", usd)
	}
	out := Price("claude-opus-4-8", 0, 1_000_000)
	if out != 25.0 {
		t.Errorf("opus-4-8 output/M = $%v, want $25", out)
	}
}

// TestPricedFromMixed checks a model priced from both a provider call and a
// table-fallback call is marked mixed (the run-rate is part-real, part-guess).
func TestPricedFromMixed(t *testing.T) {
	l := NewLedger()
	l.Add("google/gemini-3.1-flash-lite", Usage{InputTokens: 1000, ProviderCostUSD: 0.5, ProviderReported: true})
	l.Add("google/gemini-3.1-flash-lite", Usage{InputTokens: 1000}) // no provider cost → table
	b := l.Breakdown()
	if len(b) != 1 || b[0].PricedFrom != PricedMixed {
		t.Fatalf("PricedFrom = %v, want mixed", b)
	}
}

// TestCacheTokensRollUp checks the cache split accumulates onto the rollup for
// the per-model telemetry line.
func TestCacheTokensRollUp(t *testing.T) {
	l := NewLedger()
	l.Add("claude-opus-4-8", Usage{InputTokens: 1000, CachedInputTokens: 600, CacheWriteTokens: 100})
	b := l.Breakdown()
	if b[0].CachedInputTokens != 600 || b[0].CacheWriteTokens != 100 {
		t.Errorf("cache split not rolled up: %+v", b[0])
	}
}

// TestClassifyTable confirms the known price-table ids classify onto the ladder.
func TestClassifyTable(t *testing.T) {
	cases := map[string]Tier{
		"claude-opus-4-8":                  TierStrong,
		"claude-opus-4-7":                  TierStrong,
		"claude-sonnet-4-6":                TierStrong,
		"claude-haiku-4-5-20251001":        TierMid,
		"google/gemini-3.1-flash-lite":     TierMid,
		"deepseek-v4-pro":                  TierMid,
		"qwen2.5-32b":                      TierLocal,
		"Qwen2.5-32B-Instruct-Q4_K_M.gguf": TierLocal,
		"echo":                             TierLocal,
	}
	for model, want := range cases {
		if got := Classify(model); got != want {
			t.Errorf("Classify(%q) = %q, want %q", model, got, want)
		}
	}
}

// TestClassifyFamilyFallback confirms drifted/suffixed ids classify by family
// substring without a table edit, frontier families winning over cheaper ones.
func TestClassifyFamilyFallback(t *testing.T) {
	cases := map[string]Tier{
		"claude-opus-4-9-20260601":   TierStrong, // a new Opus snapshot
		"anthropic/claude-sonnet-5":  TierStrong,
		"claude-haiku-5":             TierMid,
		"google/gemini-4-flash":      TierMid,
		"deepseek-r2":                TierMid,
		"Qwen3-30B-Instruct.gguf":    TierLocal, // qwen needle wins
		"some-local-model.gguf":      TierLocal, // .gguf needle
		"echo-stub":                  TierLocal,
		"mystery-model-from-nowhere": TierUnknown,
	}
	for model, want := range cases {
		if got := Classify(model); got != want {
			t.Errorf("Classify(%q) = %q, want %q", model, got, want)
		}
	}
}
