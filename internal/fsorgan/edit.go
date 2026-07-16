package fsorgan

import (
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"strings"
)

// editParams is the typed param struct for fs.edit. file_path, old_string, and
// new_string are required; replace_all defaults false.
type editParams struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// UnmarshalJSON accepts `path` as an alias for the canonical `file_path` when
// the latter is absent (see readParams.UnmarshalJSON).
func (p *editParams) UnmarshalJSON(data []byte) error {
	type alias editParams
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = editParams(a)
	if strings.TrimSpace(p.FilePath) == "" {
		var fb struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(data, &fb)
		p.FilePath = fb.Path
	}
	return nil
}

// editResult is the success shape for fs.edit.
type editResult struct {
	FilePath     string `json:"file_path"`
	Created      bool   `json:"created"` // an empty old_string created a new file
	Replacements int    `json:"replacements"`
}

// handleEdit replaces old_string with new_string in file_path.
//
// Contract:
//   - old_string == new_string → a "no changes" error.
//   - content is read and CRLF normalized to LF before matching; the result is
//     written with LF endings.
//   - old_string must occur exactly once unless replace_all is set (0 → not-found;
//     >1 without replace_all → an ambiguity error naming the count). replace_all
//     replaces every occurrence; otherwise the first.
//   - empty old_string creates a new file from new_string (nonexistent path) or
//     fills an empty existing file; an empty old_string on a non-empty file errs.
//   - editing an EXISTING file requires it was fully read first and unchanged
//     since (the read/write/edit precondition).
func (p *Provider) handleEdit(root string, params map[string]any) (map[string]any, error) {
	var ep editParams
	if err := decodeParams(params, &ep); err != nil {
		return nil, fmt.Errorf("fs.edit: invalid params: %w", err)
	}
	if strings.TrimSpace(ep.FilePath) == "" {
		return nil, errors.New("fs.edit requires file_path")
	}
	if ep.OldString == ep.NewString {
		return nil, errors.New("fs.edit: old_string and new_string are identical, so this edit is a no-op. old_string must be the EXACT existing text to find; new_string must be that text WITH your change applied — they must differ. Put the current code in old_string and the edited code in new_string.")
	}
	resolved, err := resolveWithin(root, ep.FilePath)
	if err != nil {
		return nil, fmt.Errorf("fs.edit: %w", err)
	}
	ep.FilePath = resolved

	info, statErr := os.Stat(ep.FilePath)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, iofs.ErrNotExist) {
		if errors.Is(statErr, iofs.ErrPermission) {
			return nil, fmt.Errorf("fs.edit: permission denied: %s", ep.FilePath)
		}
		return nil, fmt.Errorf("fs.edit: %w", statErr)
	}
	if exists && info.IsDir() {
		return nil, fmt.Errorf("fs.edit: %s is a directory, not a file", ep.FilePath)
	}

	// Nonexistent file: only an empty old_string is valid (new file creation).
	if !exists {
		if ep.OldString != "" {
			return nil, fmt.Errorf("fs.edit: file does not exist: %s", ep.FilePath)
		}
		if err := writeFileMkdir(ep.FilePath, ep.NewString); err != nil {
			return nil, fmt.Errorf("fs.edit: %w", err)
		}
		p.markWritten(ep.FilePath)
		return asResult(editResult{FilePath: ep.FilePath, Created: true, Replacements: 1})
	}

	// Existing file: require a prior full read (and no change since).
	if ok, reason := p.reads.checkWritable(ep.FilePath, info.ModTime().UnixMilli()); !ok {
		return nil, fmt.Errorf("fs.edit: %s", reason)
	}
	raw, err := os.ReadFile(ep.FilePath)
	if err != nil {
		return nil, fmt.Errorf("fs.edit: %w", err)
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")

	// Empty old_string on an existing file: valid only when the file is empty.
	if ep.OldString == "" {
		if strings.TrimSpace(content) != "" {
			return nil, errors.New("fs.edit: Cannot create new file - file already exists.")
		}
		if err := writeFileMkdir(ep.FilePath, ep.NewString); err != nil {
			return nil, fmt.Errorf("fs.edit: %w", err)
		}
		p.markWritten(ep.FilePath)
		return asResult(editResult{FilePath: ep.FilePath, Replacements: 1})
	}

	count := strings.Count(content, ep.OldString)
	if count == 0 {
		// Whitespace-tolerant fallback (single replacement only): a model frequently
		// reproduces a block's text correctly but with slightly-off leading indentation
		// or trailing whitespace, so the exact match misses and the worker thrashes a
		// whole budget on edits that never land. Retry matching line-by-line ignoring
		// each line's surrounding whitespace, and apply the edit ONLY when exactly one
		// contiguous block matches (uniqueness — never resolve an ambiguous fuzzy hit).
		// The block's real file indentation is preserved by reindenting new_string, so a
		// uniformly-dedented old/new pair cannot corrupt the result. A genuinely wrong
		// edit still fails the orchestrator-owned build/test gate and is revised.
		if !ep.ReplaceAll {
			if out, ok := lineTrimmedReplace(content, ep.OldString, ep.NewString); ok {
				if err := writeFileMkdir(ep.FilePath, out); err != nil {
					return nil, fmt.Errorf("fs.edit: %w", err)
				}
				p.markWritten(ep.FilePath)
				return asResult(editResult{FilePath: ep.FilePath, Replacements: 1})
			}
		}
		// fs.read-paste fallback: fs.read renders "<n>\t<content>" numbered lines, and a
		// worker frequently pastes that numbered text into old_string — which must match
		// the file's RAW (unnumbered) bytes, so the exact match misses and the worker
		// thrashes its whole budget to a strong-bound halt with ZERO edits. When EVERY
		// line of old_string carries the exact "<digits>\t" fs.read shape, retry against
		// the file with the prefixes stripped, then fall through to the normal
		// uniqueness/replace_all handling below (a stripped string that matches >1 site
		// without replace_all still errs as ambiguous; one that matches none stays
		// not-found). A genuinely wrong edit still fails the orchestrator-owned build/test
		// gate, exactly as the whitespace-tolerant fallback above relies on.
		if stripped, ok := stripLineNumberPrefixes(ep.OldString); ok && stripped != ep.OldString {
			if c := strings.Count(content, stripped); c > 0 {
				ep.OldString, count = stripped, c // fall through to normal handling
			} else if !ep.ReplaceAll {
				if out, ok := lineTrimmedReplace(content, stripped, ep.NewString); ok {
					if err := writeFileMkdir(ep.FilePath, out); err != nil {
						return nil, fmt.Errorf("fs.edit: %w", err)
					}
					p.markWritten(ep.FilePath)
					return asResult(editResult{FilePath: ep.FilePath, Replacements: 1})
				}
			}
		}
		if count == 0 {
			msg := "fs.edit: String to replace not found in file. If you copied old_string from fs.read output, strip the leading \"<n>\\t\" line-number prefix from each line — old_string must match the file's raw text, not the numbered display."
			if hint, ok := nearestEditHint(content, ep.OldString); ok {
				// The model usually drifted (abbreviated/paraphrased the target). Show the
				// closest ACTUAL text so it copies verbatim instead of re-guessing.
				msg += fmt.Sprintf("\nThe closest existing text in the file is below — copy it EXACTLY into old_string (verbatim; do not abbreviate, paraphrase, or add quotes):\n%s", hint)
			}
			// A multi-line old_string that missed is usually an INSERTION the worker tried to
			// do by replacing a whole block it then drifted reproducing. Coach the minimal-edit
			// pattern at the failure moment: anchor on ONE existing line, not a block.
			if editLineCount(ep.OldString) >= editBlockLineThreshold {
				msg += "\nYour old_string spans several lines — reproducing a whole block verbatim is what missed. If you are INSERTING code, do NOT replace a block: set old_string to ONE short unique existing line next to the insertion point, and new_string to that SAME line plus your added lines."
			}
			return nil, fmt.Errorf("%s\nString: %s", msg, ep.OldString)
		}
	}
	if count > 1 && !ep.ReplaceAll {
		return nil, fmt.Errorf("fs.edit: Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.\nString: %s", count, ep.OldString)
	}

	var out string
	var n int
	if ep.ReplaceAll {
		out, n = strings.ReplaceAll(content, ep.OldString, ep.NewString), count
	} else {
		out, n = strings.Replace(content, ep.OldString, ep.NewString, 1), 1
	}
	if err := writeFileMkdir(ep.FilePath, out); err != nil {
		return nil, fmt.Errorf("fs.edit: %w", err)
	}
	p.markWritten(ep.FilePath)
	return asResult(editResult{FilePath: ep.FilePath, Replacements: n})
}

