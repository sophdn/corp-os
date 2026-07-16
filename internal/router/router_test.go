package router

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/tool"
)

// stub is a model.Adapter with a fixed id and availability.
type stub struct {
	id    string
	avail bool
}

func (s stub) Model() string   { return s.id }
func (s stub) Available() bool { return s.avail }
func (s stub) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	return model.Response{Model: s.id}, nil
}

func TestSingleTierDoesNotEscalate(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true})
	if r.NextAdapter().Model() != "cheap" {
		t.Error("default tier should be cheap")
	}
	r.Observe(Signals{ToolErrors: 99}) // escalation not configured
	if r.State() != StateCheap {
		t.Error("single-tier router must never escalate")
	}
}

func TestColdStartFallbackToStrong(t *testing.T) {
	r := New(stub{"cheap", false}, stub{"strong", true})
	if r.NextAdapter().Model() != "strong" {
		t.Error("unavailable cheap tier should fall back to strong")
	}
	if r.ColdStartFallbacks() != 1 {
		t.Errorf("cold-start fallbacks = %d, want 1", r.ColdStartFallbacks())
	}
}

func TestStrongUnavailableStaysCheap(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", false}, WithEscalation(1, 2))
	r.Observe(Signals{ToolErrors: 1})
	if r.State() != StateEscalated {
		t.Fatal("should be escalated after the threshold")
	}
	if r.NextAdapter().Model() != "cheap" {
		t.Error("unavailable strong tier should degrade to cheap")
	}
	if r.StrongUnavailableFallbacks() != 1 {
		t.Errorf("strong-unavailable fallbacks = %d, want 1", r.StrongUnavailableFallbacks())
	}
}

func TestEscalateThenDeescalate(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithEscalation(2, 2))
	r.Observe(Signals{ToolErrors: 2})
	if r.State() != StateEscalated {
		t.Fatal("should escalate at the threshold")
	}
	if r.NextAdapter().Model() != "strong" {
		t.Error("escalated state should route to strong")
	}
	r.Observe(Signals{}) // clean turn 1
	if r.State() != StateEscalated {
		t.Error("one clean turn should not de-escalate yet")
	}
	r.Observe(Signals{}) // clean turn 2
	if r.State() != StateCheap {
		t.Error("should de-escalate after the configured clean turns")
	}
}

func TestEscalatedResetsStreakOnError(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithEscalation(1, 2))
	r.Observe(Signals{ToolErrors: 1}) // escalate
	r.Observe(Signals{})              // clean 1
	// A FIRED trigger while escalated resets the clean streak (ported reference
	// semantics: only a fired trigger — not a sub-threshold signal — resets it).
	// At the top rung this is saturated: no further climb, no edge, streak reset.
	r.Observe(Signals{ToolErrors: 1})
	r.Observe(Signals{}) // clean 1 again (not 2)
	if r.State() != StateEscalated {
		t.Error("a fired trigger should reset de-escalation; still escalated")
	}
}

// localMidStrong is the three-rung ladder of the 2026-06-02 routing decision.
func localMidStrong(avail bool) []model.Adapter {
	return []model.Adapter{stub{"local", true}, stub{"mid", true}, stub{"strong", avail}}
}

func TestLadderClimbsOneRungPerEscalation(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0, WithEscalation(1, 2))
	if r.NextAdapter().Model() != "local" {
		t.Fatalf("floor rung should be local, got %s", r.NextAdapter().Model())
	}
	r.Observe(Signals{ToolErrors: 1}) // local → mid
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Fatalf("first escalation should climb to mid, got %s", got)
	}
	r.Observe(Signals{ToolErrors: 1}) // mid → strong
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Fatalf("second escalation should climb to strong, got %s", got)
	}
	r.Observe(Signals{ToolErrors: 1}) // already at the top — stays strong
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Errorf("a climb at the top rung should stay strong, got %s", got)
	}
}

