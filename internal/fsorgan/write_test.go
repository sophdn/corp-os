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

func writeCall(p *Provider, path, content string) tool.Result {
	return p.Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "write",
		Params: map[string]any{"file_path": path, "content": content},
	})
}

// writeCallOverwrite issues an fs.write with the explicit overwrite escape hatch.
func writeCallOverwrite(p *Provider, path, content string) tool.Result {
	return p.Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "write",
		Params: map[string]any{"file_path": path, "content": content, "overwrite": true},
	})
}

func TestWrite_CreatesNewFileAndParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested/deep/new.txt")
	m := mustValue(t, writeCall(New(), path, "hello\nworld"))
	if m["created"].(bool) != true {
		t.Fatal("created should be true for a new file")
	}
	if got := m["bytes_written"].(float64); got != 11 {
		t.Fatalf("bytes_written = %v, want 11", got)
	}
	if got := m["line_count"].(float64); got != 2 {
		t.Fatalf("line_count = %v, want 2", got)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "hello\nworld" {
		t.Fatalf("file content = %q err=%v", b, err)
	}
}

func TestWrite_EmptyContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	m := mustValue(t, writeCall(New(), path, ""))
	if got := m["bytes_written"].(float64); got != 0 {
		t.Fatalf("bytes_written = %v, want 0", got)
	}
	if got := m["line_count"].(float64); got != 0 {
		t.Fatalf("line_count = %v, want 0", got)
	}
}

func TestWrite_OverwriteRequiresPriorRead(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "original")
	// overwrite:true clears the clobber guard so the read precondition is the
	// check under test: an unread existing file still cannot be overwritten.
	r := writeCallOverwrite(New(), path, "replaced")
	if r.OK {
		t.Fatal("overwriting an unread existing file must fail")
	}
	if !strings.Contains(r.Value.(map[string]any)["error"].(string), "has not been read yet") {
		t.Fatalf("error = %v", r.Value)
	}
}

// TestWrite_ExistingPathRefusedWithoutOverwrite is the load-bearing regression
// for the data-loss bug: a plain fs.write to an EXISTING path must be REFUSED
// (clobber guard) — even when the read precondition is satisfied — so a worker
// intending to "create" cannot silently destroy the file. The refusal must be
// actionable (name fs.edit and the overwrite escape hatch).
func TestWrite_ExistingPathRefusedWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "original")
	p := New()
	readCall(p, path, 0, 0) // read precondition satisfied — the guard must STILL refuse
	r := writeCall(p, path, "replaced")
	if r.OK {
		t.Fatal("fs.write to an existing path must be refused without overwrite:true")
	}
	msg := r.Value.(map[string]any)["error"].(string)
	if !strings.Contains(msg, "already exists") || !strings.Contains(msg, "overwrite") || !strings.Contains(msg, "fs.edit") {
		t.Fatalf("refusal message not actionable: %v", msg)
	}
	// The file must be untouched (no data loss).
	if b, _ := os.ReadFile(path); string(b) != "original" {
		t.Fatalf("file was clobbered despite refusal: %q", b)
	}
}

func TestWrite_OverwriteEscapeHatchSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "original")
	p := New()
	readCall(p, path, 0, 0) // satisfy the read precondition
	m := mustValue(t, writeCallOverwrite(p, path, "replaced"))
	if m["created"].(bool) != false {
		t.Fatal("created should be false when overwriting")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "replaced" {
		t.Fatalf("content = %q, want replaced", b)
	}
}

func TestWrite_NewPathSucceedsWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brand-new.txt")
	m := mustValue(t, writeCall(New(), path, "fresh"))
	if m["created"].(bool) != true {
		t.Fatal("created should be true for a brand-new path")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "fresh" {
		t.Fatalf("content = %q, want fresh", b)
	}
}

func TestWrite_ModifiedSinceReadBlocks(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "v1")
	p := New()
	readCall(p, path, 0, 0)
	// Bump the file's mtime to the future → modified-since-read.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	// Use the overwrite escape hatch so the clobber guard is bypassed and the
	// read-state precondition (modified-since-read) is the check under test.
	r := writeCallOverwrite(p, path, "v2")
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "modified since read") {
		t.Fatalf("want modified-since-read block, got %v", r.Value)
	}
}

func TestWrite_ImmediateSecondWriteNeedsNoReread(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	p := New()
	writeCall(p, path, "first") // creates + records written state
	// The second write targets a now-EXISTING path, so the clobber guard requires
	// an explicit overwrite intent; given that, no re-read is needed.
	r := writeCallOverwrite(p, path, "second")
	if !r.OK {
		t.Fatalf("an overwrite right after a write should not require a re-read: %v", r.Value)
	}
}

func TestWrite_DirectoryPath(t *testing.T) {
	r := writeCall(New(), t.TempDir(), "x")
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "is a directory") {
		t.Fatalf("want is-a-directory error, got %v", r.Value)
	}
}

func TestWrite_MissingFilePath(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "write", Params: map[string]any{"content": "x"},
	})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "requires file_path") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestWrite_InvalidParams(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "write", Params: map[string]any{"file_path": 7},
	})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestWrite_PathAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aliased.txt")
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "write",
		Params: map[string]any{"path": path, "content": "z"},
	})
	if !r.OK {
		t.Fatalf("path-alias write failed: %v", r.Value)
	}
}

func TestWrite_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r := writeCall(New(), filepath.Join(locked, "f.txt"), "x")
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "permission denied") {
		t.Fatalf("want permission-denied error, got %v", r.Value)
	}
}

func TestLineCount(t *testing.T) {
	cases := map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2}
	for in, want := range cases {
		if got := lineCount(in); got != want {
			t.Errorf("lineCount(%q) = %d, want %d", in, got, want)
		}
	}
}
