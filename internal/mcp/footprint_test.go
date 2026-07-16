package mcp

import (
	"context"
	"testing"

	"corpos/internal/tool"
)

func TestFootprint_CountsAndTotals(t *testing.T) {
	t.Parallel()
	specs := []tool.Spec{
		{Name: "a", Description: "1234", InputSchema: map[string]any{"x": "y"}}, // schema {"x":"y"} = 9 bytes
		{Name: "b", Description: "12345678", InputSchema: nil},                  // schema "null" = 4 bytes
	}
	per, total, enriched := Footprint(specs)
	if len(per) != 2 {
		t.Fatalf("per-spec len = %d, want 2", len(per))
	}
	// Neither spec carries the enriched action-enum shape, so both are thin.
	if enriched != 0 {
		t.Errorf("enriched count = %d, want 0 (both specs are thin)", enriched)
	}
	if per[0].Enriched || per[1].Enriched {
		t.Errorf("thin specs flagged enriched: %+v %+v", per[0], per[1])
	}
	if per[0].DescriptionBytes != 4 || per[0].SchemaBytes != 9 {
		t.Errorf("spec a sizes = %+v", per[0])
	}
	if per[0].ApproxTokens != (4+9+3)/4 {
		t.Errorf("spec a tokens = %d, want %d", per[0].ApproxTokens, (4+9+3)/4)
	}
	if per[1].DescriptionBytes != 8 || per[1].SchemaBytes != 4 {
		t.Errorf("spec b sizes = %+v", per[1])
	}
	want := per[0].ApproxTokens + per[1].ApproxTokens
	if total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
}

func TestFootprint_ProjectionShrinksTheTax(t *testing.T) {
	t.Parallel()
	// The whole point: a projected subset must cost strictly fewer approx tokens
	// than the full enriched set.
	full := EnrichedSpecs(context.Background(), newScripted())
	_, fullTokens, fullEnriched := Footprint(full)

	projected := Project(full, Scope{"work": {"chain_status"}})
	_, projTokens, projEnriched := Footprint(projected)

	if projTokens >= fullTokens {
		t.Errorf("projection did not shrink the tax: projected %d >= full %d", projTokens, fullTokens)
	}
	if projTokens == 0 {
		t.Error("projected footprint should be non-zero (work/chain_status kept)")
	}
	// The scripted substrate enriches the surfaces it scripts (one surface is not
	// scripted and thin-falls), so the enriched count is the honest-gauge signal the
	// bug 1066 secondary adds: it must agree with a direct IsEnriched sweep and be
	// non-zero. A projected enriched spec keeps its enum, so it stays enriched too.
	wantEnriched := 0
	for _, s := range full {
		if IsEnriched(s) {
			wantEnriched++
		}
	}
	if fullEnriched != wantEnriched || fullEnriched == 0 {
		t.Errorf("enriched count = %d, want %d (and non-zero)", fullEnriched, wantEnriched)
	}
	if projEnriched == 0 {
		t.Error("a projected enriched spec should still be flagged enriched (keeps its action enum)")
	}
}

// TestFootprint_EnrichedVsThinFlag pins the IsEnriched predicate the honest gauge
// rests on: the enriched envelope (action enum) reads enriched; the thin static
// envelope (no enum) reads thin.
func TestFootprint_EnrichedVsThinFlag(t *testing.T) {
	t.Parallel()
	enriched := EnvelopeSpec("work", "purpose", []string{"a", "b"}, []string{"a()", "b()"})
	if !IsEnriched(enriched) {
		t.Error("an enriched envelope (action enum) should read enriched")
	}
	thin := tool.Spec{Name: "work", Description: "purpose", InputSchema: envelopeSchema()}
	if IsEnriched(thin) {
		t.Error("the thin static envelope (no enum) should read NOT enriched")
	}
}
