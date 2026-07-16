package fsorgan

import (
	"os"
	"strings"
	"testing"
)

// stripLineNumberPrefixes is the fs.read-paste fallback: when a worker copies
// fs.read's "<n>\t<content>" numbered output verbatim into old_string (which must
// match the file's RAW text), the exact match misses. The helper strips a single
// leading "<digits>\t" from every line ONLY when every line carries that exact
// shape, so ordinary text is never silently rewritten.

func TestStripLineNumberPrefixes_AllLinesNumbered(t *testing.T) {
	in := "12\tfunc f() {\n13\t\treturn 1\n14\t}"
	out, ok := stripLineNumberPrefixes(in)
	if !ok {
		t.Fatal("a fully numbered block must strip")
	}
	if want := "func f() {\n\treturn 1\n}"; out != want {
		t.Fatalf("strip = %q, want %q", out, want)
	}
}

func TestStripLineNumberPrefixes_EmptyContentLineNumbered(t *testing.T) {
	// fs.read numbers blank lines too: "<n>\t" with empty content.
	in := "5\ta\n6\t\n7\tb"
	out, ok := stripLineNumberPrefixes(in)
	if !ok {
		t.Fatal("a numbered block containing a blank-content line must strip")
	}
	if want := "a\n\nb"; out != want {
		t.Fatalf("strip = %q, want %q", out, want)
	}
}

func TestStripLineNumberPrefixes_OnlyStripsOnePrefix(t *testing.T) {
	// File is itself TSV; fs.read prepends its own number, so the raw line keeps
	// its leading "1\t". Exactly one prefix is removed.
	in := "1\t1\tapple"
	out, ok := stripLineNumberPrefixes(in)
	if !ok || out != "1\tapple" {
		t.Fatalf("strip = %q ok=%v, want %q true", out, ok, "1\tapple")
	}
}

func TestStripLineNumberPrefixes_RefusesWhenNothingToStrip(t *testing.T) {
	// The strip only fires when it ACTUALLY removes a prefix; a block with no
	// "<digits>\t" line at all has nothing to recover and returns ok=false, so
	// ordinary un-numbered pasted code is never rewritten on the strength of this
	// recovery. (A leading tab or bare digits are not the fs.read shape.)
	if _, ok := stripLineNumberPrefixes("\tfoo"); ok {
		t.Fatal("a leading tab with no digits must refuse (nothing to strip)")
	}
	if _, ok := stripLineNumberPrefixes("12foo"); ok {
		t.Fatal("digits without a following tab must refuse (nothing to strip)")
	}
	if _, ok := stripLineNumberPrefixes("func f() {\n\treturn 1\n}"); ok {
		t.Fatal("a fully un-numbered block must refuse (nothing to strip)")
	}
	if _, ok := stripLineNumberPrefixes("12\t"); ok {
		// A single line whose only content is the prefix strips to "" — meaningless
		// as an old_string; refuse rather than match the empty string everywhere.
		t.Fatal("a prefix-only single line must refuse")
	}
}

// Bug 1147: the load-bearing recovery. A worker pastes a block where SOME lines
// carry fs.read's "<n>\t" prefix and some do not (partially hand-stripped). The
// lenient per-line strip removes the prefixes it finds and keeps the rest verbatim,
// so the block can then match the file's raw text — the earlier all-or-nothing
// strip bailed here and the edit never landed.
func TestStripLineNumberPrefixes_MixedContaminationStripsPerLine(t *testing.T) {
	// line 1 numbered, line 2 already clean (leading tab, no number).
	in := "76\t\t\tvar temp = x\n\tfunc foo() {}"
	out, ok := stripLineNumberPrefixes(in)
	if !ok {
		t.Fatal("a mixed numbered/un-numbered block must strip the numbered lines")
	}
	if want := "\t\tvar temp = x\n\tfunc foo() {}"; out != want {
		t.Fatalf("strip = %q, want %q", out, want)
	}
}

func TestStripLineNumberPrefixes_MixedLeadingUnprefixed(t *testing.T) {
	// The un-numbered line comes FIRST — strip must not require the shape up front.
	in := "\tif x {\n8\t\t\tdoThing()\n9\t\t}"
	out, ok := stripLineNumberPrefixes(in)
	if !ok {
		t.Fatal("a leading un-numbered line must not abort the per-line strip")
	}
	if want := "\tif x {\n\t\tdoThing()\n\t}"; out != want {
		t.Fatalf("strip = %q, want %q", out, want)
	}
}

func TestStripLineNumberPrefixes_ToleratesTrailingNewline(t *testing.T) {
	// A worker may include the final newline; mirror lineTrimmedReplace and drop a
	// single trailing empty element before checking.
	in := "3\tx\n4\ty\n"
	out, ok := stripLineNumberPrefixes(in)
	if !ok || out != "x\ny" {
		t.Fatalf("strip = %q ok=%v, want %q true", out, ok, "x\ny")
	}
}

