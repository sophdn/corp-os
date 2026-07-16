package main

import (
	"io"
	"testing"

	"corpos/internal/profile"
)

func TestRescopeLadder_ResolvesDeclaredTargets(t *testing.T) {
	// code-review declares rescope_to=["bug-fix"] in the embedded library.
	reg, err := profile.Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	cr, _ := reg.Get("code-review")
	ladder := rescopeLadder(&cr, "", io.Discard)
	if len(ladder) != 1 || ladder[0].Name != "bug-fix" {
		t.Fatalf("code-review ladder = %+v, want one bug-fix rung", ladder)
	}
	// The rung carries bug-fix's scope (it grants fs.write, which code-review lacks).
	if !ladder[0].Scope.Allows("fs", "write") {
		t.Fatal("bug-fix rung should grant fs.write")
	}
}

func TestRescopeLadder_EmptyAndUnknown(t *testing.T) {
	if l := rescopeLadder(nil, "", io.Discard); l != nil {
		t.Errorf("nil profile → nil ladder, got %v", l)
	}
	// A profile with no rescope_to yields no ladder.
	p := profile.JobProfile{Name: "x", Tier: profile.TierLocal, Tools: []profile.SurfaceScope{{Surface: "fs"}}}
	if l := rescopeLadder(&p, "", io.Discard); l != nil {
		t.Errorf("no rescope_to → nil ladder, got %v", l)
	}
	// An unknown target is skipped (not fatal), yielding an empty ladder.
	p.RescopeTo = []string{"no-such-profile"}
	if l := rescopeLadder(&p, "", io.Discard); len(l) != 0 {
		t.Errorf("unknown target should be skipped, got %v", l)
	}
}
