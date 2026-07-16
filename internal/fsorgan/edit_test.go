package fsorgan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"corpos/internal/tool"
)

func editCall(p *Provider, path, oldS, newS string, replaceAll bool) tool.Result {
	params := map[string]any{"file_path": path, "old_string": oldS, "new_string": newS}
	if replaceAll {
		params["replace_all"] = true
	}
	return p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "edit", Params: params})
}

// readThenProvider returns a Provider that has already recorded a full read of
// path (satisfying the edit precondition).
func readThenProvider(t *testing.T, path string) *Provider {
	t.Helper()
	p := New()
	if r := readCall(p, path, 0, 0); !r.OK {
		t.Fatalf("setup read failed: %v", r.Value)
	}
	return p
}

func TestEdit_NoChangeWhenOldEqualsNew(t *testing.T) {
	r := editCall(New(), "/whatever", "same", "same", false)
	if r.OK {
		t.Fatalf("identical old/new must be rejected, got %v", r.Value)
	}
	msg := r.Value.(map[string]any)["error"].(string)
	if !strings.Contains(msg, "must differ") || !strings.Contains(msg, "no-op") {
		t.Fatalf("old==new error should be actionable (name no-op + must differ), got %q", msg)
	}
}

func TestEdit_CreateNewFileViaEmptyOldString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "created.txt")
	m := mustValue(t, editCall(New(), path, "", "brand new", false))
	if m["created"].(bool) != true || m["replacements"].(float64) != 1 {
		t.Fatalf("result = %v", m)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "brand new" {
		t.Fatalf("content = %q", b)
	}
}

func TestEdit_NonexistentFileNonEmptyOld(t *testing.T) {
	r := editCall(New(), filepath.Join(t.TempDir(), "nope.txt"), "x", "y", false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "does not exist") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_ExistingFileRequiresPriorRead(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "hello world")
	r := editCall(New(), path, "world", "there", false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "has not been read yet") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_SingleReplacement(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "alpha beta alpha")
	p := readThenProvider(t, path)
	m := mustValue(t, editCall(p, path, "beta", "GAMMA", false))
	if m["replacements"].(float64) != 1 {
		t.Fatalf("replacements = %v, want 1", m["replacements"])
	}
	b, _ := os.ReadFile(path)
	if string(b) != "alpha GAMMA alpha" {
		t.Fatalf("content = %q", b)
	}
}

func TestEdit_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "x x x")
	p := readThenProvider(t, path)
	m := mustValue(t, editCall(p, path, "x", "y", true))
	if m["replacements"].(float64) != 3 {
		t.Fatalf("replacements = %v, want 3", m["replacements"])
	}
	b, _ := os.ReadFile(path)
	if string(b) != "y y y" {
		t.Fatalf("content = %q", b)
	}
}

func TestEdit_AmbiguousWithoutReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "dup dup")
	p := readThenProvider(t, path)
	r := editCall(p, path, "dup", "z", false)
	msg := r.Value.(map[string]any)["error"].(string)
	if r.OK || !strings.Contains(msg, "Found 2 matches") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "content")
	p := readThenProvider(t, path)
	r := editCall(p, path, "absent", "z", false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "not found in file") {
		t.Fatalf("error = %v", r.Value)
	}
}

