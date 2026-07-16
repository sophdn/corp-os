package mcp

import (
	"context"
	"testing"

	"corpos/internal/tool"
)

// recordingProvider is a fake tool.Provider that tags every result with a label
// so a routing test can assert which provider handled a call.
type recordingProvider struct {
	label string
	calls []tool.Call
}

func (p *recordingProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	p.calls = append(p.calls, c)
	return tool.Result{Call: c, OK: true, Value: map[string]any{"handled_by": p.label}}
}

func TestNewAggregator_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := NewAggregator(); err == nil {
		t.Fatal("want error on zero servers")
	}
	if _, err := NewAggregator(Server{Name: "x", Provider: nil}); err == nil {
		t.Fatal("want error on nil provider")
	}
	// A surface offered by two servers is an ambiguous route.
	a := &recordingProvider{label: "a"}
	b := &recordingProvider{label: "b"}
	_, err := NewAggregator(
		Server{Name: "a", Provider: a, Specs: []tool.Spec{thinSpec("fs", "fs")}},
		Server{Name: "b", Provider: b, Specs: []tool.Spec{thinSpec("fs", "fs")}},
	)
	if err == nil {
		t.Fatal("want error on duplicate surface across servers")
	}
}

func TestAggregator_RoutesBySurface(t *testing.T) {
	t.Parallel()
	toolkit := &recordingProvider{label: "toolkit"}
	web := &recordingProvider{label: "web"}
	agg, err := NewAggregator(
		Server{Name: "toolkit", Provider: toolkit, Specs: []tool.Spec{thinSpec("work", "work"), thinSpec("fs", "fs")}},
		Server{Name: "web", Provider: web, Specs: []tool.Spec{thinSpec("web", "web")}},
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	res := agg.Dispatch(context.Background(), tool.Call{Surface: "web", Action: "search"})
	if !res.OK || res.Value.(map[string]any)["handled_by"] != "web" {
		t.Fatalf("web call routed wrong: %+v", res)
	}
	if len(web.calls) != 1 || len(toolkit.calls) != 0 {
		t.Fatalf("routing leaked: web=%d toolkit=%d", len(web.calls), len(toolkit.calls))
	}

	res = agg.Dispatch(context.Background(), tool.Call{Surface: "work", Action: "chain_status"})
	if res.Value.(map[string]any)["handled_by"] != "toolkit" {
		t.Fatalf("work call routed wrong: %+v", res)
	}
}

func TestAggregator_UnmountedSurfaceFailsAsToolError(t *testing.T) {
	t.Parallel()
	agg, _ := NewAggregator(Server{Name: "web", Provider: &recordingProvider{label: "web"}, Specs: []tool.Spec{thinSpec("web", "web")}})
	res := agg.Dispatch(context.Background(), tool.Call{Surface: "git", Action: "status"})
	if res.OK {
		t.Fatal("unmounted surface should fail")
	}
	if res.ErrorClass != tool.ClassTool {
		t.Fatalf("class = %q, want tool_error", res.ErrorClass)
	}
}

func TestAggregator_SpecsAreUnionInMountOrder(t *testing.T) {
	t.Parallel()
	agg, _ := NewAggregator(
		Server{Name: "toolkit", Provider: &recordingProvider{}, Specs: []tool.Spec{thinSpec("work", "work"), thinSpec("knowledge", "knowledge")}},
		Server{Name: "web", Provider: &recordingProvider{}, Specs: []tool.Spec{thinSpec("web", "web")}},
	)
	got := agg.Specs()
	want := []string{"work", "knowledge", "web"}
	if len(got) != len(want) {
		t.Fatalf("specs = %d, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("spec[%d] = %q, want %q", i, got[i].Name, name)
		}
	}
	if owner, ok := agg.Owner("web"); !ok || owner != "web" {
		t.Fatalf("Owner(web) = %q,%v", owner, ok)
	}
	if _, ok := agg.Owner("absent"); ok {
		t.Fatal("Owner(absent) should be false")
	}
}

func TestEnvelopeSpec_BuildsProjectableShape(t *testing.T) {
	t.Parallel()
	// EnvelopeSpec must yield a spec that Project can narrow — i.e. the action enum
	// must be present and match the names, so a static surface (web) projects like
	// an enriched one.
	spec := EnvelopeSpec("web", "the web surface", []string{"fetch", "search"},
		[]string{"fetch(url, max_bytes?) — get a URL", "search(query, max_results?) — web search"})
	if spec.Name != "web" {
		t.Fatalf("name = %q", spec.Name)
	}
	enum := actionEnum(t, spec)
	if !enum["fetch"] || !enum["search"] {
		t.Fatalf("enum missing actions: %v", enum)
	}
	got := Project([]tool.Spec{spec}, Scope{"web": {"search"}})
	if len(got) != 1 {
		t.Fatalf("project len = %d, want 1", len(got))
	}
	if e := actionEnum(t, got[0]); e["fetch"] || !e["search"] {
		t.Fatalf("projected enum = %v, want only search", e)
	}
}
