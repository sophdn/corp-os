package profile

import "testing"

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMergeProfilesExtendsSameNameSkills(t *testing.T) {
	base := []JobProfile{{Name: "a", Skills: []string{"x", "y"}, Tier: TierLocal, Duty: "d"}}
	overlay := []JobProfile{{Name: "a", Skills: []string{"y", "z"}}} // partial: name + skills
	out, err := mergeProfiles(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 merged profile, got %d", len(out))
	}
	if !equalStrs(out[0].Skills, []string{"x", "y", "z"}) {
		t.Errorf("skills = %v, want [x y z]", out[0].Skills)
	}
	// non-skill fields come from base, not the partial overlay entry
	if out[0].Tier != TierLocal || out[0].Duty != "d" {
		t.Errorf("base fields lost: tier=%q duty=%q", out[0].Tier, out[0].Duty)
	}
}

func TestMergeProfilesAddsNewName(t *testing.T) {
	base := []JobProfile{{Name: "a"}}
	overlay := []JobProfile{{Name: "b", Skills: []string{"s"}}}
	out, err := mergeProfiles(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 profiles (a + new b), got %d", len(out))
	}
	if out[1].Name != "b" || !equalStrs(out[1].Skills, []string{"s"}) {
		t.Errorf("new overlay profile not appended as-is: %+v", out[1])
	}
}

func TestMergeProfilesDoesNotMutateBase(t *testing.T) {
	base := []JobProfile{{Name: "a", Skills: []string{"x"}}}
	if _, err := mergeProfiles(base, []JobProfile{{Name: "a", Skills: []string{"y"}}}); err != nil {
		t.Fatal(err)
	}
	if !equalStrs(base[0].Skills, []string{"x"}) {
		t.Errorf("mergeProfiles mutated base: %v", base[0].Skills)
	}
}

func TestMergeProfilesEmptyOverlayIsIdentity(t *testing.T) {
	base := []JobProfile{{Name: "a", Skills: []string{"x"}}}
	out, err := mergeProfiles(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !equalStrs(out[0].Skills, []string{"x"}) {
		t.Errorf("empty overlay should be identity, got %+v", out)
	}
}

func TestMergeProfilesRejectsUnnamedOverlay(t *testing.T) {
	if _, err := mergeProfiles(nil, []JobProfile{{Name: "  "}}); err == nil {
		t.Error("want an error for an unnamed overlay profile")
	}
}

func TestUnionSkillsDedupsOrdersDropsEmpty(t *testing.T) {
	got := unionSkills([]string{"a", "", "b", "a"}, []string{"b", "c", ""})
	if !equalStrs(got, []string{"a", "b", "c"}) {
		t.Errorf("unionSkills = %v, want [a b c]", got)
	}
}
