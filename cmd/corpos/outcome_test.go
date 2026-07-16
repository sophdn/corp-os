package main

import (
	"strings"
	"testing"

	"corpos/internal/agent"
)

// TestRunOutcomeCode is the bug-1102 regression: a oneshot run that did not deliver a verified,
// usable answer must map to a non-zero exit so a script/principal cannot mistake it for success.
// In particular a graceful model-call timeout with no answer — the orchestrate synthesis-turn
// timeout that exited 0 with an empty, unverified artifact — must be exitIncomplete.
func TestRunOutcomeCode(t *testing.T) {
	cases := []struct {
		name string
		res  agent.Result
		want int
	}{
		{"clean answer", agent.Result{Text: "the work is done"}, 0},
		{"graceful timeout, no answer (bug 1102)", agent.Result{ModelFault: "timeout"}, exitIncomplete},
		{"unverified escalate (self-report ignored)", agent.Result{Text: "worker says it passed", Escalate: "unverified/escalate: gate red"}, exitIncomplete},
		{"verify gate failed", agent.Result{VerifyFailed: true}, exitIncomplete},
		{"fabricated done-claim refused", agent.Result{Fabricated: "no-work: zero substantive mutations"}, exitIncomplete},
		{"breaker halt, no answer", agent.Result{Stopped: "cost ceiling $1.50 reached"}, exitIncomplete},
		{"breaker halt WITH a partial answer", agent.Result{Stopped: "cost ceiling $1.50 reached", Text: "here is what I synthesized so far"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runOutcomeCode(c.res); got != c.want {
				t.Errorf("runOutcomeCode(%+v) = %d, want %d", c.res, got, c.want)
			}
		})
	}
}

// TestPrintResult_SurfacesFailureVerdicts: the honest terminal verdicts must be rendered so a
// non-zero exit is always explained (a run that could not verify never looks like a clean pass).
func TestPrintResult_SurfacesFailureVerdicts(t *testing.T) {
	var out, errOut strings.Builder
	printResult(&out, &errOut, agent.Result{
		Escalate:   "unverified/escalate: the gate is still failing",
		Fabricated: "no-work: a tool call was narrated in prose",
		ModelFault: "timeout",
	})
	e := errOut.String()
	for _, want := range []string{"unverified/escalate", "no-work", "recovered model-call fault (timeout)"} {
		if !strings.Contains(e, want) {
			t.Errorf("printResult should surface %q on stderr; got:\n%s", want, e)
		}
	}
}
