package fsorgan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// vcsExcludeDirs are version-control metadata directories excluded from every
// tree walk (ls lists a directory's immediate entries and does NOT use this;
// glob/grep recurse and skip them as noise).
var vcsExcludeDirs = map[string]struct{}{
	".git": {}, ".svn": {}, ".hg": {}, ".bzr": {}, ".jj": {}, ".sl": {},
}

// resolveDir resolves path (default = working dir, or the sandbox root when one
// is set) to an absolute directory, erroring (with the given surface prefix) if
// it is missing, not a directory, or — under a sandbox root — outside it.
func resolveDir(root, path, surface string) (string, error) {
	if strings.TrimSpace(path) == "" {
		// Default to the sandbox root when confined, else the process CWD. A bare
		// directory action under a worktree root must not silently walk the CWD.
		if root != "" {
			path = root
		} else {
			wd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("%s: resolve working directory: %w", surface, err)
			}
			path = wd
		}
	}
	resolved, err := resolveWithin(root, path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", surface, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("%s: absolutize %q: %w", surface, path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s: path does not exist: %s", surface, path)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s: %s is not a directory", surface, path)
	}
	return abs, nil
}

// walkFiles returns every regular file under root, as forward-slashed paths
// relative to root, skipping version-control metadata directories. Hidden files
// are included (the discovery surfaces match dotfiles unless a pattern excludes
// them).
func walkFiles(root string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := vcsExcludeDirs[d.Name()]; skip && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rels, nil
}

// globToRegexp translates a glob pattern into an anchored regexp over a
// forward-slashed path. Supports `*` (any run except `/`), `?` (one non-`/`),
// `**` (any run including `/`), and literal text. A pattern with no `/` matches
// against the path's basename (gitignore-style "match at any depth"); a pattern
// containing `/` or `**` matches the full relative path.
func globToRegexp(pattern string) (*regexp.Regexp, bool) {
	anchorBasename := !strings.Contains(pattern, "/")
	var b strings.Builder
	b.WriteString(`\A`)
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(`.*`) // ** crosses separators
				i++
				// swallow a trailing slash after ** so "**/x" also matches "x"
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					b.WriteString(`(?:/|\A|)`)
					i++
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, false
	}
	return re, anchorBasename
}

// matchGlob reports whether the forward-slashed relative path rel matches the
// compiled glob. When anchorBasename is set the match is tried against the
// basename (so "*.go" matches at any depth); otherwise against the full path.
func matchGlob(re *regexp.Regexp, anchorBasename bool, rel string) bool {
	if anchorBasename {
		return re.MatchString(filepath.Base(rel))
	}
	return re.MatchString(rel)
}

// sortRelByMtimeDesc sorts forward-slashed paths (relative to root) by file
// modification time, newest first, with the path as a tiebreaker. Unstattable
// paths sort as mtime 0.
func sortRelByMtimeDesc(root string, rels []string) {
	mtime := make(map[string]int64, len(rels))
	for _, r := range rels {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(r))); err == nil {
			mtime[r] = fi.ModTime().UnixNano()
		}
	}
	sort.SliceStable(rels, func(i, j int) bool {
		if mtime[rels[i]] != mtime[rels[j]] {
			return mtime[rels[i]] > mtime[rels[j]]
		}
		return rels[i] < rels[j]
	})
}
