package router

import (
	"strings"
	"testing"

	"corpos/internal/model"
)

// TestEscalateForRetryExhaustionClimbsOneRung: spending a rung's whole round budget without
// converging lifts the ladder one rung (retry_exhaustion), naming the spent budget, and stops
// at the top rung.
func TestEscalateForRetryExhaustionClimbsOneRung(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0)
	e := r.EscalateForRetryExhaustion(40)
	if e.Direction != EdgeEscalate || e.Trigger != TriggerRetryExhaustion {
		t.Fatalf("round exhaustion must escalate on retry_exhaustion, got %+v", e)
	}
	if e.FromModel != "local" || e.ToModel != "mid" {
		t.Errorf("edge models = %s→%s, want local→mid", e.FromModel, e.ToModel)
	}
	if e.StateBefore != "cheap" || e.StateAfter != "escalated" {
		t.Errorf("states = %s→%s, want cheap→escalated", e.StateBefore, e.StateAfter)
	}
	if e.FiredThreshold != 40 || !strings.Contains(e.Detail, "round_budget_exhausted rounds=40") {
		t.Errorf("detail/threshold = %q / %v, want it to name the spent budget", e.Detail, e.FiredThreshold)
	}
	// Climbs again mid→strong, then refuses at the top rung.
	if e2 := r.EscalateForRetryExhaustion(40); e2.FromModel != "mid" || e2.ToModel != "strong" {
		t.Errorf("second climb = %s→%s, want mid→strong", e2.FromModel, e2.ToModel)
	}
	if e3 := r.EscalateForRetryExhaustion(40); e3.Direction != EdgeNone {
		t.Errorf("at the top rung exhaustion must NOT escalate (let the run end honestly), got %+v", e3)
	}
}

// TestEscalateForRetryExhaustion_SingleTier: with no higher rung there is nowhere to climb.
func TestEscalateForRetryExhaustion_SingleTier(t *testing.T) {
	r := NewLadder([]model.Adapter{stub{"only", true}}, 0)
	if e := r.EscalateForRetryExhaustion(12); e.Direction != EdgeNone {
		t.Errorf("a single-tier ladder cannot escalate, got %+v", e)
	}
}
