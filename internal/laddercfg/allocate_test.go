package laddercfg

import "testing"

func TestFidelityFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		window int
		want   Fidelity
	}{
		{0, FidelityHigh},         // undetected → neutral default
		{-5, FidelityHigh},        // negative → neutral default
		{8192, FidelityLow},       // local floor
		{16383, FidelityLow},      // just under the mid threshold
		{16384, FidelityMid},      // mid threshold
		{65535, FidelityMid},      // just under high
		{65536, FidelityHigh},     // high threshold
		{128000, FidelityHigh},    // a typical cloud window
		{200000, FidelityExtreme}, // extreme threshold (Opus)
		{1000000, FidelityExtreme},
	}
	for _, c := range cases {
		if got := FidelityFor(c.window); got != c.want {
			t.Errorf("FidelityFor(%d) = %s, want %s", c.window, got, c.want)
		}
	}
}

func TestFidelity_StringAndParse(t *testing.T) {
	t.Parallel()
	for _, f := range []Fidelity{FidelityLow, FidelityMid, FidelityHigh, FidelityExtreme} {
		got, ok := ParseFidelity(f.String())
		if !ok || got != f {
			t.Errorf("round-trip %s: ParseFidelity=%s ok=%v", f, got, ok)
		}
	}
	if _, ok := ParseFidelity("auto"); ok {
		t.Error(`"auto" is not a fidelity preset; ParseFidelity should report ok=false`)
	}
	if Fidelity(99).String() != "unknown" {
		t.Errorf("out-of-range fidelity should render unknown, got %q", Fidelity(99).String())
	}
}

func TestAllocate_NonRegression(t *testing.T) {
	t.Parallel()
	// The compaction + skill budgets must equal the proven window-only formulas (the
	// profile path must not regress).
	for _, w := range []int{8192, 32768, 128000, 200000, 1000000} {
		a := Allocate(w)
		if a.CompactionBudget != CompactionBudgetForWindow(w) {
			t.Errorf("window %d: CompactionBudget=%d, want %d", w, a.CompactionBudget, CompactionBudgetForWindow(w))
		}
		if a.SkillBudget != SkillInjectionBudget(w) {
			t.Errorf("window %d: SkillBudget=%d, want %d", w, a.SkillBudget, SkillInjectionBudget(w))
		}
		if a.Fidelity != FidelityFor(w) {
			t.Errorf("window %d: Fidelity=%s, want %s", w, a.Fidelity, FidelityFor(w))
		}
	}
}

func TestAllocate_ZeroWindowUncapped(t *testing.T) {
	t.Parallel()
	a := Allocate(0)
	if a.CompactionBudget != 0 || a.SkillBudget != 0 || a.InjectBudget != 0 || a.PerResultCap != 0 {
		t.Fatalf("an undetected window must yield an all-uncapped allocation (compaction off), got %+v", a)
	}
}

func TestAllocate_FixedCapsTuneDownWithFidelity(t *testing.T) {
	t.Parallel()
	// Criterion 2: a small window auto-tunes the fixed caps DOWN relative to a big one.
	small := Allocate(8192) // FidelityLow
	big := Allocate(256000) // FidelityExtreme
	if small.InjectBudget <= 0 {
		t.Fatal("a detected window should set a positive inject budget")
	}
	if small.PerResultCap <= 0 {
		t.Fatal("a low-fidelity per-result cap should be positive (capped)")
	}
	// Extreme lets one result use the whole transcript budget (effectively uncapped),
	// while Low restricts it to a quarter — so Extreme's per-result cap is far larger.
	if big.PerResultCap != big.CompactionBudget {
		t.Fatalf("extreme per-result cap should equal the whole compaction budget, got %d (budget %d)", big.PerResultCap, big.CompactionBudget)
	}
	if small.PerResultCap >= big.PerResultCap {
		t.Fatalf("low-fidelity per-result cap (%d) should be tighter than extreme (%d)", small.PerResultCap, big.PerResultCap)
	}
	// The low-fidelity inject cap is a smaller FRACTION of its (smaller) budget than high's.
	highFrac := float64(Allocate(128000).InjectBudget) / float64(Allocate(128000).CompactionBudget)
	lowFrac := float64(small.InjectBudget) / float64(small.CompactionBudget)
	if lowFrac >= highFrac {
		t.Errorf("low fidelity inject fraction (%.3f) should be tighter than high (%.3f)", lowFrac, highFrac)
	}
}

func TestAllocateAt_OverrideGovernsCapsNotCompaction(t *testing.T) {
	t.Parallel()
	w := 8192
	// Pinning Extreme on a small window lifts the per-result cap but keeps the
	// window-derived compaction/skill budgets (no regression of the proven path).
	a := AllocateAt(w, FidelityExtreme)
	if a.PerResultCap != a.CompactionBudget {
		t.Errorf("pinned extreme should let one result use the whole budget %d, got %d", a.CompactionBudget, a.PerResultCap)
	}
	if a.CompactionBudget != CompactionBudgetForWindow(w) || a.SkillBudget != SkillInjectionBudget(w) {
		t.Errorf("override must not change the window-derived compaction/skill budgets: %+v", a)
	}
	// Pinning Low on a big window tightens the per-result cap below Extreme's (uncapped).
	if low := AllocateAt(256000, FidelityLow); low.PerResultCap <= 0 {
		t.Errorf("pinned low on a wide window should impose a positive per-result cap, got %d", low.PerResultCap)
	}
}

func TestPerResultCap_Floor(t *testing.T) {
	t.Parallel()
	// A tiny budget still yields a usable (floored) per-result cap.
	if got := perResultCap(100, FidelityLow); got != perResultCapFloor {
		t.Errorf("perResultCap(100,low) = %d, want floor %d", got, perResultCapFloor)
	}
}