func TestLadderDescendsOneRungPerCleanStreak(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0, WithEscalation(1, 2))
	r.Observe(Signals{ToolErrors: 1}) // → mid
	r.Observe(Signals{ToolErrors: 1}) // → strong
	r.Observe(Signals{})              // clean 1
	r.Observe(Signals{})              // clean 2 → descend strong → mid
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Fatalf("two clean turns should descend one rung to mid, got %s", got)
	}
	r.Observe(Signals{}) // clean 1
	r.Observe(Signals{}) // clean 2 → descend mid → local (the floor)
	if got := r.NextAdapter().Model(); got != "local" {
		t.Errorf("further clean turns should descend to the floor (local), got %s", got)
	}
	if r.State() != StateCheap {
		t.Errorf("resting on the floor should report StateCheap")
	}
}

func TestMidFloorRestsOnMidAndDescendsNoLower(t *testing.T) {
	// A mid-floor worker (the orchestrator) rests on mid, climbs to strong, and
	// descends back to mid — never down to local.
	r := NewLadder(localMidStrong(true), 1, WithEscalation(1, 2))
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Fatalf("mid-floor router should rest on mid, got %s", got)
	}
	r.Observe(Signals{ToolErrors: 1}) // mid → strong
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Fatalf("mid-floor router should escalate to strong, got %s", got)
	}
	r.Observe(Signals{}) // clean 1
	r.Observe(Signals{}) // clean 2 → descend to the mid floor
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Errorf("mid-floor router should descend no lower than mid, got %s", got)
	}
}

func TestBoundedTopRefusesClimbWhenSpent(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0, WithEscalation(1, 2), WithBoundedTop(1))
	r.Observe(Signals{ToolErrors: 1})                    // → mid
	r.Observe(Signals{ToolErrors: 1})                    // → strong
	if got := r.NextAdapter().Model(); got != "strong" { // first Opus turn allowed
		t.Fatalf("first strong turn should serve strong, got %s", got)
	}
	if r.BoundedTurns() != 1 {
		t.Errorf("bounded turns = %d, want 1", r.BoundedTurns())
	}
	if got := r.NextAdapter().Model(); got != "mid" { // bound spent → drop a rung
		t.Errorf("a spent bound should refuse strong and drop to mid, got %s", got)
	}
	if r.BoundBlocked() != 1 {
		t.Errorf("bound-blocked = %d, want 1", r.BoundBlocked())
	}
}

func TestBoundMaxReportsConfiguredCap(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0, WithBoundedTop(3))
	if r.BoundMax() != 3 {
		t.Errorf("BoundMax = %d, want 3", r.BoundMax())
	}
	if u := New(stub{"c", true}, stub{"s", true}); u.BoundMax() != 0 {
		t.Errorf("an unbounded router should report BoundMax 0, got %d", u.BoundMax())
	}
}

func TestLadderColdStartClimbsToFirstAvailable(t *testing.T) {
	// Floor (local) cold AND mid cold: climb past both to the first available rung.
	tiers := []model.Adapter{stub{"local", false}, stub{"mid", false}, stub{"strong", true}}
	r := NewLadder(tiers, 0)
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Fatalf("a cold floor should climb to the first available rung, got %s", got)
	}
	if r.ColdStartFallbacks() != 1 {
		t.Errorf("cold-start fallbacks = %d, want 1", r.ColdStartFallbacks())
	}
}

func TestNewLadderClampsFloor(t *testing.T) {
	r := NewLadder(localMidStrong(true), 9) // out of range → clamps to the top
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Errorf("an over-range floor should clamp to the top rung, got %s", got)
	}
}

func TestNewLadderClampsNegativeFloor(t *testing.T) {
	r := NewLadder(localMidStrong(true), -3) // below range → clamps to rung 0
	if got := r.NextAdapter().Model(); got != "local" {
		t.Errorf("a negative floor should clamp to the lowest rung, got %s", got)
	}
}

