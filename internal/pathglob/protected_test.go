package pathglob

import "testing"

// TestIsProtected_Exceptions is the bug 1089 enabler: a "!"-prefixed pattern un-protects
// a path so a test-authoring profile can protect all production Go source yet still write
// its own *_test.go deliverable.
func TestIsProtected_Exceptions(t *testing.T) {
	protectProdAllowTests := []string{"**/*.go", "!**/*_test.go"}
	cases := []struct {
		path string
		want bool
	}{
		{"internal/agent/loop.go", true},       // production source → protected
		{"internal/agent/loop_test.go", false}, // test file → exception un-protects
		{"cmd/corpos/main.go", true},           // nested production → protected
		{"internal/x/y/z_test.go", false},      // deeply nested test → un-protected
		{"README.md", false},                   // not Go at all → never matched a positive
	}
	for _, c := range cases {
		if got := IsProtected(c.path, protectProdAllowTests); got != c.want {
			t.Errorf("IsProtected(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestIsProtected_NoExceptionsEqualsMatchesAny: with no "!" patterns IsProtected is
// exactly MatchesAny, so every existing protected set behaves unchanged.
func TestIsProtected_NoExceptionsEqualsMatchesAny(t *testing.T) {
	pats := []string{"**/*_test.go", "acceptance/**"}
	for _, p := range []string{"a/b_test.go", "acceptance/x.go", "src/main.go", "x/y.go"} {
		if IsProtected(p, pats) != MatchesAny(p, pats) {
			t.Errorf("IsProtected and MatchesAny disagree on %q with no exceptions", p)
		}
	}
}

// TestIsProtected_ExceptionWinsRegardlessOfOrder: an exception un-protects even when it
// is listed before the positive pattern that would otherwise match.
func TestIsProtected_ExceptionWinsRegardlessOfOrder(t *testing.T) {
	if IsProtected("pkg/foo_test.go", []string{"!**/*_test.go", "**/*.go"}) {
		t.Error("a leading exception must still un-protect the path")
	}
}
