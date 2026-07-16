package model

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// decodeParamsField decodes a tool call's `params` field into the params map,
// tolerating the dominant deepseek-v3.2 malformation where params arrives as a
// STRING instead of an object (captured live: `"params": ""` and
// `"params": "{\"pattern\":...}"`). The JSON is VALID, so the arg salvage never fires
// — it is a type mismatch (`cannot unmarshal string into map`) that classified as
// FaultMalformedToolCall and escalated the coder rung straight to Opus. Handling:
//   - object            → decode directly (the normal path)
//   - absent / null     → nil (no params)
//   - "" / whitespace   → nil (the model meant "no params")
//   - "{...}" JSON text  → unwrap one level (params double-encoded as a string)
//   - any other string  → a genuine malformation (error, so the reprompt/escalate backstop stays)
func decodeParamsField(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		return m, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err // neither an object nor a string — genuinely malformed
	}
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(s), &inner); err != nil {
		return nil, fmt.Errorf("params arrived as a string that is not a JSON object: %w", err)
	}
	return inner, nil
}

// debugLogMalformedArgs appends a malformed tool-call payload to the file named by
// CORPOS_DEBUG_TOOLARGS, for capturing ground-truth weak-model malformations during a
// live run. A no-op when the env var is unset. Best-effort — a write error is ignored.
func debugLogMalformedArgs(name, raw, errMsg string) {
	path := os.Getenv("CORPOS_DEBUG_TOOLARGS")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString("=== MALFORMED tool=" + name + " err=" + errMsg + " ===\n" + raw + "\n<<<END>>>\n")
}

// Tool-argument salvage: a weak / non-OpenAI-native model (DeepSeek, local Qwen) often
// emits a tool call's `arguments` in a form the strict JSON decoder rejects — wrapped in
// the model's own function-call markup, fenced, or as a Python-repr single-quoted object.
// The strict failure costs the loop a bounded reprompt cycle then a frontier escalation
// (FaultMalformedToolCall → parse_failure), even though the intent is usually recoverable.
// salvageToolArgs makes ONE conservative repair pass before the call is declared malformed
// (rehearsal Run-33: deepseek-v3.2 leaked a `…function_calls>` single-quoted blob → wasted
// escalation to Opus). It runs ONLY after a strict parse has already failed, so a
// well-formed call is never touched.

// salvageToolArgs attempts to recover a JSON arguments object from raw when the strict
// decoder rejected it. It returns the recovered JSON bytes and ok=true only when the
// result is a valid JSON object; otherwise ok=false and the caller keeps treating the
// call as malformed (the reprompt + escalate backstop). Two bounded repairs, in order:
//  1. extract the first brace-balanced {…} object, dropping any native-format wrapper /
//     markup / fenced fences / trailing junk around it;
//  2. if that still isn't valid JSON AND the candidate contains no double-quote (so single
//     quotes are unambiguously the string delimiters and there is no double-quoted value to
//     corrupt), convert single quotes to double and retry.
func salvageToolArgs(raw string) ([]byte, bool) {
	cand := firstBalancedJSONObject(raw)
	if cand == "" {
		return nil, false
	}
	if json.Valid([]byte(cand)) {
		return []byte(cand), true
	}
	// Python-repr style: single-quoted object with NO double quotes anywhere — the only
	// case where blind '→" conversion is unambiguous and cannot corrupt a real string.
	if !strings.Contains(cand, `"`) {
		if converted := strings.ReplaceAll(cand, "'", `"`); json.Valid([]byte(converted)) {
			return []byte(converted), true
		}
	}
	return nil, false
}

// firstBalancedJSONObject returns the first brace-balanced {…} run in s, honoring
// double-quoted JSON string quoting so a brace inside a string does not skew the depth.
// Returns "" when there is no balanced object. It strips any wrapper/markup around the
// object (the leading `<…function_calls>` / fence and the trailing close tag).
func firstBalancedJSONObject(s string) string {
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
					return s[start : i+1]
				}
			}
		}
	}
	return ""
}