// lineTrimmedReplace performs a single whitespace-tolerant replacement of oldStr with
// newStr in content, used only as a fallback when the exact oldStr is absent. It matches
// oldStr against content one line at a time ignoring each line's leading/trailing
// whitespace, and succeeds ONLY when exactly one contiguous block of content lines
// matches — an ambiguous (multiple) or absent fuzzy match returns ok=false so the caller
// keeps the honest not-found error rather than guessing. The matched block's real
// indentation is preserved: newStr is reindented by the leading-whitespace prefix the
// model dropped on the first line, so an under-indented old/new pair edits in place
// without corrupting the file's indentation. ReplaceAll is intentionally not supported
// here (fuzzy + replace-every is too blunt). Returns the rewritten content and true on a
// unique match.
func lineTrimmedReplace(content, oldStr, newStr string) (string, bool) {
	oldLines := strings.Split(oldStr, "\n")
	// A trailing newline in oldStr yields a final empty element; drop it so the block is
	// the meaningful lines (models inconsistently include the final newline).
	if n := len(oldLines); n > 1 && oldLines[n-1] == "" {
		oldLines = oldLines[:n-1]
	}
	if len(oldLines) == 0 {
		return "", false
	}

	contentLines := strings.Split(content, "\n")
	// Byte offset where each content line starts (every line counts its "\n" separator;
	// the final line has none, but its span end is computed from its own length below).
	offsets := make([]int, len(contentLines))
	pos := 0
	for i, ln := range contentLines {
		offsets[i] = pos
		pos += len(ln) + 1
	}

	win := len(oldLines)
	matchStart, matches := -1, 0
	for i := 0; i+win <= len(contentLines); i++ {
		all := true
		for j := 0; j < win; j++ {
			if strings.TrimSpace(contentLines[i+j]) != strings.TrimSpace(oldLines[j]) {
				all = false
				break
			}
		}
		if all {
			matches++
			matchStart = i
			if matches > 1 {
				return "", false // ambiguous — refuse to fuzzy-guess
			}
		}
	}
	if matches != 1 {
		return "", false
	}

	start := offsets[matchStart]
	last := matchStart + win - 1
	end := offsets[last] + len(contentLines[last]) // end of the last matched line (no trailing \n)
	if end > len(content) {
		end = len(content)
	}

	// Rebase newStr onto the matched block's REAL indentation. The fuzzy match ignores
	// whitespace, so newStr may carry a different indentation basis than the file — a
	// model commonly dedents old_string while leaving new_string indented (or the
	// reverse). Shift every non-blank new line by (contentBasis − newBasis): strip
	// new_string's OWN basis (the indent of its first non-blank line) and prepend the
	// matched block's real indent, which preserves new_string's internal relative
	// structure. Rebasing on new_string's own basis — NOT old_string's — is what stops a
	// dedented-old / indented-new pair from double-indenting the block (bug 1110: the
	// surrounding lines went 1 tab → 2). When the bases already match, newStr is written
	// verbatim.
	nl := strings.Split(newStr, "\n")
	newBasis := ""
	for _, ln := range nl {
		if strings.TrimSpace(ln) != "" {
			newBasis = leadingWS(ln)
			break
		}
	}
	contentBasis := leadingWS(contentLines[matchStart])
	if newBasis != contentBasis {
		for k, ln := range nl {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			rest := strings.TrimLeft(ln, " \t")
			if strings.HasPrefix(ln, newBasis) {
				rest = ln[len(newBasis):] // keep indent relative to the block's own basis
			}
			nl[k] = contentBasis + rest
		}
	}
	replacement := strings.Join(nl, "\n")
	return content[:start] + replacement + content[end:], true
}

