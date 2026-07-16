package mcp

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// TestLazyEnrichedSpecs_PreservesEnumDropsCatalog is the core #3100 guarantee: a lazy
// spec keeps the full action enum (no capability/scoping loss) but drops the per-action
// param signatures from the description.
func TestLazyEnrichedSpecs_PreservesEnumDropsCatalog(t *testing.T) {
	t.Parallel()
	src := newScripted()
	lazy := LazyEnrichedSpecs(context.Background(), src)
	full := EnrichedSpecs(context.Background(), newScripted())

	byName := func(specs []tool.Spec) map[string]tool.Spec {
		m := make(map[string]tool.Spec, len(specs))
		for _, s := range specs {
			m[s.Name] = s
		}
		return m
	}
	l, f := byName(lazy), byName(full)
	for name, fs := range f {
		ls, ok := l[name]
		if !ok {
			t.Fatalf("lazy is missing surface %q", name)
		}
		le, lok := enumOf(ls)
		fe, fok := enumOf(fs)
		// Lazy and full must agree on enrich-vs-thin status for every surface.
		if lok != fok {
			t.Fatalf("surface %q enrich status differs: lazy enum=%v full enum=%v", name, lok, fok)
		}
		if !lok {
			continue // both fell back to the thin spec (e.g. an unscripted surface) → equal
		}
		// Same action enum → every action is still offered and scopable (no capability loss).
		if strings.Join(le, ",") != strings.Join(fe, ",") {
			t.Fatalf("surface %q enum differs: lazy=%v full=%v", name, le, fe)
		}
		// The lazy description carries no param signatures "name(" — params are deferred.
		for _, act := range le {
			if strings.Contains(ls.Description, act+"(") {
				t.Errorf("surface %q lazy desc should not inline %q's params, got:\n%s", name, act, ls.Description)
			}
		}
		// But it still points at action_describe.
		if !strings.Contains(ls.Description, "action_describe") {
			t.Errorf("surface %q lazy desc should point at admin.action_describe", name)
		}
	}
}

// newFatScripted scripts a realistic surface — actions with full param lists AND long
// purpose clauses — so the lazy-vs-enriched footprint gap reflects real toolkit
// surfaces (40+ actions, multi-param signatures), not the tiny default fixture.
func newFatScripted() *scriptedProvider {
	const longPurpose = "Search and return matching records with full pagination, filtering, and projection options applied"
	mkParams := func() []map[string]any {
		return []map[string]any{
			param("project", "string", true), param("query", "string", true),
			param("limit", "integer", false), param("offset", "integer", false),
			param("status", "string", false), param("rationale", "string", false),
		}
	}
	// ~30 actions, like a real toolkit surface, so the per-action catalog dominates the
	// fixed schema/header overhead and the lazy cut reflects production reality.
	acts := make([]string, 0, 30)
	params := map[string][]map[string]any{}
	purposes := map[string]string{}
	for i := 0; i < 30; i++ {
		a := "action_" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		acts = append(acts, a)
		params[a] = mkParams()
		purposes[a] = longPurpose
	}
	return &scriptedProvider{
		actions:  map[string][]string{"work": acts},
		params:   map[string]map[string][]map[string]any{"work": params},
		purposes: map[string]map[string]string{"work": purposes},
	}
}

// TestLazyEnrichedSpecs_OverheadDropsMaterially is criterion 2: the lazy footprint is
// materially smaller than the full enriched footprint.
func TestLazyEnrichedSpecs_OverheadDropsMaterially(t *testing.T) {
	t.Parallel()
	_, lazyTok, _ := Footprint(LazyEnrichedSpecs(context.Background(), newFatScripted()))
	_, fullTok, _ := Footprint(EnrichedSpecs(context.Background(), newFatScripted()))
	if lazyTok >= fullTok {
		t.Fatalf("lazy footprint (%d) should be smaller than enriched (%d)", lazyTok, fullTok)
	}
	// "Materially" (criterion 2): on a realistic multi-param, long-purpose surface the
	// lazy envelope is well under half the enriched catalog.
	if lazyTok*2 > fullTok {
		t.Errorf("lazy footprint %d is not materially below enriched %d (want <=50%%)", lazyTok, fullTok)
	}
}

// TestLazyEnrichedSpecs_CheaperToBuild: a lazy build makes one __actions__ call per
// surface and NO per-action describe calls, so it dispatches far fewer times.
func TestLazyEnrichedSpecs_CheaperToBuild(t *testing.T) {
	t.Parallel()
	lazySrc := newScripted()
	LazyEnrichedSpecs(context.Background(), lazySrc)
	fullSrc := newScripted()
	EnrichedSpecs(context.Background(), fullSrc)
	if lazySrc.calls >= fullSrc.calls {
		t.Fatalf("lazy build (%d calls) should dispatch fewer than enriched (%d)", lazySrc.calls, fullSrc.calls)
	}
}

// TestLazyEnvelopeSpec_ProjectNarrows: Project scopes a lazy spec exactly like a full
// one — the enum narrows AND the name-only action lines are filtered to the granted set.
func TestLazyEnvelopeSpec_ProjectNarrows(t *testing.T) {
	t.Parallel()
	spec := LazyEnvelopeSpec("work", "the work ledger", []string{"chain_find", "chain_status", "task_read"})
	projected := Project([]tool.Spec{spec}, Scope{"work": {"chain_find"}})
	if len(projected) != 1 {
		t.Fatalf("want 1 projected spec, got %d", len(projected))
	}
	enum, ok := enumOf(projected[0])
	if !ok || len(enum) != 1 || enum[0] != "chain_find" {
		t.Fatalf("projected enum should be [chain_find], got %v", enum)
	}
	if strings.Contains(projected[0].Description, "task_read") || strings.Contains(projected[0].Description, "chain_status") {
		t.Errorf("projected lazy desc should drop disallowed action lines, got:\n%s", projected[0].Description)
	}
	if !strings.Contains(projected[0].Description, "chain_find") {
		t.Errorf("projected lazy desc should keep the granted action, got:\n%s", projected[0].Description)
	}
}

// TestLazyEnrichedSpecs_FailSoft: a surface whose enumeration fails degrades to the
// thin static spec (enum-less), same as EnrichedSpecs.
func TestLazyEnrichedSpecs_FailSoft(t *testing.T) {
	t.Parallel()
	src := newScripted()
	src.actions["work"] = nil // enumeration fails for work
	specs := LazyEnrichedSpecs(context.Background(), src)
	for _, s := range specs {
		if s.Name != "work" {
			continue
		}
		if _, ok := enumOf(s); ok {
			t.Error("a failed-enumeration surface should fall back to the thin (enum-less) spec")
		}
	}
}
