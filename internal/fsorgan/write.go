package fsorgan

import (
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
)

// writeParams is the typed param struct for fs.write. file_path and content are
// required (content may be empty — that writes an empty file). overwrite is the
// explicit escape hatch that permits replacing the WHOLE of an existing file
// (default false — an existing path is otherwise refused, so a "create" cannot
// silently clobber).
type writeParams struct {
	FilePath  string `json:"file_path"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite"`
}

// UnmarshalJSON accepts `path` as an alias for the canonical `file_path` when
// the latter is absent (see readParams.UnmarshalJSON).
func (p *writeParams) UnmarshalJSON(data []byte) error {
	type alias writeParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = writeParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// writeResult is the success shape for fs.write.
type writeResult struct {
	FilePath     string `json:"file_path"`
	Created      bool   `json:"created"` // the file did not exist before the write
	BytesWritten int    `json:"bytes_written"`
	LineCount    int    `json:"line_count"`
}

// handleWrite writes content to file_path, replacing the whole file.
//
// Contract:
//   - content is written verbatim (UTF-8); parent directories are created.
//   - fs.write to an EXISTING path is REFUSED unless overwrite:true — a plain
//     write is for creating a brand-new file, so a worker that means to "create"
//     cannot silently clobber an existing one (fs.move and fs.edit refuse to
//     clobber too; this closes the lone unguarded mutator). The refusal is
//     actionable: use fs.edit for a targeted change, or pass overwrite:true to
//     replace the whole file deliberately.
//   - overwriting an EXISTING file (overwrite:true) additionally requires it was
//     fully read first and has not changed since (the read/write/edit
//     precondition) — else a typed "read it first" / "modified since read" error.
//     Creating a NEW file needs no read.
//   - after a successful write the new state is recorded as read, so an immediate
//     follow-up write/edit needs no re-read.
func (p *Provider) handleWrite(root string, params map[string]any) (map[string]any, error) {
	var wp writeParams
	if err := decodeParams(params, &wp); err != nil {
		return nil, fmt.Errorf("fs.write: invalid params: %w", err)
	}
	if strings.TrimSpace(wp.FilePath) == "" {
		return nil, errors.New("fs.write requires file_path")
	}
	resolved, err := resolveWithin(root, wp.FilePath)
	if err != nil {
		return nil, fmt.Errorf("fs.write: %w", err)
	}
	wp.FilePath = resolved

	info, statErr := os.Stat(wp.FilePath)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, iofs.ErrNotExist) {
		if errors.Is(statErr, iofs.ErrPermission) {
			return nil, fmt.Errorf("fs.write: permission denied: %s", wp.FilePath)
		}
		return nil, fmt.Errorf("fs.write: %w", statErr)
	}
	if exists && info.IsDir() {
		return nil, fmt.Errorf("fs.write: %s is a directory, not a file", wp.FilePath)
	}
	if exists {
		// Clobber guard: a plain fs.write to an existing path is refused, so a
		// worker intending to "create" cannot silently destroy the file. The
		// explicit overwrite:true escape hatch permits the rare deliberate replace.
		if !wp.Overwrite {
			return nil, fmt.Errorf("fs.write: %s already exists — use fs.edit for a targeted change, or pass overwrite:true to replace the whole file deliberately", wp.FilePath)
		}
		if ok, reason := p.reads.checkWritable(wp.FilePath, info.ModTime().UnixMilli()); !ok {
			return nil, fmt.Errorf("fs.write: %s", reason)
		}
	}

	if err := writeFileMkdir(wp.FilePath, wp.Content); err != nil {
		return nil, fmt.Errorf("fs.write: %w", err)
	}
	p.markWritten(wp.FilePath)

	return asResult(writeResult{
		FilePath:     wp.FilePath,
		Created:      !exists,
		BytesWritten: len(wp.Content),
		LineCount:    lineCount(wp.Content),
	})
}

// writeFileMkdir writes content to path, creating parent directories as needed.
// Shared by fs.write and fs.edit's file-creation paths.
func writeFileMkdir(path, content string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create parent dir: %w", err)
		}
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// markWritten records the just-written file's state as read, so the family
// precondition is satisfied for an immediate follow-up write/edit.
func (p *Provider) markWritten(path string) {
	if ni, err := os.Stat(path); err == nil {
		p.reads.markRead(path, ni.ModTime().UnixMilli(), true)
	}
}

// lineCount counts newline-terminated lines plus a trailing unterminated line.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
