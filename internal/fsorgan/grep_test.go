package fsorgan

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"corpos/internal/tool"
)

func grepCall(p *Provider, params map[string]any) tool.Result {
	return p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "grep", Params: params})
}

// numOf reads an int result field, returning 0 when it's absent (omitempty).
func numOf(m map[string]any, k string) int {
	if v, ok := m[k]; ok {
		return int(v.(float64))
	}
	return 0
}

func grepFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "a.go", "package main\nfunc Alpha() {}\n// TODO: alpha\n")
	writeFile(t, dir, "b.go", "package main\nfunc Beta() {}\n")
	writeFile(t, dir, "c.txt", "nothing here\nTODO maybe\n")
	return dir
}

func TestGrep_FilesWithMatchesDefault(t *testing.T) {
	dir := grepFixture(t)
	m := mustValue(t, grepCall(New(), map[string]any{"pattern": "func ", "path": dir}))
	if m["mode"].(string) != "files_with_matches" {
		t.Fatalf("mode = %v", m["mode"])
	}
	got := filenames(m)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Fatalf("filenames = %v, want [a.go b.go]", got)
	}
	if int(m["num_files"].(float64)) != 2 {
		t.Fatalf("num_files = %v", m["num_files"])
	}
}

func TestGrep_ContentMode(t *testing.T) {
	dir := grepFixture(t)
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "TODO", "path": dir, "output_mode": "content",
	}))
	content := m["content"].(string)
	// Two TODO lines across a.go and c.txt, "rel:line:text".
	if !strings.Contains(content, "a.go:3:// TODO: alpha") {
		t.Fatalf("content missing a.go TODO line:\n%s", content)
	}
	if !strings.Contains(content, "c.txt:2:TODO maybe") {
		t.Fatalf("content missing c.txt TODO line:\n%s", content)
	}
	if int(m["num_lines"].(float64)) != 2 {
		t.Fatalf("num_lines = %v, want 2", m["num_lines"])
	}
}

func TestGrep_ContentModeNoLineNumbers(t *testing.T) {
	dir := grepFixture(t)
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "TODO", "path": dir, "output_mode": "content", "show_line_numbers": false,
	}))
	content := m["content"].(string)
	if !strings.Contains(content, "a.go:// TODO: alpha") {
		t.Fatalf("expected rel:text without line number:\n%s", content)
	}
}

func TestGrep_CountMode(t *testing.T) {
	dir := grepFixture(t)
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "TODO", "path": dir, "output_mode": "count",
	}))
	if int(m["num_matches"].(float64)) != 2 {
		t.Fatalf("num_matches = %v, want 2", m["num_matches"])
	}
	if int(m["num_files"].(float64)) != 2 {
		t.Fatalf("num_files = %v, want 2", m["num_files"])
	}
}

func TestGrep_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f.txt", "Hello World\n")
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "hello", "path": dir, "case_insensitive": true,
	}))
	if len(filenames(m)) != 1 {
		t.Fatalf("case-insensitive match expected, got %v", m["filenames"])
	}
}

func TestGrep_SingleFilePath(t *testing.T) {
	dir := grepFixture(t)
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "Alpha", "path": filepath.Join(dir, "a.go"), "output_mode": "content",
	}))
	if !strings.Contains(m["content"].(string), "a.go:2:func Alpha() {}") {
		t.Fatalf("single-file content = %v", m["content"])
	}
}

func TestGrep_GlobFilter(t *testing.T) {
	dir := grepFixture(t)
	// TODO appears in a.go and c.txt; restrict to *.go.
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "TODO", "path": dir, "glob": "*.go",
	}))
	got := filenames(m)
	if len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("glob-filtered grep = %v, want [a.go]", got)
	}
}

func TestGrep_ContextLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f.txt", "one\ntwo\nMATCH\nfour\nfive\n")
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "MATCH", "path": dir, "output_mode": "content", "context": 1,
	}))
	content := m["content"].(string)
	// Match line uses ':'; context lines use '-'.
	if !strings.Contains(content, "f.txt:3:MATCH") {
		t.Fatalf("match line missing:\n%s", content)
	}
	if !strings.Contains(content, "f.txt-2-two") || !strings.Contains(content, "f.txt-4-four") {
		t.Fatalf("context lines missing:\n%s", content)
	}
}

