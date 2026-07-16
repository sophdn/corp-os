package coding

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"corpos/internal/agent"
	"corpos/internal/profile"
)

// Independent read-only verifier (chain 366 T5): corpos had no independent verifier — the
// same worker that wrote the fix also narrated its success. Ported from claude-code's
// verificationAgent: after the coding worker claims done, a SEPARATE corpos sub-run on the
// atomic-coding-verifier profile (mutating surfaces STRIPPED — it physically cannot edit
// code or tests) re-runs the principal-owned gate and must emit `VERDICT: PASS|FAIL` plus a
// fenced command-block of the REAL captured output. The orchestrator owns the verdict, not
// the verifier: a PASS without a command-block is rejected ("a check without a run block is
// not a PASS — it's a skip"), and the orchestrator SPOT-CHECKS by re-running >=1 gate command
// itself ("the implementer is an LLM — verify independently").

// Verifier runs the independent verification pass over a completed AT and returns the
// verifier agent's raw report (its VERDICT + command-block) plus any infra error. It does
// NOT decide the authoritative verdict — runVerifyPhase does, by requiring evidence and
// spot-checking. A nil Verifier on the orchestrator skips the phase (back-compat / tests).
type Verifier interface {
	Verify(ctx context.Context, spec AtomicTask, dir string) (report string, err error)
}

// Verdict is the parsed verifier verdict.
type Verdict string

const (
	VerdictPass    Verdict = "PASS"
	VerdictFail    Verdict = "FAIL"
	VerdictUnknown Verdict = ""
)

// verdictRe extracts a `VERDICT: PASS|FAIL` line (case-insensitive, `:` or `=`).
var verdictRe = regexp.MustCompile(`(?i)VERDICT\s*[:=]\s*(PASS|FAIL)`)

// goTestEvidenceTokens are output fragments that mark a fenced block as a REAL command run
// (go test/build output), so a bare ```PASS``` cannot pass as evidence.
var goTestEvidenceTokens = []string{"ok ", "ok\t", "--- FAIL", "FAIL\t", "FAIL ", "exit status", "no test files", "=== RUN", "PASS\nok", "build failed", "cannot find"}

// parseVerifierReport extracts the verdict and whether the report carries a command-block of
// real output. Evidence = a fenced ``` block that mentions a gate command head OR a go
// test/build output token. It is a pure parser (no IO) so it is exhaustively unit-testable.
func parseVerifierReport(text string, gate [][]string) (Verdict, bool) {
	v := VerdictUnknown
	if m := verdictRe.FindStringSubmatch(text); m != nil {
		v = Verdict(strings.ToUpper(m[1]))
	}
	return v, hasCommandEvidence(text, gate)
}

// hasCommandEvidence reports whether text contains a fenced code block that looks like a
// real captured command run (mentions a gate command head or a go-test output token).
func hasCommandEvidence(text string, gate [][]string) bool {
	block := firstFencedBlock(text)
	if block == "" {
		return false
	}
	for _, cmd := range gate {
		if len(cmd) > 0 && cmd[0] != "" && strings.Contains(block, cmd[0]) {
			return true
		}
	}
	for _, tok := range goTestEvidenceTokens {
		if strings.Contains(block, tok) {
			return true
		}
	}
	return false
}

