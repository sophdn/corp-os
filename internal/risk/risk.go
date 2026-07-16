// Package risk is Corp-OS' risk-gating layer — the axis ORTHOGONAL to capability
// scope (§4.4.4, §7 gap #1). Scope (mcp.Project) decides WHICH tools a worker
// has; risk-gating decides WHETHER a specific destructive/mutating/outward
// invocation should proceed — the judgment Claude Code's auto-mode classifier
// provided and which cannibalizing it away would otherwise drop, leaving cheap
// autonomous workers free to rm / edit / push within their scope unchecked.
//
// It must land BEFORE sub-orchestration wires autonomous spawning.
//
// Posture (2026-06-03; fs.remove added 2026-06-06): only genuinely high-blast,
// hard-to-reverse actions are gated by default — arbitrary command execution
// (sys.exec), ledger record deletion (work.forge_delete), and filesystem
// DELETION (fs.remove, the owned move/remove primitive's destructive half).
// Filesystem MUTATION (fs.write/edit/move) is CLASSIFIED but NOT gated by
// default: we do not pre-assume a low-tier worker's writes/relocations are
// dangerous — prove the failure rather than bar it ahead of time. Deletion is
// the exception: it is hard to reverse, so it joins the gated set. The spike
// (§4.1.1) showed scope-of-mutation is real (a worker rewrote its own gate test),
// but that is a PROTECTED-PATH concern (don't let a worker edit the gate it runs),
// not a reason to block all writes; the protected-path policy is the future
// refinement that will gate specific paths. When a Gate IS consulted it must be
// orchestrator-owned + immutable — structurally, the Gate is constructed by the
// composition root and is never exposed to the worker as a tool, so a worker
// cannot disable its own gate.
//
// The gate is the generalization of the atomic-coding gate: "a risky action
// proceeds only if an external, orchestrator-owned check approves it." Confirm
// (a human) and the future automated repo-gate are both Gate implementations.
package risk

import (
	"fmt"
	"strings"

	"corpos/internal/hooks"
	"corpos/internal/shellsafe"
	"corpos/internal/tool"
)

// Class is the risk category of a tool call.
type Class string

const (
	// ClassSafe is read-only or reversible owned-substrate work — never gated.
	ClassSafe Class = "safe"
	// ClassMutating mutates files on disk — the scope-of-mutation axis (§4.1.1).
	// Classified but NOT gated by default (prove-the-failure); a future
	// protected-path policy gates specific paths (e.g. a worker's own gate).
	ClassMutating Class = "mutating"
	// ClassDestructive runs arbitrary commands or removes records — high blast radius.
	ClassDestructive Class = "destructive"
	// ClassOutward sends data to an external service (web/push). No such surface
	// exists yet; reserved so the classifier grows with the web surface.
	ClassOutward Class = "outward"
)

// Verdict is the risk classification of one call. Gated is true when the action
// requires gate approval before it may proceed.
type Verdict struct {
	Class  Class
	Gated  bool
	Reason string
}

