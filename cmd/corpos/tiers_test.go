package main

import (
	"testing"

	"corpos/internal/model"
	"corpos/internal/profile"
)

// TestRunAdapterPromotesByTier locks the orchestrate window-sizing fix: a
// tier=mid/strong profile must run on (and be window-sized to) the promoted rung,
// clamping down when that rung is unconfigured.
func TestRunAdapterPromotesByTier(t *testing.T) {
	t.Parallel()
	base := model.NewEcho("base")
	mid := model.NewEcho("mid")
	strong := model.NewEcho("strong")
	full := tierSet{base: base, mid: mid, strong: strong}

	cases := []struct {
		name string
		ts   tierSet
		p    *profile.JobProfile
		want model.Adapter
	}{
		{"nil profile rests on base", full, nil, base},
		{"local rests on base", full, &profile.JobProfile{Tier: profile.TierLocal}, base},
		{"mid promotes to mid", full, &profile.JobProfile{Tier: profile.TierMid}, mid},
		{"strong promotes to strong", full, &profile.JobProfile{Tier: profile.TierStrong}, strong},
		{"mid with no mid rung falls to base", tierSet{base: base}, &profile.JobProfile{Tier: profile.TierMid}, base},
		{"strong clamps to mid when no strong rung", tierSet{base: base, mid: mid}, &profile.JobProfile{Tier: profile.TierStrong}, mid},
	}
	for _, c := range cases {
		if got := c.ts.runAdapter(c.p); got != c.want {
			t.Errorf("%s: runAdapter = %q, want %q", c.name, got.Model(), c.want.Model())
		}
	}
}
