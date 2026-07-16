package fsorgan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadState_PartialReadDoesNotDowngradeFullMark is the run-10 regression: a
// worker reads a file WHOLE, then pages it in ranges (offset/limit) to fit a small
// context window — the ranged pages must NOT revoke the edit permission the whole
// read earned, or the worker can never fs.edit a file it has fully read.
func TestReadState_PartialReadDoesNotDowngradeFullMark(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.go")
	if err := os.WriteFile(fp, []byte("package x\n\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New()
	if r := readCall(p, fp, 0, 0); !r.OK { // whole read → full mark
		t.Fatalf("whole read failed: %v", r.Value)
	}
	if r := readCall(p, fp, 2, 0); !r.OK { // ranged page (offset) — must not downgrade
		t.Fatalf("ranged read failed: %v", r.Value)
	}
	if r := readCall(p, fp, 1, 2); !r.OK { // ranged page (limit) — must not downgrade
		t.Fatalf("ranged read failed: %v", r.Value)
	}
	if r := editCall(p, fp, "Foo", "Bar", false); !r.OK {
		t.Fatalf("edit after whole-read-then-pages must be allowed, got %v", r.Value)
	}
}

// TestReadState_ActionableErrors: a never-read file names the whole-file read; a
// file only ever read partially names the partial cause (so the worker recovers
// instead of looping on a generic "not read yet" — the run-10 loop).
func TestReadState_ActionableErrors(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.go")
	if err := os.WriteFile(fp, []byte("package x\n\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	never := editCall(New(), fp, "Foo", "Bar", false)
	if never.OK || !strings.Contains(never.Value.(map[string]any)["error"].(string), "no offset/limit") {
		t.Fatalf("never-read edit should name a whole-file read, got %v", never.Value)
	}

	p := New()
	if r := readCall(p, fp, 2, 0); !r.OK { // a partial read only, no prior full
		t.Fatalf("ranged read failed: %v", r.Value)
	}
	partial := editCall(p, fp, "Foo", "Bar", false)
	if partial.OK || !strings.Contains(partial.Value.(map[string]any)["error"].(string), "partial/ranged") {
		t.Fatalf("partial-only edit should name the partial-read cause, got %v", partial.Value)
	}
}

// TestReadState_FreshFullReadStillRecordsAfterStaleFullMark: when the file changes
// after a full read, a later non-full read does replace the (now stale) full mark,
// so editing is correctly re-blocked until a fresh whole read.
func TestReadState_PartialReplacesStaleFullMark(t *testing.T) {
	r := newReadRegistry()
	r.markRead("/f", 100, true)  // full mark at mtime 100
	r.markRead("/f", 200, false) // file changed (mtime 200) + a partial read → stale full replaced
	if ok, reason := r.checkWritable("/f", 200); ok || !strings.Contains(reason, "partial/ranged") {
		t.Fatalf("a partial read after the file changed must re-block editing, got ok=%v reason=%q", ok, reason)
	}
}
