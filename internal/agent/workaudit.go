package agent

import (
	"context"
	"regexp"

	"corpos/internal/tool"
)

// Fabrication / no-work audit (run-6c: at the Qwen floor the worker emitted JSON tool
// calls AS PROSE, invented a fictional task.go, ran no real tool, spent $0, made 0 real
// fs.write events, then declared "the regression test passes" — and nothing caught it).
//
// This is a post-turn audit the loop runs when the agent claims done (a turn with no tool
// calls). It is the agent-loop counterpart to hermes's structurally-unforgeable post-turn
// file-mutation footer: it cross-checks the done-claim against the REAL dispatch record —
// the worker cannot revise it because it is computed by the loop AFTER generation, from the
// dispatch results, not from anything the model says. Three signals:
//
//   - no-work: a done-claim on a task whose job is to mutate files (RequireMutation) where
//     ZERO substantive mutation (fs.write/edit/move/remove) dispatches occurred.
//   - prose-tool-call: the assistant narrated a tool-call envelope in its text instead of
//     emitting a real tool call (the run-6c pattern).
//   - verification-fabrication: a mutation-expecting task that DID mutate code and CLAIMS the
//     change is verified (tests/build pass) but ran NO build/test/format gate (a successful
//     sys.exec of go test/build/vet or gofmt) in the run. This is the swap-rehearsal Run-20
//     false green: a bare (ungated) coding worker pasted "go test … PASSED" output it never
//     produced. It is the loop-side backstop for an ungated code edit (bug
//     corpos-orchestrate-spawn-coding-done-claim-not-gated-…); it never trips the orchestrator
//     (which only spawns, mutating nothing) or a read-only duty (which mutates nothing).
//
// A non-empty verdict is surfaced on Result.Fabricated and the done-claim does NOT register
// as a clean success.

// WorkAudit configures the post-turn fabrication/no-work audit. The zero value (off) is the
// safe default for read-only duties — a summary or a code review legitimately mutates
// nothing. The coding worker, whose job is to change files, enables RequireMutation.
type WorkAudit struct {
	// RequireMutation rejects a done-claim that made zero substantive mutation dispatches
	// (the no-work/fabrication signal for a mutation-expecting task).
	RequireMutation bool
}

// proseToolCallRe matches an assistant text that contains a JSON tool-call envelope —
// an object carrying an "action" key alongside a "surface"/"params"/"tool"/"arguments"
// key. That is the corpos/MCP call shape (surface+action+params) the model is supposed to
// emit as a structured tool call, not narrate in prose. Whitespace-tolerant; the keys may
// appear in any order, so it is checked as two independent presence probes below.
var (
	jsonActionKeyRe  = regexp.MustCompile(`"action"\s*:`)
	jsonEnvelopeKeRe = regexp.MustCompile(`"(surface|params|tool|arguments|tool_call)"\s*:`)
	jsonOpenBraceRe  = regexp.MustCompile(`\{`)
)

// verificationClaimRe matches a done-claim that ASSERTS the change was verified — the
// tests/build pass — rather than merely describing the edit. It also matches pasted
// go-test output markers (--- PASS, a bare PASS line, `ok corpos/…`), the exact shape
// the Run-20 worker fabricated. It is consulted only when the worker actually mutated
// code AND no real gate ran, so a match there is a fabricated verification, not a
// legitimate report (a genuinely-gated run trips ranGate and is exempt).
var verificationClaimRe = regexp.MustCompile(`(?i)(tests?\s+(all\s+)?(pass|passed|passing|are\s+green)|all\s+tests?\s+pass|--- pass|\bPASS\b|\bok\s+corpos/|build\s+(succeeded|successful|passes|is\s+clean|completed\s+success)|verif(y|ied|ication)\s+(step|complete|succe|pass)|passed\s+success)`)

// gateExecRe matches a sys.exec command that runs the build/test/format gate (go
// test/build/vet, gofmt) — the unforgeable evidence that a code change was actually
// verified rather than merely claimed.
var gateExecRe = regexp.MustCompile(`(?i)\b(go\s+(test|build|vet)|gofmt)\b`)

