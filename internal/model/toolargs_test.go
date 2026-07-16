package model

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSalvageToolArgs(t *testing.T) {
	cases := []struct {
		name, raw, want string
		ok              bool
	}{
		{
			name: "single-quoted python-repr object",
			raw:  `{'action': 'grep', 'params': {'pattern': 'resolveChainID'}, 'rationale': ''}`,
			want: `{"action": "grep", "params": {"pattern": "resolveChainID"}, "rationale": ""}`,
			ok:   true,
		},
		{
			name: "wrapped in native function-call markup",
			raw:  "<｜DSML｜function_calls>\n{\"action\":\"read\",\"params\":{\"path\":\"a.go\"}}\n</｜DSML｜function_calls>",
			want: `{"action":"read","params":{"path":"a.go"}}`,
			ok:   true,
		},
		{
			name: "single-quoted AND markup-wrapped",
			raw:  `garble [{'action': 'ls', 'params': {}}] </｜DSML｜function_calls>`,
			want: `{"action": "ls", "params": {}}`,
			ok:   true,
		},
		{
			name: "already valid (idempotent)",
			raw:  `{"action":"write","params":{"path":"x"}}`,
			want: `{"action":"write","params":{"path":"x"}}`,
			ok:   true,
		},
		{
			name: "truncated object is not salvageable",
			raw:  `{'action': 'grep', 'params': {'pattern':`,
			ok:   false,
		},
		{
			name: "no object at all",
			raw:  `let me call grep on the file`,
			ok:   false,
		},
		{
			// A double-quote present means single→double conversion is ambiguous, so it is
			// NOT attempted; this stays malformed rather than risk corrupting a real string.
			name: "mixed quotes not converted",
			raw:  `{'action': 'grep', 'note': "it's mixed"}`,
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := salvageToolArgs(c.raw)
			if ok != c.ok {
				t.Fatalf("salvageToolArgs ok = %v, want %v (got %q)", ok, c.ok, got)
			}
			if ok && string(got) != c.want {
				t.Fatalf("salvaged = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFirstBalancedJSONObject_RespectsStrings(t *testing.T) {
	// A brace inside a double-quoted string must not skew the depth.
	got := firstBalancedJSONObject(`prefix {"k":"a}b","n":{"x":1}} trailing`)
	if got != `{"k":"a}b","n":{"x":1}}` {
		t.Fatalf("balanced object = %q", got)
	}
	if got := firstBalancedJSONObject("no braces here"); got != "" {
		t.Fatalf("want empty for no object, got %q", got)
	}
}

// toToolCalls salvages a single-quoted arguments object instead of failing the call.
func TestToToolCalls_SalvagesSingleQuotedArgs(t *testing.T) {
	calls, err := toToolCalls([]oacToolCall{{
		ID:       "c1",
		Function: oacFunctionCall{Name: "fs", Arguments: `{'action': 'grep', 'params': {'pattern': 'X'}}`},
	}})
	if err != nil {
		t.Fatalf("salvageable args must not error, got %v", err)
	}
	if len(calls) != 1 || calls[0].Surface != "fs" || calls[0].Action != "grep" {
		t.Fatalf("call not recovered: %+v", calls)
	}
	if calls[0].Params["pattern"] != "X" {
		t.Fatalf("params not recovered: %+v", calls[0].Params)
	}
}

// A genuinely unsalvageable (truncated) args blob still surfaces ErrMalformedToolCall,
// preserving the loop's reprompt+escalate backstop.
func TestToToolCalls_TruncatedArgsStillMalformed(t *testing.T) {
	_, err := toToolCalls([]oacToolCall{{
		ID:       "c1",
		Function: oacFunctionCall{Name: "fs", Arguments: `{"action":"grep","params":{`},
	}})
	if !errors.Is(err, ErrMalformedToolCall) {
		t.Fatalf("truncated args must stay malformed, got %v", err)
	}
}

// TestToToolCalls_ParamsAsEmptyString recovers the dominant deepseek-v3.2 malformation
// (captured live): params sent as an empty STRING instead of an object. The JSON is
// valid, so salvage never fired — it classified as FaultMalformedToolCall and escalated
// the coder rung to Opus. It must now decode to a clean call with no params.
func TestToToolCalls_ParamsAsEmptyString(t *testing.T) {
	calls, err := toToolCalls([]oacToolCall{{
		ID:       "c1",
		Function: oacFunctionCall{Name: "fs", Arguments: `{"action":"read","params":"","rationale":"x"}`},
	}})
	if err != nil {
		t.Fatalf("params:\"\" must recover, got %v", err)
	}
	if len(calls) != 1 || calls[0].Action != "read" || calls[0].Params != nil {
		t.Fatalf("empty-string params should decode to a no-params call: %+v", calls)
	}
}

// TestToToolCalls_ParamsAsJSONString recovers the double-encoded form: params sent as a
// STRING whose contents are the params object as JSON text. Unwrap one level.
func TestToToolCalls_ParamsAsJSONString(t *testing.T) {
	calls, err := toToolCalls([]oacToolCall{{
		ID:       "c1",
		Function: oacFunctionCall{Name: "fs", Arguments: `{"action":"grep","params":"{\"pattern\":\"TestAcc\",\"show_line_numbers\":true}"}`},
	}})
	if err != nil {
		t.Fatalf("double-encoded params must recover, got %v", err)
	}
	if len(calls) != 1 || calls[0].Params["pattern"] != "TestAcc" || calls[0].Params["show_line_numbers"] != true {
		t.Fatalf("double-encoded params not unwrapped: %+v", calls)
	}
}

// A params string that is neither empty nor a JSON object stays malformed (backstop).
func TestToToolCalls_ParamsAsGarbageStringStillMalformed(t *testing.T) {
	_, err := toToolCalls([]oacToolCall{{
		ID:       "c1",
		Function: oacFunctionCall{Name: "fs", Arguments: `{"action":"read","params":"not json at all"}`},
	}})
	if !errors.Is(err, ErrMalformedToolCall) {
		t.Fatalf("a non-object non-empty params string must stay malformed, got %v", err)
	}
}

func TestDebugLogMalformedArgs(t *testing.T) {
	// Unset env → no-op (no panic, nothing written).
	t.Setenv("CORPOS_DEBUG_TOOLARGS", "")
	debugLogMalformedArgs("fs", `{"bad"`, "err")
	// Set env → appends a capture record to the file.
	path := t.TempDir() + "/toolargs.log"
	t.Setenv("CORPOS_DEBUG_TOOLARGS", path)
	debugLogMalformedArgs("fs", `{"action":"read","params":""}`, "cannot unmarshal string")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("capture file not written: %v", err)
	}
	if s := string(b); !strings.Contains(s, "tool=fs") || !strings.Contains(s, `"params":""`) {
		t.Fatalf("capture record missing content: %q", s)
	}
}

func TestDecodeParamsField(t *testing.T) {
	obj := func(s string) json.RawMessage { return json.RawMessage(s) }
	// recovers to a map with pattern=X
	for _, in := range []string{`{"pattern":"X"}`, `"{\"pattern\":\"X\"}"`} {
		m, err := decodeParamsField(obj(in))
		if err != nil || m["pattern"] != "X" {
			t.Errorf("decodeParamsField(%s) = %v,%v; want pattern=X", in, m, err)
		}
	}
	// recovers to nil (no params)
	for _, in := range []string{``, `null`, `""`, `"   "`} {
		m, err := decodeParamsField(obj(in))
		if err != nil || m != nil {
			t.Errorf("decodeParamsField(%q) = %v,%v; want nil,nil", in, m, err)
		}
	}
	// genuine malformations error
	for _, in := range []string{`"nope"`, `42`, `[1,2]`} {
		if _, err := decodeParamsField(obj(in)); err == nil {
			t.Errorf("decodeParamsField(%s) should error", in)
		}
	}
}

// Well-formed args are decoded directly (salvage never runs on a strict-parse success).
func TestToToolCalls_WellFormedArgsUnchanged(t *testing.T) {
	calls, err := toToolCalls([]oacToolCall{{
		ID:       "c1",
		Function: oacFunctionCall{Name: "work", Arguments: `{"action":"task_list","params":{"chain":"c"}}`},
	}})
	if err != nil {
		t.Fatalf("well-formed args must not error: %v", err)
	}
	if len(calls) != 1 || calls[0].Action != "task_list" || calls[0].Params["chain"] != "c" {
		t.Fatalf("well-formed call wrong: %+v", calls)
	}
}
