package agent

import (
	"regexp"
	"strings"
)

// distillGateOutput reduces raw `go build`/`go test` combined output to the failure-salient
// lines a worker needs to revise, dropping the unrelated-package log spam and passing-test
// chatter that buries the real signal. Run-42 surfaced the need: a whole-module `sh -c "go
// build ./... && go test ./..."` gate (which ScopeGoTest cannot narrow inside the shell
// string) fed back multi-KB dominated by hundreds of `{"level":"WARN",…}` slog lines, with
// the one real `--- FAIL` lost mid-wall — illegible to a floor model. The distilled text
// keeps compile errors (file:line:col:), `--- FAIL` blocks and their indented assertion
// lines, panics, import-cycle lines, `# pkg` build headers, and the `FAIL` summary; it drops
// structured app-log JSON, `=== RUN`/`--- PASS`/`--- SKIP` chatter, and `ok` passing-package
// lines. It is display-only (the grounding reflex still parses the RAW output, so a dropped
// line can never break grounding) and falls back to the raw output when nothing is recognized.
func distillGateOutput(raw string) string {
	const (
		maxLines   = 80  // bound the distilled size on a many-failure run
		maxLineLen = 400 // a kept assertion line can be huge (asserts on a 50KB string)
	)
	var kept []string
	for _, ln := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || isNoiseLine(t) {
			continue
		}
		if !isSalientGateLine(t) {
			continue
		}
		if len(ln) > maxLineLen {
			ln = ln[:maxLineLen] + " …(line truncated)"
		}
		kept = append(kept, ln)
		if len(kept) >= maxLines {
			kept = append(kept, "…(further output omitted)")
			break
		}
	}
	if strings.TrimSpace(strings.Join(kept, "\n")) == "" {
		// Nothing recognized (a non-go gate, or an unfamiliar format) — feed the raw output
		// back rather than an empty message, so the worker never loses the signal entirely.
		return raw
	}
	return strings.Join(kept, "\n")
}

// goFileLocus matches a Go source locus — a compile error (`file.go:12:34:`) or a test
// assertion line (`file_test.go:24:`). The trailing colon distinguishes a diagnostic from a
// bare stack-frame reference (`file.go:123 +0x16`), which we don't need.
var goFileLocus = regexp.MustCompile(`\.go:\d+(:\d+)?:`)

// isNoiseLine reports whether a trimmed line is known build/test noise to drop BEFORE the
// salient check (so a noisy line that happens to contain a salient token is still dropped).
func isNoiseLine(t string) bool {
	// Structured slog JSON the test binary emitted (the Run-42 WARN spam).
	if strings.HasPrefix(t, "{") && strings.Contains(t, `"level":`) {
		return true
	}
	// go test progress/marker chatter and passing results.
	switch {
	case strings.HasPrefix(t, "=== RUN"),
		strings.HasPrefix(t, "=== CONT"),
		strings.HasPrefix(t, "=== PAUSE"),
		strings.HasPrefix(t, "=== NAME"),
		strings.HasPrefix(t, "--- PASS"),
		strings.HasPrefix(t, "--- SKIP"),
		t == "PASS",
		strings.HasPrefix(t, "ok  "), // a passing package summary ("ok  \tpkg\t0.1s")
		strings.HasPrefix(t, "ok\t"):
		return true
	}
	return false
}

// isSalientGateLine reports whether a trimmed line carries failure signal worth feeding back.
func isSalientGateLine(t string) bool {
	switch {
	case strings.Contains(t, "FAIL"), // "--- FAIL: TestX", "FAIL\tpkg", "[build failed]"
		strings.HasPrefix(t, "panic:"),
		strings.Contains(t, "import cycle"),
		strings.HasPrefix(t, "# "), // go build's per-package error header ("# pkg/path")
		goFileLocus.MatchString(t):
		return true
	}
	return false
}