// ranGate reports whether the run actually executed a build/test/format gate via a
// SUCCESSFUL sys.exec — the evidence behind a "verified" done-claim.
func ranGate(dispatches []tool.Result) bool {
	for _, d := range dispatches {
		if !d.OK || d.Call.Surface != "sys" || d.Call.Action != "exec" {
			continue
		}
		if cmd, ok := d.Call.Params["command"].(string); ok && gateExecRe.MatchString(cmd) {
			return true
		}
	}
	return false
}

// claimsVerification reports whether finalText asserts the change passed a gate.
func claimsVerification(finalText string) bool {
	return finalText != "" && verificationClaimRe.MatchString(finalText)
}

// looksLikeProseToolCall reports whether finalText narrates a tool-call envelope as prose
// (an object with an "action" key plus a surface/params/tool/arguments key). It is a pure
// heuristic; it is only consulted at a done-claim (a turn with NO real tool calls), so a
// match there means the model described a call instead of making one.
func looksLikeProseToolCall(finalText string) bool {
	if finalText == "" {
		return false
	}
	return jsonOpenBraceRe.MatchString(finalText) &&
		jsonActionKeyRe.MatchString(finalText) &&
		jsonEnvelopeKeRe.MatchString(finalText)
}

// countMutations counts the substantive mutation dispatches (fs.write/edit/move/remove that
// succeeded) across a turn's dispatch record.
func countMutations(dispatches []tool.Result) int {
	n := 0
	for _, d := range dispatches {
		if isMutatingWrite(d) {
			n++
		}
	}
	return n
}

// assess returns a non-empty fabrication/no-work verdict when the done-claim is not backed
// by real work, or "" when the claim is sound. The prose-tool-call signal fires regardless
// of RequireMutation (narrating a call is always fabrication); the no-work signal fires only
// for a mutation-expecting task.
func (a WorkAudit) assess(finalText string, dispatches []tool.Result, loopGated bool) string {
	if looksLikeProseToolCall(finalText) {
		return "fabrication: the assistant narrated a tool-call envelope in prose instead of emitting a real tool call (no actual dispatch)"
	}
	if a.RequireMutation && countMutations(dispatches) == 0 {
		return "no-work: done was claimed but zero substantive mutation (fs.write/edit/move/remove) dispatches occurred"
	}
	// verification-fabrication is the loop-side backstop for an UNGATED code edit (Run-20: a
	// bare worker pasted "go test … PASSED" output it never produced). When the LOOP owns an
	// authoritative verify gate it runs that gate ITSELF immediately after this audit
	// (onDeclaredDone), so an optimistic "the tests should pass" claim is verified for real
	// there — the text-based backstop is then redundant AND harmful: it pre-empts the gate and
	// TRAPS a gated coding worker that (per its own atomic-coding-chain prompt) is told NOT to
	// run the gate and frequently cannot run one in its worktree, so a CORRECT landed fix
	// thrashes to a Fabricated terminal and never merges (self-verify cwd trap). Suppress it
	// when gated; the gate itself (T7: self-report never overrides the gate) plus the post-gate
	// fake-green guard remain the real anti-fake-green defense.
	if !loopGated && a.RequireMutation && countMutations(dispatches) > 0 && claimsVerification(finalText) && !ranGate(dispatches) {
		return "verification-fabrication: done claims the change is verified (tests/build pass) but no build/test/format gate (go test/build/vet or gofmt) was run — the claim is not backed by a real gate"
	}
	return ""
}

// WorkAudit is a fabrication-stage Guard: at a done-claim it refuses a claim not backed by
// real work. Name/Describe/Stage implement the Guard interface; Assess wraps the existing
// pure assess() so the verdict is byte-identical to the bespoke wiring.
func (a WorkAudit) Name() string      { return "work-audit" }
func (a WorkAudit) Stage() GuardStage { return StageFabrication }
func (a WorkAudit) Describe() string {
	return "refuses a done-claim not backed by real work (no-work on a mutation-expecting task, or a tool-call envelope narrated in prose)"
}
func (a WorkAudit) Assess(_ context.Context, in GuardInput) GuardVerdict {
	if r := a.assess(in.FinalText, in.Dispatches, in.LoopGated); r != "" {
		return fail(r)
	}
	return pass()
}
