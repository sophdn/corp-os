package coding

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"corpos/internal/agent"
	"corpos/internal/profile"
)

func TestParseVerifierReport(t *testing.T) {
	gate := [][]string{{"go", "test", "./internal/fs/"}}
	cases := []struct {
		name        string
		text        string
		wantVerdict Verdict
		wantEvid    bool
	}{
		{"pass with go-test block", "VERDICT: PASS\n```\nok  \tcorpos/internal/fs\t0.2s\n```", VerdictPass, true},
		{"pass with gate-cmd block", "VERDICT: PASS\n```\n$ go test ./internal/fs/\nall good\n```", VerdictPass, true},
		{"pass no block", "VERDICT: PASS — I read the code and it looks right.", VerdictPass, false},
		{"pass empty block", "VERDICT: PASS\n```\n\n```", VerdictPass, false},
		{"pass block no evidence", "VERDICT: PASS\n```\nlooks fine\n```", VerdictPass, false},
		{"fail with block", "VERDICT: FAIL\n```\n--- FAIL: TestX\n```", VerdictFail, true},
		{"unknown", "I think it's probably fine?", VerdictUnknown, false},
		{"case insensitive + equals", "verdict = pass\n```\nok  \tpkg\n```", VerdictPass, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, evid := parseVerifierReport(c.text, gate)
			if v != c.wantVerdict || evid != c.wantEvid {
				t.Fatalf("got (%q, %v), want (%q, %v)", v, evid, c.wantVerdict, c.wantEvid)
			}
		})
	}
}

func TestFirstFencedBlock(t *testing.T) {
	if got := firstFencedBlock("no fences here"); got != "" {
		t.Fatalf("no fences → empty, got %q", got)
	}
	if got := firstFencedBlock("```go\nok\tpkg\n```"); got != "ok\tpkg" {
		t.Fatalf("block = %q", got)
	}
	if got := firstFencedBlock("```\nopen but not closed"); got != "" {
		t.Fatalf("unclosed fence → empty, got %q", got)
	}
}

type fakeVerifier struct {
	report string
	err    error
	called bool
}

func (f *fakeVerifier) Verify(context.Context, AtomicTask, string) (string, error) {
	f.called = true
	return f.report, f.err
}

func verifySpec() AtomicTask {
	return AtomicTask{Slug: "fix", Gate: [][]string{{"go", "test", "./..."}}}
}

func TestRunVerifyPhase_NilVerifierSkips(t *testing.T) {
	o := New(WithRunner(okRunner()))
	if st, _ := o.runVerifyPhase(context.Background(), verifySpec(), "/w"); st != WorkerSuccess {
		t.Fatalf("nil verifier should skip to success, got %q", st)
	}
}

func TestRunVerifyPhase_PassWithEvidenceAndGreenSpotCheck(t *testing.T) {
	v := &fakeVerifier{report: "VERDICT: PASS\n```\nok  \tcorpos\t0.1s\n```"}
	o := New(WithVerifier(v), WithRunner(okRunner())) // spot-check exits 0
	if st, diag := o.runVerifyPhase(context.Background(), verifySpec(), "/w"); st != WorkerSuccess {
		t.Fatalf("got %q (%s), want WorkerSuccess", st, diag)
	}
	if !v.called {
		t.Fatal("verifier should have been invoked")
	}
}

func TestRunVerifyPhase_PassWithoutEvidenceRejected(t *testing.T) {
	v := &fakeVerifier{report: "VERDICT: PASS — trust me"}
	o := New(WithVerifier(v), WithRunner(okRunner()))
	st, diag := o.runVerifyPhase(context.Background(), verifySpec(), "/w")
	if st != WorkerVerifierRejected || !strings.Contains(diag, "without a command-block") {
		t.Fatalf("got %q (%s), want VerifierRejected (no evidence)", st, diag)
	}
}

func TestRunVerifyPhase_FailVerdictRejected(t *testing.T) {
	v := &fakeVerifier{report: "VERDICT: FAIL\n```\n--- FAIL: TestX\n```"}
	o := New(WithVerifier(v), WithRunner(okRunner()))
	if st, _ := o.runVerifyPhase(context.Background(), verifySpec(), "/w"); st != WorkerVerifierRejected {
		t.Fatalf("FAIL verdict should be rejected, got %q", st)
	}
}

// The headline case: the verifier reports PASS with plausible evidence, but the
// orchestrator's own deterministic spot-check re-run is RED — the fake green is caught.
func TestRunVerifyPhase_FakeGreenCaughtBySpotCheck(t *testing.T) {
	v := &fakeVerifier{report: "VERDICT: PASS\n```\nok  \tcorpos\t0.1s\n```"}
	red := funcRunner(func(cmd []string, _ string) CommandResult {
		return CommandResult{Command: cmd, ExitCode: 1, Stdout: "--- FAIL"}
	})
	o := New(WithVerifier(v), WithRunner(red))
	st, diag := o.runVerifyPhase(context.Background(), verifySpec(), "/w")
	if st != WorkerVerifierRejected || !strings.Contains(diag, "spot-check FAILED") {
		t.Fatalf("got %q (%s), want spot-check rejection", st, diag)
	}
}