// TestEdit_NotFoundShowsClosestText reproduces the fs-edit-reliability failure: a
// model abbreviates the target line (a 50-char prefix of the real 100+-char line),
// so the exact match misses. The error must now show the closest ACTUAL text —
// including the tail the model dropped — so it can copy verbatim next attempt.
func TestEdit_NotFoundShowsClosestText(t *testing.T) {
	dir := t.TempDir()
	full := "\t\treturn fmt.Errorf(\"fs.read: file content exceeds maximum allowed size (%s). Use offset and limit to read portions.\")\n"
	path := writeFile(t, dir, "f.go", "package fs\n\nfunc f() {\n"+full+"}\n")
	p := readThenProvider(t, path)
	// The model reproduces only a prefix of the real line and adds a stray quote
	// (the real drift from the repro), so it is genuinely absent from the file.
	r := editCall(p, path, "fs.read: file content exceeds maximum allowed size'", "z", false)
	msg := r.Value.(map[string]any)["error"].(string)
	if r.OK || !strings.Contains(msg, "not found in file") {
		t.Fatalf("expected not-found error, got %v", r.Value)
	}
	if !strings.Contains(msg, "closest existing text") {
		t.Fatalf("error should offer the closest text hint, got %q", msg)
	}
	if !strings.Contains(msg, "Use offset and limit to read portions") {
		t.Fatalf("hint should include the tail the model dropped, got %q", msg)
	}
}

// TestEdit_MultiLineNotFoundCoachesSingleAnchor: a multi-line old_string that misses
// gets the insertion-pattern coaching (anchor on one line), the lever for the block-drift
// failure. A single-line miss does NOT (it needs the verbatim/hint guidance, not this).
func TestEdit_MultiLineNotFoundCoachesSingleAnchor(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.go", "package p\n\nfunc a() {}\nfunc b() {}\nfunc c() {}\n")
	p := readThenProvider(t, path)
	// A 3-line old_string that is absent (drifted block).
	r := editCall(p, path, "func a() {}\nWRONG LINE\nfunc c() {}", "z", false)
	msg := r.Value.(map[string]any)["error"].(string)
	if r.OK || !strings.Contains(msg, "INSERTING code") || !strings.Contains(msg, "ONE short unique") {
		t.Fatalf("multi-line not-found should coach the single-anchor insertion pattern, got %q", msg)
	}
	// A single-line miss must NOT carry the block coaching.
	r2 := editCall(p, path, "func absent_single_line() {}", "z", false)
	msg2 := r2.Value.(map[string]any)["error"].(string)
	if strings.Contains(msg2, "INSERTING code") {
		t.Fatalf("single-line miss should not carry block coaching, got %q", msg2)
	}
}

