package agent

import (
	"context"
	"sort"
	"strings"

	"corpos/internal/tool"
)

// Required-read guard (bug 1033, rehearsal run-5): the prompt told the worker to "read
// eval_test.go first for the exact API", the fs.read of that path FAILED (it was not in the
// container's mounts), and instead of surfacing "I cannot read the contract" the worker
// INVENTED a contract (a wrong signature) + fabricated its own test, then narrated success.
//
// The loop-owned VerifyGate (bug 1073) and the workaudit/fakegreen footers already close the
// "narrate false green under a RED gate" hazard. This is the OTHER hazard the bug names: when
// a read the DUTY declared as the contract source fails and is never satisfied, the worker
// must NOT be allowed to register a clean done on an invented spec.
//
// The guard is a post-turn footer in the same unforgeable idiom as workaudit/fakegreen: it is
// computed by the loop AFTER generation, from the REAL dispatch record, so the worker cannot
// revise it by narrating success. At a done-claim it checks every path the duty declared
// required: a path that was attempted (an fs.read dispatch occurred for it) but NEVER read
// successfully is an unsatisfied required read — the done-claim is refused with a
// "required read failed — cannot proceed" verdict, so an unsupervised consumer is never handed
// an invented-contract false green. A required path the worker simply never touched is also
// unsatisfied (it cannot have built the contract from a read it never did).

// RequiredReads configures the post-turn required-read guard. The zero value (empty Paths)
// is off — the safe default for a duty with no declared contract source. A coding worker
// whose duty names a file as the API/contract source lists that path here.
type RequiredReads struct {
	// Paths are the file paths the duty declared REQUIRED to read before the work can be
	// trusted (the contract/API source the worker must not invent). A done-claim is refused
	// while any of these was not read successfully.
	Paths []string
}

// fsReadPath extracts the target path of an fs.read call, "" for any other call. It mirrors
// fsCallPath's alias resolution but is read-scoped so a write/edit to a required path does
// not count as satisfying the read.
func fsReadPath(c tool.Call) string {
	if c.Surface != "fs" || c.Action != "read" {
		return ""
	}
	return fsCallPath(c)
}

// assess returns a non-empty verdict naming the required path(s) that were never read
// successfully, or "" when every declared required path was satisfied. It is consulted only
// at a done-claim. A required path is satisfied iff at least one fs.read of it succeeded
// (OK) across the turn's dispatch record; a path that was only attempted-and-failed, or never
// touched at all, is unsatisfied — the worker cannot have legitimately built the contract
// from it, so a "done" is a fabricated-contract false green.
func (r RequiredReads) assess(dispatches []tool.Result) string {
	if len(r.Paths) == 0 {
		return ""
	}
	satisfied := map[string]bool{}
	attempted := map[string]bool{}
	for _, d := range dispatches {
		p := fsReadPath(d.Call)
		if p == "" {
			continue
		}
		if d.OK {
			satisfied[p] = true
		} else {
			attempted[p] = true
		}
	}
	var unread []string
	for _, p := range r.Paths {
		if !satisfied[p] {
			unread = append(unread, p)
		}
	}
	if len(unread) == 0 {
		return ""
	}
	sort.Strings(unread)
	// Distinguish the two shapes in the message for the operator/escalation reader: a path
	// that FAILED to read (the run-5 shape: surface it, don't invent) vs one never attempted.
	failed := false
	for _, p := range unread {
		if attempted[p] {
			failed = true
			break
		}
	}
	lead := "required-read-unsatisfied"
	if failed {
		lead = "required-read-failed"
	}
	return lead + ": the duty declared the contract source(s) " + strings.Join(unread, ", ") +
		" as required, but they were not read successfully — surface this and stop, do NOT invent a contract and proceed (a done-claim built on an unread required source is a fabricated-contract false green)"
}

// RequiredReads is a fabrication-stage Guard: a done-claim is refused while a declared
// required contract source was not read successfully. Assess wraps the pure assess() so the
// verdict is byte-identical to the bespoke wiring.
func (r RequiredReads) Name() string      { return "required-read" }
func (r RequiredReads) Stage() GuardStage { return StageFabrication }
func (r RequiredReads) Describe() string {
	return "refuses a done-claim while a path the duty declared a REQUIRED contract source was not read successfully (the worker must not invent the contract)"
}
func (r RequiredReads) Assess(_ context.Context, in GuardInput) GuardVerdict {
	if v := r.assess(in.Dispatches); v != "" {
		return fail(v)
	}
	return pass()
}
