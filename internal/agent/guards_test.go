package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
)

// stubGuard is a test Guard with a scripted stage + verdict.
type stubGuard struct {
	name    string
	stage   GuardStage
	verdict string // non-empty → refuses
}

func (g stubGuard) Name() string      { return g.name }
func (g stubGuard) Stage() GuardStage { return g.stage }
func (g stubGuard) Describe() string  { return "stub: " + g.name }
func (g stubGuard) Assess(_ context.Context, _ GuardInput) GuardVerdict {
	if g.verdict != "" {
		return fail(g.verdict)
	}
	return pass()
}

func TestGuardRegistry_FirstMatchWinsPerStage(t *testing.T) {
	var r guardRegistry
	r.register(stubGuard{name: "a", stage: StageFabrication, verdict: ""})
	r.register(stubGuard{name: "b", stage: StageFabrication, verdict: "B-refused"})
	r.register(stubGuard{name: "c", stage: StageFabrication, verdict: "C-refused"})

	v, refused := r.assess(context.Background(), StageFabrication, GuardInput{})
	if !refused {
		t.Fatal("a refusing guard in the stage should refuse")
	}
	if v.Reason != "B-refused" {
		t.Fatalf("first refusal should win; got %q", v.Reason)
	}
}

func TestGuardRegistry_StageIsolation(t *testing.T) {
	var r guardRegistry
	// A fake-green-stage refusal must NOT fire when assessing the fabrication stage.
	r.register(stubGuard{name: "fg", stage: StageFakeGreen, verdict: "fake-green-refused"})
	r.register(stubGuard{name: "wa", stage: StageFabrication, verdict: ""})

	if _, refused := r.assess(context.Background(), StageFabrication, GuardInput{}); refused {
		t.Fatal("a fake-green-stage guard must not refuse at the fabrication stage")
	}
	v, refused := r.assess(context.Background(), StageFakeGreen, GuardInput{})
	if !refused || v.Reason != "fake-green-refused" {
		t.Fatalf("the fake-green guard should refuse at its own stage; got refused=%v reason=%q", refused, v.Reason)
	}
}

func TestGuardRegistry_AllPass(t *testing.T) {
	var r guardRegistry
	r.register(stubGuard{name: "a", stage: StageFabrication})
	r.register(stubGuard{name: "b", stage: StageFabrication})
	if _, refused := r.assess(context.Background(), StageFabrication, GuardInput{}); refused {
		t.Fatal("all-sound guards should not refuse")
	}
}

func TestGuardRegistry_RegisterNilSafe(t *testing.T) {
	var r guardRegistry
	r.register(nil)
	if len(r.all()) != 0 {
		t.Fatal("registering nil must be a no-op")
	}
}

func TestGuardRegistry_AllPreservesOrder(t *testing.T) {
	var r guardRegistry
	r.register(stubGuard{name: "first", stage: StageFabrication})
	r.register(stubGuard{name: "second", stage: StageFakeGreen})
	got := r.all()
	if len(got) != 2 || got[0].Name() != "first" || got[1].Name() != "second" {
		t.Fatalf("all() must preserve registration order; got %v", names(got))
	}
}

// names renders guard names for a diagnostic.
func names(gs []Guard) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.Name()
	}
	return out
}

// The three real guards satisfy the Guard interface with the expected stage and a non-empty
// description — the -print-guards view reads these.
func TestRealGuardsImplementInterface(t *testing.T) {
	cases := []struct {
		g     Guard
		stage GuardStage
	}{
		{WorkAudit{RequireMutation: true}, StageFabrication},
		{RequiredReads{Paths: []string{"x.go"}}, StageFabrication},
		{FakeGreenGuard{}, StageFakeGreen},
	}
	for _, c := range cases {
		if c.g.Name() == "" {
			t.Errorf("%T: empty Name", c.g)
		}
		if c.g.Describe() == "" {
			t.Errorf("%T: empty Describe", c.g)
		}
		if c.g.Stage() != c.stage {
			t.Errorf("%T: stage = %v, want %v", c.g, c.g.Stage(), c.stage)
		}
	}
}

// The loop registers the three guards (the enumerable set the -print-guards view reads):
// a coding-shaped loop carries work-audit + required-read + fake-green.
func TestLoopGuards_Enumerable(t *testing.T) {
	l := New(single(model.NewEcho("qwen", model.Response{})), &fakeProvider{}, nil,
		WithWorkAudit(WorkAudit{RequireMutation: true}),
		WithRequiredReads(RequiredReads{Paths: []string{"contract.go"}}),
	)
	got := names(l.Guards())
	want := map[string]bool{"work-audit": true, "required-read": true, "fake-green": true}
	if len(got) != 3 {
		t.Fatalf("expected 3 registered guards, got %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected guard %q", n)
		}
	}
}

// GuardCatalog enumerates every known guard, in run order, for the -print-guards view.
func TestGuardCatalog(t *testing.T) {
	cat := GuardCatalog()
	if len(cat) != 4 {
		t.Fatalf("catalog should hold the 4 known guards; got %v", names(cat))
	}
	want := []string{"work-audit", "required-read", "fake-green", "scaffold-fab"}
	for i, n := range want {
		if cat[i].Name() != n {
			t.Errorf("catalog[%d] = %q, want %q", i, cat[i].Name(), n)
		}
	}
}

func TestGuardStageString(t *testing.T) {
	if !strings.Contains(StageFabrication.String(), "Fabricated") {
		t.Errorf("fabrication stage string should mention the Result field; got %q", StageFabrication.String())
	}
	if !strings.Contains(StageFakeGreen.String(), "Escalate") {
		t.Errorf("fake-green stage string should mention the Result field; got %q", StageFakeGreen.String())
	}
	if GuardStage(99).String() != "unknown" {
		t.Errorf("an unknown stage should render 'unknown'; got %q", GuardStage(99).String())
	}
}

// A read-only loop (no audit options) still carries the always-on fake-green guard (a no-op
// without a verify gate) and nothing else.
func TestLoopGuards_ReadOnlyDefault(t *testing.T) {
	l := New(single(model.NewEcho("qwen", model.Response{})), &fakeProvider{}, nil)
	got := names(l.Guards())
	if len(got) != 1 || got[0] != "fake-green" {
		t.Fatalf("a read-only loop should carry only the always-on fake-green guard; got %v", got)
	}
}
