package coding

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"corpos/internal/agent"
)

// Coding-organ gate scoping: the atomic-coding profile's gate is `go build ./... && go
// test ./...` — the whole-module test run, which on a large module (corpos-toolkit) blows
// past codingGateTimeout (10m) and hands the worker a bare "timeout" instead of a RED test
// result, so it can't iterate. This narrows `go test ./...` to the package(s) the worker
// edited (the same scoping the agent auto-verify gate got in bug 1099), keeping `go build
// ./...` whole-module for cross-package compile safety. It works off the git diff the organ
// already has (changedGoFiles), since the organ doesn't see tool dispatches.

// isGitRepo reports whether the orchestrator's repo seam is a real git worktree (*GitRepo),
// the precondition for diff-based gate scoping — NoopRepo runs in a shared dir with no git.
func (o *Orchestrator) isGitRepo() bool {
	_, ok := o.repo.(*GitRepo)
	return ok
}

// osDirExists reports whether p is an existing directory (the production dirExists; tests
// inject a fake).
func osDirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// goTestScopes derives the gateDir-relative `./pkg/...` test scopes for the changed .go
// files (which are relative to the worktree root, dir). gateDir is the module root the gate
// runs in (= dir for a module-at-root repo, = dir/go for a subdir-module repo). It returns
// nil — meaning "do not scope, run the whole module" — when there are no Go edits, or any
// edit cannot be safely mapped to an existing package dir UNDER gateDir (a wrong scope's
// false `no such package` failure is worse than the timeout, so the fallback is conservative,
// mirroring the agent gate's goPackagesFromEdits).
func goTestScopes(changed []string, dir, gateDir string, dirExists func(string) bool) []string {
	if len(changed) == 0 {
		return nil
	}
	prefix, err := filepath.Rel(dir, gateDir)
	if err != nil {
		return nil
	}
	if prefix == "" {
		prefix = "."
	}
	seen := map[string]bool{}
	var scopes []string
	for _, f := range changed {
		// f is worktree-relative (e.g. "go/internal/work/chain.go"); make it module-relative
		// to the gate dir (e.g. "internal/work/chain.go").
		modRel, err := filepath.Rel(prefix, filepath.FromSlash(f))
		if err != nil || modRel == "." || strings.HasPrefix(modRel, "..") {
			return nil // edit lives outside the gate's module → can't scope safely
		}
		pkgDir := filepath.Dir(modRel)
		if pkgDir == "" || pkgDir == "." || filepath.IsAbs(pkgDir) || strings.HasPrefix(pkgDir, "..") {
			return nil // module-root or ambiguous edit → bail
		}
		if dirExists == nil || !dirExists(filepath.Join(gateDir, pkgDir)) {
			return nil // derived package dir doesn't exist → bail rather than risk a false failure
		}
		scope := "./" + filepath.ToSlash(pkgDir) + "/..."
		if !seen[scope] {
			seen[scope] = true
			scopes = append(scopes, scope)
		}
	}
	sort.Strings(scopes)
	return scopes
}

// gateHasWholeRepoGoTest reports whether any gate command contains a whole-module
// `go test ./...`. It gates the (runner-touching) scope computation so a non-go gate — or a
// gate already without `./...` — pays nothing, keeping the default path (and unit tests with
// a counting gate runner) free of an extra git call.
func gateHasWholeRepoGoTest(cmds [][]string) bool {
	for _, c := range cmds {
		for _, tok := range c {
			if strings.Contains(tok, "go test ./...") {
				return true
			}
		}
	}
	return false
}

// scopeOrSkipGate decides how to run a whole-module-`go test ./...` gate on a git coding
// run. It narrows the test to the package(s) the changed files live in (via agent.ScopeGoTest,
// the same rewrite the agent gate uses) and returns the scoped commands with an empty skipDiag.
// When it CANNOT narrow — no changed Go files, or edits that don't map to a package under the
// module root (goTestScopes returns nil) — it returns nil commands and a non-empty skipDiag:
// the organ must NOT run the whole-module suite, which on a large module only blows past the
// gate timeout and hands the worker a bare "timeout" instead of a RED it can act on (bug
// corpos-coding-organ-gate-runs-whole-module-go-test-on-empty-unscopable-diff-times-out). The
// caller feeds skipDiag back as the attempt's gate diagnostic. A gate without a whole-module
// `go test ./...` is returned unchanged with an empty skipDiag (nothing to scope or skip).
func scopeOrSkipGate(cmds [][]string, changed []string, dir, gateDir string, dirExists func(string) bool) (scoped [][]string, skipDiag string) {
	if !gateHasWholeRepoGoTest(cmds) {
		return cmds, ""
	}
	scopes := goTestScopes(changed, dir, gateDir, dirExists)
	if len(scopes) == 0 {
		return nil, noScopeGateDiagnostic(changed, gateDir)
	}
	out := make([][]string, len(cmds))
	for i, c := range cmds {
		out[i] = agent.ScopeGoTest(c, scopes)
	}
	return out, ""
}

// noScopeGateDiagnostic is the actionable RED the organ feeds back instead of running an
// unscopable whole-module `go test ./...`. It distinguishes the worker having landed no Go
// edits at all (the common thrash case — tell it to make the change) from edits that exist
// but can't be mapped to a package under the module root.
func noScopeGateDiagnostic(changed []string, gateDir string) string {
	if len(changed) == 0 {
		return "gate not run: no Go edits detected in the worktree. The build/test gate verifies your CHANGE — an unchanged tree has nothing to test, and the whole-module suite would only time out. Make the code edit with your fs tools (do not just describe it), then the gate re-runs scoped to the package you touched."
	}
	return fmt.Sprintf("gate not run: the edited Go file(s) [%s] could not be mapped to a package directory under the module root %q, so the whole-module `go test ./...` (which times out on a large module) was skipped. Put the fix in a package under the module root.", strings.Join(changed, ", "), gateDir)
}
