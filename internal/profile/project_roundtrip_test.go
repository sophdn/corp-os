package profile_test

// External test: exercises the real load→project round trip for every starter
// profile against mcp.Project, the path main.go uses at spawn. Lives in
// profile_test (not mcp) because it depends on both the embedded library and the
// projector; mcp does not import profile, so there is no cycle.

import (
	"sort"
	"strings"
	"testing"

	"corpos/internal/mcp"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

// catalogOffering builds a full enriched spec set whose every surface offers the
// given actions as a hard enum — a synthetic stand-in for the live substrate so
// the projection can be exercised without a server.
func catalogOffering(perSurface map[string][]string) []tool.Spec {
	specs := make([]tool.Spec, 0, len(perSurface))
	surfaces := make([]string, 0, len(perSurface))
	for s := range perSurface {
		surfaces = append(surfaces, s)
	}
	sort.Strings(surfaces)
	for _, s := range surfaces {
		actions := perSurface[s]
		enum := make([]any, len(actions))
		lines := make([]string, len(actions))
		for i, a := range actions {
			enum[i] = a
			lines[i] = "- " + a + "(x?) — does " + a
		}
		desc := "Surface " + s + ".\n\nActions:\n" + strings.Join(lines, "\n") +
			"\n\nUse these exact action names."
		specs = append(specs, tool.Spec{
			Name:        s,
			Description: desc,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": enum},
				},
				"required": []any{"action"},
			},
		})
	}
	return specs
}

// fullCatalog offers every action any starter profile names, on every surface,
// so each profile's declared scope can project cleanly.
func fullCatalog(t *testing.T, reg *profile.Registry) []tool.Spec {
	t.Helper()
	perSurface := map[string]map[string]bool{}
	for _, name := range reg.Names() {
		p, _ := reg.Get(name)
		for _, ts := range p.Tools {
			if perSurface[ts.Surface] == nil {
				perSurface[ts.Surface] = map[string]bool{}
			}
			for _, a := range ts.Actions {
				perSurface[ts.Surface][a] = true
			}
			// A whole-surface scope still needs at least one action to enrich.
			perSurface[ts.Surface]["__probe__"] = true
		}
	}
	flat := map[string][]string{}
	for s, set := range perSurface {
		for a := range set {
			flat[s] = append(flat[s], a)
		}
		sort.Strings(flat[s])
	}
	return catalogOffering(flat)
}

func TestEveryStarterProfileProjectsItsDeclaredEnvelope(t *testing.T) {
	reg, err := profile.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	catalog := fullCatalog(t, reg)

	for _, name := range reg.Names() {
		p, _ := reg.Get(name)
		scope := make(mcp.Scope, len(p.Tools))
		for _, ts := range p.Tools {
			scope[ts.Surface] = ts.Actions
		}
		projected := mcp.Project(catalog, scope)

		// Every declared surface must survive projection, and nothing else.
		gotSurfaces := make([]string, 0, len(projected))
		for _, s := range projected {
			gotSurfaces = append(gotSurfaces, s.Name)
		}
		sort.Strings(gotSurfaces)
		want := p.Surfaces()
		if strings.Join(gotSurfaces, ",") != strings.Join(want, ",") {
			t.Errorf("%s: projected surfaces %v, want %v", name, gotSurfaces, want)
			continue
		}

		// For each action-scoped surface, the projected enum must equal the
		// declared action set (the catalog offers them all, so none are dropped).
		byName := map[string]tool.Spec{}
		for _, s := range projected {
			byName[s.Name] = s
		}
		for _, ts := range p.Tools {
			if len(ts.Actions) == 0 {
				continue // whole-surface scope — no action assertion
			}
			enum := enumSet(t, byName[ts.Surface])
			for _, a := range ts.Actions {
				if !enum[a] {
					t.Errorf("%s: surface %s dropped declared action %q (enum=%v)", name, ts.Surface, a, enum)
				}
			}
			if len(enum) != len(ts.Actions) {
				t.Errorf("%s: surface %s projected %d actions, want %d", name, ts.Surface, len(enum), len(ts.Actions))
			}
		}
	}
}

func enumSet(t *testing.T, s tool.Spec) map[string]bool {
	t.Helper()
	props, _ := s.InputSchema["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	raw, _ := action["enum"].([]any)
	out := map[string]bool{}
	for _, v := range raw {
		if str, ok := v.(string); ok {
			out[str] = true
		}
	}
	return out
}
