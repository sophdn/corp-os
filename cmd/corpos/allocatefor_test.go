package main

import (
	"io"
	"testing"

	"corpos/internal/laddercfg"
)

func TestAllocateFor(t *testing.T) {
	const w = 8192 // local floor → auto fidelity = low
	// auto derives from the window.
	if a := allocateFor(w, "auto", io.Discard); a.Fidelity != laddercfg.FidelityLow {
		t.Errorf(`"auto" at %d should derive low, got %s`, w, a.Fidelity)
	}
	// empty behaves like auto.
	if a := allocateFor(w, "", io.Discard); a.Fidelity != laddercfg.FidelityLow {
		t.Errorf(`"" should behave like auto, got %s`, a.Fidelity)
	}
	// a pinned preset overrides the window-derived level.
	if a := allocateFor(w, "extreme", io.Discard); a.Fidelity != laddercfg.FidelityExtreme {
		t.Errorf(`pinned "extreme" should override, got %s`, a.Fidelity)
	}
	// an unknown value falls back to auto (no panic, warned).
	if a := allocateFor(w, "bogus", io.Discard); a.Fidelity != laddercfg.FidelityLow {
		t.Errorf(`unknown fidelity should fall back to auto, got %s`, a.Fidelity)
	}
}