// Classify is the pure risk classifier over a tool call. It GATES only the
// high-blast, hard-to-reverse actions — arbitrary command execution (sys.exec),
// ledger record deletion (work.forge_delete), and filesystem deletion
// (fs.remove). Filesystem mutation (fs.write/edit/move) is classified
// ClassMutating but left ungated (prove-the-failure posture); read-only
// introspection and reversible ledger lifecycle writes pass safe. Protected-path
// granularity on fs writes and the outward class join when those
// policies/surfaces land.
func Classify(c tool.Call) Verdict {
	switch c.Surface {
	case "sys":
		if c.Action == "exec" {
			return Verdict{Class: ClassDestructive, Gated: true, Reason: "sys.exec runs an arbitrary (allowlisted) shell command"}
		}
		return Verdict{Class: ClassSafe} // ps/ports/units/containers are read-only
	case "fs":
		if c.Action == "write" || c.Action == "edit" || c.Action == "move" {
			// Classified as mutation for observability + the future protected-path
			// policy, but NOT gated by default — don't pre-bar low-tier writes.
			// fs.move relocates a path (and refuses to clobber an existing dest),
			// so it sits with write/edit on the prove-the-failure side.
			return Verdict{Class: ClassMutating, Gated: false, Reason: "fs." + c.Action + " mutates a file on disk"}
		}
		if c.Action == "remove" {
			// Deletion is hard-to-reverse, high blast radius — gate it like the
			// other destructive owned action (work.forge_delete). The owned
			// fs.remove refuses non-empty dirs without recursive and protected
			// roots in-handler, but the risk gate is the orchestrator-owned
			// approval layer on top.
			return Verdict{Class: ClassDestructive, Gated: true, Reason: "fs.remove deletes a file or directory from disk"}
		}
		return Verdict{Class: ClassSafe} // read/ls/glob/grep
	case "work":
		if c.Action == "forge_delete" {
			return Verdict{Class: ClassDestructive, Gated: true, Reason: "work.forge_delete removes a ledger record"}
		}
		return Verdict{Class: ClassSafe} // lifecycle writes are reversible owned-substrate ops
	default:
		return Verdict{Class: ClassSafe}
	}
}

// Gate decides whether a gated call proceeds. It is only consulted for Gated
// verdicts. ok=false blocks the call and feeds reason back to the model. A Gate is
// constructed by the orchestrator/composition root and never exposed to the worker
// as a tool — that is what makes it orchestrator-owned and tamper-proof.
type Gate interface {
	Approve(c tool.Call, v Verdict) (ok bool, reason string)
}

// AllowAll is the explicit opt-out gate: every gated call proceeds. Use only for
// trusted, supervised runs (-risk-gate=off).
type AllowAll struct{}

// Approve always permits.
func (AllowAll) Approve(tool.Call, Verdict) (bool, string) { return true, "" }

// denyGated is the fail-closed default: every gated call is blocked with a clear,
// actionable message. The safe default for an autonomous worker until a human
// confirm channel or an automated gate (the atomic-coding gate) is wired.
type denyGated struct{}

// DenyGated returns the fail-closed default gate.
func DenyGated() Gate { return denyGated{} }

func (denyGated) Approve(c tool.Call, v Verdict) (bool, string) {
	return false, fmt.Sprintf("risk gate blocked %s.%s (%s: %s) — no approver is wired; re-run with -risk-gate=off to permit, or attach an automated gate",
		c.Surface, c.Action, v.Class, v.Reason)
}

// safeExecHeads is the command set the build/test gate auto-approves for sys.exec:
// the coding worker's self-verification (go build/test/vet, gofmt) plus read-only
// inspection. It deliberately EXCLUDES anything that mutates state outside the
// working tree or runs arbitrary code — rm/mv, curl, docker/podman, systemctl,
// make, npm/node/python — so auto-approval can't become a blanket shell. `git` is
// NOT listed here: it is gated separately to a safe recovery/inspection subcommand
// set (see safeGitSubcommands). Extend deliberately, not reflexively.
var safeExecHeads = map[string]bool{
	"go": true, "gofmt": true,
	"cat": true, "ls": true, "head": true, "tail": true, "wc": true,
	"grep": true, "rg": true, "find": true, "pwd": true, "echo": true,
	"test": true, "true": true, "false": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "diff": true,
	"stat": true, "file": true,
}

// safeGitSubcommands is the git subcommand set the build/test gate auto-approves: a
// coding worker's RECOVERY (restore/checkout/stash a botched edit back to a clean
// state — the run-9 gap, where the worker corrupted a file with a bad whole-file
// write and every `git` call was blocked, so it could not revert and burned its
// rounds) plus read-only INSPECTION. It deliberately EXCLUDES history, remote, and
// destructive-clean ops (commit, push, pull, fetch, reset, rebase, merge, clean,
// branch, tag, remote) so auto-approval can't rewrite history, reach the network, or
// delete untracked files (e.g. a principal-owned acceptance test). Extend deliberately.
var safeGitSubcommands = map[string]bool{
	"restore": true, "checkout": true, "stash": true,
	"diff": true, "status": true, "show": true, "log": true,
	"ls-files": true, "rev-parse": true,
}

