package fsorgan

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"corpos/internal/tool"
)

// bufioReaderOf wraps r so selectLines (which takes a *bufio.Reader) can be
// driven directly in tests.
func bufioReaderOf(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// writeFile writes content to a fresh temp file and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", p, err)
	}
	return p
}

// readCall dispatches an fs.read for path with optional offset/limit and returns
// the Result.
func readCall(p *Provider, path string, offset, limit int64) tool.Result {
	params := map[string]any{"file_path": path}
	if offset != 0 {
		params["offset"] = offset
	}
	if limit != 0 {
		params["limit"] = limit
	}
	return p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "read", Params: params})
}

func mustValue(t *testing.T, r tool.Result) map[string]any {
	t.Helper()
	if !r.OK {
		t.Fatalf("expected ok result, got error: %v", r.Value)
	}
	m, ok := r.Value.(map[string]any)
	if !ok {
		t.Fatalf("Value is not map[string]any: %T", r.Value)
	}
	return m
}

func TestSpecs(t *testing.T) {
	p := New()
	specs := p.Specs()
	if len(specs) != 1 {
		t.Fatalf("want 1 spec, got %d", len(specs))
	}
	if specs[0].Name != Surface {
		t.Fatalf("spec name = %q, want %q", specs[0].Name, Surface)
	}
	if specs[0].Description == "" {
		t.Fatal("spec description must not be empty")
	}
	schema := specs[0].InputSchema
	props, ok := schema["properties"].(map[string]any)
	if !ok || props["action"] == nil {
		t.Fatalf("input schema missing the action property: %v", schema)
	}
	req, ok := schema["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "action" {
		t.Fatalf("input schema required = %v, want [action]", schema["required"])
	}
}

func TestRead_WholeFileNumbering(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "alpha\nbeta\ngamma")
	m := mustValue(t, readCall(New(), path, 0, 0))

	if got := m["content"].(string); got != "1\talpha\n2\tbeta\n3\tgamma" {
		t.Fatalf("content = %q", got)
	}
	if got := m["start_line"].(float64); got != 1 {
		t.Fatalf("start_line = %v, want 1", got)
	}
	if got := m["line_count"].(float64); got != 3 {
		t.Fatalf("line_count = %v, want 3", got)
	}
	if got := m["total_lines"].(float64); got != 3 {
		t.Fatalf("total_lines = %v, want 3", got)
	}
	if _, ok := m["warning"]; ok {
		t.Fatalf("unexpected warning on a normal read: %v", m["warning"])
	}
}

func TestRead_RangedReadAndOffsetNumbering(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "l1\nl2\nl3\nl4\nl5")
	m := mustValue(t, readCall(New(), path, 2, 2))
	if got := m["content"].(string); got != "2\tl2\n3\tl3" {
		t.Fatalf("ranged content = %q", got)
	}
	if got := m["start_line"].(float64); got != 2 {
		t.Fatalf("start_line = %v, want 2", got)
	}
	if got := m["total_lines"].(float64); got != 5 {
		t.Fatalf("total_lines = %v, want 5", got)
	}
}

func TestRead_OffsetBelowOneNormalizedToOne(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "only")
	m := mustValue(t, readCall(New(), path, -5, 0))
	if got := m["content"].(string); got != "1\tonly" {
		t.Fatalf("content = %q (offset<1 must normalize to 1)", got)
	}
}

func TestRead_TrailingNewlineCountsEmptyFinalLine(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "a\nb\n")
	m := mustValue(t, readCall(New(), path, 0, 0))
	// "a\nb\n" splits to [a, b, ""] — the empty final fragment is counted.
	if got := m["total_lines"].(float64); got != 3 {
		t.Fatalf("total_lines = %v, want 3 (trailing empty line counted)", got)
	}
	if got := m["content"].(string); got != "1\ta\n2\tb\n3\t" {
		t.Fatalf("content = %q", got)
	}
}

func TestRead_StripsBOMAndCRLF(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "\xEF\xBB\xBFwin\r\nlines\r\n")
	m := mustValue(t, readCall(New(), path, 0, 0))
	got := m["content"].(string)
	if strings.Contains(got, "\r") {
		t.Fatalf("CRLF not stripped: %q", got)
	}
	if !strings.HasPrefix(got, "1\twin") {
		t.Fatalf("BOM not stripped (content = %q)", got)
	}
}

func TestRead_EmptyFileWarning(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "empty.txt", "")
	m := mustValue(t, readCall(New(), path, 0, 0))
	// An empty file splits to one empty part: total_lines=1 and the
	// shorter-than-offset(1) warning (matches the toolkit fs contract).
	w, ok := m["warning"].(string)
	if !ok || !strings.Contains(w, "shorter than the provided offset (1). The file has 1 lines") {
		t.Fatalf("empty-file warning = %v", m["warning"])
	}
	if m["content"].(string) != "" {
		t.Fatalf("empty file should have empty content, got %q", m["content"])
	}
	if got := m["total_lines"].(float64); got != 1 {
		t.Fatalf("empty file total_lines = %v, want 1", got)
	}
}

