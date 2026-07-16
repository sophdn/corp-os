package coding

import (
	"strings"
	"testing"
)

func gateFor(pkg string) [][]string {
	return [][]string{{"sh", "-c", "go test ./internal/" + pkg + "/"}}
}

// The gate-authoring contract: a feature task with no authored gate is REFUSED — a feature
// has no pre-existing oracle, so an ungated task would fake-green.
func TestBuildFeatureChain_RefusesUngatedTask(t *testing.T) {
	_, err := BuildFeatureChain("feat", "/repo", []FeatureTask{
		{Slug: "a", Goal: "do a", Gate: gateFor("a")},
		{Slug: "b", Goal: "do b" /* no Gate */},
	})
	if err == nil {
		t.Fatal("a feature task without an authored gate must be refused")
	}
	if !strings.Contains(err.Error(), "authored gate") {
		t.Fatalf("error should name the missing authored gate; got %v", err)
	}
}

func TestBuildFeatureChain_RefusesEmptyGoal(t *testing.T) {
	if _, err := BuildFeatureChain("feat", "/repo", []FeatureTask{{Slug: "a", Goal: "  ", Gate: gateFor("a")}}); err == nil {
		t.Fatal("an empty goal must be refused")
	}
}

func TestBuildFeatureChain_RefusesEmptyTasks(t *testing.T) {
	if _, err := BuildFeatureChain("feat", "/repo", nil); err == nil {
		t.Fatal("a feature chain with no tasks must be refused")
	}
}

// A well-formed feature chain assembles: model workers, **/*_test.go protected on every
// task, gates carried through, and the chain passes the full validation.
func TestBuildFeatureChain_AssemblesAndDefaultsProtected(t *testing.T) {
	ch, err := BuildFeatureChain("feat-parse-double", "/repo", []FeatureTask{
		{Slug: "parse", Goal: "create Parse", Gate: gateFor("parse"), Workspace: []string{"internal/parse/parse.go"}},
		{Slug: "double", Goal: "create Double", Gate: gateFor("double"), Workspace: []string{"internal/double/double.go"}},
	})
	if err != nil {
		t.Fatalf("BuildFeatureChain: %v", err)
	}
	if len(ch.Tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(ch.Tasks))
	}
	for _, at := range ch.Tasks {
		if at.Worker.Kind != WorkerModel {
			t.Errorf("task %q should be a model worker", at.Slug)
		}
		if len(at.Protected) != 1 || at.Protected[0] != "**/*_test.go" {
			t.Errorf("task %q should default-protect **/*_test.go; got %v", at.Slug, at.Protected)
		}
		if len(at.Gate) == 0 {
			t.Errorf("task %q lost its gate", at.Slug)
		}
	}
}

// Input refs must point backward — the bridge runs the chain's own validation, so a forward
// reference is rejected.
func TestBuildFeatureChain_RejectsForwardInputRef(t *testing.T) {
	_, err := BuildFeatureChain("feat", "/repo", []FeatureTask{
		{Slug: "a", Goal: "do a", Gate: gateFor("a"), Inputs: map[string]InputRef{"x": {From: "b", Field: "out"}}},
		{Slug: "b", Goal: "do b", Gate: gateFor("b")},
	})
	if err == nil {
		t.Fatal("a forward input reference must be rejected by chain validation")
	}
}

// A valid backward input ref (task b consumes task a's output) assembles fine.
func TestBuildFeatureChain_AcceptsBackwardInputRef(t *testing.T) {
	ch, err := BuildFeatureChain("feat", "/repo", []FeatureTask{
		{Slug: "a", Goal: "do a", Gate: gateFor("a")},
		{Slug: "b", Goal: "do b", Gate: gateFor("b"), Inputs: map[string]InputRef{"x": {From: "a", Field: "out"}}},
	})
	if err != nil {
		t.Fatalf("a backward input ref should assemble: %v", err)
	}
	if ch.Tasks[1].Inputs["x"].From != "a" {
		t.Fatal("the backward input ref should be carried through")
	}
}