// buildTestGate is the automated, orchestrator-owned "atomic-coding gate" risk.go's
// posture anticipates: it APPROVES sys.exec of build/test/inspection commands so a
// coding worker can run its own `go test`/`go build` to self-verify (the run-6d gap:
// DenyGated blocked every sys.exec, so the worker could never test its fix), and it
// DENIES everything else gated — unsafe sys.exec commands, filesystem deletion
// (fs.remove), and ledger-record deletion (work.forge_delete) — with an actionable
// reason. It is constructed by the composition root and never exposed to the worker,
// so it stays tamper-proof.
type buildTestGate struct{}

// BuildTestGate returns the build/test auto-approval gate (see buildTestGate).
func BuildTestGate() Gate { return buildTestGate{} }

func (buildTestGate) Approve(c tool.Call, v Verdict) (bool, string) {
	if c.Surface == "sys" && c.Action == "exec" {
		cmd := strings.TrimSpace(execCommandOf(c))
		if cmd == "" {
			return false, `sys.exec: missing "command" string param — pass {"command": "go test ./..."} (the argv is one shell string under "command", not a list)`
		}
		if reason := unsafeExecReason(cmd); reason != "" {
			return false, reason
		}
		return true, ""
	}
	return false, fmt.Sprintf("build-test gate denies %s.%s (%s: %s) — only build/test/inspection sys.exec is auto-approved; a destructive action needs an explicit approver (or -risk-gate=off for a trusted run)",
		c.Surface, c.Action, v.Class, v.Reason)
}

// execCommandOf extracts the sys.exec command string from a call's params, salvaging a
// list-shaped argv (["go","test","./..."]) into one shell string (bug 1113) so the gate
// approves a weak model's list-malformed build/test call instead of blocking it (each block
// wasted a worker round and tipped the large-repo coding worker into retry-exhaustion).
func execCommandOf(c tool.Call) string {
	if c.Params == nil {
		return ""
	}
	return tool.CommandString(c.Params["command"])
}

// unsafeExecReason returns "" when every ;|&-separated segment of cmd is auto-
// approvable — its head is in safeExecHeads, or it is a `git` command whose
// subcommand is in safeGitSubcommands — and the command carries no head-escaping
// shell construct; otherwise it returns an actionable reason. The shell-shape deny
// rule is shared with the sys.exec allowlist via internal/shellsafe so the two cannot
// drift (bug 1107); this gate keeps its OWN head-extraction and applies the TIGHTER
// build/test head set rather than the full exec allowlist.
func unsafeExecReason(cmd string) string {
	if r := shellsafe.RejectReason(cmd); r != "" {
		return "sys.exec: " + r + " is not auto-approved by the build-test gate"
	}
	sawHead := false
	for _, seg := range execSegments(cmd) {
		head := segHead(seg)
		if head == "" {
			continue // an env-only / empty segment contributes no command
		}
		sawHead = true
		if head == "git" {
			if reason := unsafeGitReason(seg); reason != "" {
				return reason
			}
			continue
		}
		if !safeExecHeads[head] {
			return fmt.Sprintf("sys.exec: command %q is not auto-approved by the build-test gate (it auto-approves build/test/inspection only: go, gofmt, read-only tools, and safe git recovery/inspection). Run it with an explicit approver or -risk-gate=off if it is genuinely needed.", head)
		}
	}
	if !sawHead {
		return "sys.exec: no command found"
	}
	return ""
}

