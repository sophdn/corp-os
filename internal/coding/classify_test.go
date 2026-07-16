package coding

import (
	"strings"
	"testing"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name      string
		ar        *ATRecord
		wantLabel string
		wantHint  string
	}{
		{"workspace", &ATRecord{WorkerStatus: WorkerWorkspaceViolation}, "spec_bug", "edit"},
		{"compile-build", &ATRecord{Diagnostic: "go build: undefined: Foo"}, "impl_bug", "branch_fix"},
		{"compile-test", &ATRecord{Diagnostic: "go test [build failed]: imported and not used"}, "test_bug", "edit"},
		{"assertion", &ATRecord{Diagnostic: "--- FAIL: got 1 expected 2"}, "ambiguous", "impl bug"},
		{"exhausted", &ATRecord{WorkerStatus: WorkerMaxIterationsExhausted, Diagnostic: "nothing matched"}, "ambiguous", "branch_fix"},
		{"unknown", &ATRecord{Diagnostic: "weird"}, "ambiguous", "inspect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label, hint := classifyFailure(c.ar)
			if label != c.wantLabel {
				t.Fatalf("label = %q, want %q", label, c.wantLabel)
			}
			if !strings.Contains(hint, c.wantHint) {
				t.Fatalf("hint %q missing %q", hint, c.wantHint)
			}
		})
	}
}
