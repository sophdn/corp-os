package fsorgan

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// globMaxResults caps the number of files fs.glob returns; beyond it the result
// is marked truncated.
const globMaxResults = 100

// globParams is the typed param struct for fs.glob. pattern is required.
type globParams struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// UnmarshalJSON accepts `file_path` as an alias for the canonical `path`.
func (p *globParams) UnmarshalJSON(data []byte) error {
	type alias globParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = globParams(a)
	if strings.TrimSpace(p.Path) == "" {
		var fb struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.Path = fb.FilePath
	}
	return nil
}

// globResult is the success shape for fs.glob. Filenames is non-nil even when
// empty, reported relative to the search root, sorted by modification time
// (newest first).
type globResult struct {
	Filenames  []string `json:"filenames"`
	NumFiles   int      `json:"num_files"`
	Truncated  bool     `json:"truncated"`
	DurationMS int64    `json:"duration_ms"`
}

// handleGlob matches files under the search root by glob pattern, in pure Go (no
// external ripgrep dependency — the organ stays self-contained for distroless
// corpos). Results are mtime-sorted (newest first), relative to the root, and
// capped at globMaxResults. Pattern semantics: `*`/`?` within a path segment,
// `**` across segments; a pattern with no `/` matches the basename at any depth.
func (p *Provider) handleGlob(root string, params map[string]any) (map[string]any, error) {
	start := p.now()
	var gp globParams
	if err := decodeParams(params, &gp); err != nil {
		return nil, fmt.Errorf("fs.glob: invalid params: %w", err)
	}
	if strings.TrimSpace(gp.Pattern) == "" {
		return nil, errors.New("fs.glob requires pattern")
	}
	re, anchorBase := globToRegexp(gp.Pattern)
	if re == nil {
		return nil, fmt.Errorf("fs.glob: invalid pattern: %s", gp.Pattern)
	}

	dir, err := resolveDir(root, gp.Path, "fs.glob")
	if err != nil {
		return nil, err
	}
	rels, err := walkFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("fs.glob: %w", err)
	}

	matched := make([]string, 0, len(rels))
	for _, rel := range rels {
		if matchGlob(re, anchorBase, rel) {
			matched = append(matched, rel)
		}
	}
	sortRelByMtimeDesc(dir, matched)

	truncated := false
	if len(matched) > globMaxResults {
		matched = matched[:globMaxResults]
		truncated = true
	}

	return asResult(globResult{
		Filenames:  matched,
		NumFiles:   len(matched),
		Truncated:  truncated,
		DurationMS: p.now().Sub(start).Milliseconds(),
	})
}
