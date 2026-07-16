package agent

import (
	"encoding/json"
	"strings"

	"corpos/internal/tool"
)

// A small local model often WRITES a tool call as JSON in its message body — a
// fenced ```json block or a bare {"name": …, "arguments": {…}} object — instead of
// emitting a real structured tool call. The call then does nothing ("narration is
// not execution"), the turn looks like a content-only answer, and the run stalls
// (rehearsal runs 15–17: Qwen narrated fs.edit/sys.exec repeatedly). recoverNarrated
// parses that text back into a real dispatch so the worker's intent is executed,
// not discarded — a forgiving adapter for models with unreliable tool-call emission.

// narratedCall is the OpenAI-style function-call shape a model writes when it
// narrates: {"name": "<surface>", "arguments": {"action", "params", "rationale"}}.
type narratedCall struct {
	Name      string       `json:"name"`
	Arguments narratedArgs `json:"arguments"`
}

type narratedArgs struct {
	Action    string         `json:"action"`
	Params    map[string]any `json:"params"`
	Rationale string         `json:"rationale"`
}

// recoverNarrated extracts the FIRST tool call a model narrated as JSON in text,
// or nil when none is present. It is conservative — it fires only when the parsed
// object has the exact {name, arguments{action}} tool-call shape AND name is an
// offered surface — so prose that merely contains JSON is not misread as a call.
// Only the first call is recovered: the loop dispatches it, the model sees the
// result, and continues — mirroring normal one-step tool use rather than blindly
// firing a narrated batch against stale state.
func recoverNarrated(text string, validSurfaces map[string]bool) []tool.Call {
	for _, blob := range jsonObjectCandidates(text) {
		var nc narratedCall
		if err := json.Unmarshal([]byte(blob), &nc); err != nil {
			continue
		}
		if nc.Name == "" || nc.Arguments.Action == "" {
			continue
		}
		if len(validSurfaces) > 0 && !validSurfaces[nc.Name] {
			continue // not an offered surface — don't dispatch into the void
		}
		return []tool.Call{{
			Surface:   nc.Name,
			Action:    nc.Arguments.Action,
			Params:    nc.Arguments.Params,
			Rationale: nc.Arguments.Rationale,
		}}
	}
	return nil
}

// jsonObjectCandidates yields candidate JSON object substrings from text, in
// document order: the contents of ```json (or bare ```) fenced blocks first (the
// common narration shape), then any top-level brace-balanced {…} runs. Each is a
// best-effort slice handed to json.Unmarshal, which rejects non-objects.
func jsonObjectCandidates(text string) []string {
	var out []string
	for _, fenced := range fencedBlocks(text) {
		if b := firstBalancedObject(fenced); b != "" {
			out = append(out, b)
		}
	}
	// Also scan the whole text for balanced objects, so an unfenced narration is
	// still recovered. Dedup is unnecessary — Unmarshal of a repeat is cheap and
	// recoverNarrated returns on the first valid call.
	for _, b := range allBalancedObjects(text) {
		out = append(out, b)
	}
	return out
}

// fencedBlocks returns the inner text of each ``` … ``` fenced block (the language
// tag, e.g. "json", is stripped from the opening fence line).
func fencedBlocks(text string) []string {
	var blocks []string
	parts := strings.Split(text, "```")
	// Odd indices are inside fences (parts[0] is before the first fence).
	for i := 1; i < len(parts); i += 2 {
		inner := parts[i]
		if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
			// Drop the language tag on the opening fence line (```json\n…).
			if tag := strings.TrimSpace(inner[:nl]); tag == "" || isLangTag(tag) {
				inner = inner[nl+1:]
			}
		}
		blocks = append(blocks, inner)
	}
	return blocks
}

// isLangTag reports whether s looks like a fenced-code language tag (a single
// short word) rather than content — so only a tag line is stripped.
func isLangTag(s string) bool {
	if s == "" || len(s) > 12 || strings.ContainsAny(s, " \t{}\"") {
		return false
	}
	return true
}

// firstBalancedObject returns the first brace-balanced {…} run in s, or "".
func firstBalancedObject(s string) string {
	objs := balancedObjects(s, true)
	if len(objs) == 0 {
		return ""
	}
	return objs[0]
}

// allBalancedObjects returns every top-level brace-balanced {…} run in s.
func allBalancedObjects(s string) []string { return balancedObjects(s, false) }

// balancedObjects scans s for brace-balanced {…} runs at the top nesting level,
// honoring JSON string quoting (so a { or } inside a string does not skew the
// depth). When first is true it stops after the first complete object.
func balancedObjects(s string, first bool) []string {
	var out []string
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
					start = -1
					if first {
						return out
					}
				}
			}
		}
	}
	return out
}
