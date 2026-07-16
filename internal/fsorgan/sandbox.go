package fsorgan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// rootKey is the unexported context key under which a sandbox root is carried.
type rootKey struct{}

// WithRoot returns a context that confines every fs action dispatched under it to
// root: a relative path resolves UNDER root, and any path that escapes root — a
// `..` climb or an absolute path outside root — is rejected at the organ
// boundary. The coding orchestrator sets this to a worker's per-attempt worktree
// so a model that emits a relative path (resolved against the process CWD by the
// raw OS call) cannot write outside its worktree into the host repo (bug 1081).
// An empty/whitespace root is a no-op: the organ resolves against the process CWD
// exactly as before, the correct default for direct CLI use.
func WithRoot(ctx context.Context, root string) context.Context {
	if strings.TrimSpace(root) == "" {
		return ctx
	}
	return context.WithValue(ctx, rootKey{}, root)
}

// rootFromContext returns the sandbox root carried by ctx, or "" when none is set
// (the unsandboxed default).
func rootFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if r, ok := ctx.Value(rootKey{}).(string); ok {
		return r
	}
	return ""
}

// recoverWorktreePath rescues a worker-supplied path that references a file by its
// location in the ORIGINAL target repo rather than by a path relative to the sandbox
// worktree the worker actually operates in. A coding worker's fs is rooted at its
// per-attempt worktree, but the bug report/duty often names the file by its absolute
// path in the source repo (e.g. "/tmp/proj/go/x.go") — or the same with the leading
// slash dropped by the model ("tmp/proj/go/x.go"), which then joins UNDER the worktree
// into a doubled, nonexistent path. Since the worktree MIRRORS the source repo, the
// file lives at some TAIL of that path under the root. This finds the tail: it strips
// leading segments one at a time and, when EXACTLY ONE resulting suffix names an
// existing entry under root, returns that suffix (root-relative). Uniqueness is the
// safety gate — an ambiguous match (two suffixes both exist) returns ("", false) so
// the caller keeps the honest not-found rather than editing the wrong file. It is a
// RECOVERY only: callers invoke it after the worker's own path fails to resolve.
func recoverWorktreePath(root, path string) (string, bool) {
	if strings.TrimSpace(root) == "" {
		return "", false
	}
	slashed := filepath.ToSlash(strings.TrimSpace(path))
	segs := make([]string, 0, 8)
	for _, s := range strings.Split(slashed, "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	// A bare filename (or empty) is too generic to disambiguate — require at least one
	// parent segment so the tail carries locating context.
	if len(segs) < 2 {
		return "", false
	}
	found := ""
	for i := 0; i <= len(segs)-2; i++ {
		suffix := filepath.Join(segs[i:]...)
		if _, err := os.Stat(filepath.Join(root, suffix)); err == nil {
			if found != "" {
				return "", false // ambiguous — two tails exist; refuse to guess
			}
			found = suffix
		}
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// resolveWithin expands a leading ~ and, when a sandbox root is set, resolves path
// under that root — joining a relative path onto the root and requiring an
// absolute path to already live within it — rejecting any result that escapes the
// root (a `..` climb or an absolute-outside path). The returned path is absolute
// whenever a root is set. With no root it returns the tilde-expanded path
// unchanged (resolved against the process CWD by the OS call, the pre-sandbox
// behavior every direct-CLI caller relies on).
func resolveWithin(root, path string) (string, error) {
	path = expandUserPath(path)
	if root == "" {
		return path, nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox root %q: %w", root, err)
	}
	rootAbs = filepath.Clean(rootAbs)
	abs := filepath.Join(rootAbs, path)
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the worktree sandbox %q", path, rootAbs)
	}
	// Lexical containment is not enough: a symlink INSIDE the worktree can point at an
	// external target, and following it would read/write outside the sandbox (bug 1106).
	// Resolve symlinks on the deepest EXISTING ancestor of abs and require the real path
	// to still live under the real (symlink-resolved) root. A not-yet-created leaf has
	// its existing parent checked; a same-worktree symlink whose target is also inside
	// the worktree still passes.
	realRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		// The root must exist for confinement to mean anything; fail closed rather than
		// silently degrade to the lexical-only check.
		return "", fmt.Errorf("resolve sandbox root %q: %w", rootAbs, err)
	}
	realAbs, err := evalExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	rrel, err := filepath.Rel(realRoot, realAbs)
	if err != nil || rrel == ".." || strings.HasPrefix(rrel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the worktree sandbox %q via a symlink", path, rootAbs)
	}
	return abs, nil
}

// evalExistingPrefix resolves symlinks on the longest EXISTING prefix of abs (which
// must be absolute and Clean) and re-appends the trailing not-yet-existing
// components. This lets a path to a new file be checked through its real, existing
// parent: any symlink on the existing portion is followed, so a containment check on
// the result catches an escape, while a leaf that does not exist yet does not error.
func evalExistingPrefix(abs string) (string, error) {
	var missing []string
	p := abs
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p { // reached the filesystem root; nothing along the path existed
			return abs, nil
		}
		missing = append(missing, filepath.Base(p))
		p = parent
	}
}
