package cost

import (
	"sync"
	"testing"
)

// A StrongTurnBudget permits strong-rung turns up to its cap, then reports Exhausted — the
// turn analogue of SpawnBudget's spawn cap. Take tallies a served turn; Exhausted flips once
// the cap is reached.
func TestStrongTurnBudgetExhaustsAtCap(t *testing.T) {
	b := NewStrongTurnBudget(2)
	if b.Cap() != 2 {
		t.Fatalf("Cap = %d, want 2", b.Cap())
	}
	if b.Exhausted() {
		t.Fatal("a fresh budget with turns remaining must not be exhausted")
	}
	b.Take()
	if b.Exhausted() {
		t.Fatalf("after 1 of 2 turns the budget is not yet exhausted (count=%d)", b.Count())
	}
	b.Take()
	if !b.Exhausted() {
		t.Fatalf("after the full cap (%d/%d) the budget must report exhausted", b.Count(), b.Cap())
	}
	if b.Count() != 2 {
		t.Fatalf("Count = %d, want 2", b.Count())
	}
}

// A budget built with a non-positive cap tracks served turns but never reports exhausted — the
// tracking-only mode when -max-strong-turns is 0 (default-off, matching -max-cost-usd).
func TestStrongTurnBudgetNoCapNeverExhausts(t *testing.T) {
	b := NewStrongTurnBudget(0)
	for i := 0; i < 1000; i++ {
		if b.Exhausted() {
			t.Fatalf("a budget with cap <= 0 must never report exhausted (did at %d)", i)
		}
		b.Take()
	}
	if b.Count() != 1000 {
		t.Fatalf("Count = %d, want 1000", b.Count())
	}
}

// Take is safe under concurrent strong turns (the tree may run workers in parallel); Count
// reflects every served turn exactly.
func TestStrongTurnBudgetConcurrentTakeIsSafe(t *testing.T) {
	b := NewStrongTurnBudget(0) // tracking-only so every Take lands
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Take()
		}()
	}
	wg.Wait()
	if b.Count() != 200 {
		t.Fatalf("concurrent Take Count = %d, want 200", b.Count())
	}
}
