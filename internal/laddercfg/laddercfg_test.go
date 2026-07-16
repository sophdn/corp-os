package laddercfg

import (
	"testing"

	"corpos/internal/profile"
)

// TestCompactionBudgetForWindow locks the latency cap: a very wide cloud window (Gemini ~1M, Opus
// 200k) is bounded at MaxLiveContextBudget so the live context cannot grow to a timeout-inducing
// size before compacting, while the local floor and a narrow cloud window keep a budget sized
// below their own window (overflow safety).
func TestCompactionBudgetForWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		window int
		want   int
	}{
		{"local floor preserved", 8192, 6144},                           // 8192 - 2048 reserve
		{"future 64k cloud model sized below its window", 64000, 59904}, // 64000 - 4096, no overflow
		{"opus 200k capped", 200_000, MaxLiveContextBudget},
		{"gemini ~1M capped", 1_000_000, MaxLiveContextBudget},
		{"tiny window floors at 1", 1, 1},
	}
	for _, c := range cases {
		if got := CompactionBudgetForWindow(c.window); got != c.want {
			t.Errorf("%s: CompactionBudgetForWindow(%d) = %d, want %d", c.name, c.window, got, c.want)
		}
	}
}

// TestSkillInjectionBudget: a 0 window means no cap (full bodies); otherwise it is a third of the
// window's compaction budget (the floor-fit keystone — ~2048 tok on the 8192 local floor).
func TestSkillInjectionBudget(t *testing.T) {
	t.Parallel()
	if got := SkillInjectionBudget(0); got != 0 {
		t.Errorf("SkillInjectionBudget(0) = %d, want 0 (no cap)", got)
	}
	if got := SkillInjectionBudget(8192); got != 6144/SkillBudgetFraction {
		t.Errorf("SkillInjectionBudget(8192) = %d, want %d", got, 6144/SkillBudgetFraction)
	}
}

// TestFloorForTier: nil/local rest on the base rung; mid/strong promote to their rung, clamping
// down to the nearest configured one when the requested rung is absent (-1).
func TestFloorForTier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		p                *profile.JobProfile
		midIdx, strongIx int
		want             int
	}{
		{"nil profile → base", nil, 1, 2, 0},
		{"local → base", &profile.JobProfile{Tier: profile.TierLocal}, 1, 2, 0},
		{"mid → mid rung", &profile.JobProfile{Tier: profile.TierMid}, 1, 2, 1},
		{"mid but no mid rung → base", &profile.JobProfile{Tier: profile.TierMid}, -1, 2, 0},
		{"strong → strong rung", &profile.JobProfile{Tier: profile.TierStrong}, 1, 2, 2},
		{"strong but no strong rung → mid", &profile.JobProfile{Tier: profile.TierStrong}, 1, -1, 1},
		{"strong but neither → base", &profile.JobProfile{Tier: profile.TierStrong}, -1, -1, 0},
	}
	for _, c := range cases {
		if got := FloorForTier(c.p, c.midIdx, c.strongIx); got != c.want {
			t.Errorf("%s: FloorForTier = %d, want %d", c.name, got, c.want)
		}
	}
}
