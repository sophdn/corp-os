package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"corpos/internal/tool"
)

// Verify-gate test scoping: the coding profiles' gate is `go build ./... && go test ./...`
// — the whole-repo test run, which on a large repo (mcp-servers' DB/FTS suites) took 10
// minutes and timed out (rehearsal finding: decompose-admin-surface). A single-package
// change does not need the entire suite as its gate. The loop scopes `go test ./...` to
// the package(s) the worker actually edited, keeping `go build ./...` whole-repo for
// cross-package compile safety. Scoping is SAFE: it only narrows when every edited .go
// file maps to an existing package dir under the module root; otherwise the gate runs
// whole-repo, exactly as before (a wrong scope would cause a false `no such package`
// failure, which is worse than the timeout — so the fallback is conservative).

// goPackagesFromEdits derives the module-relative `./pkg/...` test scopes for the .go
// files mutated this turn. verifyDir is the gate's working dir (the module root). dirExists
// is injected so the derivation is testable without a real tree. It returns nil — meaning
// "do not scope, run the whole repo" — when there are no Go edits, or any edit cannot be
// safely mapped to an existing package dir under verifyDir.
func goPackagesFromEdits(dispatches []tool.Result, verifyDir string, dirExists func(string) bool) []string {
	base := filepath.Base(verifyDir)
	seen := map[string]bool{}
	var pkgs []string
	sawGoEdit := false
	for _, d := range dispatches {
		if !isMutatingWrite(d) {
			continue
		}
		p := ToolCallPath(d.Call)
		if p == "" || !strings.HasSuffix(p, ".go") {
			continue
		}
		sawGoEdit = true
		// The worker's path may be module-relative ("internal/admin/x.go") or repo-relative
		// in a subdir-module layout ("go/internal/admin/x.go" with verifyDir = "<repo>/go").
		// Strip the verify-dir's basename prefix when present so both conventions land
		// module-relative.
		rel := p
		if base != "" && base != "." {
			rel = strings.TrimPrefix(rel, base+"/")
		}
		dir := filepath.Dir(rel)
		if dir == "" || dir == "." || filepath.IsAbs(dir) || strings.HasPrefix(dir, "..") {
			return nil // root / ambiguous edit → can't narrow safely
		}
		if dirExists == nil || !dirExists(filepath.Join(verifyDir, filepath.FromSlash(dir))) {
			return nil // derived package dir doesn't exist → bail rather than risk a false failure
		}
		scope := "./" + filepath.ToSlash(dir) + "/..."
		if !seen[scope] {
			seen[scope] = true
			pkgs = append(pkgs, scope)
		}
	}
	if !sawGoEdit {
		return nil
	}
	sort.Strings(pkgs)
	return pkgs
}

// realDirExists reports whether p is an existing directory (the production dirExists).
func realDirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ScopeGoTest rewrites a whole-repo `go test ./...` in a go build/test gate to test only
// the given package scopes (keeping `go build ./...` whole-repo). It handles the
// `["sh","-c","..."]` shell form and a bare `["go","test","./..."]` argv. An empty scope
// list, or an unrecognized command shape, returns the command unchanged. Exported so the
// coding organ's gate (internal/coding) reuses the same rewrite as the agent gate.
func ScopeGoTest(cmd []string, pkgs []string) []string {
	if len(pkgs) == 0 {
		return cmd
	}
	scope := strings.Join(pkgs, " ")
	if len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c" {
		if !strings.Contains(cmd[2], "go test ./...") {
			return cmd
		}
		return []string{"sh", "-c", strings.Replace(cmd[2], "go test ./...", "go test "+scope, 1)}
	}
	if len(cmd) >= 3 && cmd[0] == "go" && cmd[1] == "test" {
		for i := 2; i < len(cmd); i++ {
			if cmd[i] == "./..." {
				out := make([]string, 0, len(cmd)+len(pkgs))
				out = append(out, cmd[:i]...)
				out = append(out, pkgs...)
				out = append(out, cmd[i+1:]...)
				return out
			}
		}
	}
	return cmd
}
