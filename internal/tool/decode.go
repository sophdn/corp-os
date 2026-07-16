package tool

import (
	"encoding/json"
	"strings"
)

// DecodeValue re-decodes a dispatch Result.Value (a decoded any — typically a map[string]any
// from a JSON response) into the typed struct T, by re-marshaling and unmarshaling. It is the
// one shared definition of the marshal→unmarshal "read the dispatch Value as a typed struct"
// helper that recurred verbatim across callers (arcreview, memory). ok is false when either
// step errors, so a caller falls back to its untyped path rather than acting on a zero value.
func DecodeValue[T any](v any) (T, bool) {
	var out T
	raw, err := json.Marshal(v)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, false
	}
	return out, true
}

// CommandString extracts a sys.exec "command" argv as a single shell string, salvaging the
// common weak-model malformation of passing the argv as a LIST of tokens
// (["go","test","./..."]) instead of one shell string (bug 1113). A string passes through;
// a []any / []string of strings is space-joined; anything else (a non-string element, a
// number, nil) yields "" so the caller still reports the actionable "missing command" error.
// Both the risk gate (pre-dispatch approval) and the sysorgan exec handler call it, so a
// list-shaped command is salvaged identically on both sides instead of being rejected.
func CommandString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return joinArgv(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return "" // a non-string element is not a salvageable argv
			}
			parts = append(parts, s)
		}
		return joinArgv(parts)
	default:
		return ""
	}
}

// joinArgv joins argv tokens into one space-separated shell string, dropping empty tokens so
// a ["go","test",""] does not yield a trailing space the allow-list check would trip on.
func joinArgv(parts []string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}
