package agent

import (
	"strings"
	"testing"
)

// The Run-42 shape: a whole-module gate's output dominated by unrelated-package WARN slog
// lines with one real `--- FAIL` block buried inside. Distillation must surface the FAIL +
// its assertion and drop the JSON spam.
func TestDistillGateOutput_DropsLogSpamKeepsFail(t *testing.T) {
	raw := strings.Join([]string{
		`{"time":"2026-06-26T19:38:26Z","level":"WARN","msg":"forge registry: reserved field-name collision","file":"a.toml"}`,
		`{"time":"2026-06-26T19:38:27Z","level":"WARN","msg":"forge registry: reserved field-name collision","file":"b.toml"}`,
		`=== RUN   TestCapForResponse`,
		`--- FAIL: TestCapForResponse (0.00s)`,
		`    remote_test.go:24: For input 'aaa': expected truncate true, got truncate false`,
		`--- PASS: TestParseFAILError (0.01s)`, // a PASSING test whose NAME contains "FAIL"
		`FAIL`,
		`FAIL	toolkit/internal/admin	14.949s`,
		`ok  	toolkit/internal/work	0.10s`,
	}, "\n")
	got := distillGateOutput(raw)
	if strings.Contains(got, "forge registry") {
		t.Fatalf("distilled output must drop the unrelated WARN log spam; got:\n%s", got)
	}
	// A passing line must be dropped even when its test name contains "FAIL" (the
	// passing-chatter drop must run BEFORE the salient-"FAIL" keep — isNoiseLine first).
	if strings.Contains(got, "--- PASS") || strings.Contains(got, "TestParseFAILError") || strings.Contains(got, "=== RUN") || strings.Contains(got, "ok  ") {
		t.Fatalf("distilled output must drop passing/progress chatter; got:\n%s", got)
	}
	for _, want := range []string{"--- FAIL: TestCapForResponse", "remote_test.go:24:", "expected truncate true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("distilled output must keep the salient failure line %q; got:\n%s", want, got)
		}
	}
}

// Compile errors and import cycles are grounding-relevant and must always survive distillation.
func TestDistillGateOutput_KeepsCompileAndCycleErrors(t *testing.T) {
	raw := strings.Join([]string{
		`{"level":"INFO","msg":"noise"}`,
		`# toolkit/internal/admin`,
		`./remote_test.go:5:2: "strings" imported and not used`,
		`./remote_test.go:12:9: undefined: capForResponse`,
		`package admin`,
		`	imports toolkit/internal/admin: import cycle not allowed in test`,
	}, "\n")
	got := distillGateOutput(raw)
	for _, want := range []string{
		"# toolkit/internal/admin",
		`./remote_test.go:5:2: "strings" imported and not used`,
		"undefined: capForResponse",
		"import cycle not allowed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("distilled output must keep grounding-relevant line %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "noise") {
		t.Fatalf("distilled output must drop the INFO log line; got:\n%s", got)
	}
}

// A very long kept line (an assertion on a 50KB string) is truncated so it can't re-bloat the
// feedback it was meant to make legible.
func TestDistillGateOutput_TruncatesLongLines(t *testing.T) {
	long := "    remote_test.go:24: got " + strings.Repeat("a", 5000)
	got := distillGateOutput(long)
	if len(got) > 600 {
		t.Fatalf("a long kept line must be truncated, got %d bytes", len(got))
	}
	if !strings.Contains(got, "line truncated") {
		t.Fatalf("a truncated line must be marked; got:\n%s", got)
	}
}

// When nothing is recognized (a non-go gate, or an unfamiliar format), distillation falls back
// to the raw output rather than feeding back an empty message.
func TestDistillGateOutput_FallsBackToRawWhenNothingSalient(t *testing.T) {
	raw := "some non-go gate said: command failed with code 2"
	if got := distillGateOutput(raw); got != raw {
		t.Fatalf("unrecognized output must pass through unchanged; got %q", got)
	}
}
