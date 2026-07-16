package cost

import (
	"sync"
	"testing"
)

// A meter accumulates spend and reports Exceeded only once the cumulative total
// reaches its ceiling — the shared whole-tree bound behind bug 1124.
func TestMeterAccumulatesAndReportsExceeded(t *testing.T) {
	m := NewMeter(1.0)
	if m.Ceiling() != 1.0 {
		t.Fatalf("Ceiling = %v, want 1.0", m.Ceiling())
	}
	if m.Exceeded() {
		t.Fatal("an empty meter must not be exceeded")
	}
	if got := m.Add(0.4); got != 0.4 {
		t.Fatalf("Add returned %v, want the new total 0.4", got)
	}
	m.Add(0.4)
	if m.Total() != 0.8 {
		t.Fatalf("Total = %v, want 0.8", m.Total())
	}
	if m.Exceeded() {
		t.Fatal("0.8 < 1.0 must not be exceeded")
	}
	m.Add(0.2) // 1.0 — at the ceiling
	if !m.Exceeded() {
		t.Fatalf("Total %v >= ceiling 1.0 must be exceeded", m.Total())
	}
}

// A meter built with a non-positive ceiling tracks spend but never trips — the
// tracking-only mode used when no -max-cost-usd is set.
func TestMeterNoCeilingNeverExceeds(t *testing.T) {
	m := NewMeter(0)
	m.Add(1000)
	if m.Exceeded() {
		t.Fatal("a meter with ceiling <= 0 must never report exceeded")
	}
	if m.Total() != 1000 {
		t.Fatalf("Total = %v, want 1000", m.Total())
	}
}

// The meter is shared across concurrently-running workers, so Add must be race-safe
// (the gate runs -race).
func TestMeterConcurrentAddIsSafe(t *testing.T) {
	m := NewMeter(0)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Add(0.01)
		}()
	}
	wg.Wait()
	if got := m.Total(); got < 0.99 || got > 1.01 {
		t.Fatalf("concurrent Total = %v, want ~1.0", got)
	}
}

// A spawn budget permits Reserve up to its cap, then refuses WITHOUT consuming budget — the
// fan-out bound behind the orchestrator over-decomposition fix (Run-42: 34 workers for one task).
func TestSpawnBudgetReservesUpToCap(t *testing.T) {
	b := NewSpawnBudget(3)
	if b.Cap() != 3 {
		t.Fatalf("Cap = %d, want 3", b.Cap())
	}
	for i := 1; i <= 3; i++ {
		if !b.Reserve() {
			t.Fatalf("Reserve %d within cap must be permitted", i)
		}
		if b.Count() != i {
			t.Fatalf("Count after %d reserves = %d", i, b.Count())
		}
	}
	if b.Reserve() {
		t.Fatal("Reserve past the cap must be refused")
	}
	if b.Count() != 3 {
		t.Fatalf("a refused Reserve must not consume budget; Count = %d, want 3", b.Count())
	}
}

// A spawn budget built with a non-positive cap tracks count but never refuses — the
// unbounded/tracking-only mode when -max-spawns is 0.
func TestSpawnBudgetNoCapNeverRefuses(t *testing.T) {
	b := NewSpawnBudget(0)
	for i := 0; i < 1000; i++ {
		if !b.Reserve() {
			t.Fatalf("a budget with cap <= 0 must never refuse (refused at %d)", i)
		}
	}
	if b.Count() != 1000 {
		t.Fatalf("Count = %d, want 1000", b.Count())
	}
}

// Reserve is safe under concurrent spawns (the tree fans out workers in parallel) and never
// admits more than the cap.
func TestSpawnBudgetConcurrentReserveRespectsCap(t *testing.T) {
	const cap = 50
	b := NewSpawnBudget(cap)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Reserve() {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != cap {
		t.Fatalf("concurrent Reserve granted %d, want exactly the cap %d", granted, cap)
	}
}
