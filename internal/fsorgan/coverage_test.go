package fsorgan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/tool"
)

// TestDecodeParams_MarshalError feeds a value json cannot marshal (a channel),
// exercising decodeParams' marshal-error return via the read handler.
func TestDecodeParams_MarshalError(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "read",
		Params: map[string]any{"file_path": make(chan int)},
	})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

// TestRead_OpenErrorAfterStat covers the open-fails-after-stat-succeeds branch:
// a 0-perm file stats fine (size, mode) but cannot be opened for reading.
func TestRead_OpenErrorAfterStat(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "secret")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	r := readCall(New(), path, 0, 0)
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.read:") {
		t.Fatalf("want an open error, got %v", r.Value)
	}
}

func TestCopyTree_ReadDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, srcDir, "a.txt", "x")
	info, _ := os.Stat(srcDir)
	if err := os.Chmod(srcDir, 0o000); err != nil { // unreadable → ReadDir fails
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(srcDir, 0o755) })
	if err := copyTree(srcDir, filepath.Join(dir, "dst"), info); err == nil {
		t.Fatal("copyTree should error when src is unreadable")
	}
}

func TestRemove_RecursiveRemoveAllError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	sub := filepath.Join(parent, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "f.txt", "x")
	if err := os.Chmod(sub, 0o000); err != nil { // can't traverse → RemoveAll fails
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	r := removeCall(New(), parent, true)
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.remove:") {
		t.Fatalf("want a RemoveAll error, got %v", r.Value)
	}
}
