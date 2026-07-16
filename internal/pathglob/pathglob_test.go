package pathglob

import "testing"

func TestMatches(t *testing.T) {
	hits := [][2]string{
		{"a/b/c", "a/b/c"},
		{"a/b/c", "a/**"},
		{"x/y_test.go", "**/*_test.go"},
		{"internal/dispatch/dispatch_test.go", "**/*_test.go"},
		{"a/b", "a/*"},
		{"a/b/c/d", "a/**/d"},
		{"/leading/slash", "leading/slash"},
		{"a", "**"},
		{"", "**"},
	}
	for _, c := range hits {
		if !Matches(c[0], c[1]) {
			t.Errorf("Matches(%q,%q) = false, want true", c[0], c[1])
		}
	}
	misses := [][2]string{
		{"a/b/c", "a/b"},
		{"dispatch.go", "**/*_test.go"},
		{"a/b/c", "a/*"},
		{"a/x/d", "a/b/**"},
		{"", "a"},
		{"a/b", "a/b/c"},
	}
	for _, c := range misses {
		if Matches(c[0], c[1]) {
			t.Errorf("Matches(%q,%q) = true, want false", c[0], c[1])
		}
	}
}

func TestMatchesAny(t *testing.T) {
	if !MatchesAny("x/y_test.go", []string{"a/**", "**/*_test.go"}) {
		t.Error("MatchesAny should hit the second pattern")
	}
	if MatchesAny("dispatch.go", []string{"a/**", "**/*_test.go"}) {
		t.Error("MatchesAny should miss when no pattern matches")
	}
	if MatchesAny("anything", nil) {
		t.Error("MatchesAny over an empty pattern set must be false")
	}
}
