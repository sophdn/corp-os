package agent

import (
	"context"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
)

// TestRunStampsHighestTierModel: every Run return path carries the highest rung the
// router served, so the coding orchestrator can read the reached tier off the Result
// (chain 392 task 3314). An Echo with no script ends the turn on the floor rung.
func TestRunStampsHighestTierModel(t *testing.T) {
	r := router.NewLadder([]model.Adapter{model.NewEcho("local"), model.NewEcho("mid")}, 0)
	l := New(r, &fakeProvider{}, nil)
	res, err := l.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.HighestTierModel != "local" {
		t.Fatalf("highest tier = %q, want local (floor)", res.HighestTierModel)
	}
}

// TestWithStartRungLiftsLoopFloor: a carried start-rung lifts the loop's router floor
// so the worker begins resting on that tier rather than the local floor (chain 392
// task 3314).
func TestWithStartRungLiftsLoopFloor(t *testing.T) {
	r := router.NewLadder([]model.Adapter{model.NewEcho("local"), model.NewEcho("mid")}, 0)
	l := New(r, &fakeProvider{}, nil, WithStartRung("mid"))
	res, err := l.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.HighestTierModel != "mid" {
		t.Fatalf("WithStartRung should start the loop on mid, got %q", res.HighestTierModel)
	}
}