func TestNextAdapterNoAvailableNeighbourReturnsChosen(t *testing.T) {
	// Every rung down: neither fallback direction finds an available rung, so the
	// chosen (unavailable) adapter is returned unchanged rather than panicking.
	allDown := []model.Adapter{stub{"local", false}, stub{"mid", false}, stub{"strong", false}}
	floor0 := NewLadder(allDown, 0)
	if got := floor0.NextAdapter().Model(); got != "local" {
		t.Errorf("a cold floor with no available higher rung should return the floor, got %s", got)
	}
	topFloor := NewLadder(allDown, 2)
	if got := topFloor.NextAdapter().Model(); got != "strong" {
		t.Errorf("an unavailable top rung with no available lower rung should return the top, got %s", got)
	}
}

func TestSingleRungLadderNeverEscalates(t *testing.T) {
	r := NewLadder([]model.Adapter{stub{"only", true}}, 0, WithEscalation(1, 2))
	r.Observe(Signals{ToolErrors: 9})
	if got := r.NextAdapter().Model(); got != "only" {
		t.Errorf("a single-rung ladder has nowhere to climb, got %s", got)
	}
	if r.State() != StateCheap {
		t.Errorf("a single-rung ladder is always at its floor")
	}
}

// DeEscalateToFloor drops straight back to the free floor (rate-limit recovery),
// regardless of how many rungs up the router currently sits; at the floor it is a
// no-op.
func TestDeEscalateToFloorDropsStraightToFloor(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0)
	r.EscalateForFault(model.FaultMalformedToolCall) // local → mid
	r.EscalateForFault(model.FaultContextOverflow)   // mid → strong
	if got := r.NextAdapter().Model(); got != "strong" {
		t.Fatalf("setup: expected strong rung, got %s", got)
	}
	edge := r.DeEscalateToFloor()
	if edge.Direction != EdgeDeescalate || edge.FromModel != "strong" || edge.ToModel != "local" {
		t.Fatalf("DeEscalateToFloor edge = %+v, want strong→local de-escalate", edge)
	}
	if got := r.NextAdapter().Model(); got != "local" {
		t.Fatalf("after de-escalate, rung = %s, want local floor", got)
	}
	if e := r.DeEscalateToFloor(); e.Direction != EdgeNone {
		t.Fatalf("at floor DeEscalateToFloor = %+v, want EdgeNone", e)
	}
}

func TestEscalateForStuckVerify(t *testing.T) {
	tiers := []model.Adapter{stub{"local", true}, stub{"mid", true}, stub{"strong", true}}
	r := NewLadder(tiers, 0)

	e1 := r.EscalateForStuckVerify(2)
	if e1.Direction != EdgeEscalate || e1.FromModel != "local" || e1.ToModel != "mid" {
		t.Fatalf("first stuck-verify escalation = %+v, want local→mid escalate", e1)
	}
	if e1.Trigger != TriggerRetryExhaustion {
		t.Errorf("trigger = %q, want retry_exhaustion (closed taxonomy, like EscalateForFault)", e1.Trigger)
	}
	if e1.FiredThreshold != 2 {
		t.Errorf("FiredThreshold = %v, want the consecutive-failure count (2)", e1.FiredThreshold)
	}
	if !strings.Contains(e1.Detail, "verify_stuck") || !strings.Contains(e1.Detail, "consecutive_verify_fails=2") {
		t.Errorf("detail = %q, want it to name verify_stuck + the count", e1.Detail)
	}

	// Still stuck on the new rung → climb again to the top.
	if e2 := r.EscalateForStuckVerify(4); e2.Direction != EdgeEscalate || e2.ToModel != "strong" {
		t.Fatalf("second escalation = %+v, want mid→strong", e2)
	}
	// At the top rung → no further climb.
	if e3 := r.EscalateForStuckVerify(6); e3.Direction != EdgeNone {
		t.Fatalf("at the top rung want EdgeNone, got %+v", e3)
	}
}

func TestEscalateForStuckVerify_SingleTier(t *testing.T) {
	r := NewLadder([]model.Adapter{stub{"only", true}}, 0)
	if e := r.EscalateForStuckVerify(2); e.Direction != EdgeNone {
		t.Fatalf("a single-tier ladder has no rung to climb to, got %+v", e)
	}
}
