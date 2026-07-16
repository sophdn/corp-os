package fsorgan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"corpos/internal/tool"
)

func moveCall(p *Provider, src, dest string) tool.Result {
	return p.Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "move",
		Params: map[string]any{"source": src, "dest": dest},
	})
}

func removeCall(p *Provider, path string, recursive bool) tool.Result {
	params := map[string]any{"file_path": path}
	if recursive {
		params["recursive"] = true
	}
	return p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "remove", Params: params})
}

func TestMove_RenameFile(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "data")
	dest := filepath.Join(dir, "b.txt")
	m := mustValue(t, moveCall(New(), src, dest))
	if m["dest"].(string) != dest || m["is_dir"].(bool) || m["cross_device"].(bool) {
		t.Fatalf("result = %v", m)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source should be gone after move")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "data" {
		t.Fatalf("dest content = %q", b)
	}
}

func TestMove_IntoExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "x")
	destDir := filepath.Join(dir, "target")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := mustValue(t, moveCall(New(), src, destDir))
	want := filepath.Join(destDir, "a.txt")
	if m["dest"].(string) != want {
		t.Fatalf("final dest = %v, want %v (mv-into-dir semantics)", m["dest"], want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("entry not moved into the dir: %v", err)
	}
}

func TestMove_CreatesDestParents(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "x")
	dest := filepath.Join(dir, "new/deep/b.txt")
	if !moveCall(New(), src, dest).OK {
		t.Fatal("move should create missing dest parents")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("dest missing: %v", err)
	}
}

func TestMove_MoveDirectory(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "moved")
	m := mustValue(t, moveCall(New(), srcDir, dest))
	if m["is_dir"].(bool) != true {
		t.Fatal("is_dir should be true for a directory move")
	}
}

func TestMove_RefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "x")
	dest := writeFile(t, dir, "b.txt", "exists")
	r := moveCall(New(), src, dest)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "destination already exists") {
		t.Fatalf("move must refuse to clobber, got %v", r.Value)
	}
}

func TestMove_SourceDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	r := moveCall(New(), filepath.Join(dir, "nope"), filepath.Join(dir, "x"))
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "source does not exist") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestMove_MissingParams(t *testing.T) {
	r1 := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "move", Params: map[string]any{"dest": "/x"}})
	if r1.OK || !strings.Contains(r1.Value.(map[string]any)["error"].(string), "requires source") {
		t.Fatalf("error = %v", r1.Value)
	}
	r2 := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "move", Params: map[string]any{"source": "/x"}})
	if r2.OK || !strings.Contains(r2.Value.(map[string]any)["error"].(string), "requires dest") {
		t.Fatalf("error = %v", r2.Value)
	}
}

func TestMove_InvalidParams(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "move", Params: map[string]any{"source": 5}})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestMove_Aliases(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "x")
	dest := filepath.Join(dir, "b.txt")
	// src/to aliases instead of source/dest.
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "move",
		Params: map[string]any{"src": src, "to": dest},
	})
	if !r.OK {
		t.Fatalf("alias move failed: %v", r.Value)
	}

	// from/destination aliases.
	src2 := writeFile(t, dir, "c.txt", "y")
	dest2 := filepath.Join(dir, "d.txt")
	r2 := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "move",
		Params: map[string]any{"from": src2, "destination": dest2},
	})
	if !r2.OK {
		t.Fatalf("from/destination alias move failed: %v", r2.Value)
	}
}

func TestMove_CrossDeviceFallback(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, srcDir, "a.txt", "alpha")
	writeFile(t, filepath.Join(srcDir, "sub"), "b.txt", "beta")
	dest := filepath.Join(dir, "moved")

	p := New()
	p.renameFn = func(_, _ string) error { return syscall.EXDEV } // force the fallback
	m := mustValue(t, moveCall(p, srcDir, dest))
	if m["cross_device"].(bool) != true {
		t.Fatal("cross_device should be true when rename returns EXDEV")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "sub/b.txt")); string(b) != "beta" {
		t.Fatalf("nested file not copied across devices: %q", b)
	}
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Fatal("source should be removed after a cross-device move")
	}
}

func TestMove_NonCrossDeviceRenameError(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "x")
	p := New()
	p.renameFn = func(_, _ string) error { return os.ErrInvalid } // not EXDEV → surfaced as-is
	r := moveCall(p, src, filepath.Join(dir, "b.txt"))
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.move:") {
		t.Fatalf("a non-EXDEV rename error should surface, got %v", r.Value)
	}
}

func TestMove_SourcePermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, locked, "a.txt", "x")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r := moveCall(New(), filepath.Join(locked, "a.txt"), filepath.Join(dir, "b.txt"))
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "permission denied") {
		t.Fatalf("want permission-denied, got %v", r.Value)
	}
}

