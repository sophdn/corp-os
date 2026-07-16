package fsorgan

import (
	"os"
	"strings"
	"testing"
)

// lineTrimmedReplace is the whitespace-tolerant fallback: it matches line-by-line
// ignoring surrounding whitespace, only on a UNIQUE contiguous block, and reindents
// new_string to the block's real indentation.
func TestLineTrimmedReplace_DroppedIndentUniqueMatch(t *testing.T) {
	content := "package x\n\nfunc f() {\n\t\tproject := \"\"\n\t\treturn project\n}\n"
	// The model dropped the leading tabs on both lines (a common error).
	old := "project := \"\"\nreturn project"
	newStr := "project := resolve()\nreturn project"
	out, ok := lineTrimmedReplace(content, old, newStr)
	if !ok {
		t.Fatal("a unique line-trimmed match must succeed")
	}
	// The replacement is reindented to the block's real two-tab indent.
	want := "package x\n\nfunc f() {\n\t\tproject := resolve()\n\t\treturn project\n}\n"
	if out != want {
		t.Fatalf("reindented replace wrong:\n got %q\nwant %q", out, want)
	}
}

func TestLineTrimmedReplace_TrailingWhitespaceTolerated(t *testing.T) {
	content := "a := 1   \nb := 2\n" // trailing spaces in the file
	out, ok := lineTrimmedReplace(content, "a := 1", "a := 9")
	if !ok {
		t.Fatal("trailing-whitespace-only difference must match")
	}
	if !strings.Contains(out, "a := 9") || strings.Contains(out, "a := 1") {
		t.Fatalf("expected a:=9 replacement, got %q", out)
	}
}

func TestLineTrimmedReplace_AmbiguousRefuses(t *testing.T) {
	// Two whitespace-normalized matches → refuse (never fuzzy-guess which one).
	content := "  dup\nother\n  dup\n"
	if _, ok := lineTrimmedReplace(content, "dup", "changed"); ok {
		t.Fatal("an ambiguous fuzzy match must be refused, not guessed")
	}
}

func TestLineTrimmedReplace_NoMatchReturnsFalse(t *testing.T) {
	if _, ok := lineTrimmedReplace("foo\nbar\n", "nope", "x"); ok {
		t.Fatal("absent match must return ok=false")
	}
}

func TestLineTrimmedReplace_LastLineNoTrailingNewline(t *testing.T) {
	content := "first\n    target" // last line, no trailing newline, indented
	out, ok := lineTrimmedReplace(content, "target", "fixed")
	if !ok {
		t.Fatal("match on the final (newline-less) line must succeed")
	}
	if out != "first\n    fixed" {
		t.Fatalf("got %q", out)
	}
}

// End-to-end through handleEdit: an existing file read first, then an fs.edit whose
// old_string has the wrong indentation still lands (the worker no longer thrashes).
func TestEdit_WhitespaceTolerantThroughDispatch(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.go", "func g() {\n\t\tx := old\n}\n")
	p := readThenProvider(t, path)
	// old_string drops the two tabs the file actually has.
	r := editCall(p, path, "x := old", "x := new", false)
	if !r.OK {
		t.Fatalf("whitespace-tolerant edit should succeed, got %v", r.Value)
	}
	got, _ := os.ReadFile(path)
	if want := "func g() {\n\t\tx := new\n}\n"; string(got) != want {
		t.Fatalf("file after edit = %q, want %q", got, want)
	}
}

// The fuzzy fallback never fires for replace_all (too blunt): a non-exact old_string
// with replace_all set still returns the honest not-found error.
func TestEdit_NoFuzzyForReplaceAll(t *testing.T) {
	dir := t.TempDir()
	// File indents with a TAB; the old_string carries SPACES — so it is not an exact
	// substring (count==0) but IS a line-trimmed match. With replace_all the fuzzy
	// fallback is skipped, so this stays an honest not-found.
	path := writeFile(t, dir, "f.go", "\tx := old\n")
	p := readThenProvider(t, path)
	r := editCall(p, path, "  x := old", "  x := new", true)
	if r.OK {
		t.Fatal("replace_all must not use the fuzzy fallback")
	}
	if msg := r.Value.(map[string]any)["error"].(string); !strings.Contains(msg, "not found") {
		t.Fatalf("want not-found error, got %q", msg)
	}
}
