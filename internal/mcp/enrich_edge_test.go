package mcp

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// describeFailProvider enumerates fine but fails every action_describe, so the
// builder must fall back to name-only signatures.
type describeFailProvider struct{}

func (describeFailProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	if c.Action == introspectAction {
		return tool.Result{Call: c, OK: true, Value: map[string]any{"actions": []any{"chain_find"}}}
	}
	return tool.Result{Call: c, OK: false, Value: map[string]any{"error": "no corpus"}, ErrorClass: tool.ClassTool}
}

func TestEnrichSurface_DescribeFailureFallsBackToNameOnly(t *testing.T) {
	specs := EnrichedSpecs(context.Background(), describeFailProvider{})
	work := specs[0]
	// Name appears, but with no signature parens (describe gave us no params).
	if !strings.Contains(work.Description, "chain_find") {
		t.Fatalf("missing action name: %s", work.Description)
	}
	if strings.Contains(work.Description, "chain_find(") {
		t.Errorf("expected name-only (no parens) when describe fails: %s", work.Description)
	}
}

// emptyEnumProvider returns an empty action list, forcing the no-actions fallback.
type emptyEnumProvider struct{}

func (emptyEnumProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	return tool.Result{Call: c, OK: true, Value: map[string]any{"actions": []any{}}}
}

func TestEnrichSurface_EmptyEnumerationFallsBackToThin(t *testing.T) {
	specs := EnrichedSpecs(context.Background(), emptyEnumProvider{})
	for _, s := range specs {
		props := s.InputSchema["properties"].(map[string]any)
		if _, hasEnum := props["action"].(map[string]any)["enum"]; hasEnum {
			t.Errorf("%s: empty enumeration should fall back to a thin spec", s.Name)
		}
	}
}

func TestSignature_EmptyAndTruncated(t *testing.T) {
	if got := signature("noparams", nil); got != "noparams()" {
		t.Errorf("empty params: got %q, want noparams()", got)
	}

	many := make([]specParam, 10)
	for i := range many {
		many[i] = specParam{name: "p", required: true}
	}
	got := signature("big", many)
	if !strings.HasSuffix(got, "…)") {
		t.Errorf("over-long param list should truncate with …: %q", got)
	}
	if strings.Count(got, "p") != 8 { // exactly maxParams names before the …
		t.Errorf("expected 8 params before truncation: %q", got)
	}
}

func TestActionsFrom_MalformedShapes(t *testing.T) {
	cases := []any{
		"not a map",
		map[string]any{},                              // no actions key
		map[string]any{"actions": "not a slice"},      // wrong type
		map[string]any{"actions": []any{1, "", "ok"}}, // mixed: only "ok" survives
	}
	wants := [][]string{nil, nil, nil, {"ok"}}
	for i, c := range cases {
		got := actionsFrom(c)
		if len(got) != len(wants[i]) {
			t.Errorf("case %d: got %v, want %v", i, got, wants[i])
			continue
		}
		for j := range got {
			if got[j] != wants[i][j] {
				t.Errorf("case %d: got %v, want %v", i, got, wants[i])
			}
		}
	}
}

func TestParamsFrom_MalformedShapes(t *testing.T) {
	if paramsFrom("not a map") != nil {
		t.Error("non-map should yield nil")
	}
	if paramsFrom(map[string]any{}) != nil {
		t.Error("missing params key should yield nil")
	}
	if paramsFrom(map[string]any{"params": "nope"}) != nil {
		t.Error("non-slice params should yield nil")
	}
	mixed := map[string]any{"params": []any{
		"not a dict",                                 // skipped
		map[string]any{"required": true},             // no name -> skipped
		map[string]any{"name": "", "required": true}, // empty name -> skipped
		map[string]any{"name": "keep", "required": true},
	}}
	got := paramsFrom(mixed)
	if len(got) != 1 || got[0].name != "keep" || !got[0].required {
		t.Errorf("got %+v, want one {keep,true}", got)
	}
}
