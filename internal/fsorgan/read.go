package fsorgan

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"strings"
)

// Read contract constants:
//   - maxReadSizeBytes is the byte cap above which a whole-file read fails (a
//     ranged read bypasses it). The organ is model-agnostic, so a byte cap is
//     the size guard — no model-coupled token cap.
//   - defaultReadOffset is the 1-based default start line.
//   - maxReadLineLen truncates a pathological single line so it cannot flood the
//     window (a model-agnostic safety guard, not a content-shape rule).
const (
	defaultReadOffset = 1
	maxReadSizeBytes  = 256 * 1024 // 0.25 MB
	maxReadLineLen    = 60000
)

// readParams is the typed param struct for fs.read. file_path is required;
// offset (1-based start line) and limit (max lines) are optional.
type readParams struct {
	FilePath string `json:"file_path"`
	Offset   int64  `json:"offset"`
	Limit    int64  `json:"limit"`
}

// UnmarshalJSON accepts `path` as an alias for the canonical `file_path` when
// the latter is absent, so a caller that guesses the directory/search family's
// spelling still succeeds. Canonical-name behavior is unchanged (the alias only
// fills an empty FilePath).
func (p *readParams) UnmarshalJSON(data []byte) error {
	type alias readParams // avoids recursion into this method
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = readParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// readResult is the success shape for fs.read. Content is the numbered-line text
// and is empty when no lines are selected; in that case Warning carries the
// system-reminder text. The line metadata lets a caller page.
type readResult struct {
	FilePath   string `json:"file_path"`
	Content    string `json:"content"`
	Warning    string `json:"warning,omitempty"`
	StartLine  int    `json:"start_line"`
	LineCount  int    `json:"line_count"`
	TotalLines int    `json:"total_lines"`
}

// handleRead reads a file (optionally a line range) and returns its contents as
// numbered lines, recording read-state so a later write/edit on the same path is
// permitted.
//
// Contract:
//   - reader: strip a leading UTF-8 BOM, split on "\n", strip a trailing "\r"
//     per line, select the range [offset-1, offset-1+limit), join with "\n"
//     (NO trailing newline). The final fragment after the last "\n" is always
//     counted, so a file ending in "\n" yields a trailing empty line and
//     total_lines counts that fragment.
//   - format: "<n>\t<content>" with an UNPADDED line number, numbered from
//     offset, joined with "\n".
//   - byte cap: a whole-file read (no limit) of a file larger than the cap
//     fails; a ranged read (limit set) skips the cap.
//   - empty output: offset past EOF (or an empty file) returns a system-reminder
//     Warning, not an error.
//   - offset < 1 is normalized to 1.
func (p *Provider) handleRead(root string, params map[string]any) (map[string]any, error) {
	var rp readParams
	if err := decodeParams(params, &rp); err != nil {
		return nil, fmt.Errorf("fs.read: invalid params: %w", err)
	}
	if strings.TrimSpace(rp.FilePath) == "" {
		return nil, errors.New("fs.read requires file_path")
	}
	resolved, err := resolveWithin(root, rp.FilePath)
	if err != nil {
		return nil, fmt.Errorf("fs.read: %w", err)
	}
	rp.FilePath = resolved

	start := rp.Offset
	if start < 1 {
		start = defaultReadOffset
	}
	lineOffset := start - 1 // 0-based first line to select
	whole := rp.Limit <= 0  // absent / non-positive == whole file
	endLine := int64(-1)    // -1 sentinel == unbounded
	if !whole {
		endLine = lineOffset + rp.Limit
	}

	info, err := os.Stat(rp.FilePath)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, fmt.Errorf("fs.read: file does not exist: %s", rp.FilePath)
		}
		if errors.Is(err, iofs.ErrPermission) {
			return nil, fmt.Errorf("fs.read: permission denied: %s", rp.FilePath)
		}
		return nil, fmt.Errorf("fs.read: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("fs.read: %s is a directory, not a file", rp.FilePath)
	}
	if whole && info.Size() > maxReadSizeBytes {
		return nil, fmt.Errorf(
			"fs.read: file content (%s) exceeds maximum allowed size (%s). Use offset and limit parameters to read specific portions of the file, or search for specific content instead of reading the whole file.",
			humanByteSize(info.Size()), humanByteSize(maxReadSizeBytes),
		)
	}

	f, err := os.Open(rp.FilePath)
	if err != nil {
		return nil, fmt.Errorf("fs.read: %w", err)
	}
	defer f.Close()

	selected, totalLines, err := selectLines(bufio.NewReader(f), lineOffset, endLine)
	if err != nil {
		return nil, fmt.Errorf("fs.read: %w", err)
	}

	// Record read-state: a whole-file read from line 1 satisfies the write/edit
	// precondition; a ranged read is a partial view and does not.
	full := rp.Offset <= 1 && rp.Limit <= 0
	p.reads.markRead(rp.FilePath, info.ModTime().UnixMilli(), full)

	if strings.Join(selected, "\n") == "" {
		// No selectable content: an empty file (the split yields one empty part,
		// so total_lines is 1) or an offset past EOF. Both report the
		// shorter-than-offset system-reminder rather than erroring. (total_lines
		// is always ≥1 — selectLines counts the final fragment — so there is no
		// distinct "contents are empty" case to report.)
		warn := fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", start, totalLines)
		return asResult(readResult{FilePath: rp.FilePath, Warning: warn, StartLine: int(start), TotalLines: totalLines})
	}

	var b strings.Builder
	for i, line := range selected {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\t%s", start+int64(i), line)
	}

	return asResult(readResult{
		FilePath:   rp.FilePath,
		Content:    b.String(),
		StartLine:  int(start),
		LineCount:  len(selected),
		TotalLines: totalLines,
	})
}