func TestRunVerifyPhase_VerifierError(t *testing.T) {
	v := &fakeVerifier{err: errors.New("spawn failed")}
	o := New(WithVerifier(v), WithRunner(okRunner()))
	if st, _ := o.runVerifyPhase(context.Background(), verifySpec(), "/w"); st != WorkerVerifierError {
		t.Fatalf("verifier infra error should be WorkerVerifierError, got %q", st)
	}
}

// No gate command → the spot-check is skipped but a PASS-with-evidence still succeeds.
func TestRunVerifyPhase_NoGateCommandStillPassesOnEvidence(t *testing.T) {
	v := &fakeVerifier{report: "VERDICT: PASS\n```\nok  \tpkg\n```"}
	o := New(WithVerifier(v), WithRunner(okRunner()))
	spec := AtomicTask{Slug: "fix"} // no Gate
	if st, _ := o.runVerifyPhase(context.Background(), spec, "/w"); st != WorkerSuccess {
		t.Fatalf("got %q, want WorkerSuccess", st)
	}
}

func TestModelVerifier_Verify(t *testing.T) {
	fs := &fakeSpawner{res: agent.Result{Text: "VERDICT: PASS\n```\nok\tpkg\n```"}}
	v := &ModelVerifier{spawner: fs, profile: &profile.JobProfile{Name: "atomic-coding-verifier"}}
	report, err := v.Verify(context.Background(), verifySpec(), "/w")
	if err != nil || !strings.Contains(report, "VERDICT: PASS") {
		t.Fatalf("report=%q err=%v", report, err)
	}
	if !strings.Contains(fs.gotDuty, "VERDICT: PASS") || !strings.Contains(fs.gotDuty, "go test ./...") {
		t.Fatalf("verify duty missing instructions/gate: %q", fs.gotDuty)
	}
}

// Integration: an AT whose worker passes its own gate is still FAILED when the independent
// verifier does not confirm it — a fake green caught at the verify phase, end to end.
func TestRunToCompletion_VerifierCatchesFakeGreen(t *testing.T) {
	at := AtomicTask{
		Slug:   "fix",
		Gate:   [][]string{{"true"}},
		Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"true"}},
	}
	v := &fakeVerifier{report: "VERDICT: PASS — looks right"} // PASS without command-block evidence
	o := New(WithRepo(NoopRepo{Dir: t.TempDir()}), WithRunner(okRunner()), WithVerifier(v))
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	final := o.RunToCompletion(context.Background(), st)
	if final.Status != ChainFailed {
		t.Fatalf("chain status = %q, want failed", final.Status)
	}
	if final.ATs[0].WorkerStatus != WorkerVerifierRejected {
		t.Fatalf("AT worker status = %q, want WorkerVerifierRejected", final.ATs[0].WorkerStatus)
	}
	if !v.called {
		t.Fatal("verifier should have run")
	}
}

func TestModelVerifier_SpawnError(t *testing.T) {
	fs := &fakeSpawner{err: errors.New("down")}
	v := &ModelVerifier{spawner: fs, profile: &profile.JobProfile{Name: "v"}}
	if _, err := v.Verify(context.Background(), verifySpec(), "/w"); err == nil {
		t.Fatal("want spawn error")
	}
}

// verdictLabel maps a parsed Verdict to a human label; VerdictUnknown is special-cased.
func TestVerdictLabel(t *testing.T) {
	cases := []struct {
		name    string
		verdict Verdict
		label   string
	}{
		// unknown verdict gets the explanatory "no parseable VERDICT: line" label
		{"unknown", VerdictUnknown, "UNKNOWN (no parseable VERDICT: line)"},
		// a real PASS verdict renders as its own string
		{"pass", VerdictPass, "PASS"},
		// a real FAIL verdict renders as its own string
		{"fail", VerdictFail, "FAIL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdictLabel(c.verdict); got != c.label {
				t.Fatalf("verdictLabel(%q) = %q; want %q", c.verdict, got, c.label)
			}
		})
	}
}

// firstNonEmptyCommand returns the first non-empty command slice in a gate, or nil.
func TestFirstNonEmptyCommand(t *testing.T) {
	cases := []struct {
		name string
		gate [][]string
		want []string
	}{
		// empty gate → no command
		{"empty gate", [][]string{}, nil},
		// only empty command slices → nil
		{"only empty commands", [][]string{{}, {}}, nil},
		// first command is non-empty → returned as-is
		{"first non-empty", [][]string{{"command", "arg1"}, {}}, []string{"command", "arg1"}},
		// leading empties are skipped to the first non-empty command
		{"skip leading empty", [][]string{{}, {"command", "arg1"}}, []string{"command", "arg1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstNonEmptyCommand(c.gate); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("firstNonEmptyCommand(%v) = %v; want %v", c.gate, got, c.want)
			}
		})
	}
}
