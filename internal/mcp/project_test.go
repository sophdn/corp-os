package mcp

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// enrichedFixture returns the full enriched spec set from the scripted provider
// (work/knowledge/fs/measure enriched; sys thin-fallback).
func enrichedFixture(t *testing.T) []tool.Spec {
	t.Helper()
	return EnrichedSpecs(context.Background(), newScripted())
}

func specByName(specs []tool.Spec, name string) (tool.Spec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return tool.Spec{}, false
}

func TestProject_DeniesUnscopedSurfaces(t *testing.T) {
	t.Parallel()
	specs := enrichedFixture(t)
	got := Project(specs, Scope{"work": {"chain_status"}})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (only work scoped)", len(got))
	}
	if got[0].Name != "work" {
		t.Fatalf("kept %q, want work", got[0].Name)
	}
	for _, denied := range []string{"knowledge", "fs", "measure", "sys"} {
		if _, ok := specByName(got, denied); ok {
			t.Errorf("surface %q leaked past projection", denied)
		}
	}
}

func TestProject_ActionLevelNarrowsEnumAndDescription(t *testing.T) {
	t.Parallel()
	specs := enrichedFixture(t)
	got := Project(specs, Scope{"work": {"chain_status"}}) // drop chain_find
	work, ok := specByName(got, "work")
	if !ok {
		t.Fatal("work not projected")
	}
	enum := actionEnum(t, work)
	if !enum["chain_status"] {
		t.Errorf("chain_status missing from projected enum: %v", enum)
	}
	if enum["chain_find"] {
		t.Errorf("chain_find should be projected out of the enum: %v", enum)
	}
	// The description line for the dropped action must be gone; the kept one stays.
	if strings.Contains(work.Description, "chain_find(") {
		t.Errorf("chain_find description line leaked: %s", work.Description)
	}
	if !strings.Contains(work.Description, "chain_status(") {
		t.Errorf("chain_status description line dropped: %s", work.Description)
	}
	// The header + canonical-docs footer survive the filter.
	if !strings.Contains(work.Description, "Use these exact action names") {
		t.Errorf("footer dropped by line filter: %s", work.Description)
	}
}

func TestProject_WholeSurfaceScopeKeepsSpecAsIs(t *testing.T) {
	t.Parallel()
	specs := enrichedFixture(t)
	full, _ := specByName(specs, "fs")
	got := Project(specs, Scope{"fs": {}}) // empty actions = whole surface
	fsSpec, ok := specByName(got, "fs")
	if !ok {
		t.Fatal("fs not projected")
	}
	if fsSpec.Description != full.Description {
		t.Error("whole-surface scope should keep the description verbatim")
	}
	enum := actionEnum(t, fsSpec)
	for _, a := range []string{"read", "write", "edit"} {
		if !enum[a] {
			t.Errorf("whole-surface fs missing action %q", a)
		}
	}
}

func TestProject_ThinFallbackFailsClosedOnActionScope(t *testing.T) {
	t.Parallel()
	specs := enrichedFixture(t)
	// sys is the thin-fallback spec (scripted provider doesn't enumerate it).
	sys, ok := specByName(specs, "sys")
	if !ok {
		t.Fatal("fixture missing sys thin spec")
	}
	if _, hasEnum := enumOf(sys); hasEnum {
		t.Fatal("precondition: sys should be a thin (enum-less) spec")
	}
	// Action-level scope on a thin spec → dropped (cannot honor the restriction).
	got := Project(specs, Scope{"sys": {"exec"}})
	if _, ok := specByName(got, "sys"); ok {
		t.Error("thin spec with an action-level scope must fail closed (be dropped)")
	}
	// Whole-surface scope on a thin spec → kept (the profile asked for all of it).
	got = Project(specs, Scope{"sys": {}})
	if _, ok := specByName(got, "sys"); !ok {
		t.Error("thin spec with a whole-surface scope should be kept")
	}
}

func TestProject_NoneOfRequestedActionsExistDropsSurface(t *testing.T) {
	t.Parallel()
	specs := enrichedFixture(t)
	got := Project(specs, Scope{"work": {"nonexistent_action"}})
	if _, ok := specByName(got, "work"); ok {
		t.Error("surface with no matching actions should be dropped")
	}
}

func TestProject_EmptyScopeYieldsNothing(t *testing.T) {
	t.Parallel()
	got := Project(enrichedFixture(t), Scope{})
	if len(got) != 0 {
		t.Fatalf("empty scope projected %d specs, want 0", len(got))
	}
}

func TestActionOfLine(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"- chain_status(chain?) — Return a chain's summary": "chain_status",
		"- read(file_path, offset?, limit?)":                "read",
		"- bare_name":                                       "bare_name",
		"- no_params()":                                     "no_params",
		"- with_space and tail":                             "with_space",
	}
	for line, want := range cases {
		if got := actionOfLine(line); got != want {
			t.Errorf("actionOfLine(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestEnumOf_Malformed(t *testing.T) {
	t.Parallel()
	// thin envelope: no enum.
	if _, ok := enumOf(tool.Spec{InputSchema: envelopeSchema()}); ok {
		t.Error("envelope schema has no enum; enumOf should report false")
	}
	// missing properties entirely.
	if _, ok := enumOf(tool.Spec{InputSchema: map[string]any{}}); ok {
		t.Error("no properties → false")
	}
	// properties without an action key.
	if _, ok := enumOf(tool.Spec{InputSchema: map[string]any{"properties": map[string]any{}}}); ok {
		t.Error("no action property → false")
	}
}