func TestGrep_HeadLimitAndOffset(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("hit line " + itoa(i) + "\n")
	}
	writeFile(t, dir, "many.txt", sb.String())
	limit := 3
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "hit", "path": dir, "output_mode": "content",
		"head_limit": limit, "offset": 2,
	}))
	if int(m["num_lines"].(float64)) != 3 {
		t.Fatalf("num_lines = %v, want 3", m["num_lines"])
	}
	if int(m["applied_limit"].(float64)) != 3 || int(m["applied_offset"].(float64)) != 2 {
		t.Fatalf("paging markers = limit %v offset %v", m["applied_limit"], m["applied_offset"])
	}
}

func TestGrep_HeadLimitZeroUnlimited(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		sb.WriteString("hit\n")
	}
	writeFile(t, dir, "many.txt", sb.String())
	zero := 0
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "hit", "path": dir, "output_mode": "content", "head_limit": zero,
	}))
	if int(m["num_lines"].(float64)) != 300 {
		t.Fatalf("num_lines = %v, want 300 (unlimited)", m["num_lines"])
	}
}

func TestGrep_DefaultHeadLimitTruncates(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 0; i < defaultGrepHeadLimit+20; i++ {
		sb.WriteString("hit\n")
	}
	writeFile(t, dir, "many.txt", sb.String())
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "hit", "path": dir, "output_mode": "content",
	}))
	if int(m["num_lines"].(float64)) != defaultGrepHeadLimit {
		t.Fatalf("num_lines = %v, want %d", m["num_lines"], defaultGrepHeadLimit)
	}
	if int(m["applied_limit"].(float64)) != defaultGrepHeadLimit {
		t.Fatalf("applied_limit = %v", m["applied_limit"])
	}
}

func TestGrep_MaxColumnsClamp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.txt", "MATCH"+strings.Repeat("x", grepMaxColumns+100)+"\n")
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "MATCH", "path": dir, "output_mode": "content",
	}))
	if !strings.Contains(m["content"].(string), "[... truncated]") {
		t.Fatal("long matched line should be clamped")
	}
}

func TestGrep_SkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte("MATCH\x00\x01nul"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "text.txt", "MATCH\n")
	got := filenames(mustValue(t, grepCall(New(), map[string]any{"pattern": "MATCH", "path": dir})))
	if len(got) != 1 || got[0] != "text.txt" {
		t.Fatalf("binary file should be skipped, got %v", got)
	}
}

func TestGrep_EmptyResultNonNil(t *testing.T) {
	dir := grepFixture(t)
	m := mustValue(t, grepCall(New(), map[string]any{"pattern": "zzzz-nomatch", "path": dir}))
	if m["filenames"] == nil {
		t.Fatal("filenames must be non-nil even with no matches")
	}
	if int(m["num_files"].(float64)) != 0 {
		t.Fatalf("num_files = %v, want 0", m["num_files"])
	}
}

func TestGrep_RejectsUnsupportedTypeAndMultiline(t *testing.T) {
	dir := grepFixture(t)
	r1 := grepCall(New(), map[string]any{"pattern": "x", "path": dir, "type": "go"})
	if r1.OK || !containsStr(r1, "`type` filter is not supported") {
		t.Fatalf("type should be rejected, got %v", r1.Value)
	}
	r2 := grepCall(New(), map[string]any{"pattern": "x", "path": dir, "multiline": true})
	if r2.OK || !containsStr(r2, "`multiline` is not supported") {
		t.Fatalf("multiline should be rejected, got %v", r2.Value)
	}
}

func TestGrep_InvalidOutputMode(t *testing.T) {
	dir := grepFixture(t)
	r := grepCall(New(), map[string]any{"pattern": "x", "path": dir, "output_mode": "bogus"})
	if r.OK || !containsStr(r, "invalid output_mode") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestGrep_InvalidPattern(t *testing.T) {
	dir := grepFixture(t)
	r := grepCall(New(), map[string]any{"pattern": "([unclosed", "path": dir})
	if r.OK || !containsStr(r, "invalid pattern") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestGrep_RequiresPatternAndValidates(t *testing.T) {
	r := grepCall(New(), map[string]any{"path": "/tmp"})
	if r.OK || !containsStr(r, "requires pattern") {
		t.Fatalf("error = %v", r.Value)
	}
	r2 := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "grep", Params: map[string]any{"pattern": 5}})
	if r2.OK || !containsStr(r2, "invalid params") {
		t.Fatalf("error = %v", r2.Value)
	}
}