// leadingWS returns the run of spaces/tabs at the start of s.
func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// Nearest-text hint tuning. When an old_string matches nothing (even after the
// whitespace and line-number fallbacks), a weaker model has usually DRIFTED — it
// reproduced the target from memory and abbreviated or paraphrased it rather than
// copying the file's bytes (the fs-edit-reliability finding: a 56-char abbreviation
// of a 233-char line). Showing the closest ACTUAL text turns that miss into a
// verbatim copy on the next attempt instead of a grep-and-re-read hunt.
const (
	// editHintMinAnchorRunes is the shortest trimmed anchor line that may trigger a
	// hint. Below it a match is too generic to trust (a bare "return nil" matches
	// many lines), so no hint is offered and the caller keeps the plain error.
	editHintMinAnchorRunes = 12
	// editHintMinScore is the fraction of the anchor's trigrams that must appear in
	// the best-matching file line for it to be shown as the closest text.
	editHintMinScore = 0.5
	// editHintMaxLines caps a multi-line hint so a hint for a large drifted block
	// cannot itself blow the tool-result budget.
	editHintMaxLines = 12
	// editBlockLineThreshold is the old_string line count at/above which a not-found
	// error also coaches the single-line-anchor insertion pattern (a block that big is
	// where verbatim reproduction drifts).
	editBlockLineThreshold = 3
)

