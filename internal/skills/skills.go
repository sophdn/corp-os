// Package skills loads a skills tree (markdown) into a session system prompt.
// Two shapes: top-level <name>.md and directory <name>/SKILL.md. Ported from
// bridge-harness skills.py.
//
// The _manifest.toml bucket support (ambient vs lazy selection) is deferred
// until a TOML dependency is vetted (blueprint §6); with no manifest, Select(nil)
// returns all discovered skills — matching the Python port's no-manifest fallback.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

const systemPromptPreamble = "You are an agent operating through Corp-OS over toolkit-server. " +
	"The disciplines below are loaded from the skills tree and are binding for this session. Follow them as written."

// Skill is one discovered skill.
type Skill struct {
	Name        string
	Path        string
	Description string
	Body        string
}

// Loader discovers and assembles skills from one or more layers: an optional
// embedded baseline (builtin) overlaid by an optional on-disk tree (dir). A skill
// present in both layers is taken from dir — live edits win. New yields a
// disk-only Loader (back-compat); Builtin / BuiltinWithOverride add the baseline.
type Loader struct {
	dir     string  // on-disk overlay tree (highest priority); "" = none
	builtin []Skill // embedded baseline (lower priority); nil for New(dir)
}

// New returns a Loader rooted at the on-disk tree dir (no embedded baseline).
func New(dir string) *Loader { return &Loader{dir: dir} }

// Dir returns the on-disk overlay tree root.
func (l *Loader) Dir() string { return l.dir }

// Discover finds every skill across the Loader's layers (embedded baseline first,
// then the on-disk overlay, which overrides by name), sorted by name. Absent
// layers contribute nothing (not an error).
func (l *Loader) Discover() ([]Skill, error) {
	found := make(map[string]Skill, len(l.builtin))
	for _, s := range l.builtin {
		found[s.Name] = s
	}
	disk, err := l.discoverDisk()
	if err != nil {
		return nil, err
	}
	for name, s := range disk {
		found[name] = s // on-disk overlay wins on a name collision
	}
	return sortedSkills(found), nil
}

// discoverDisk discovers the on-disk overlay tree (l.dir). An empty, absent, or
// non-directory tree yields no skills (not an error) — the original disk contract.
func (l *Loader) discoverDisk() (map[string]Skill, error) {
	if l.dir == "" {
		return nil, nil
	}
	info, err := os.Stat(l.dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil // absent tree → no skills (not an error)
	case err != nil:
		return nil, err // a real stat error propagates
	case !info.IsDir():
		return nil, nil // a non-directory where a tree was expected → no skills
	}
	return discoverFS(os.DirFS(l.dir), ".")
}

// discoverFS discovers both skill shapes under root in fsys: directory-shape
// <name>/SKILL.md and top-level <name>.md (README skipped; a directory-shape skill
// wins over a same-named file). root must be an existing directory in fsys.
func discoverFS(fsys fs.FS, root string) (map[string]Skill, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, err
	}
	found := map[string]Skill{}
	// Directory-shape skills: <name>/SKILL.md
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := path.Join(root, e.Name(), "SKILL.md")
		if b, err := fs.ReadFile(fsys, p); err == nil {
			found[e.Name()] = readSkill(e.Name(), p, b)
		}
	}
	// Top-level file skills: <name>.md (skip README and already-found names)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if strings.EqualFold(name, "readme") {
			continue
		}
		if _, ok := found[name]; ok {
			continue
		}
		p := path.Join(root, e.Name())
		if b, err := fs.ReadFile(fsys, p); err == nil {
			found[name] = readSkill(name, p, b)
		}
	}
	return found, nil
}

// sortedSkills flattens a name→Skill map into a name-sorted slice.
func sortedSkills(found map[string]Skill) []Skill {
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Skill, 0, len(found))
	for _, n := range names {
		out = append(out, found[n])
	}
	return out
}