func TestEditLineCount(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{{"a", 1}, {"a\nb", 2}, {"a\nb\n", 2}, {"a\nb\nc", 3}} {
		if got := editLineCount(c.in); got != c.want {
			t.Errorf("editLineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNearestEditHint_Abbreviation(t *testing.T) {
	content := "line zero\n\tconst message = \"exceeds the maximum allowed size limit\"\nline two\n"
	// Anchor drifts (drops the tail) but shares most trigrams with the real line.
	hint, ok := nearestEditHint(content, "exceeds the maximum allowed")
	if !ok {
		t.Fatalf("expected a hint for a near-miss abbreviation")
	}
	if !strings.Contains(hint, "exceeds the maximum allowed size limit") {
		t.Fatalf("hint should be the full real line, got %q", hint)
	}
}

func TestNearestEditHint_MultiLineWindow(t *testing.T) {
	content := "alpha line one here\nbeta line two here\ngamma line three here\ndelta line four\n"
	// 3-line old_string; only the first line anchors, the rest drifted.
	old := "alpha line one here\nWRONG SECOND LINE\nWRONG THIRD LINE"
	hint, ok := nearestEditHint(content, old)
	if !ok {
		t.Fatalf("expected a hint")
	}
	if !strings.Contains(hint, "beta line two here") || !strings.Contains(hint, "gamma line three here") {
		t.Fatalf("hint window should span oldStr's line count of real lines, got %q", hint)
	}
	if strings.Contains(hint, "delta line four") {
		t.Fatalf("hint window should stop at oldStr's line count, got %q", hint)
	}
}

func TestNearestEditHint_TrailingNewlineWindow(t *testing.T) {
	content := "alpha line one here\nbeta line two here\n"
	hint, ok := nearestEditHint(content, "alpha line one here\n")
	if !ok || strings.Contains(hint, "beta") {
		t.Fatalf("a trailing-newline single-line anchor should return one line, got %q (ok=%v)", hint, ok)
	}
}

func TestNearestEditHint_ClampsWindowToMaxLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("anchor line for matching here\n")
	for i := 0; i < 30; i++ {
		b.WriteString("filler body line number here\n")
	}
	// A 20-line old_string anchored on the first line; the window must clamp to 12.
	old := "anchor line for matching here" + strings.Repeat("\nx", 19)
	hint, ok := nearestEditHint(b.String(), old)
	if !ok {
		t.Fatalf("expected a hint")
	}
	if got := strings.Count(hint, "\n") + 1; got != editHintMaxLines {
		t.Fatalf("window should clamp to %d lines, got %d", editHintMaxLines, got)
	}
}

func TestNearestEditHint_ClampsWindowToEOF(t *testing.T) {
	content := "first unrelated line here\nthe anchor line lives at the end"
	// A 5-line old_string anchored on the LAST content line: the window truncates to
	// what remains (one line) rather than running past EOF.
	hint, ok := nearestEditHint(content, "the anchor line lives at the end\na\nb\nc\nd")
	if !ok {
		t.Fatalf("expected a hint")
	}
	if strings.Contains(hint, "first unrelated") || strings.Count(hint, "\n") != 0 {
		t.Fatalf("EOF window should be the single trailing line, got %q", hint)
	}
}

func TestNearestEditHint_ShortAnchorNoHint(t *testing.T) {
	if _, ok := nearestEditHint("the quick brown fox line\n", "short"); ok {
		t.Fatalf("a sub-12-rune anchor must not produce a hint")
	}
}

func TestNearestEditHint_NothingSimilarNoHint(t *testing.T) {
	if _, ok := nearestEditHint("the quick brown fox jumps over\n}\n", "zzzzzzzzzzzz nothing alike qqqqq"); ok {
		t.Fatalf("a dissimilar anchor must not produce a hint")
	}
}

func TestTrigrams_ShortReturnsEmpty(t *testing.T) {
	if len(trigrams("ab")) != 0 {
		t.Fatalf("fewer than 3 runes yields no trigrams")
	}
	if trigramOverlap(trigrams("abcabc"), trigrams("abc")) < 1 {
		t.Fatalf("overlapping strings should share trigrams")
	}
}

func TestEdit_EmptyOldStringOnNonEmptyFileErrs(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "has content")
	p := readThenProvider(t, path)
	r := editCall(p, path, "", "x", false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "Cannot create new file") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_EmptyOldStringFillsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "")
	p := readThenProvider(t, path)
	m := mustValue(t, editCall(p, path, "", "filled", false))
	if m["created"].(bool) != false || m["replacements"].(float64) != 1 {
		t.Fatalf("result = %v", m)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "filled" {
		t.Fatalf("content = %q", b)
	}
}

func TestEdit_CRLFNormalizedBeforeMatch(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "line1\r\nline2\r\n")
	p := readThenProvider(t, path)
	// old_string uses LF; matching works because CRLF is normalized first.
	m := mustValue(t, editCall(p, path, "line1\nline2", "merged", false))
	if m["replacements"].(float64) != 1 {
		t.Fatalf("replacements = %v", m["replacements"])
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "\r") {
		t.Fatalf("output must be LF-normalized, got %q", b)
	}
}

func TestEdit_DirectoryPath(t *testing.T) {
	r := editCall(New(), t.TempDir(), "a", "b", false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "is a directory") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_MissingFilePath(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "edit",
		Params: map[string]any{"old_string": "a", "new_string": "b"},
	})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "requires file_path") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_InvalidParams(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "edit", Params: map[string]any{"file_path": 9},
	})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_ModifiedSinceReadBlocks(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "abc")
	p := readThenProvider(t, path)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	r := editCall(p, path, "abc", "xyz", false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "modified since read") {
		t.Fatalf("error = %v", r.Value)
	}
}