// editLineCount returns the number of lines s spans, ignoring a single trailing
// newline (so "a\nb\n" and "a\nb" both count 2).
func editLineCount(s string) int {
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// nearestEditHint returns the run of file lines most similar to oldStr, to show a
// model that supplied a drifted (abbreviated or paraphrased) old_string the REAL
// text so it can copy it verbatim. It anchors on the first non-blank line of oldStr,
// scores every content line by character-trigram overlap with that anchor, and — when
// the best line clears editHintMinScore — returns that line plus enough following
// lines to cover oldStr's line count (capped at editHintMaxLines), verbatim. It
// returns ("", false) when the anchor is too short to match reliably or nothing is
// similar enough, so the caller keeps the plain not-found error rather than a
// misleading hint. It never edits — it is diagnostic only.
func nearestEditHint(content, oldStr string) (string, bool) {
	oldLines := strings.Split(oldStr, "\n")
	anchor := ""
	for _, ln := range oldLines {
		if t := strings.TrimSpace(ln); t != "" {
			anchor = t
			break
		}
	}
	if len([]rune(anchor)) < editHintMinAnchorRunes {
		return "", false
	}
	// An anchor of >= editHintMinAnchorRunes (12) runes always yields >= 10 trigrams,
	// so total is never zero here and the division below is safe.
	want := trigrams(anchor)
	total := 0
	for _, c := range want {
		total += c
	}

	contentLines := strings.Split(content, "\n")
	best, bestScore := -1, 0.0
	for i, ln := range contentLines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		score := float64(trigramOverlap(want, trigrams(strings.TrimSpace(ln)))) / float64(total)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 || bestScore < editHintMinScore {
		return "", false
	}

	// Window: the matched line plus enough following lines to cover oldStr's shape.
	// n >= 1 always: the anchor came from a non-blank oldLines entry, so oldLines is
	// non-empty; dropping a single trailing-newline artifact leaves at least it.
	n := len(oldLines)
	if n > 1 && oldLines[n-1] == "" {
		n-- // drop a trailing-newline artifact (mirrors the fuzzy matcher)
	}
	if n > editHintMaxLines {
		n = editHintMaxLines
	}
	end := best + n
	if end > len(contentLines) {
		end = len(contentLines)
	}
	return strings.Join(contentLines[best:end], "\n"), true
}

// trigrams returns the multiset of 3-rune substrings of s. A rune (not byte) window
// keeps it correct for multi-byte source. Fewer than 3 runes yields an empty set.
func trigrams(s string) map[string]int {
	r := []rune(s)
	m := make(map[string]int, len(r))
	for i := 0; i+3 <= len(r); i++ {
		m[string(r[i:i+3])]++
	}
	return m
}

// trigramOverlap counts trigrams shared between a and b, honoring multiplicity (the
// min of the two counts per trigram) so a short anchor cannot over-score against a
// line that merely repeats one trigram.
func trigramOverlap(a, b map[string]int) int {
	shared := 0
	for k, av := range a {
		if bv, ok := b[k]; ok {
			shared += min(av, bv)
		}
	}
	return shared
}

// stripLineNumberPrefixes removes the "<n>\t" line-number prefix that fs.read
// prepends to each line of its display output (read.go format: "%d\t%s"). It is
// the recovery for a worker that pastes fs.read's numbered text into fs.edit's
// old_string, which must match the file's RAW bytes.
//
// The strip is LENIENT / per-line: a line beginning with a run of ASCII digits
// followed by a tab has that single prefix removed; any other line (ordinary code,
// a leading tab with no number, digits with no tab) is kept VERBATIM. This is what
// recovers the common MIXED contamination — a model numbers some lines of the block
// and not others (bug 1147: the earlier all-or-nothing strip bailed the moment one
// line lacked the shape, so no edit ever landed and the run thrashed its whole
// budget to a strong-bound halt). The un-numbered lines are exactly the ones the
// whitespace-tolerant lineTrimmedReplace already handles, so lenient-strip +
// fuzzy-fallback compose to cover a block in any mix of the two contaminations.
//
// It returns ok=true only when at least one prefix was ACTUALLY stripped AND the
// result still has some non-empty content. So an entirely un-numbered block
// (nothing stripped) and a block that strips to all-empty both return ok=false and
// are never fed to the matcher — ordinary pasted code is never rewritten on the
// strength of this recovery alone. Conservatism is preserved downstream: the caller
// still requires the stripped form to locate a UNIQUE span in the real file (exact
// strings.Count, or the unique-match lineTrimmedReplace) before any edit lands, so a
// wrongly-stripped line simply fails to match rather than editing the wrong span. A
// single trailing empty line (a worker who kept the final newline) is dropped before
// checking, mirroring lineTrimmedReplace.
func stripLineNumberPrefixes(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	out := make([]string, len(lines))
	strippedAny, anyContent := false, false
	for i, ln := range lines {
		j := 0
		for j < len(ln) && ln[j] >= '0' && ln[j] <= '9' {
			j++
		}
		if j > 0 && j < len(ln) && ln[j] == '\t' {
			out[i] = ln[j+1:] // "<digits>\t" fs.read prefix — strip exactly one
			strippedAny = true
		} else {
			out[i] = ln // un-numbered line: keep verbatim (mixed contamination)
		}
		if out[i] != "" {
			anyContent = true
		}
	}
	if !strippedAny || !anyContent {
		return "", false
	}
	return strings.Join(out, "\n"), true
}