// End-to-end through handleEdit: the exact convergence blocker. A worker reads a
// file (numbered output), pastes a numbered single line into old_string, and the
// edit now lands instead of thrashing the budget to a strong-bound halt.
func TestEdit_LineNumberedSingleLineThroughDispatch(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.go", "alpha\n\tbeta\ngamma\n")
	p := readThenProvider(t, path)
	// fs.read rendered line 2 as "2\t\tbeta"; the worker pasted that verbatim.
	r := editCall(p, path, "2\t\tbeta", "\tBETA", false)
	if !r.OK {
		t.Fatalf("line-numbered old_string should land via fallback, got %v", r.Value)
	}
	got, _ := os.ReadFile(path)
	if want := "alpha\n\tBETA\ngamma\n"; string(got) != want {
		t.Fatalf("file after edit = %q, want %q", got, want)
	}
}

func TestEdit_LineNumberedMultiLineThroughDispatch(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.go", "func f() {\n\treturn 1\n}\n")
	p := readThenProvider(t, path)
	// Worker pasted lines 2-3 of fs.read output; new_string is raw (the fix only
	// strips old_string, never new_string).
	r := editCall(p, path, "2\t\treturn 1\n3\t}", "\treturn 2\n}", false)
	if !r.OK {
		t.Fatalf("multi-line numbered old_string should land, got %v", r.Value)
	}
	got, _ := os.ReadFile(path)
	if want := "func f() {\n\treturn 2\n}\n"; string(got) != want {
		t.Fatalf("file after edit = %q, want %q", got, want)
	}
}

// The not-found error must teach the worker to strip the prefix itself.
func TestEdit_NotFoundHintsLineNumberStrip(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.txt", "content")
	p := readThenProvider(t, path)
	r := editCall(p, path, "absent", "z", false)
	if r.OK {
		t.Fatal("absent old_string must still fail")
	}
	msg := r.Value.(map[string]any)["error"].(string)
	if !strings.Contains(msg, "fs.read") || !strings.Contains(msg, "line-number") {
		t.Fatalf("not-found error should hint the fs.read line-number strip, got %q", msg)
	}
}

// A genuinely numbered paste whose stripped form still does not exist returns the
// honest not-found error — the fallback never invents a match.
func TestEdit_LineNumberedButStrippedStillAbsent(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.go", "func f() {}\n")
	p := readThenProvider(t, path)
	r := editCall(p, path, "9\tnonexistent line", "x", false)
	if r.OK {
		t.Fatal("a numbered paste with no real match must stay not-found")
	}
}

// Bug 1147 end-to-end: the dominant GREEN-blocker. A worker pastes a multi-line
// old_string where only SOME lines carry fs.read's "<n>\t" prefix (the model hand-
// stripped others). The lenient strip restores each numbered line's raw text and
// the edit lands, instead of 22 failed edits thrashing to a strong-bound halt.
func TestEdit_MixedNumberedAndCleanLinesThroughDispatch(t *testing.T) {
	dir := t.TempDir()
	// The real file: a 3-line body with tab indentation.
	path := writeFile(t, dir, "f.go", "func g() int {\n\tx := 1\n\treturn x\n}\n")
	p := readThenProvider(t, path)
	// Worker numbered line 2 but left line 3 clean (mixed contamination). fs.read
	// rendered line 2 "\tx := 1" as "2\t\tx := 1" (number, sep tab, then raw "\tx := 1").
	r := editCall(p, path, "2\t\tx := 1\n\treturn x", "\tx := 2\n\treturn x", false)
	if !r.OK {
		t.Fatalf("mixed numbered/clean old_string should land via lenient strip, got %v", r.Value)
	}
	got, _ := os.ReadFile(path)
	if want := "func g() int {\n\tx := 2\n\treturn x\n}\n"; string(got) != want {
		t.Fatalf("file after edit = %q, want %q", got, want)
	}
}

// Bug 1147 end-to-end: lenient-strip composes with the whitespace-tolerant fuzzy
// fallback. Here the numbered line strips cleanly, but the un-numbered line also
// carries indentation drift (the model dedented it). After the per-line strip the
// block still doesn't match byte-for-byte, so lineTrimmedReplace lands it — the two
// recoveries stack to cover a block contaminated both ways at once.
func TestEdit_MixedNumberedPlusIndentDriftThroughDispatch(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "f.go", "func h() {\n\t\tif cond {\n\t\t\twork()\n\t\t}\n}\n")
	p := readThenProvider(t, path)
	// Line 2 numbered ("2\t\t\tif cond {"); line 3 un-numbered AND dedented by the
	// model ("work()" with only one tab instead of three). Strip fixes line 2; the
	// residual indentation mismatch on line 3 is absorbed by the fuzzy fallback.
	r := editCall(p, path, "2\t\t\tif cond {\n\twork()", "\t\tif cond {\n\t\t\twork2()", false)
	if !r.OK {
		t.Fatalf("mixed strip + indent drift should land via composed fallbacks, got %v", r.Value)
	}
	got, _ := os.ReadFile(path)
	if want := "func h() {\n\t\tif cond {\n\t\t\twork2()\n\t\t}\n}\n"; string(got) != want {
		t.Fatalf("file after edit = %q, want %q", got, want)
	}
}
