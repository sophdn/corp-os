package fsorgan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// defaultGrepHeadLimit caps grep output when head_limit is unset, so an
// unbounded content search cannot flood the context. A head_limit of 0 means
// unlimited.
const defaultGrepHeadLimit = 250

// grepMaxColumns clamps emitted line length so minified/base64 content does not
// flood output.
const grepMaxColumns = 500

// grepParams is the typed param struct for fs.grep. pattern is required; the
// rest default per the contract. Tri-state flags are pointers so an absent value
// is distinguishable from an explicit false/0.
//
// The organ implements grep in PURE GO (Go's RE2 regexp engine), so two
// ripgrep-specific knobs are intentionally NOT reimplemented and are rejected
// with a clear error rather than silently ignored: `type` (rg's file-type
// registry) and `multiline`. Use `glob` for file filtering.
type grepParams struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Glob            string `json:"glob,omitempty"`
	Type            string `json:"type,omitempty"`
	OutputMode      string `json:"output_mode,omitempty"`
	ContextBefore   *int   `json:"context_before,omitempty"`
	ContextAfter    *int   `json:"context_after,omitempty"`
	Context         *int   `json:"context,omitempty"`
	ShowLineNumbers *bool  `json:"show_line_numbers,omitempty"`
	CaseInsensitive *bool  `json:"case_insensitive,omitempty"`
	HeadLimit       *int   `json:"head_limit,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	Multiline       bool   `json:"multiline,omitempty"`
}

// UnmarshalJSON accepts `file_path` as an alias for the canonical `path`.
func (p *grepParams) UnmarshalJSON(data []byte) error {
	type alias grepParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = grepParams(a)
	if strings.TrimSpace(p.Path) == "" {
		var fb struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.Path = fb.FilePath
	}
	return nil
}

// grepResult is the success shape for fs.grep. Filenames is non-nil even when
// empty. Content/NumLines apply to content mode; NumMatches to count mode; the
// applied_* paging markers are set only when they took effect.
type grepResult struct {
	Mode          string   `json:"mode"`
	NumFiles      int      `json:"num_files"`
	Filenames     []string `json:"filenames"`
	Content       string   `json:"content,omitempty"`
	NumLines      int      `json:"num_lines,omitempty"`
	NumMatches    int      `json:"num_matches,omitempty"`
	AppliedLimit  int      `json:"applied_limit,omitempty"`
	AppliedOffset int      `json:"applied_offset,omitempty"`
}

// fileMatch is one searched file's match data, relative path + the matching line
// numbers (1-based) and the file's lines.
type fileMatch struct {
	rel     string
	lines   []string
	matched []int // 1-based line numbers that matched
}

// handleGrep searches file contents by regular expression, in pure Go.
func (p *Provider) handleGrep(root string, params map[string]any) (map[string]any, error) {
	var gp grepParams
	if err := decodeParams(params, &gp); err != nil {
		return nil, fmt.Errorf("fs.grep: invalid params: %w", err)
	}
	if strings.TrimSpace(gp.Pattern) == "" {
		return nil, errors.New("fs.grep requires pattern")
	}
	if strings.TrimSpace(gp.Type) != "" {
		return nil, errors.New("fs.grep: the `type` filter is not supported by the native organ; use `glob` to filter files")
	}
	if gp.Multiline {
		return nil, errors.New("fs.grep: `multiline` is not supported by the native organ (line-oriented search only)")
	}

	mode := gp.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	if mode != "content" && mode != "files_with_matches" && mode != "count" {
		return nil, fmt.Errorf("fs.grep: invalid output_mode %q (want content|files_with_matches|count)", mode)
	}

	re, err := compileGrep(gp.Pattern, gp.CaseInsensitive != nil && *gp.CaseInsensitive)
	if err != nil {
		return nil, fmt.Errorf("fs.grep: invalid pattern: %w", err)
	}

	base, files, err := grepFiles(root, gp.Path, gp.Glob)
	if err != nil {
		return nil, err
	}

	matches := make([]fileMatch, 0, len(files))
	for _, rel := range files {
		fm, ok := searchFile(filepath.Join(base, filepath.FromSlash(rel)), rel, re)
		if ok {
			matches = append(matches, fm)
		}
	}

	res := grepResult{Mode: mode, Filenames: []string{}}
	if gp.Offset > 0 {
		res.AppliedOffset = gp.Offset
	}
	switch mode {
	case "content":
		buildGrepContent(&res, matches, gp)
	case "count":
		buildGrepCount(&res, matches, gp)
	default:
		buildGrepFilenames(&res, matches, base, gp)
	}
	return asResult(res)
}

// compileGrep compiles the pattern, prefixing (?i) for a case-insensitive search.
func compileGrep(pattern string, insensitive bool) (*regexp.Regexp, error) {
	if insensitive {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// grepFiles resolves the search target and returns the root the results are
// reported relative to, plus the list of candidate files (relative to root). A
// single-file path searches just that file (root = its parent); a directory
// walks its tree (VCS-excluded). A non-empty glob restricts the candidates.
// grepPathHint returns an actionable suffix for a path-does-not-exist error when
// the path looks like a shell pattern the caller expected the shell to expand —
// brace-expansion / a comma-list ("dir/{a.go,b.go}") or a glob ("dir/*.go"). fs.grep
// takes a single literal file/dir in `path`; multi-file filtering is the `glob`
// param. Without this nudge a model that reached for shell syntax (run-7: the
// worker grepped path="…/{task.go,stamp_validate.go,…}") just sees an opaque
// "does not exist" and can't tell syntax-error from missing-file. Empty for a
// plain path so a genuine typo's error stays clean.
func grepPathHint(target string) string {
	switch {
	case strings.ContainsAny(target, "{}") || strings.Contains(target, ","):
		return " — `path` is a single file or directory, not a shell brace-expansion or comma-list. To search several files, set `path` to their directory and use the `glob` param to match them (e.g. glob=\"*.go\")."
	case strings.ContainsAny(target, "*?"):
		return " — `path` is a literal file or directory, not a glob. Put the directory in `path` and the wildcard in the `glob` param (e.g. path=\"dir\", glob=\"*.go\")."
	default:
		return ""
	}
}

func grepFiles(sandbox, path, glob string) (root string, files []string, err error) {
	target := strings.TrimSpace(path)
	if target == "" {
		// Default to the sandbox root when confined, else the process CWD.
		if sandbox != "" {
			target = sandbox
		} else {
			wd, werr := os.Getwd()
			if werr != nil {
				return "", nil, fmt.Errorf("fs.grep: resolve working directory: %w", werr)
			}
			target = wd
		}
	}
	resolved, rerr := resolveWithin(sandbox, target)
	if rerr != nil {
		return "", nil, fmt.Errorf("fs.grep: %w", rerr)
	}
	abs, aerr := filepath.Abs(resolved)
	if aerr != nil {
		return "", nil, fmt.Errorf("fs.grep: absolutize %q: %w", target, aerr)
	}
	fi, serr := os.Stat(abs)
	if serr != nil {
		return "", nil, fmt.Errorf("fs.grep: path does not exist: %s%s", target, grepPathHint(target))
	}

	if !fi.IsDir() {
		return filepath.Dir(abs), []string{filepath.Base(abs)}, nil
	}

	rels, werr := walkFiles(abs)
	if werr != nil {
		return "", nil, fmt.Errorf("fs.grep: %w", werr)
	}
	if g := strings.TrimSpace(glob); g != "" {
		gre, anchorBase := globToRegexp(g)
		if gre == nil {
			return "", nil, fmt.Errorf("fs.grep: invalid glob: %s", g)
		}
		filtered := rels[:0]
		for _, r := range rels {
			if matchGlob(gre, anchorBase, r) {
				filtered = append(filtered, r)
			}
		}
		rels = filtered
	}
	return abs, rels, nil
}

// searchFile reads a file and records which lines match. Binary files (a NUL in
// the content) and unreadable files are skipped (ok=false).
func searchFile(absPath, rel string, re *regexp.Regexp) (fileMatch, bool) {
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return fileMatch{}, false
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return fileMatch{}, false // binary
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	var matched []int
	for i, ln := range lines {
		if re.MatchString(ln) {
			matched = append(matched, i+1)
		}
	}
	if len(matched) == 0 {
		return fileMatch{}, false
	}
	return fileMatch{rel: rel, lines: lines, matched: matched}, true
}

// buildGrepFilenames fills res for files_with_matches mode: matching files,
// mtime-sorted newest first, paged by offset/head_limit.
func buildGrepFilenames(res *grepResult, matches []fileMatch, root string, gp grepParams) {
	rels := make([]string, len(matches))
	for i, m := range matches {
		rels[i] = m.rel
	}
	sortRelByMtimeDesc(root, rels)
	paged, applied := applyGrepHeadLimit(rels, gp.HeadLimit, gp.Offset)
	res.Filenames = paged
	res.NumFiles = len(paged)
	res.AppliedLimit = applied
}

// buildGrepCount fills res for count mode: a "rel:count" line per matching file.
func buildGrepCount(res *grepResult, matches []fileMatch, gp grepParams) {
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		lines = append(lines, m.rel+":"+strconv.Itoa(len(m.matched)))
	}
	paged, applied := applyGrepHeadLimit(lines, gp.HeadLimit, gp.Offset)
	total, files := 0, 0
	for _, ln := range paged {
		if c, ok := parseTrailingCount(ln); ok {
			total += c
			files++
		}
	}
	res.Content = strings.Join(paged, "\n")
	res.NumMatches = total
	res.NumFiles = files
	res.AppliedLimit = applied
}

// buildGrepContent fills res for content mode: "rel:line:text" for match lines
// and "rel-line-text" for context lines (before/after/context), clamped to
// grepMaxColumns and paged by offset/head_limit.
func buildGrepContent(res *grepResult, matches []fileMatch, gp grepParams) {
	before, after := contextWindow(gp)
	showLines := gp.ShowLineNumbers == nil || *gp.ShowLineNumbers

	var out []string
	for _, m := range matches {
		matchSet := make(map[int]struct{}, len(m.matched))
		for _, n := range m.matched {
			matchSet[n] = struct{}{}
		}
		// Collect the union of match lines and their context windows, in order.
		emit := map[int]struct{}{}
		for _, n := range m.matched {
			lo, hi := n-before, n+after
			for l := lo; l <= hi; l++ {
				if l >= 1 && l <= len(m.lines) {
					emit[l] = struct{}{}
				}
			}
		}
		for l := 1; l <= len(m.lines); l++ {
			if _, ok := emit[l]; !ok {
				continue
			}
			sep := "-"
			if _, isMatch := matchSet[l]; isMatch {
				sep = ":"
			}
			out = append(out, formatGrepLine(m.rel, l, m.lines[l-1], sep, showLines))
		}
	}
	paged, applied := applyGrepHeadLimit(out, gp.HeadLimit, gp.Offset)
	res.Content = strings.Join(paged, "\n")
	res.NumLines = len(paged)
	res.AppliedLimit = applied
}

// contextWindow resolves the before/after context line counts; `context` (-C)
// takes precedence over before/after.
func contextWindow(gp grepParams) (before, after int) {
	if gp.Context != nil {
		c := *gp.Context
		if c < 0 {
			c = 0
		}
		return c, c
	}
	if gp.ContextBefore != nil && *gp.ContextBefore > 0 {
		before = *gp.ContextBefore
	}
	if gp.ContextAfter != nil && *gp.ContextAfter > 0 {
		after = *gp.ContextAfter
	}
	return before, after
}

// formatGrepLine renders one output line, clamped to grepMaxColumns.
func formatGrepLine(rel string, line int, text, sep string, showLine bool) string {
	if len(text) > grepMaxColumns {
		text = text[:grepMaxColumns] + " [... truncated]"
	}
	if showLine {
		return rel + sep + strconv.Itoa(line) + sep + text
	}
	return rel + sep + text
}

// applyGrepHeadLimit pages items: skip offset, then cap at head_limit (default
// defaultGrepHeadLimit, 0 = unlimited). appliedLimit is non-zero only when
// truncation actually occurred.
func applyGrepHeadLimit(items []string, limit *int, offset int) ([]string, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(items) {
		offset = len(items)
	}
	rest := items[offset:]
	if limit != nil && *limit == 0 {
		return rest, 0
	}
	eff := defaultGrepHeadLimit
	if limit != nil {
		eff = *limit
	}
	if len(rest) > eff {
		return rest[:eff], eff
	}
	return rest, 0
}

// parseTrailingCount reads the integer after the last colon of a "path:count"
// line.
func parseTrailingCount(line string) (int, bool) {
	i := strings.LastIndexByte(line, ':')
	if i < 0 {
		return 0, false
	}
	count, err := strconv.Atoi(line[i+1:])
	return count, err == nil
}