func TestMove_GenericStatError(t *testing.T) {
	r := moveCall(New(), "bad\x00src", "/tmp/x")
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.move:") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestMove_DestGenericStatError(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "x")
	r := moveCall(New(), src, "bad\x00dest")
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.move:") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestMove_DestPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	src := writeFile(t, dir, "a.txt", "x")
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r := moveCall(New(), src, filepath.Join(locked, "b.txt"))
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "permission denied") {
		t.Fatalf("want dest permission-denied, got %v", r.Value)
	}
}

func TestRemove_File(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "x")
	m := mustValue(t, removeCall(New(), path, false))
	if m["was_dir"].(bool) != false {
		t.Fatal("was_dir should be false for a file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file should be gone")
	}
}

func TestRemove_EmptyDirectoryWithoutRecursive(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	m := mustValue(t, removeCall(New(), empty, false))
	if m["was_dir"].(bool) != true {
		t.Fatal("was_dir should be true")
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("empty dir should be removed without recursive")
	}
}

func TestRemove_NonEmptyDirNeedsRecursive(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full")
	if err := os.Mkdir(full, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, full, "child.txt", "x")
	r := removeCall(New(), full, false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "non-empty directory") {
		t.Fatalf("want non-empty error, got %v", r.Value)
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatal("nothing should have been deleted")
	}
	// With recursive, it goes.
	m := mustValue(t, removeCall(New(), full, true))
	if m["was_dir"].(bool) != true {
		t.Fatal("recursive remove of a dir should report was_dir")
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Fatal("dir should be gone after recursive remove")
	}
}

func TestRemove_DoesNotExist(t *testing.T) {
	r := removeCall(New(), filepath.Join(t.TempDir(), "nope"), false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "does not exist") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestRemove_RefusesProtectedRoot(t *testing.T) {
	r := removeCall(New(), "/etc", true)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "protected filesystem root") {
		t.Fatalf("want protected-root refusal, got %v", r.Value)
	}
}

func TestRemove_MissingFilePath(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "remove", Params: map[string]any{}})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "requires file_path") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestRemove_InvalidParams(t *testing.T) {
	r := New().Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "remove", Params: map[string]any{"file_path": 1}})
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestRemove_PathAlias(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "x")
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "remove",
		Params: map[string]any{"path": path},
	})
	if !r.OK {
		t.Fatalf("path-alias remove failed: %v", r.Value)
	}
}

func TestRemove_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses filesystem permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, locked, "f.txt", "x")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r := removeCall(New(), filepath.Join(locked, "f.txt"), false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "permission denied") {
		t.Fatalf("want permission-denied error, got %v", r.Value)
	}
}

func TestCopyTree_FileAndDir(t *testing.T) {
	// Directly exercise the cross-device copy helpers (the EXDEV fallback can't
	// be triggered portably in a unit test, but the copy machinery can).
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, srcDir, "top.txt", "top")
	writeFile(t, filepath.Join(srcDir, "nested"), "deep.txt", "deep")

	dst := filepath.Join(dir, "dst")
	info, _ := os.Stat(srcDir)
	if err := copyTree(srcDir, dst, info); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "nested/deep.txt")); string(b) != "deep" {
		t.Fatalf("nested file not copied: %q", b)
	}

	// copyFile error path: a non-existent source.
	if err := copyFile(filepath.Join(dir, "nope"), filepath.Join(dir, "x"), 0o644); err == nil {
		t.Fatal("copyFile should error on a missing source")
	}

	// copyFile dst-open error: dst parent is a regular file, not a dir.
	asFile := writeFile(t, dir, "afile", "x")
	if err := copyFile(filepath.Join(srcDir, "top.txt"), filepath.Join(asFile, "x"), 0o644); err == nil {
		t.Fatal("copyFile should error when the dst parent is a file")
	}

	// copyTree MkdirAll error: dst under a regular file.
	if err := copyTree(srcDir, filepath.Join(asFile, "x"), info); err == nil {
		t.Fatal("copyTree should error when dst's parent is a file")
	}

	// copyTree child-copy error propagation: a dir whose child file cannot be
	// written because the destination subtree collides with a file.
	collide := filepath.Join(dir, "collide")
	if err := os.WriteFile(collide, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	// dst exists as a FILE, so MkdirAll(dst) fails immediately — exercises the
	// top-level dir branch error too.
	if err := copyTree(srcDir, collide, info); err == nil {
		t.Fatal("copyTree should error when dst already exists as a file")
	}
}

func TestRemove_GenericStatError(t *testing.T) {
	r := removeCall(New(), "bad\x00path", false)
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.remove:") {
		t.Fatalf("error = %v", r.Value)
	}
}
