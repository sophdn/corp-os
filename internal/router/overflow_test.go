package router

import (
	"testing"

	"corpos/internal/model"
)

func topModel(r *Router) string { return r.tiers[len(r.tiers)-1].Model() }

func TestWillServeFloor(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0, WithEscalation(1, 2), WithBoundedTop(1))
	if !r.WillServeFloor() {
		t.Error("resting on floor: WillServeFloor should be true")
	}
	r.EscalateForOverflow() // floor→mid
	if r.WillServeFloor() {
		t.Error("escalated to mid: WillServeFloor should be false")
	}
	r.EscalateForOverflow() // mid→top
	if r.WillServeFloor() {
		t.Error("on the top rung: WillServeFloor should be false")
	}
}

func TestEscalateForOverflowClimbsThenSticksAtTop(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0, WithEscalation(1, 2), WithBoundedTop(1))
	if e := r.EscalateForOverflow(); e.Direction != EdgeEscalate {
		t.Fatalf("floor overflow should escalate, got %v", e.Direction)
	}
	if e := r.EscalateForOverflow(); e.Direction != EdgeEscalate {
		t.Fatalf("mid overflow should escalate to the top, got %v", e.Direction)
	}
	// On the top rung, the bound is BYPASSED for an overflow climb: NextAdapter keeps
	// serving the top, never bouncing back into the (overflowing) floor.
	top := topModel(r)
	for i := 0; i < 5; i++ {
		if got := r.NextAdapter().Model(); got != top {
			t.Fatalf("call %d: served %q, want sticky top %q (overflow must bypass the bound)", i, got, top)
		}
	}
	if r.BoundBlocked() != 0 {
		t.Errorf("a sticky-top overflow climb should never record a bound-block, got %d", r.BoundBlocked())
	}
}

func TestEscalateForOverflowUnsticksSpentBound(t *testing.T) {
	// The run-12 death path is a TWO-tier ladder (Qwen floor → Opus top): the bounded
	// top is reached + spent via a NON-overflow fault (bound applies, not sticky), so
	// NextAdapter bounces the next call straight to the FLOOR; an overflow on that
	// floor must UNSTICK so the next call serves the top instead.
	r := NewLadder([]model.Adapter{stub{id: "qwen", avail: true}, stub{id: "opus", avail: true}}, 0,
		WithEscalation(1, 2), WithBoundedTop(1))
	r.EscalateForFault(model.FaultTimeout) // floor→top (non-sticky)
	_ = r.NextAdapter()                    // serve the top once → spends the bound
	if !r.WillServeFloor() {
		t.Fatal("setup: a spent bound should bounce the next call to the floor")
	}
	e := r.EscalateForOverflow()
	if e.Direction != EdgeEscalate {
		t.Fatalf("an overflow on a bounced floor must escalate (unstick the bound), got %v", e.Direction)
	}
	if r.WillServeFloor() {
		t.Error("after the unstick, the next call must serve the top, not the floor")
	}
	if got := r.NextAdapter().Model(); got != topModel(r) {
		t.Errorf("served %q, want the top rung after the unstick", got)
	}
}

func TestEscalateForOverflowNoHigherRungIsEdgeNone(t *testing.T) {
	one := []model.Adapter{stub{id: "only", avail: true}}
	r := NewLadder(one, 0)
	if e := r.EscalateForOverflow(); e.Direction != EdgeNone {
		t.Errorf("single-tier overflow has no higher rung: want EdgeNone, got %v", e.Direction)
	}
}
