package coding

import (
	"context"
	"path"
	"sort"
	"strings"
)

// packageReader is the optional Repo capability the operator uses to read the
// current package state and prior-attempt diffs. The git Repo satisfies it; the
// NoopRepo does not, so context assembly degrades to empty rather than failing.
type packageReader interface {
	ListPackage(ctx context.Context, dir string) ([]string, error)
	Show(ctx context.Context, path string) (string, error)
	Diff(ctx context.Context, fromSHA, toSHA string) (string, error)
	DiffWorktree(ctx context.Context, dir, fromSHA string) (string, error)
}

// CurrentPackageCap bounds how many bytes of each current-package file are
// inlined into the operator context, keeping the prompt bounded.
const CurrentPackageCap = 2400

// currentPackageFiles reads the CURRENT non-test source of the AT's package(s)
// from the integration branch — the operator's ground truth for "what symbols
// exist NOW" (Finding 0). It is empty when the repo cannot read packages (NoopRepo)
// or the AT touches no .go packages. Without it even the strong tier impasses, so
// this is a hard requirement of the operator seat, not an enhancement.
func currentPackageFiles(ctx context.Context, repo Repo, spec AtomicTask) string {
	pr, ok := repo.(packageReader)
	if !ok {
		return ""
	}
	dirs := packageDirs(spec.Workspace)
	if len(dirs) == 0 {
		return ""
	}
	var chunks []string
	for _, dir := range dirs {
		files, err := pr.ListPackage(ctx, dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
				continue
			}
			body, err := pr.Show(ctx, f)
			if err != nil || body == "" {
				continue
			}
			chunks = append(chunks, "// "+f+"\n"+capBytes(body, CurrentPackageCap))
		}
	}
	return strings.Join(chunks, "\n\n")
}

// packageDirs returns the unique parent directories of the .go patterns in a
// workspace allowlist (sorted, stable).
func packageDirs(workspace []string) []string {
	seen := map[string]bool{}
	for _, p := range workspace {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		dir := path.Dir(p)
		if dir == "" || dir == "." {
			continue
		}
		seen[dir] = true
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// captureDiff returns the unified diff an AT's attempt produced relative to its
// fork point: parent..commit for a succeeded AT, or the preserved worktree against
// parent for a failed one. Empty when no fork point/source exists.
func captureDiff(ctx context.Context, repo Repo, ar *ATRecord) string {
	pr, ok := repo.(packageReader)
	if !ok || ar.ParentSHA == "" {
		return ""
	}
	if ar.CommitSHA != "" {
		if d, err := pr.Diff(ctx, ar.ParentSHA, ar.CommitSHA); err == nil {
			return d
		}
		return ""
	}
	if ar.WorktreePath != "" {
		if d, err := pr.DiffWorktree(ctx, ar.WorktreePath, ar.ParentSHA); err == nil {
			return d
		}
	}
	return ""
}

// capBytes truncates s to at most limit bytes.
func capBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
