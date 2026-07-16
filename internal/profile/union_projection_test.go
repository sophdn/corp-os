package profile_test

// Task-3 proof: per-profile projection selects a lean subset of the toolkit+web
// UNION. A web-needing profile (web-research) receives the web surface across the
// server boundary; a mechanical profile (task-lifecycle) does not. The catalog
// here is the genuine union — a synthetic toolkit surface set plus the REAL web
// surface spec from web.New().Specs() — so projection runs over web's actual enum
// shape, exactly as the aggregator offers it at runtime.

import (
	"testing"

	"corpos/internal/mcp"
	"corpos/internal/profile"
	"corpos/internal/tool"
	"corpos/internal/web"
)

// unionCatalog is the toolkit+web union the aggregator offers the loop: a few
// synthetic toolkit surfaces plus the real web surface spec.
func unionCatalog() []tool.Spec {
	toolkit := catalogOffering(map[string][]string{
		"work":      {"chain_status", "task_complete", "record"},
		"knowledge": {"vault_search", "vault_read", "kiwix_search", "kiwix_fetch", "knowledge_search", "library_find", "library_get"},
		"fs":        {"read", "grep", "glob", "ls"},
	})
	return append(toolkit, web.New().Specs()...)
}

func findSpec(specs []tool.Spec, name string) (tool.Spec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return tool.Spec{}, false
}

// projectProfile runs the load→scope→project path main uses at spawn time.
func projectProfile(t *testing.T, reg *profile.Registry, catalog []tool.Spec, name string) []tool.Spec {
	t.Helper()
	p, ok := reg.Get(name)
	if !ok {
		t.Fatalf("profile %q not in library", name)
	}
	scope := make(mcp.Scope, len(p.Tools))
	for _, ts := range p.Tools {
		scope[ts.Surface] = ts.Actions
	}
	return mcp.Project(catalog, scope)
}

func TestUnionProjection_WebProfileGetsWebOthersDont(t *testing.T) {
	t.Parallel()
	reg, err := profile.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	catalog := unionCatalog()
	if _, ok := findSpec(catalog, "web"); !ok {
		t.Fatal("union catalog is missing the web surface")
	}

	// web-research declares the whole web surface → projection keeps it, with BOTH
	// actions, AND keeps it lean (no work surface it never declared).
	wr := projectProfile(t, reg, catalog, "web-research")
	webSpec, ok := findSpec(wr, "web")
	if !ok {
		t.Fatal("web-research did not receive the web surface across the server boundary")
	}
	enum := enumSet(t, webSpec)
	if !enum["search"] || !enum["fetch"] {
		t.Fatalf("web-research web enum = %v, want both search and fetch", enum)
	}
	if _, leaked := findSpec(wr, "work"); leaked {
		t.Error("web-research leaked the work surface (projection not lean)")
	}

	// task-lifecycle does NOT declare web → projection over the union excludes it,
	// while still granting its own declared surface.
	tl := projectProfile(t, reg, catalog, "task-lifecycle")
	if _, leaked := findSpec(tl, "web"); leaked {
		t.Error("task-lifecycle received web despite not declaring it — union projection leaked across servers")
	}
	if _, ok := findSpec(tl, "work"); !ok {
		t.Error("task-lifecycle missing its declared work surface")
	}
}
