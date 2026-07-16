package model

import "corpos/internal/tool"

// toolEnvelope renders a tool call as the {action, params, rationale} object the
// toolkit-server surfaces expect. Both adapters share it: OpenAICompat marshals
// it to the function-call arguments string; Anthropic sends it as the tool_use
// input object. Provider-shared logic stays here, not duplicated per adapter.
func toolEnvelope(tc tool.Call) map[string]any {
	env := map[string]any{"action": tc.Action}
	if tc.Params != nil {
		env["params"] = tc.Params
	}
	if tc.Rationale != "" {
		env["rationale"] = tc.Rationale
	}
	return env
}
