package profile

import "testing"

// TestSelect_BuiltinLibrary pins the live embedded library: representative daily-driver
// prompts must land on the intended profile (no -profile flag), and a generic prompt
// must fall back to the safe default. This is the end-to-end guard that the authored
// `signals` keep matching real phrasings as the library evolves.
func TestSelect_BuiltinLibrary(t *testing.T) {
	reg, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	const def = "orchestrate"
	cases := []struct {
		prompt string
		want   string
	}{
		{"review this diff for correctness", "code-review"},
		// Bug 1144: a free-form code-fix with NO filed bug referenced (nil envelope → no
		// bug_slug) falls through to the coding default, NOT the flat bug-fix worker — that
		// is how the DeepSeek coding rung is reached for a direct code-fix prompt.
		{"fix the crash in the parser", def},
		{"research the latest Go generics proposals on the web", "web-research"},
		{"refactor the router to preserve behavior", "refactor"},
		{"commit and push the change", "git-process"},
		{"synthesize the patterns across recent retros", "synthesis"},
		{"hello, what can you do today?", def}, // no signal → safe default
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			got := Select(c.prompt, nil, reg, def)
			if got.Profile != c.want {
				t.Fatalf("Select(%q) = %q (%s), want %q", c.prompt, got.Profile, got.Reason, c.want)
			}
		})
	}
}

// TestSelect_BuiltinBugFixNeedsFiledBug pins bug 1144's constraint on the LIVE library: the
// bug-fix profile is auto-selected only when a filed bug (a bug_slug) is referenced. The
// same "fix ..." prompt routes to bug-fix WITH a bug_slug and to the coding default WITHOUT.
func TestSelect_BuiltinBugFixNeedsFiledBug(t *testing.T) {
	reg, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	const def = "orchestrate"
	prompt := "fix the failing test in calc.go"
	if got := Select(prompt, envOf("path"), reg, def); got.Profile != def {
		t.Fatalf("code-fix with no filed bug must route to the coding default, got %q (%s)", got.Profile, got.Reason)
	}
	if got := Select(prompt, envOf("bug_slug"), reg, def); got.Profile != "bug-fix" {
		t.Fatalf("a filed-bug reference must select bug-fix, got %q (%s)", got.Profile, got.Reason)
	}
}

// TestSelect_DefaultIsToolBearing guards criterion 2: the safe default must be a real,
// projected, tool-bearing profile — never unprojected/toolless.
func TestSelect_DefaultIsToolBearing(t *testing.T) {
	reg, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	p, ok := reg.Get("orchestrate")
	if !ok {
		t.Fatal("default profile orchestrate missing from the library")
	}
	if len(p.Tools) == 0 {
		t.Fatal("default profile orchestrate is toolless — violates the safe-default contract")
	}
}
