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

func lsCall(p *Provider, path string, all bool) tool.Result {
	params := map[string]any{}
	if path != "" {
		params["path"] = path
	}
	if all {
		params["all"] = true
	}
	return p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "ls", Params: params})
}

func globCall(p *Provider, pattern, path string) tool.Result {
	params := map[string]any{"pattern": pattern}
	if path != "" {
		params["path"] = path
	}
	return p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "glob", Params: params})
}

func entryNames(m map[string]any) []string {
	var out []string
	for _, e := range m["entries"].([]any) {
		out = append(out, e.(map[string]any)["name"].(string))
	}
	sort.Strings(out)
	return out
}

func filenames(m map[string]any) []string {
	var out []string
	for _, f := range m["filenames"].([]any) {
		out = append(out, f.(string))
	}
	return out
}

func TestLS_ListsEntriesAndTypes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "aaa")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, ".hidden", "x")

	// Without all: dotfiles excluded.
	m := mustValue(t, lsCall(New(), dir, false))
	names := entryNames(m)
	if len(names) != 2 || names[0] != "a.txt" || names[1] != "sub" {
		t.Fatalf("entries = %v, want [a.txt sub]", names)
	}
	if got := int(m["count"].(float64)); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	// Type + size of the file entry.
	for _, e := range m["entries"].([]any) {
		ent := e.(map[string]any)
		switch ent["name"] {
		case "a.txt":
			if ent["type"] != "file" || int(ent["size"].(float64)) != 3 {
				t.Fatalf("a.txt entry = %v", ent)
			}
		case "sub":
			if ent["type"] != "dir" || int(ent["size"].(float64)) != 0 {
				t.Fatalf("sub entry = %v", ent)
			}
		}
	}
}

func TestLS_AllIncludesDotfiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".hidden", "x")
	writeFile(t, dir, "visible", "y")
	m := mustValue(t, lsCall(New(), dir, true))
	names := entryNames(m)
	if len(names) != 2 || names[0] != ".hidden" {
		t.Fatalf("with all=true want [.hidden visible], got %v", names)
	}
}

func TestLS_Symlink(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, dir, "target.txt", "data")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	m := mustValue(t, lsCall(New(), dir, false))
	for _, e := range m["entries"].([]any) {
		ent := e.(map[string]any)
		if ent["name"] == "link" && ent["type"] != "symlink" {
			t.Fatalf("link entry type = %v, want symlink", ent["type"])
		}
	}
}

func TestLS_FileAlias_DefaultDir_Errors(t *testing.T) {
	// `file_path` alias resolves to the canonical path field.
	dir := t.TempDir()
	writeFile(t, dir, "x", "1")
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "ls", Params: map[string]any{"file_path": dir},
	})
	if !r.OK {
		t.Fatalf("file_path alias ls failed: %v", r.Value)
	}
}

func TestLS_NotADirectory(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "f.txt", "x")
	r := lsCall(New(), f, false)
	if r.OK || !containsStr(r, "is not a directory") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestLS_DoesNotExist(t *testing.T) {
	r := lsCall(New(), filepath.Join(t.TempDir(), "nope"), false)
	if r.OK || !containsStr(r, "does not exist") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestLS_InvalidParams(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "ls", Params: map[string]any{"path": 5},
	})
	if r.OK || !containsStr(r, "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestGlob_MatchesByBasenameAtAnyDepth(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "top.go", "x")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub"), "deep.go", "y")
	writeFile(t, dir, "note.txt", "z")

	m := mustValue(t, globCall(New(), "*.go", dir))
	got := filenames(m)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "sub/deep.go" || got[1] != "top.go" {
		t.Fatalf("*.go matches = %v, want [sub/deep.go top.go]", got)
	}
	if int(m["num_files"].(float64)) != 2 || m["truncated"].(bool) {
		t.Fatalf("result = %v", m)
	}
}

