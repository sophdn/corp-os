package mcp

import "corpos/internal/tool"

// abSurfaces are the toolkit-server surfaces the Phase-A/B loop exposes to the
// model, each as one tool taking an {action, params, rationale} envelope. fs is
// read/write/edit only (T2); project scope is added by the client, not the model.
//
// ml is deliberately NOT here: it is in no current job-profile's envelope and
// corpos owns its own model layer, so exposing it flat to every agent is pure
// schema tax (§4.4.1 — the one clean GLOBAL prune). sys, by contrast, IS exposed
// here and scoped PER-PROFILE (the coding/git profiles need sys.exec; the
// lifecycle/file-sort/doc-filing profiles deny it) — projection, not a global
// toggle, is what denies it where it doesn't belong.
var abSurfaces = []struct{ name, description string }{
	{"work", "Chains, tasks, bugs, roadmap, and forge on the toolkit-server work ledger."},
	{"knowledge", "Vault, kiwix, library, and parse_context on the knowledge surface."},
	{"fs", "Owned filesystem: read, write, edit, grep (regex content search), glob (file pattern match), ls (directory listing)."},
	{"measure", "Benchmarks (record/query/replay) and the Qwen-local rubric-classify family — severity, proportionality, routing-trigger, and other classifiers."},
	{"sys", "Owned system surface. Read-only introspection (ungated): ps (processes), ports (listening sockets + owning pid), units (systemd-user units), containers (podman+docker). Plus exec (gated: allowlisted command + rationale) to run shell commands."},
}

// envelopeSchema is the JSON Schema for a surface tool's arguments: the
// {action, params, rationale} envelope mirroring the MCP dispatch shape.
func envelopeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":    map[string]any{"type": "string", "description": "the action to dispatch on this surface"},
			"params":    map[string]any{"type": "object", "description": "action parameters"},
			"rationale": map[string]any{"type": "string", "description": "required for write actions"},
		},
		"required": []any{"action"},
	}
}

// thinSpec is the static, un-enriched tool spec for a surface — the envelope schema with no
// per-action detail. It is the SINGLE thin-spec definition (EnrichedSpecs' fallback is the one
// caller); the former DefaultSpecs duplicated this shape and is gone (chain 379 task 3).
func thinSpec(name, description string) tool.Spec {
	return tool.Spec{
		Name:        name,
		Description: description,
		InputSchema: envelopeSchema(),
	}
}
