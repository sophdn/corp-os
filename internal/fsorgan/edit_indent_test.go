package fsorgan

import (
	"os"
	"testing"
)

// Bug 1110: inserting into a tab-indented Go map via the whitespace-tolerant
// fallback re-indented the SURROUNDING lines (1 tab -> 2), producing gofmt-breaking
// diffs. Root cause: lineTrimmedReplace rebased new_string by OLD_string's indent
// basis, so a model that dedented old_string but left new_string indented got every
// new line double-indented. The reindent must rebase on new_string's OWN basis.

func TestLineTrimmedReplace_IndentedNewDedentedOld_NoDoubleIndent(t *testing.T) {
	// File map is one-tab-indented; old_string dedented (0 tabs); new_string keeps
	// the one-tab indent and inserts "c". Output must stay at one tab everywhere.
	content := "var t = map[string]int{\n\t\"a\": 1,\n\t\"b\": 2,\n}\n"
	old := "\"a\": 1,\n\"b\": 2,"                     // model dropped the tabs
	newStr := "\t\"a\": 1,\n\t\"c\": 3,\n\t\"b\": 2," // model kept the tabs
	out, ok := lineTrimmedReplace(content, old, newStr)
	if !ok {
		t.Fatal("whitespace-tolerant match should succeed")
	}
	want := "var t = map[string]int{\n\t\"a\": 1,\n\t\"c\": 3,\n\t\"b\": 2,\n}\n"
	if out != want {
		t.Fatalf("neighbour indentation corrupted:\n got %q\nwant %q", out, want)
	}
}

func TestLineTrimmedReplace_NestedRelativeIndentPreserved(t *testing.T) {
	// new_string carries internal relative indent (a nested block); rebasing must
	// preserve the relative structure, not flatten or double it.
	content := "func f() {\n\tif x {\n\t\told()\n\t}\n}\n"
	old := "if x {\nold()\n}"                  // dedented
	newStr := "if x {\n\tnewer()\n\tmore()\n}" // dedented, but with internal nesting
	out, ok := lineTrimmedReplace(content, old, newStr)
	if !ok {
		t.Fatal("match should succeed")
	}
	// The block rebases to the file's one-tab basis; the inner lines keep their extra tab.
	want := "func f() {\n\tif x {\n\t\tnewer()\n\t\tmore()\n\t}\n}\n"
	if out != want {
		t.Fatalf("nested indent not preserved:\n got %q\nwant %q", out, want)
	}
}

// End-to-end through handleEdit: the Run-20/24 repro — adding a priceTable-shaped
// entry to a tab-indented Go map yields a gofmt-clean diff (neighbours untouched).
func TestEdit_TabIndentedMapInsertPreservesNeighbourIndent(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nvar m = map[string]int{\n\t\"alpha\": 1,\n\t\"beta\": 2,\n}\n"
	path := writeFile(t, dir, "m.go", src)
	p := readThenProvider(t, path)
	// Model dedents old_string, leaves new_string tab-indented, inserts "gamma".
	old := "\"alpha\": 1,\n\"beta\": 2,"
	newS := "\t\"alpha\": 1,\n\t\"gamma\": 3,\n\t\"beta\": 2,"
	r := editCall(p, path, old, newS, false)
	if !r.OK {
		t.Fatalf("edit should land: %v", r.Value)
	}
	got, _ := os.ReadFile(path)
	want := "package p\n\nvar m = map[string]int{\n\t\"alpha\": 1,\n\t\"gamma\": 3,\n\t\"beta\": 2,\n}\n"
	if string(got) != want {
		t.Fatalf("neighbour indentation corrupted:\n got %q\nwant %q", got, want)
	}
}
