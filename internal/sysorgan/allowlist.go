package sysorgan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"corpos/internal/shellsafe"
)

// defaultExecAllowlist is the built-in set of command heads sys.exec permits. It
// deliberately EXCLUDES shells and eval-style binaries (sh/bash/zsh/env/eval/exec)
// — allowing them would let a model run anything past the head check. Extend
// per-deployment via CORPOS_EXEC_ALLOWLIST (comma-separated), never per-call.
var defaultExecAllowlist = []string{
	"git", "go", "gofmt", "make", "cargo", "npm", "node", "python3", "python",
	"ls", "cat", "head", "tail", "wc", "grep", "rg", "find", "echo", "pwd",
	"test", "true", "false", "sort", "uniq", "sed", "awk", "cut", "tr",
	"cd", "mkdir", "stat", "file", "du", "df",
	"podman", "docker", "systemctl", "curl",
}

// commandAllowlist gates which command heads sys.exec will run.
type commandAllowlist struct {
	allowed map[string]bool
}

// loadAllowlist builds the allowlist from the built-in default plus a
// comma-separated extension string (e.g. the CORPOS_EXEC_ALLOWLIST env var).
func loadAllowlist(extra string) commandAllowlist {
	set := make(map[string]bool, len(defaultExecAllowlist))
	for _, c := range defaultExecAllowlist {
		set[c] = true
	}
	for _, c := range strings.Split(extra, ",") {
		if c = strings.TrimSpace(c); c != "" {
			set[c] = true
		}
	}
	return commandAllowlist{allowed: set}
}

// loadAllowlistFromEnv builds the allowlist with the CORPOS_EXEC_ALLOWLIST
// extension applied.
func loadAllowlistFromEnv() commandAllowlist {
	return loadAllowlist(os.Getenv("CORPOS_EXEC_ALLOWLIST"))
}

// permit reports nil when every command head in the (possibly compound) command is allowlisted
// and the command carries no shell construct that escapes the head check — command substitution
// ($(...) / backticks), process substitution (<(...) / >(...)), parameter expansion (${...}),
// or file redirection (>, >>, < to a file). Otherwise it returns an error explaining the
// rejection. The shell-shape rule lives in internal/shellsafe so sys.exec and the build-test
// risk gate cannot drift on it (bug 1107).
func (a commandAllowlist) permit(command string) error {
	if strings.TrimSpace(command) == "" {
		return errors.New("empty command")
	}
	// A shell construct can smuggle a disallowed command past the head check or write/read a
	// file outside the allowlisted command — reject before extracting heads.
	if r := shellsafe.RejectReason(command); r != "" {
		return errors.New(r + " is not permitted in gated exec")
	}
	heads := commandHeads(command)
	if len(heads) == 0 {
		return errors.New("no command found")
	}
	for _, h := range heads {
		if !a.allowed[h] {
			return fmt.Errorf("command %q is not in the exec allowlist (extend via CORPOS_EXEC_ALLOWLIST)", h)
		}
	}
	return nil
}

// commandHeads extracts the head (executable basename) of every segment of a
// shell command, splitting on the operators ; | &. Leading VAR=value
// assignments are skipped; the head is the basename of the first real token.
func commandHeads(command string) []string {
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '|' || r == '&'
	})
	var heads []string
	for _, seg := range segments {
		for _, tok := range strings.Fields(seg) {
			if isEnvAssignment(tok) {
				continue // skip FOO=bar prefixes
			}
			heads = append(heads, filepath.Base(tok))
			break
		}
	}
	return heads
}

// isEnvAssignment reports whether tok looks like a leading VAR=value assignment.
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
