package router

import (
	"testing"

	"corpos/internal/model"
)

func ladder3() []model.Adapter {
	return []model.Adapter{model.NewEcho("local"), model.NewEcho("mid"), model.NewEcho("strong")}
}

// TestHighestModelTracksServedRung: HighestModel reports the highest rung NextAdapter
// actually SERVED, and never regresses on de-escalation (chain 392 task 3314 — the
// signal the coding orchestrator carries into the next respawn).
func TestHighestModelTracksServedRung(t *testing.T) {
	r := NewLadder(ladder3(), 0, WithEscalation(1, 1))
	if got := r.NextAdapter().Model(); got != "local" {
		t.Fatalf("floor served %q, want local", got)
	}
	if got := r.HighestModel(); got != "local" {
		t.Fatalf("highest = %q, want local", got)
	}
	r.Observe(Signals{ToolErrors: 1}) // escalate one rung
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Fatalf("after escalate served %q, want mid", got)
	}
	if got := r.HighestModel(); got != "mid" {
		t.Fatalf("highest = %q, want mid", got)
	}
	r.Observe(Signals{}) // clean turn → de-escalate toward floor
	r.NextAdapter()      // back on local
	if got := r.HighestModel(); got != "mid" {
		t.Fatalf("highest regressed to %q on de-escalation, want mid", got)
	}
}

// TestLiftFloorToModel: the lift raises the resting floor to the named rung; an
// unknown id or one at/below the current floor is a no-op (chain 392 task 3314).
func TestLiftFloorToModel(t *testing.T) {
	r := NewLadder(ladder3(), 0, WithEscalation(1, 2))
	r.LiftFloorToModel("mid")
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Fatalf("after lift served %q, want mid (floor raised)", got)
	}
	if got := r.HighestModel(); got != "mid" {
		t.Fatalf("lift should set highest; got %q", got)
	}
	r.LiftFloorToModel("nope")  // unknown id: no-op
	r.LiftFloorToModel("local") // at/below floor: no-op
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Fatalf("no-op lifts must not lower the floor; served %q", got)
	}
}

// TestLiftFloorCappedBelowBoundedTop: a carried floor caps one rung below the
// usage-bounded top, so it may rest on mid but never on the bounded strong (Opus)
// rung — the frontier stays escalation-only (chain 392 task 3314).
func TestLiftFloorCappedBelowBoundedTop(t *testing.T) {
	r := NewLadder(ladder3(), 0, WithEscalation(1, 2), WithBoundedTop(1))
	r.LiftFloorToModel("strong")
	if got := r.NextAdapter().Model(); got != "mid" {
		t.Fatalf("lift to the bounded top should cap at mid, served %q", got)
	}
	// A 2-rung bounded ladder: capping to one below the top lands at the floor → no-op.
	r2 := NewLadder([]model.Adapter{model.NewEcho("local"), model.NewEcho("strong")}, 0, WithBoundedTop(1))
	r2.LiftFloorToModel("strong")
	if got := r2.NextAdapter().Model(); got != "local" {
		t.Fatalf("cap-to-floor lift must be a no-op, served %q", got)
	}
}