// Select returns the named skills (order preserved, unknown names skipped), or
// all discovered skills when names is nil.
func (l *Loader) Select(names []string) ([]Skill, error) {
	all, err := l.Discover()
	if err != nil {
		return nil, err
	}
	if names == nil {
		return all, nil
	}
	byName := make(map[string]Skill, len(all))
	for _, s := range all {
		byName[s.Name] = s
	}
	out := make([]Skill, 0, len(names))
	for _, n := range names {
		if s, ok := byName[n]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// SystemPrompt concatenates the selected skills (full bodies) into one
// system-prompt string. Equivalent to SystemPromptWithin with no cap.
func SystemPrompt(skills []Skill) string {
	return SystemPromptWithin(skills, 0)
}

// SystemPromptWithin assembles the selected skills into a system prompt while
// keeping the total skill text within maxTokens (estimated at ~4 chars/token, the
// same heuristic the loop's compactor uses). Skills are taken in order: a skill's
// FULL body is included while the running estimate stays within budget; once a
// full body would exceed it, that skill is injected in a TERSE tier (the skill's
// authored `## Core` section if it has one, else its one-line description plus an
// outline of its section headings); and when even the terse body would exceed the
// budget it falls to a DIGEST tier — just the one-line description plus a pointer to
// read the skill. The terse tier is itself budget-checked (it was not, originally:
// several big-`## Core` disciplines — the coding worker's set — blew far past the cap
// even "terse", bloating a narrow-window worker's preamble until its first model call
// timed out before any tool call). Every selected skill is always represented at one
// tier or another; nothing is silently dropped, so the binding preamble ("follow them
// as written") stays honest even on a narrow window. maxTokens <= 0 means no cap — full
// bodies for all — preserving prior behavior for large-window models.
func SystemPromptWithin(skills []Skill, maxTokens int) string {
	var b strings.Builder
	b.WriteString(systemPromptPreamble)
	used := estTokens(systemPromptPreamble)
	for _, s := range skills {
		full := strings.TrimSpace(s.Body)
		if maxTokens <= 0 || used+estTokens(full)+estTokens(s.Name)+4 <= maxTokens {
			b.WriteString("\n\n# Skill: ")
			b.WriteString(s.Name)
			b.WriteString("\n\n")
			b.WriteString(full)
			used += estTokens(full) + estTokens(s.Name) + 4
			continue
		}
		// Full doesn't fit (maxTokens > 0 here). Try the terse Core within budget; else digest.
		if terse := terseBody(s); used+estTokens(terse)+estTokens(s.Name)+4 <= maxTokens {
			b.WriteString("\n\n# Skill (terse — full text omitted to fit the context window): ")
			b.WriteString(s.Name)
			b.WriteString("\n\n")
			b.WriteString(terse)
			used += estTokens(terse) + estTokens(s.Name) + 4
			continue
		}
		dg := digestBody(s)
		b.WriteString("\n\n# Skill (digest — name + summary only; read the skill for detail): ")
		b.WriteString(s.Name)
		b.WriteString("\n\n")
		b.WriteString(dg)
		used += estTokens(dg) + estTokens(s.Name) + 4
	}
	return b.String()
}

// digestBody is the leanest skill representation — its one-line description plus a
// pointer — for when even the terse `## Core` would exceed the window. It keeps the
// skill named and honest ("read it if a call turns on its detail") without spending
// the narrow window on the discipline.
func digestBody(s Skill) string {
	d := strings.TrimSpace(s.Description)
	if d == "" {
		d = "(no description)"
	}
	return d + "\n\n(Full skill omitted to fit the context window; read the skill if a call turns on its detail.)"
}

// estTokens approximates a string's token cost as len/4 — the same chars/token
// heuristic the agent loop's transcript estimator uses, so the skill-budget math
// is consistent with the budget the compactor enforces.
func estTokens(s string) int { return len(s) / 4 }

// terseBody renders the load-bearing slice of a skill for a narrow-window worker:
// the skill's authored `## Core` section when present (the tier-1 core), else its
// one-line description followed by an outline of its section headings. It names
// what the skill governs and signals that the full text was elided, without
// spending the window on the whole discipline.
func terseBody(s Skill) string {
	if core := coreSection(s.Body); core != "" {
		return core
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(s.Description))
	if heads := headings(s.Body); len(heads) > 0 {
		b.WriteString("\n\nCovers: ")
		b.WriteString(strings.Join(heads, "; "))
		b.WriteString(".")
	}
	b.WriteString("\n\n(Full discipline text omitted to fit the context window; read the skill if a call turns on its detail.)")
	return b.String()
}

// coreSection returns the body content under a top-level `## Core` heading — the
// tier-1 terse core a skill may author — trimmed, or "" if absent. Extraction
// stops at the next heading at level 2 or shallower.
func coreSection(body string) string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "## ") && strings.EqualFold(strings.TrimSpace(t[len("## "):]), "Core") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	var out []string
	for _, ln := range lines[start:] {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "# ") {
			break
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// headings lists a body's level-2 (`## `) section titles, in document order.
func headings(body string) []string {
	var hs []string
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "## ") {
			hs = append(hs, strings.TrimSpace(t[len("## "):]))
		}
	}
	return hs
}

// Digest is a stable content digest (name+body) of the loaded set, for provenance.
func Digest(skills []Skill) string {
	sorted := append([]Skill(nil), skills...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	for _, s := range sorted {
		h.Write([]byte(s.Name))
		h.Write([]byte{0})
		h.Write([]byte(s.Body))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// readSkill parses frontmatter + body from a skill file.
func readSkill(name, path string, raw []byte) Skill {
	meta, body := splitFrontmatter(string(raw))
	desc := meta["description"]
	if desc == "" {
		desc = firstHeading(body)
	}
	if desc == "" {
		desc = name
	}
	skillName := name
	if n := meta["name"]; n != "" {
		skillName = n
	}
	return Skill{Name: skillName, Path: path, Description: desc, Body: body}
}

// splitFrontmatter strips a leading --- YAML block (scalar key:value only; no
// YAML dependency) and returns the metadata plus the remaining body.
func splitFrontmatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	if !strings.HasPrefix(text, "---") {
		return meta, text
	}
	lines := strings.Split(text, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return meta, text
	}
	for _, line := range lines[1:end] {
		if line != "" && (line[0] == ' ' || line[0] == '\t') {
			continue // skip nested / list-continuation lines
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return meta, strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\n")
}

// firstHeading returns the first markdown heading (or first non-empty line).
func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "#") {
			return strings.TrimSpace(strings.TrimLeft(s, "# "))
		}
		if s != "" {
			return s
		}
	}
	return ""
}
