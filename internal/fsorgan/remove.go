package fsorgan

import (
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// removeParams is the typed param struct for fs.remove. file_path is required
// (path is accepted as an alias). recursive must be set explicitly to delete a
// non-empty directory.
type removeParams struct {
	FilePath  string `json:"file_path"`
	Recursive bool   `json:"recursive"`
}

// UnmarshalJSON accepts `path` as an alias for file_path when the latter is absent.
func (p *removeParams) UnmarshalJSON(data []byte) error {
	type alias removeParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = removeParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// removeResult is the success shape for fs.remove.
type removeResult struct {
	FilePath string `json:"file_path"`
	WasDir   bool   `json:"was_dir"`
}

// dangerousRemoveTargets is the closed set of absolute paths fs.remove refuses
// outright — filesystem roots whose recursive deletion is never a legitimate
// owned-surface operation. A backstop, not a sandbox: real confinement is the
// process's filesystem permissions (and the container's mount set in deployment).
var dangerousRemoveTargets = map[string]struct{}{
	"/":     {},
	"/home": {},
	"/usr":  {},
	"/etc":  {},
	"/var":  {},
	"/bin":  {},
	"/lib":  {},
	"/boot": {},
	"/root": {},
}

// handleRemove deletes file_path.
//
// Contract:
//   - the target must exist; otherwise a typed "does not exist" error.
//   - a regular file (or empty directory) is removed via os.Remove.
//   - a NON-EMPTY directory is removed only when recursive is true (os.RemoveAll);
//     without the flag a typed error is returned and nothing is deleted.
//   - a small set of dangerous absolute targets (filesystem roots) is refused.
//
// fs.remove is destructive: no read-state precondition, but it IS rationale-gated
// for agent actors at the dispatch boundary.
func (p *Provider) handleRemove(root string, params map[string]any) (map[string]any, error) {
	var rp removeParams
	if err := decodeParams(params, &rp); err != nil {
		return nil, fmt.Errorf("fs.remove: invalid params: %w", err)
	}
	if strings.TrimSpace(rp.FilePath) == "" {
		return nil, errors.New("fs.remove requires file_path")
	}
	resolved, err := resolveWithin(root, rp.FilePath)
	if err != nil {
		return nil, fmt.Errorf("fs.remove: %w", err)
	}
	rp.FilePath = resolved

	cleaned := filepath.Clean(absPath(rp.FilePath))
	if _, bad := dangerousRemoveTargets[cleaned]; bad {
		return nil, fmt.Errorf("fs.remove: refusing to remove a protected filesystem root: %s", cleaned)
	}

	info, err := os.Lstat(rp.FilePath)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, fmt.Errorf("fs.remove: path does not exist: %s", rp.FilePath)
		}
		if errors.Is(err, iofs.ErrPermission) {
			return nil, fmt.Errorf("fs.remove: permission denied: %s", rp.FilePath)
		}
		return nil, fmt.Errorf("fs.remove: %w", err)
	}

	isDir := info.IsDir()
	if isDir && !rp.Recursive {
		// A non-empty directory needs the explicit recursive flag; an empty one
		// removes fine via os.Remove. Distinguish by attempting the non-recursive
		// remove and surfacing a clear error when it is non-empty.
		if rerr := os.Remove(rp.FilePath); rerr != nil {
			if isNotEmpty(rerr) {
				return nil, fmt.Errorf("fs.remove: %s is a non-empty directory; pass recursive=true to delete it and its contents", rp.FilePath)
			}
			return nil, fmt.Errorf("fs.remove: %w", rerr)
		}
		return asResult(removeResult{FilePath: rp.FilePath, WasDir: true})
	}

	if isDir {
		if err := os.RemoveAll(rp.FilePath); err != nil {
			return nil, fmt.Errorf("fs.remove: %w", err)
		}
		return asResult(removeResult{FilePath: rp.FilePath, WasDir: true})
	}

	if err := os.Remove(rp.FilePath); err != nil {
		return nil, fmt.Errorf("fs.remove: %w", err)
	}
	return asResult(removeResult{FilePath: rp.FilePath, WasDir: false})
}

// isNotEmpty reports whether err is the "directory not empty" failure os.Remove
// returns for a populated directory (ENOTEMPTY, or EEXIST on some platforms).
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
