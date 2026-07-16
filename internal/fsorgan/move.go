package fsorgan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// moveParams is the typed param struct for fs.move. source and dest are
// required; src/from and destination/to are accepted aliases.
type moveParams struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

// UnmarshalJSON accepts src/from for source and destination/to for dest when the
// canonical key is absent — the move analogue of the file_path/path alias.
func (p *moveParams) UnmarshalJSON(data []byte) error {
	type alias moveParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = moveParams(a)
	if strings.TrimSpace(p.Source) == "" {
		var fb struct {
			Src  string `json:"src"`
			From string `json:"from"`
		}
		_ = json.Unmarshal(data, &fb)
		if fb.Src != "" {
			p.Source = fb.Src
		} else {
			p.Source = fb.From
		}
	}
	if strings.TrimSpace(p.Dest) == "" {
		var fb struct {
			Destination string `json:"destination"`
			To          string `json:"to"`
		}
		_ = json.Unmarshal(data, &fb)
		if fb.Destination != "" {
			p.Dest = fb.Destination
		} else {
			p.Dest = fb.To
		}
	}
	return nil
}

// moveResult is the success shape for fs.move.
type moveResult struct {
	Source      string `json:"source"`
	Dest        string `json:"dest"`         // the FINAL destination path (dir-into resolved)
	IsDir       bool   `json:"is_dir"`       // the moved entry was a directory
	CrossDevice bool   `json:"cross_device"` // os.Rename failed EXDEV; copy-then-remove ran
}

// handleMove renames or relocates source to dest.
//
// Contract:
//   - source must exist; otherwise a typed "does not exist" error.
//   - when dest is an existing directory the entry is moved INTO it (final path =
//     dest/basename(source), mv semantics); otherwise dest is the literal target
//     and its missing parent directories are created.
//   - the final destination must NOT already exist — fs.move refuses to clobber.
//   - rename is in-process via os.Rename (no shell); a cross-filesystem move
//     (EXDEV) falls back to a recursive copy-then-remove so it still works in the
//     distroless container.
//
// fs.move is NOT read-state coupled: it relocates bytes without inspecting or
// rewriting content.
func (p *Provider) handleMove(root string, params map[string]any) (map[string]any, error) {
	var mp moveParams
	if err := decodeParams(params, &mp); err != nil {
		return nil, fmt.Errorf("fs.move: invalid params: %w", err)
	}
	if strings.TrimSpace(mp.Source) == "" {
		return nil, errors.New("fs.move requires source")
	}
	if strings.TrimSpace(mp.Dest) == "" {
		return nil, errors.New("fs.move requires dest")
	}
	src, err := resolveWithin(root, mp.Source)
	if err != nil {
		return nil, fmt.Errorf("fs.move: %w", err)
	}
	dst, err := resolveWithin(root, mp.Dest)
	if err != nil {
		return nil, fmt.Errorf("fs.move: %w", err)
	}
	mp.Source = src
	mp.Dest = dst

	srcInfo, err := os.Stat(mp.Source)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, fmt.Errorf("fs.move: source does not exist: %s", mp.Source)
		}
		if errors.Is(err, iofs.ErrPermission) {
			return nil, fmt.Errorf("fs.move: permission denied: %s", mp.Source)
		}
		return nil, fmt.Errorf("fs.move: %w", err)
	}

	// Moving onto an existing directory places the entry inside it (mv
	// semantics); otherwise dest is the literal target.
	finalDest := mp.Dest
	if di, derr := os.Stat(mp.Dest); derr == nil && di.IsDir() {
		finalDest = filepath.Join(mp.Dest, filepath.Base(mp.Source))
	}

	// Refuse to clobber an existing final destination — move stays mutating.
	if _, derr := os.Stat(finalDest); derr == nil {
		return nil, fmt.Errorf("fs.move: destination already exists: %s (remove it first if a replace is intended)", finalDest)
	} else if !errors.Is(derr, iofs.ErrNotExist) {
		if errors.Is(derr, iofs.ErrPermission) {
			return nil, fmt.Errorf("fs.move: permission denied: %s", finalDest)
		}
		return nil, fmt.Errorf("fs.move: %w", derr)
	}

	if dir := filepath.Dir(finalDest); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("fs.move: create destination parent: %w", err)
		}
	}

	crossDevice := false
	if err := p.renameFn(mp.Source, finalDest); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return nil, fmt.Errorf("fs.move: %w", err)
		}
		// Cross-filesystem move: copy then remove the source. Pure Go, no shell.
		crossDevice = true
		if cerr := copyTree(mp.Source, finalDest, srcInfo); cerr != nil {
			return nil, fmt.Errorf("fs.move: cross-device copy: %w", cerr)
		}
		if rerr := os.RemoveAll(mp.Source); rerr != nil {
			return nil, fmt.Errorf("fs.move: cross-device remove source after copy: %w", rerr)
		}
	}

	return asResult(moveResult{
		Source:      mp.Source,
		Dest:        finalDest,
		IsDir:       srcInfo.IsDir(),
		CrossDevice: crossDevice,
	})
}

// copyTree recursively copies src to dst, preserving file modes. info is src's
// already-fetched os.FileInfo. Used only by the cross-device fallback of fs.move.
func copyTree(src, dst string, info os.FileInfo) error {
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			ei, err := e.Info()
			if err != nil {
				return err
			}
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), ei); err != nil {
				return err
			}
		}
		return nil
	}
	return copyFile(src, dst, info.Mode().Perm())
}

// copyFile copies a single regular file from src to dst with the given mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
