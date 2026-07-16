package agent

import (
	"slices"
	"testing"

	"corpos/internal/model"
	"corpos/internal/profile"
)

// TestLadderForInsertsCodingRung verifies the DeepSeek coder rung lands between
// mid and strong for a CodingRung profile, and is absent for one that did not opt
// in — the atomic-coding-chain escalation path (ATOMIC_CODING_CHAIN.md §5.8).
func TestLadderForInsertsCodingRung(t *testing.T) {
	t.Parallel()
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithCodingRung(model.NewEcho("coding")),
		WithStrongTier(model.NewEcho("strong"), 1))

	names := func(p *profile.JobProfile) []string {
		tiers, _ := s.ladderFor(p)
		out := make([]string, len(tiers))
		for i, a := range tiers {
			out[i] = a.Model()
		}
		return out
	}

	if got, want := names(&profile.JobProfile{Tier: profile.TierLocal, CodingRung: true}),
		[]string{"local", "mid", "coding", "strong"}; !slices.Equal(got, want) {
		t.Errorf("coding-rung ladder = %v, want %v", got, want)
	}
	if got, want := names(&profile.JobProfile{Tier: profile.TierLocal}),
		[]string{"local", "mid", "strong"}; !slices.Equal(got, want) {
		t.Errorf("non-coding ladder (no opt-in) = %v, want %v", got, want)
	}
}

// TestLadderForCodingRungFloorAnchors confirms the inserted coder rung does not
// move the floor anchors: a tier=mid coding profile still rests on mid and a
// tier=strong one on strong (the rung is an escalation step, never a floor).
func TestLadderForCodingRungFloorAnchors(t *testing.T) {
	t.Parallel()
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"),
		WithMidTier(model.NewEcho("mid")),
		WithCodingRung(model.NewEcho("coding")),
		WithStrongTier(model.NewEcho("strong"), 1))

	if tiers, floor := s.ladderFor(&profile.JobProfile{Tier: profile.TierMid, CodingRung: true}); tiers[floor].Model() != "mid" {
		t.Errorf("tier=mid floor = %s, want mid", tiers[floor].Model())
	}
	if tiers, floor := s.ladderFor(&profile.JobProfile{Tier: profile.TierStrong, CodingRung: true}); tiers[floor].Model() != "strong" {
		t.Errorf("tier=strong floor = %s, want strong", tiers[floor].Model())
	}
}

// TestWithCodingRungNilIgnored locks the nil-guard.
func TestWithCodingRungNilIgnored(t *testing.T) {
	t.Parallel()
	s := NewSpawner(&fakeProvider{}, nilProject, nil, model.NewEcho("local"), WithCodingRung(nil))
	if s.hasCoding {
		t.Error("a nil coding rung must be ignored")
	}
}