func TestGlob_DoublestarPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a/b"), "x.ts", "1")
	writeFile(t, dir, "root.ts", "2")
	m := mustValue(t, globCall(New(), "a/**/*.ts", dir))
	got := filenames(m)
	if len(got) != 1 || got[0] != "a/b/x.ts" {
		t.Fatalf("a/**/*.ts = %v, want [a/b/x.ts]", got)
	}
}

func TestGlob_ExcludesVCSDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".git"), "config.go", "x")
	writeFile(t, dir, "real.go", "y")
	m := mustValue(t, globCall(New(), "*.go", dir))
	got := filenames(m)
	if len(got) != 1 || got[0] != "real.go" {
		t.Fatalf("glob must skip .git, got %v", got)
	}
}

func TestGlob_EmptyMatchesNonNil(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "x")
	m := mustValue(t, globCall(New(), "*.nomatch", dir))
	if m["filenames"] == nil {
		t.Fatal("filenames must be non-nil even when empty")
	}
	if int(m["num_files"].(float64)) != 0 {
		t.Fatalf("num_files = %v, want 0", m["num_files"])
	}
}

func TestGlob_Truncation(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < globMaxResults+5; i++ {
		writeFile(t, dir, "f"+itoa(i)+".dat", "x")
	}
	m := mustValue(t, globCall(New(), "*.dat", dir))
	if int(m["num_files"].(float64)) != globMaxResults || !m["truncated"].(bool) {
		t.Fatalf("expected truncation at %d, got num=%v truncated=%v", globMaxResults, m["num_files"], m["truncated"])
	}
}

func TestGlob_RequiresPattern(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "glob", Params: map[string]any{"path": "/tmp"}})
	if r.OK || !containsStr(r, "requires pattern") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestGlob_InvalidParams(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "glob", Params: map[string]any{"pattern": 5}})
	if r.OK || !containsStr(r, "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestGlob_PathDoesNotExist(t *testing.T) {
	r := globCall(New(), "*.go", filepath.Join(t.TempDir(), "nope"))
	if r.OK || !containsStr(r, "does not exist") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestLS_DefaultDirUsesWorkingDir(t *testing.T) {
	// Empty path → working directory (the package dir during tests).
	r := lsCall(New(), "", false)
	if !r.OK {
		t.Fatalf("ls with default dir failed: %v", r.Value)
	}
}

func TestLS_ReadDirPermissionError(t *testing.T) {
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
	r := lsCall(New(), locked, false)
	if r.OK {
		t.Fatalf("expected a read error on an unreadable dir, got %v", r.Value)
	}
}

func TestGlob_DefaultDir(t *testing.T) {
	r := globCall(New(), "*.go", "") // search the package dir
	if !r.OK {
		t.Fatalf("glob with default dir failed: %v", r.Value)
	}
}

func TestGlob_FilePathAlias(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "x.go", "1")
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "glob",
		Params: map[string]any{"pattern": "*.go", "file_path": dir},
	})
	if !r.OK || len(filenames(mustValue(t, r))) != 1 {
		t.Fatalf("file_path alias glob: %v", r.Value)
	}
}

func TestGlob_SkipsNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "real.go", "x")
	link := filepath.Join(dir, "link.go")
	if err := os.Symlink(filepath.Join(dir, "real.go"), link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	m := mustValue(t, globCall(New(), "*.go", dir))
	got := filenames(m)
	// The symlink is non-regular and skipped; only the real file matches.
	if len(got) != 1 || got[0] != "real.go" {
		t.Fatalf("glob should skip the symlink, got %v", got)
	}
}

func TestGlob_WalkErrorOnUnreadableSubdir(t *testing.T) {
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
	r := globCall(New(), "*.go", dir)
	if r.OK || !containsStr(r, "fs.glob:") {
		t.Fatalf("walk into an unreadable subdir should error, got %v", r.Value)
	}
}

// containsStr reports whether a failed Result's error message contains sub.
func containsStr(r tool.Result, sub string) bool {
	if r.OK {
		return false
	}
	m, ok := r.Value.(map[string]any)
	if !ok {
		return false
	}
	msg, _ := m["error"].(string)
	return msg != "" && strings.Contains(msg, sub)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
