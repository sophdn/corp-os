package agent

import (
	"testing"

	"corpos/internal/model"
	"corpos/internal/profile"
)

// TestRouterForFloorPinnedToProfileTier is a CHARACTERIZATION test (chain 392 task
// 3313). The per-worker router the spawner builds rests SOLELY on the rung the
// profile's Tier selects — there is no carried-floor input today. So every respawn
// of the same coding atom restarts the router at profile.Tier (Qwen), discarding the
// tier a prior attempt escalated to: the tree re-pays the local→mid→coder→strong
// climb each respawn (the rehearsal's "12 workers re-climbing"). Task 3314 adds a
// carried floor (WithStartRung) that lifts the floor above profile.Tier; this test
// pins the no-carry default that fix builds on (it stays the floor when nothing is
// carried).
func TestRouterForFloorPinnedToProfileTier(t *testing.T) {
	t.Parallel()
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithCodingRung(model.NewEcho("coding")),
		WithStrongTier(model.NewEcho("strong"), 1),
		WithStrongBound(1))

	for _, c := range []struct {
		tier profile.Tier
		want string
	}{
		{profile.TierLocal, "local"},
		{profile.TierMid, "mid"},
	} {
		p := &profile.JobProfile{Tier: c.tier, CodingRung: true, EscalateOn: []string{"tool_error"}}
		rt := s.routerFor(p)
		// The router rests on the rung profile.Tier picks; nothing starts it higher
		// today. T2 (3314) lifts this for a respawn carrying an escalated tier.
		if got := rt.CurrentModel(); got != c.want {
			t.Errorf("tier=%s: router rests on %q, want %q (floor is profile.Tier-only)", c.tier, got, c.want)
		}
	}
}