// firstFencedBlock returns the trimmed content between the first pair of ``` fences, or "".
func firstFencedBlock(text string) string {
	i := strings.Index(text, "```")
	if i < 0 {
		return ""
	}
	rest := text[i+3:]
	// Drop an optional language tag on the opening fence line.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	j := strings.Index(rest, "```")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// runVerifyPhase runs the independent read-only verification after the coding worker passed
// the gate. It returns WorkerSuccess when the verifier independently confirms the fix with
// evidence AND the orchestrator's own spot-check of a gate command agrees; otherwise a
// rejecting status + diagnostic. A nil verifier skips the phase (WorkerSuccess).
func (o *Orchestrator) runVerifyPhase(ctx context.Context, spec AtomicTask, dir string) (WorkerStatus, string) {
	if o.verifier == nil {
		return WorkerSuccess, ""
	}
	report, err := o.verifier.Verify(ctx, spec, dir)
	if err != nil {
		return WorkerVerifierError, "independent verifier: " + err.Error()
	}
	verdict, hasEvidence := parseVerifierReport(report, spec.Gate)
	switch {
	case verdict == VerdictPass && !hasEvidence:
		return WorkerVerifierRejected, "verifier reported PASS without a command-block of real output (a check without a run block is not a PASS)"
	case verdict != VerdictPass:
		return WorkerVerifierRejected, "independent verifier verdict: " + verdictLabel(verdict)
	}
	// Orchestrator-owned spot-check: re-run >=1 gate command itself and require it to AGREE
	// with the verifier's PASS. The implementer AND the verifier are LLMs; this deterministic
	// re-run is the backstop.
	if cmd := firstNonEmptyCommand(spec.Gate); len(cmd) > 0 {
		run := o.runner.Run(ctx, cmd, dir, o.gateTimeout)
		if run.ExitCode != 0 {
			return WorkerVerifierRejected, fmt.Sprintf("spot-check FAILED: verifier reported PASS but %q exited %d", strings.Join(cmd, " "), run.ExitCode)
		}
	}
	return WorkerSuccess, ""
}

// verdictLabel renders a verdict for a diagnostic (UNKNOWN when unparseable).
func verdictLabel(v Verdict) string {
	if v == VerdictUnknown {
		return "UNKNOWN (no parseable VERDICT: line)"
	}
	return string(v)
}

// firstNonEmptyCommand returns the first non-empty gate command, or nil.
func firstNonEmptyCommand(gate [][]string) []string {
	for _, cmd := range gate {
		if len(cmd) > 0 {
			return cmd
		}
	}
	return nil
}

// ModelVerifier is the production Verifier: it spawns a scoped sub-agent on the read-only
// verifier profile (mutating surfaces stripped) to re-run the gate and report. Like
// ModelWorker it composes over the corpos spawner; the profile's scope is what makes the
// verifier unable to mutate the worktree.
type ModelVerifier struct {
	spawner spawnRunner
	profile *profile.JobProfile
}

// NewModelVerifier builds a verifier over the spawner and the read-only verifier profile.
func NewModelVerifier(spawner *agent.Spawner, p *profile.JobProfile) *ModelVerifier {
	return &ModelVerifier{spawner: spawner, profile: p}
}

// Verify spawns the verifier sub-agent with a duty to re-run the gate and report a verdict
// with command-block evidence. A spawn error is returned as the infra error.
func (v *ModelVerifier) Verify(ctx context.Context, spec AtomicTask, dir string) (string, error) {
	res, err := v.spawner.Run(ctx, v.profile, buildVerifyDuty(spec, dir))
	if err != nil {
		return "", fmt.Errorf("verifier spawn: %w", err)
	}
	return res.Text, nil
}

// buildVerifyDuty assembles the verifier's duty: re-run the gate in the worktree and report
// a parseable verdict with a fenced command-block of the real output. It never asks the
// verifier to edit anything (its scope forbids it anyway).
func buildVerifyDuty(spec AtomicTask, dir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are an INDEPENDENT verifier. The implementer is an LLM — do NOT trust its report; verify yourself.\n")
	fmt.Fprintf(&b, "Working tree: %q (read-only to you; you cannot and must not edit code or tests).\n\n", dir)
	b.WriteString("RUN the gate command(s) below with sys.exec and read the REAL output. Reading the code is NOT verification.\n")
	for _, cmd := range spec.Gate {
		fmt.Fprintf(&b, "  - %s\n", strings.Join(cmd, " "))
	}
	b.WriteString("\nThen reply with EXACTLY:\n")
	b.WriteString("  1. a line `VERDICT: PASS` or `VERDICT: FAIL`\n")
	b.WriteString("  2. a fenced ``` code block containing the real captured command output (the run block — without it a PASS is not a PASS).\n")
	return b.String()
}