// selectLines streams r per the contract's line model: strip a leading UTF-8
// BOM, split on "\n", strip a trailing "\r" per line, and collect the lines
// whose 0-based index falls in [lineOffset, endLine) (endLine < 0 == unbounded).
// It returns the selected lines and the TOTAL line count (counting the final
// fragment after the last "\n", so a file ending in "\n" counts a trailing
// empty line).
func selectLines(r *bufio.Reader, lineOffset, endLine int64) (selected []string, totalLines int, err error) {
	if bom, perr := r.Peek(3); perr == nil && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		_, _ = r.Discard(3)
	}

	idx := int64(0)
	keep := func(line string) {
		if idx >= lineOffset && (endLine < 0 || idx < endLine) {
			selected = append(selected, capLine(line))
		}
		idx++
	}

	for {
		chunk, readErr := r.ReadString('\n')
		if readErr == nil {
			keep(strings.TrimSuffix(chunk, "\n"))
			continue
		}
		if readErr == io.EOF {
			keep(chunk) // final fragment after the last "\n", counted even when empty
			break
		}
		return nil, 0, readErr
	}
	return selected, int(idx), nil
}

// capLine strips a trailing "\r" (CRLF→LF) and truncates a pathological line so
// it cannot flood the window.
func capLine(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if len(line) > maxReadLineLen {
		line = line[:maxReadLineLen] + "... [truncated]"
	}
	return line
}

// humanByteSize renders a byte count for the byte-cap message as "<n> bytes" /
// "<kb>KB" / "<mb>MB" / "<gb>GB" — one decimal with a trailing ".0" stripped, no
// space before the unit.
func humanByteSize(sizeInBytes int64) string {
	kb := float64(sizeInBytes) / 1024
	if kb < 1 {
		return fmt.Sprintf("%d bytes", sizeInBytes)
	}
	if kb < 1024 {
		return trimDotZero(kb) + "KB"
	}
	mb := kb / 1024
	if mb < 1024 {
		return trimDotZero(mb) + "MB"
	}
	return trimDotZero(mb/1024) + "GB"
}

func trimDotZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}
