package agent

import (
	"strings"
	"testing"

	"corpos/internal/tool"
)

// writeResult is a successful fs mutation dispatch of path p (the isMutatingWrite signal).
func writeResult(action, p string) tool.Result {
	return tool.Result{Call: tool.Call{Surface: "fs", Action: action, Params: map[string]any{"path": p}}, OK: true}
}

func TestIsTestFilePath(t *testing.T) {
	yes := []string{"internal/fs/read_test.go", "a/b/c_test.go", "x_test.go"}
	for _, p := range yes {
		if !isTestFilePath(p) {
			t.Errorf("expected test-file path: %q", p)
		}
	}
	no := []string{"internal/fs/read.go", "test.go", "_testdata/x.go", "readme.md", ""}
	for _, p := range no {
		if isTestFilePath(p) {
			t.Errorf("did not expect a test-file match: %q", p)
		}
	}
}

func TestWorkerAuthoredTestPaths(t *testing.T) {
	d := []tool.Result{
		writeResult("write", "internal/fs/read_test.go"),    // counted
		writeResult("edit", "internal/fs/read.go"),          // not a test file
		writeResult("write", "internal/fs/read_test.go"),    // duplicate → deduped
		writeResult("edit", "internal/coding/gate_test.go"), // counted
		{Call: tool.Call{Surface: "fs", Action: "write", // failed write → not counted
			Params: map[string]any{"path": "internal/fs/x_test.go"}}, OK: false},
		{Call: tool.Call{Surface: "work", Action: "forge"}, OK: true}, // not fs
	}
	got := workerAuthoredTestPaths(d)
	if len(got) != 2 {
		t.Fatalf("workerAuthoredTestPaths = %v, want 2 distinct test paths", got)
	}
	want := map[string]bool{"internal/fs/read_test.go": true, "internal/coding/gate_test.go": true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected authored test path %q", p)
		}
	}
}

func TestFakeGreenVerdict(t *testing.T) {
	// No worker-authored test landed → clean green (empty verdict).
	clean := []tool.Result{
		writeResult("edit", "internal/fs/read.go"),
		writeResult("write", "internal/fs/parse.go"),
	}
	if v := fakeGreenVerdict(clean); v != "" {
		t.Fatalf("a green with no worker-authored test should be clean; got %q", v)
	}
	// A failed test-file write does not taint the green (only a landed mutation counts).
	failedTestWrite := []tool.Result{
		{Call: tool.Call{Surface: "fs", Action: "write",
			Params: map[string]any{"path": "internal/fs/read_test.go"}}, OK: false},
	}
	if v := fakeGreenVerdict(failedTestWrite); v != "" {
		t.Fatalf("a DENIED test write must not be a fake-green; got %q", v)
	}
	// A landed worker-authored test → fake-green verdict naming the path.
	authored := []tool.Result{writeResult("write", "internal/fs/read_test.go")}
	v := fakeGreenVerdict(authored)
	if v == "" {
		t.Fatal("a landed worker-authored test must yield a fake-green verdict")
	}
	if !strings.Contains(v, "fake-green") || !strings.Contains(v, "internal/fs/read_test.go") {
		t.Fatalf("verdict should be a fake-green naming the authored path; got %q", v)
	}
}
