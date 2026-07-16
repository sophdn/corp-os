package mcp

import "testing"

// TestThinSpecsCoverABSurfaces pins the static (un-enriched) catalog: thinSpec built for every
// abSurface yields the five flat surfaces (work/knowledge/fs/measure/sys), each with a
// description and the action-required envelope schema, and never the ml surface (the global
// prune, §4.4.1). This is the thin-spec path EnrichedSpecs falls back to.
func TestThinSpecsCoverABSurfaces(t *testing.T) {
	if len(abSurfaces) != 5 {
		t.Fatalf("abSurfaces len = %d, want 5", len(abSurfaces))
	}
	names := map[string]bool{}
	for _, s := range abSurfaces {
		spec := thinSpec(s.name, s.description)
		names[spec.Name] = true
		if spec.Description == "" {
			t.Errorf("%s: empty description", spec.Name)
		}
		req, ok := spec.InputSchema["required"].([]any)
		if !ok || len(req) != 1 || req[0] != "action" {
			t.Errorf("%s: required = %v, want [action]", spec.Name, spec.InputSchema["required"])
		}
	}
	for _, want := range []string{"work", "knowledge", "fs", "measure", "sys"} {
		if !names[want] {
			t.Errorf("missing surface %q", want)
		}
	}
	if names["ml"] {
		t.Error("ml must not be exposed flat — it is the global prune (§4.4.1)")
	}
}