// unsafeGitReason vets a single `git ...` segment: "" when its subcommand is in
// safeGitSubcommands (a recovery or read-only inspection op), else an actionable
// reason. A bare `git` with no subcommand is refused (it does nothing useful and
// must not be a wildcard).
func unsafeGitReason(seg string) string {
	sub := gitSubcommand(seg)
	if sub == "" {
		return "sys.exec: bare `git` is not auto-approved by the build-test gate; only recovery/inspection subcommands are (restore, checkout, stash, diff, status, show, log, ls-files, rev-parse)."
	}
	if !safeGitSubcommands[sub] {
		return fmt.Sprintf("sys.exec: `git %s` is not auto-approved by the build-test gate (it auto-approves only git recovery/inspection: restore, checkout, stash, diff, status, show, log, ls-files, rev-parse). Run it with an explicit approver or -risk-gate=off if it is genuinely needed.", sub)
	}
	return ""
}

// execSegments splits a command on the ;|& shell separators into per-command
// segments — the same shape rule the sysorgan allowlist uses, kept local so package
// risk stays decoupled from sysorgan.
func execSegments(command string) []string {
	return strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '|' || r == '&'
	})
}

// segHead returns the basename of a segment's first real token (skipping leading
// VAR=value assignments), or "" for an env-only / empty segment.
func segHead(seg string) string {
	for _, tok := range strings.Fields(seg) {
		if isEnvAssignment(tok) {
			continue
		}
		return baseName(tok)
	}
	return ""
}

// gitSubcommand returns the subcommand of a `git ...` segment — the first token
// after `git` that is not a global option — or "" if seg is not a git command or
// names no subcommand. It skips git's argument-taking global options (-C <dir>,
// -c <k=v>) so `git -C path restore f` resolves to "restore".
func gitSubcommand(seg string) string {
	toks := strings.Fields(seg)
	i := 0
	for i < len(toks) && isEnvAssignment(toks[i]) {
		i++
	}
	if i >= len(toks) || baseName(toks[i]) != "git" {
		return ""
	}
	i++
	for i < len(toks) {
		t := toks[i]
		if !strings.HasPrefix(t, "-") {
			return t
		}
		if t == "-C" || t == "-c" {
			i += 2 // a global option that consumes the following token as its argument
			continue
		}
		i++
	}
	return ""
}

// baseName returns the final path segment of tok (its executable basename).
func baseName(tok string) string {
	if i := strings.LastIndexAny(tok, "/\\"); i >= 0 {
		return tok[i+1:]
	}
	return tok
}

// isEnvAssignment reports whether tok is a leading VAR=value assignment.
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for _, r := range tok[:eq] {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// ConfirmFunc adapts a yes/no approver (e.g. a human prompt, or an automated
// orchestrator-owned check like the atomic-coding repo gate) into a Gate. The
// callback returns true to permit the gated call.
type ConfirmFunc func(c tool.Call, v Verdict) bool

// Approve consults the callback; a false answer blocks with an explanatory reason.
func (f ConfirmFunc) Approve(c tool.Call, v Verdict) (bool, string) {
	if f(c, v) {
		return true, ""
	}
	return false, fmt.Sprintf("risk gate denied %s.%s (%s: %s)", c.Surface, c.Action, v.Class, v.Reason)
}

// Guard returns a pre_tool_use hook that classifies the pending call and, when it
// is gated, consults the gate — vetoing the call (via the hook context) if the
// gate does not approve. A nil ToolCall or a non-gated verdict is a no-op, so
// registering Guard unconditionally is safe and applies regardless of profile.
func Guard(g Gate) hooks.Func {
	return func(c *hooks.Context) {
		if c.ToolCall == nil {
			return
		}
		v := Classify(*c.ToolCall)
		if !v.Gated {
			return
		}
		if ok, reason := g.Approve(*c.ToolCall, v); !ok {
			c.DenyToolCall = true
			c.DenyReason = reason
		}
	}
}