func TestGrep_PathDoesNotExist(t *testing.T) {
	r := grepCall(New(), map[string]any{"pattern": "x", "path": filepath.Join(t.TempDir(), "nope")})
	if r.OK || !containsStr(r, "does not exist") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestGrep_FilePathAliasAndDefaultDir(t *testing.T) {
	dir := grepFixture(t)
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "grep",
		Params: map[string]any{"pattern": "func ", "file_path": dir},
	})
	if !r.OK {
		t.Fatalf("file_path alias grep failed: %v", r.Value)
	}
	// Default dir (search the package dir for a token that exists here).
	r2 := grepCall(New(), map[string]any{"pattern": "package fsorgan"})
	if !r2.OK {
		t.Fatalf("default-dir grep failed: %v", r2.Value)
	}
}

func TestGrep_WalkErrorOnUnreadableSubdir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	r := grepCall(New(), map[string]any{"pattern": "x", "path": dir})
	if r.OK || !containsStr(r, "fs.grep:") {
		t.Fatalf("walk into an unreadable subdir should error, got %v", r.Value)
	}
}

func TestGrep_SkipsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	writeFile(t, dir, "ok.txt", "MATCH\n")
	locked := writeFile(t, dir, "locked.txt", "MATCH\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	got := filenames(mustValue(t, grepCall(New(), map[string]any{"pattern": "MATCH", "path": dir})))
	if len(got) != 1 || got[0] != "ok.txt" {
		t.Fatalf("unreadable file should be skipped, got %v", got)
	}
}

func TestGrep_NegativeContextAndOffsetClamp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f.txt", "HIT\n")
	// Negative context clamps to 0; a huge offset clamps to len (empty output).
	negCtx, bigOffset := -1, 999
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "HIT", "path": dir, "output_mode": "content",
		"context": negCtx, "offset": bigOffset,
	}))
	if numOf(m, "num_lines") != 0 { // omitempty → absent when 0
		t.Fatalf("offset past end should yield 0 lines, got %v", m["num_lines"])
	}

	// A negative offset clamps to 0.
	negOffset := -5
	m2 := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "HIT", "path": dir, "output_mode": "content", "offset": negOffset,
	}))
	if int(m2["num_lines"].(float64)) != 1 {
		t.Fatalf("negative offset should clamp to 0, got %v", m2["num_lines"])
	}
}

func TestParseTrailingCount(t *testing.T) {
	if _, ok := parseTrailingCount("no-colon-here"); ok {
		t.Fatal("a line with no colon should not parse")
	}
	if _, ok := parseTrailingCount("path:notanumber"); ok {
		t.Fatal("a non-integer trailing field should not parse")
	}
	if c, ok := parseTrailingCount("path:7"); !ok || c != 7 {
		t.Fatalf("parseTrailingCount(path:7) = %d,%v", c, ok)
	}
}

func TestGrep_ContextBeforeAfter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f.txt", "a\nb\nHIT\nc\nd\n")
	before, after := 1, 0
	m := mustValue(t, grepCall(New(), map[string]any{
		"pattern": "HIT", "path": dir, "output_mode": "content",
		"context_before": before, "context_after": after,
	}))
	content := m["content"].(string)
	if !strings.Contains(content, "f.txt-2-b") || strings.Contains(content, "f.txt-4-c") {
		t.Fatalf("before=1 after=0 window wrong:\n%s", content)
	}
}

// TestGrepBraceExpansionPathHint: a path that looks like shell brace-expansion or
// a glob (the run-7 worker passed "go/internal/work/{task.go,…}") returns an
// actionable error nudging toward the glob param, not just "path does not exist".
func TestGrepBraceExpansionPathHint(t *testing.T) {
	cases := []struct{ path, want string }{
		{"go/internal/work/{task.go,stamp_validate.go}", "glob"},
		{"dir/a.go,b.go", "glob"},
		{"go/internal/*.go", "glob"},
	}
	for _, c := range cases {
		r := grepCall(New(), map[string]any{"pattern": "x", "path": c.path})
		if r.OK {
			t.Errorf("path %q should not resolve", c.path)
			continue
		}
		m, ok := r.Value.(map[string]any)
		if !ok {
			t.Fatalf("error value not a map: %#v", r.Value)
		}
		msg, _ := m["error"].(string)
		if !strings.Contains(msg, c.want) {
			t.Errorf("error for %q = %q, want a hint mentioning %q", c.path, msg, c.want)
		}
	}
	// A plain missing path keeps a clean error (no glob noise).
	r := grepCall(New(), map[string]any{"pattern": "x", "path": "nope-not-here"})
	if m, _ := r.Value.(map[string]any); strings.Contains(m["error"].(string), "glob") {
		t.Error("a plain missing path should not get the glob hint")
	}
}
