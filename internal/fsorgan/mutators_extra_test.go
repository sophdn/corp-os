package fsorgan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/tool"
)

func TestEdit_PathAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aliased.txt")
	// `path` alias on a create-new edit (empty old_string).
	r := New().Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "edit",
		Params: map[string]any{"path": path, "old_string": "", "new_string": "made"},
	})
	if !r.OK {
		t.Fatalf("path-alias edit failed: %v", r.Value)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "made" {
		t.Fatalf("content = %q", b)
	}
}

func TestEdit_PermissionDenied(t *testing.T) {
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
	r := editCall(New(), filepath.Join(locked, "f.txt"), "a", "b", false)
	if r.OK || !strings.Contains(r.Value.(map[string]any)["error"].(string), "permission denied") {
		t.Fatalf("want permission-denied error, got %v", r.Value)
	}
}

func TestWrite_GenericStatError(t *testing.T) {
	// A NUL byte makes os.Stat fail with EINVAL — neither NotExist nor Permission.
	r := writeCall(New(), "bad\x00path", "x")
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.write:") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestEdit_GenericStatError(t *testing.T) {
	r := editCall(New(), "bad\x00path", "a", "b", false)
	if r.OK || !strings.HasPrefix(r.Value.(map[string]any)["error"].(string), "fs.edit:") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestWriteFileMkdir_Errors(t *testing.T) {
	dir := t.TempDir()

	// MkdirAll error: a parent path component is a regular file, not a dir.
	asFile := writeFile(t, dir, "afile", "x")
	if err := writeFileMkdir(filepath.Join(asFile, "child.txt"), "y"); err == nil {
		t.Fatal("expected MkdirAll error when a parent component is a file")
	}

	// WriteFile error: the target path is an existing directory.
	if err := writeFileMkdir(dir, "y"); err == nil {
		t.Fatal("expected WriteFile error when the target is a directory")
	}
}
