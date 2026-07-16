package fsorgan

import (
	"encoding/json"
	"fmt"
	iofs "io/fs"
	"os"
	"strings"
)

// lsParams is the typed param struct for fs.ls. path defaults to the working
// directory; all includes dotfiles.
type lsParams struct {
	Path string `json:"path,omitempty"`
	All  *bool  `json:"all,omitempty"`
}

// UnmarshalJSON accepts `file_path` as an alias for the canonical `path` when the
// latter is absent (ls/glob/grep canonically take `path`; read/write/edit take
// `file_path`).
func (p *lsParams) UnmarshalJSON(data []byte) error {
	type alias lsParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = lsParams(a)
	if strings.TrimSpace(p.Path) == "" {
		var fb struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.Path = fb.FilePath
	}
	return nil
}

// lsEntry is one listed directory entry.
type lsEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // dir | file | symlink
	Size int64  `json:"size"` // bytes; 0 for directories
}

// lsResult is the success shape for fs.ls.
type lsResult struct {
	Path    string    `json:"path"`
	Entries []lsEntry `json:"entries"`
	Count   int       `json:"count"`
}

// handleLS lists the immediate entries of a directory, sorted by name (os.ReadDir
// order), with dotfiles gated by `all`.
func (p *Provider) handleLS(root string, params map[string]any) (map[string]any, error) {
	var lp lsParams
	if err := decodeParams(params, &lp); err != nil {
		return nil, fmt.Errorf("fs.ls: invalid params: %w", err)
	}
	abs, err := resolveDir(root, lp.Path, "fs.ls")
	if err != nil {
		return nil, err
	}

	read, err := os.ReadDir(abs) // sorted by filename
	if err != nil {
		return nil, fmt.Errorf("fs.ls: %w", err)
	}

	all := lp.All != nil && *lp.All
	entries := make([]lsEntry, 0, len(read))
	for _, de := range read {
		name := de.Name()
		if !all && strings.HasPrefix(name, ".") {
			continue
		}
		entries = append(entries, lsEntryFor(name, de))
	}
	return asResult(lsResult{Path: abs, Entries: entries, Count: len(entries)})
}

func lsEntryFor(name string, de iofs.DirEntry) lsEntry {
	e := lsEntry{Name: name, Type: "file"}
	switch {
	case de.Type()&iofs.ModeSymlink != 0:
		e.Type = "symlink"
		if info, err := de.Info(); err == nil {
			e.Size = info.Size()
		}
	case de.IsDir():
		e.Type = "dir"
	default:
		if info, err := de.Info(); err == nil {
			e.Size = info.Size()
		}
	}
	return e
}
