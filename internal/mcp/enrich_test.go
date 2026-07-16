package mcp

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// scriptedProvider answers __actions__ and admin.action_describe from canned
// data so EnrichedSpecs runs the whole build path with no live server. A nil
// entry in actions makes that surface's enumeration fail (fallback path).
type scriptedProvider struct {
	actions  map[string][]string                    // surface -> action names ("" => enumeration fails)
	params   map[string]map[string][]map[string]any // surface -> action -> param dicts
	purposes map[string]map[string]string           // surface -> action -> purpose (optional)
	calls    int
}

func (s *scriptedProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	s.calls++
	switch c.Action {
	case "__actions__":
		names, ok := s.actions[c.Surface]
		if !ok || names == nil {
			return tool.Result{Call: c, OK: false, Value: map[string]any{"error": "down"}, ErrorClass: tool.ClassTransient}
		}
		raw := make([]any, len(names))
		for i, n := range names {
			raw[i] = n
		}
		return tool.Result{Call: c, OK: true, Value: map[string]any{"actions": raw}}
	case "action_describe":
		surface, _ := c.Params["surface"].(string)
		action, _ := c.Params["action"].(string)
		ps := s.params[surface][action]
		raw := make([]any, len(ps))
		for i, p := range ps {
			raw[i] = p
		}
		val := map[string]any{"surface": surface, "action": action, "params": raw}
		if p := s.purposes[surface][action]; p != "" {
			val["purpose"] = p
		}
		return tool.Result{Call: c, OK: true, Value: val}
	default:
		return tool.Result{Call: c, OK: false, Value: map[string]any{"error": "unexpected"}, ErrorClass: tool.ClassTool}
	}
}

func param(name, typ string, required bool) map[string]any {
	return map[string]any{"name": name, "type": typ, "required": required}
}

func newScripted() *scriptedProvider {
	return &scriptedProvider{
		actions: map[string][]string{
			"work":      {"chain_status", "chain_find"},
			"knowledge": {"vault_search"},
			"fs":        {"read", "write", "edit"},
			"measure":   {"classify_bug_severity"},
		},
		params: map[string]map[string][]map[string]any{
			"work": {
				"chain_status": {param("chain", "optional_string", false)},
				"chain_find":   {param("query", "string", true)},
			},
			"knowledge": {
				"vault_search": {param("query", "string", true), param("project", "optional_string", false)},
			},
			"fs": {
				"read":  {param("file_path", "string", true), param("offset", "integer", false), param("limit", "integer", false)},
				"write": {param("file_path", "string", true), param("content", "string", true)},
				"edit":  {param("file_path", "string", true)},
			},
			"measure": {
				"classify_bug_severity": {param("bug_report", "string", true)},
			},
		},
		purposes: map[string]map[string]string{
			"work": {
				"chain_status": "Return a chain's summary: id, project, slug, status, task counts.",
				"chain_find":   "Search chains by slug/title pattern. Returns compact rows.",
			},
		},
	}
}

func TestEnrichedSpecs_ExposesRealActionNamesAndParams(t *testing.T) {
	specs := EnrichedSpecs(context.Background(), newScripted())
	// 5 abSurfaces: work/knowledge/fs/measure enrich; sys (not scripted) thin-falls
	// back. ml is gone (the global prune), so it must not appear.
	if len(specs) != 5 {
		t.Fatalf("len = %d, want 5", len(specs))
	}
	for _, s := range specs {
		if s.Name == "ml" {
			t.Fatal("ml must not be enriched/exposed — it is the global prune (§4.4.1)")
		}
	}
	byName := map[string]tool.Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	// work: the real names that the model previously GUESSED wrong must appear
	// both in the description and as a hard enum on the action property.
	work := byName["work"]
	for _, want := range []string{"chain_status", "chain_find"} {
		if !strings.Contains(work.Description, want) {
			t.Errorf("work description missing real action %q: %s", want, work.Description)
		}
	}
	enum := actionEnum(t, work)
	if !enum["chain_status"] || !enum["chain_find"] {
		t.Errorf("work action enum = %v, want chain_status + chain_find", enum)
	}
	// Per-action purpose clauses disambiguate intent (list vs search) for cheap
	// models — the leading clause, sentence-trimmed.
	if !strings.Contains(work.Description, "Return a chain's summary: id, project, slug, status, task counts") {
		t.Errorf("work description missing chain_status purpose: %s", work.Description)
	}
	if !strings.Contains(work.Description, "chain_find(query) — Search chains by slug/title pattern") {
		t.Errorf("work description missing chain_find signature+purpose: %s", work.Description)
	}
	// The truncated-away tail must not leak.
	if strings.Contains(work.Description, "Returns compact rows") {
		t.Errorf("chain_find purpose was not trimmed to its leading clause: %s", work.Description)
	}
	// The bogus names the model invented must NOT be in the enum.
	if enum["list_chains"] || enum["task_search"] {
		t.Errorf("work action enum leaked a guessed name: %v", enum)
	}

	// fs.read: the confirmed param-guess case (model guessed `path`; real key is
	// file_path). The signature in the description must carry file_path.
	fsSpec := byName["fs"]
	if !strings.Contains(fsSpec.Description, "file_path") {
		t.Errorf("fs description missing file_path param hint: %s", fsSpec.Description)
	}
	if strings.Contains(fsSpec.Description, "read()") {
		t.Errorf("fs read signature dropped its params: %s", fsSpec.Description)
	}
}

func TestShortPurpose(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"Search chains by slug/title pattern. Returns compact rows.", "Search chains by slug/title pattern"},
		{"Return a chain's summary: id, project, slug, status, task counts.", "Return a chain's summary: id, project, slug, status, task counts"},
		{"Alias of task_search — both names route to the same handler.", "Alias of task_search"},
		{"Return full state; verbose detail follows.", "Return full state"},
		{strings.Repeat("x", 100), strings.Repeat("x", 80) + "…"},
	}
	for _, c := range cases {
		if got := shortPurpose(c.in); got != c.want {
			t.Errorf("shortPurpose(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPurposeFrom_Malformed(t *testing.T) {
	if purposeFrom("not a map") != "" {
		t.Error("non-map should yield empty")
	}
	if purposeFrom(map[string]any{}) != "" {
		t.Error("missing purpose should yield empty")
	}
	if purposeFrom(map[string]any{"purpose": "hi"}) != "hi" {
		t.Error("should extract the purpose string")
	}
}

func TestEnrichedSpecs_FallsBackToThinSpecWhenSurfaceUnavailable(t *testing.T) {
	sp := newScripted()
	sp.actions["work"] = nil // enumeration fails for work only
	specs := EnrichedSpecs(context.Background(), sp)

	byName := map[string]tool.Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	// work falls back to the thin static spec: no enum, plain envelope.
	work := byName["work"]
	if _, hasEnum := work.InputSchema["properties"].(map[string]any)["action"].(map[string]any)["enum"]; hasEnum {
		t.Errorf("work should have fallen back to a thin (enum-less) spec, got %v", work.InputSchema)
	}
	// knowledge still enriches (independent of work's failure).
	if !actionEnum(t, byName["knowledge"])["vault_search"] {
		t.Errorf("knowledge should still enrich when only work is down")
	}
}

// actionEnum extracts the action-property enum as a set for assertions.
func actionEnum(t *testing.T, s tool.Spec) map[string]bool {
	t.Helper()
	props, ok := s.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no properties", s.Name)
	}
	action, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no action property", s.Name)
	}
	raw, ok := action["enum"].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, v := range raw {
		if str, ok := v.(string); ok {
			out[str] = true
		}
	}
	return out
}