func TestRead_PastEOFWarning(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "one\ntwo")
	m := mustValue(t, readCall(New(), path, 99, 0))
	w, ok := m["warning"].(string)
	if !ok || !strings.Contains(w, "shorter than the provided offset (99)") {
		t.Fatalf("past-EOF warning = %v", m["warning"])
	}
}

func TestRead_WholeFileByteCapExceeded(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxReadSizeBytes+1)
	path := writeFile(t, dir, "big.txt", big)
	r := readCall(New(), path, 0, 0)
	if r.OK {
		t.Fatal("expected byte-cap failure on a whole-file read")
	}
	msg := r.Value.(map[string]any)["error"].(string)
	if !strings.Contains(msg, "exceeds maximum allowed size") {
		t.Fatalf("error = %q", msg)
	}
}

func TestRead_RangedReadBypassesByteCap(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxReadSizeBytes+1)
	path := writeFile(t, dir, "big.txt", big)
	r := readCall(New(), path, 1, 1) // limit set → ranged → cap skipped
	if !r.OK {
		t.Fatalf("ranged read of a large file should bypass the cap: %v", r.Value)
	}
}

func TestRead_PathAlias(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "aliased")
	// Use `path` instead of `file_path`.
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "read",
		Params: map[string]any{"path": path},
	})
	m := mustValue(t, r)
	if got := m["content"].(string); got != "1\taliased" {
		t.Fatalf("path-alias read content = %q", got)
	}
}

func TestRead_MissingFilePath(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "read", Params: map[string]any{}})
	if r.OK {
		t.Fatal("expected error for missing file_path")
	}
	if !strings.Contains(r.Value.(map[string]any)["error"].(string), "requires file_path") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestRead_InvalidParams(t *testing.T) {
	// file_path as a number fails to decode into the typed params.
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "read",
		Params: map[string]any{"file_path": 123},
	})
	if r.OK {
		t.Fatal("expected error for non-string file_path")
	}
	if !strings.Contains(r.Value.(map[string]any)["error"].(string), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestRead_NotExist(t *testing.T) {
	r := readCall(New(), filepath.Join(t.TempDir(), "nope.txt"), 0, 0)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "does not exist") {
		t.Fatalf("want does-not-exist error, got %v", r.Value)
	}
}

func TestRead_Directory(t *testing.T) {
	r := readCall(New(), t.TempDir(), 0, 0)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "is a directory") {
		t.Fatalf("want is-a-directory error, got %v", r.Value)
	}
}

func TestRead_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, sub, "f.txt", "secret")
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	r := readCall(New(), path, 0, 0)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "permission denied") {
		t.Fatalf("want permission-denied error, got %v", r.Value)
	}
}

func TestRead_PerLineCap(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", maxReadLineLen+50)
	path := writeFile(t, dir, "long.txt", long)
	m := mustValue(t, readCall(New(), path, 1, 1)) // ranged so the byte cap is skipped
	if !strings.Contains(m["content"].(string), "... [truncated]") {
		t.Fatal("pathological line should be truncated with the cap marker")
	}
}

func TestRead_RecordsReadStateFullVsRanged(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "a\nb\nc")
	info, _ := os.Stat(path)
	mtime := info.ModTime().UnixMilli()

	// A whole-file read records full state → writable.
	p := New()
	readCall(p, path, 0, 0)
	if ok, _ := p.reads.checkWritable(path, mtime); !ok {
		t.Fatal("whole-file read should satisfy the write precondition")
	}

	// A ranged read is partial → not writable.
	p2 := New()
	readCall(p2, path, 2, 1)
	if ok, _ := p2.reads.checkWritable(path, mtime); ok {
		t.Fatal("ranged read must NOT satisfy the write precondition")
	}
}

func TestDispatch_UnknownAction(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "frobnicate"})
	if r.OK {
		t.Fatal("unknown action should fail")
	}
	// An fs failure is worker-recoverable (ClassUsage), not a model-capability
	// fault: a costlier rung can't fix a deterministic fs error, so it must not
	// trip escalation (bug escalate-after-1-…).
	if r.ErrorClass != tool.ClassUsage {
		t.Fatalf("error class = %q, want usage_error", r.ErrorClass)
	}
	if !strings.Contains(r.Value.(map[string]any)["error"].(string), "unknown action") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestDispatch_LatencyFromInjectedClock(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "x")
	p := New()
	// Two ticks: start then end, 7ms apart.
	times := []time.Time{
		time.Unix(0, 0),
		time.Unix(0, 7*int64(time.Millisecond)),
	}
	i := 0
	p.now = func() time.Time {
		tm := times[i]
		if i < len(times)-1 {
			i++
		}
		return tm
	}
	r := readCall(p, path, 0, 0)
	if r.LatencyMS != 7 {
		t.Fatalf("latency = %d, want 7", r.LatencyMS)
	}
	if !r.OK || r.ErrorClass != tool.ClassNone {
		t.Fatalf("expected ok/none, got ok=%v class=%q", r.OK, r.ErrorClass)
	}
}

func TestExpandUserPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~":          home,
		"~/sub/x":    filepath.Join(home, "sub/x"),
		"/abs/path":  "/abs/path",
		"rel/path":   "rel/path",
		"~user/path": "~user/path", // ~user is deliberately not resolved
	}
	for in, want := range cases {
		if got := expandUserPath(in); got != want {
			t.Errorf("expandUserPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadRegistry_Semantics(t *testing.T) {
	var nilReg *readRegistry
	if ok, _ := nilReg.checkWritable("/x", 0); !ok {
		t.Fatal("nil registry imposes no guard")
	}
	nilReg.markRead("/x", 1, true) // no panic on nil

	r := newReadRegistry()
	if ok, reason := r.checkWritable("/p", 10); ok || reason == "" {
		t.Fatal("unread path must not be writable")
	}
	r.markRead("/p", 10, false) // partial
	if ok, _ := r.checkWritable("/p", 10); ok {
		t.Fatal("partial read must not satisfy the precondition")
	}
	r.markRead("/p", 10, true) // full
	if ok, _ := r.checkWritable("/p", 10); !ok {
		t.Fatal("full read at same mtime should be writable")
	}
	if ok, reason := r.checkWritable("/p", 11); ok || !strings.Contains(reason, "modified since read") {
		t.Fatalf("a newer mtime must block the write: ok=%v reason=%q", ok, reason)
	}
}

func TestRead_GenericStatError(t *testing.T) {
	// A NUL byte makes the path invalid (EINVAL) — neither NotExist nor
	// Permission, exercising the generic stat-error branch.
	r := readCall(New(), "bad\x00path", 0, 0)
	if r.OK {
		t.Fatal("expected error for an invalid path")
	}
	msg := r.Value.(map[string]any)["error"].(string)
	if !strings.HasPrefix(msg, "fs.read:") {
		t.Fatalf("error = %q", msg)
	}
}

// erroringReader returns a non-EOF error immediately, to exercise selectLines'
// mid-stream error path.
type erroringReader struct{}

func (erroringReader) Read([]byte) (int, error) { return 0, os.ErrInvalid }

func TestSelectLines_ReadError(t *testing.T) {
	_, _, err := selectLines(bufioReaderOf(erroringReader{}), 0, -1)
	if err == nil {
		t.Fatal("expected a read error to propagate")
	}
}

func TestExpandUserPath_NoHomeFallback(t *testing.T) {
	t.Setenv("HOME", "") // makes os.UserHomeDir fail on Linux → homeDir returns ""
	if os.Getenv("HOME") != "" {
		t.Skip("HOME could not be cleared")
	}
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("UserHomeDir still resolves without HOME on this platform")
	}
	// With no resolvable home, ~ and ~/x are returned unchanged.
	if got := expandUserPath("~"); got != "~" {
		t.Fatalf("expandUserPath(~) without home = %q, want ~", got)
	}
	if got := expandUserPath("~/x"); got != "~/x" {
		t.Fatalf("expandUserPath(~/x) without home = %q, want ~/x", got)
	}
}

func TestHumanByteSize(t *testing.T) {
	cases := map[int64]string{
		512:                 "512 bytes",
		1024:                "1KB",
		1536:                "1.5KB",
		1024 * 1024:         "1MB",
		1024 * 1024 * 1024:  "1GB",
		3 * 1024 * 1024 / 2: "1.5MB",
	}
	for in, want := range cases {
		if got := humanByteSize(in); got != want {
			t.Errorf("humanByteSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSpecs_ActionEnumProjectable(t *testing.T) {
	props := New().Specs()[0].InputSchema["properties"].(map[string]any)
	action := props["action"].(map[string]any)
	enum, ok := action["enum"].([]any)
	if !ok || len(enum) == 0 {
		t.Fatal("action must carry an enum so a job-profile can action-scope the fs surface (mcp.Project fails closed otherwise)")
	}
	got := map[string]bool{}
	for _, e := range enum {
		got[e.(string)] = true
	}
	for _, want := range []string{"read", "write", "edit", "move", "remove", "ls", "grep", "glob"} {
		if !got[want] {
			t.Errorf("action enum missing %q (must match the Dispatch switch)", want)
		}
	}
}
