package agent

import (
	"testing"

	"corpos/internal/model"
	"corpos/internal/profile"
)

// A profile's MaxToolRounds raises the spawned worker loop's per-cycle tool-round
// budget above the loop default; a profile that doesn't set it keeps the default.
// This is the lever that stopped a real coding fix being cut off mid-investigation
// (the t3b dogfood "exceeded max tool rounds (12)").
func TestSpawnHonorsProfileMaxToolRounds(t *testing.T) {
	t.Parallel()
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"))

	coding := s.Spawn(&profile.JobProfile{Name: "atomic-coding-chain", Tier: profile.TierLocal, MaxToolRounds: 24})
	if coding.maxRounds != 24 {
		t.Errorf("coding worker maxRounds = %d, want 24 (profile override)", coding.maxRounds)
	}

	generic := s.Spawn(&profile.JobProfile{Name: "summary", Tier: profile.TierLocal})
	if generic.maxRounds != defaultMaxRounds {
		t.Errorf("generic worker maxRounds = %d, want the loop default %d (no override)", generic.maxRounds, defaultMaxRounds)
	}
}
