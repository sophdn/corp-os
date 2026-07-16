// Package shellsafe is the single home for corpos's gated-exec shell-shape deny rule: the
// check that rejects shell constructs which run a command past an allowlisted head, or read/
// write a file outside it. Both sys.exec's allowlist (internal/sysorgan) and the build-test
// risk gate (internal/risk) call RejectReason, so the security semantics cannot drift between
// them; each keeps its OWN head-extraction parser (that duplication is owned by chain
// corpos-pre-phase-e-honing T1 and folded there, not here). Corpus is the shared decision
// table both packages' tests assert against — the anti-divergence guard (bug 1107).
package shellsafe

import "strings"

// RejectReason returns a short, caller-framable reason when command contains a shell construct
// the gated-exec surface must refuse — command/process substitution (which RUN a command past
// the head check) or file redirection (which reads/writes a path the head check never saw) —
// or "" when the command is shaped safely. Conservative by design (reject-on-doubt): the gated
// surface runs plain allowlisted commands, not shell scripting, so a redirection/expansion even
// inside quotes is refused rather than parsed. Callers add their own framing (e.g. "… is not
// permitted in gated exec" / "… is not auto-approved by the build-test gate").
func RejectReason(command string) string {
	switch {
	case strings.Contains(command, "$("), strings.Contains(command, "`"):
		return "command substitution ($(...) or backticks)"
	case strings.Contains(command, "<("), strings.Contains(command, ">("):
		return "process substitution (<(...) or >(...))"
	case strings.Contains(command, "${"):
		return "parameter expansion (${...})"
	case hasFileRedirection(command):
		return "file redirection (>, >>, <) escaping the allowlisted command"
	}
	return ""
}

// hasFileRedirection reports whether command redirects to/from a FILE (a sandbox escape —
// `go env > /etc/x`), distinguishing it from benign file-descriptor duplication (`2>&1`,
// `>&2`, `<&0`), where the redirection operator is followed by '&'. A `>>`/`<<` operator is
// collapsed to a single position; whitespace between the operator and its target is skipped.
func hasFileRedirection(command string) bool {
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c != '<' && c != '>' {
			continue
		}
		end := i
		for end+1 < len(command) && command[end+1] == c { // collapse >> / <<
			end++
		}
		next := end + 1
		for next < len(command) && command[next] == ' ' { // skip spaces to the target
			next++
		}
		if next < len(command) && command[next] == '&' {
			i = end // fd-duplication (e.g. 2>&1) — benign, keep scanning
			continue
		}
		return true
	}
	return false
}

// Case is one shared shell-shape decision: a command and whether the gated-exec deny rule must
// reject it on SHAPE grounds (substitution / redirection) — independent of whether its head is
// allowlisted. The benign cases use allowlisted heads so only the shape is under test.
type Case struct {
	Command string
	Reject  bool
	Note    string
}

// Corpus is the canonical shell-shape decision table, scoped to commands where the shape verdict
// is the DECIDING factor at the gate: the benign cases use allowlisted heads and no redirection,
// the dangerous cases each carry exactly one head-escaping construct. RejectReason must agree
// with it, and so must each caller's full gate (sysorgan permit / risk unsafeExecReason) — the
// tests in all three packages assert against this one list so the security rule can't diverge.
//
// fd-duplication (`2>&1`, `>&2`) is deliberately NOT in this list: the shape rule correctly
// treats it as benign (see TestHasFileRedirection_FdDupVsFile), but the packages' head-extraction
// parser (separately, chain corpos-pre-phase-e-honing T1) splits on `&` and over-rejects the
// `&1` fragment as a bogus head — a benign over-rejection, orthogonal to this security fix.
var Corpus = []Case{
	{"go test ./...", false, "plain allowlisted command"},
	{"go build ./... && go vet ./...", false, "compound, all allowlisted, no shell shape"},
	{"FOO=bar go test ./...", false, "leading env assignment skipped; head allowlisted in both gates"},
	{"git status", false, "plain command"},
	{"go env > /etc/passwd", true, "file redirection escapes the allowlisted head"},
	{"go env >> /tmp/out", true, "append file redirection"},
	{"cat < /etc/shadow", true, "input file redirection can exfiltrate"},
	{"echo hi &> /tmp/x", true, "&> file redirection (both streams)"},
	{"echo $(whoami)", true, "command substitution"},
	{"echo `id`", true, "backtick command substitution"},
	{"diff <(go env) <(cat x)", true, "process substitution runs commands"},
	{"tee >(cat) ", true, "output process substitution runs a command"},
	{"echo ${HOME}", true, "parameter expansion rejected conservatively"},
}
