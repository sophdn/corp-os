package fsorgan

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// TestGrepThenRangedReadOnLargeFile is the run-6d scenario in miniature: a target
// function lives deep in a file too large to read whole (task.go was 100KB; the
// worker grep-thrashed instead of ranged-reading). It proves the supported path —
// whole read refused, grep locates the symbol's line, ranged read returns just
// that function and bypasses the byte cap — works at the organ level.
func TestGrepThenRangedReadOnLargeFile(t *testing.T) {
	dir := t.TempDir()
	const targetLine = 20001
	var b strings.Builder
	for i := 1; i < targetLine; i++ {
		b.WriteString("// filler line of source code to pad the file\n")
	}
	b.WriteString("func task_stamp_sha(id int, sha string) error { return stamp(id, sha) }\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("// trailing filler\n")
	}
	path := writeFile(t, dir, "task.go", b.String())
	p := New()

	// A whole-file read is refused — the worker cannot just slurp it.
	if r := readCall(p, path, 0, 0); r.OK {
		t.Fatal("whole read of a >256KB file must fail, forcing grep+ranged read")
	}

	// grep locates the symbol with its line number.
	gr := mustValue(t, p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "grep", Params: map[string]any{
		"pattern":           "func task_stamp_sha",
		"path":              path,
		"output_mode":       "content",
		"show_line_numbers": true,
	}}))
	content, _ := gr["content"].(string)
	line := firstGrepLineNumber(t, content)
	if line != targetLine {
		t.Fatalf("grep reported line %d, want %d", line, targetLine)
	}

	// A ranged read around the hit returns the function within a tiny budget,
	// bypassing the byte cap that blocked the whole read.
	rr := mustValue(t, readCall(p, path, int64(line), 1))
	got, _ := rr["content"].(string)
	if !strings.Contains(got, "func task_stamp_sha") {
		t.Fatalf("ranged read around the grep hit should return the function, got %q", got)
	}
	if lc, _ := rr["line_count"].(float64); lc != 1 {
		t.Fatalf("ranged read should return exactly the 1 requested line, got %v", rr["line_count"])
	}
}

// firstGrepLineNumber extracts the line number from the first content-mode grep
// line ("<path>:<line>:<text>").
func firstGrepLineNumber(t *testing.T, content string) int {
	t.Helper()
	first := strings.SplitN(strings.TrimSpace(content), "\n", 2)[0]
	m := regexp.MustCompile(`:(\d+):`).FindStringSubmatch(first)
	if m == nil {
		t.Fatalf("no line number in grep content line %q", first)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("bad line number %q: %v", m[1], err)
	}
	return n
}
