// Package fsorgan is Corp-OS's owned, host-native filesystem organ. It
// implements tool.Provider for the "fs" surface — read / write / edit / move /
// remove / ls / grep / glob — operating directly on host paths in corpos's own
// process, with NO hop through a containerized toolkit-server.
//
// Why host-native: when the toolkit-server is containerized it mounts only a
// few host roots, so fs calls routed through it run in the CONTAINER's
// namespace. A write to an unmounted host path then "succeeds" into the
// container's ephemeral filesystem — invisible to corpos's own host-side verify
// gate, producing fake progress. Running the fs organ in corpos's process means
// corpos's fs and its host verification share ONE namespace, so a write the
// verify gate can never observe cannot return ok.
//
// The behavioral contract mirrors the toolkit's fs surface (the contract corpos
// already dispatches), so this organ is a drop-in: same action / params /
// result shapes. read/write/edit are coupled through an in-process read-state
// registry — write and edit require a prior full read of the target path.
//
// The organ is sans-IO testable: filesystem work runs against temp dirs, and
// the latency clock is injectable.
package fsorgan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"corpos/internal/tool"
)

// Surface is the surface name the model addresses. It deliberately matches the
// toolkit's fs surface so the organ is a drop-in replacement.
const Surface = "fs"

// Provider is the host-native fs organ. It carries the read-state registry that
// couples read→write/edit. The zero value is not ready; use New.
type Provider struct {
	reads *readRegistry
	// now is the injectable latency clock (defaults to time.Now).
	now func() time.Time
	// renameFn is the injectable rename seam (defaults to os.Rename) so the
	// cross-device (EXDEV) fallback of fs.move is exercisable without a real
	// second filesystem.
	renameFn func(oldpath, newpath string) error
}

var _ tool.Provider = (*Provider)(nil)

// New builds a ready fs organ with an empty read-state registry.
func New() *Provider {
	return &Provider{reads: newReadRegistry(), now: time.Now, renameFn: os.Rename}
}

// Specs advertises the fs surface to the model as one envelope tool. The single
// spec matches the toolkit's fs surface shape ({action, params, rationale}).
func (p *Provider) Specs() []tool.Spec {
	return []tool.Spec{{
		Name:        Surface,
		Description: "Owned host-native filesystem: read, write, edit, move, remove, ls (directory listing), grep (regex content search), glob (file pattern match). Operates directly on host paths.",
		InputSchema: envelopeSchema(),
	}}
}

// envelopeSchema is the {action, params, rationale} argument schema shared by
// the toolkit-server surfaces; the fs organ offers the same shape so the model
// addresses it identically.
func envelopeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			// The enum lists the surface's actions (sorted) so a job-profile can
			// action-scope this surface (mcp.Project fails closed on an enum-less
			// "thin" spec). Keep in sync with the Dispatch switch.
			"action": map[string]any{
				"type":        "string",
				"description": "the fs action",
				"enum":        []any{"edit", "glob", "grep", "ls", "move", "read", "remove", "write"},
			},
			"params":    map[string]any{"type": "object", "description": "action parameters"},
			"rationale": map[string]any{"type": "string", "description": "required for write actions"},
		},
		"required": []any{"action"},
	}
}

// Dispatch routes one fs call to the action handler and classifies the outcome.
// Per the Provider contract it never returns a Go error out of band: a failure
// is reported as a Result whose Value carries {"error": msg}, exactly the shape
// the MCP path produced, so the loop folds it back into the transcript.
//
// An fs failure is classified ClassUsage, not ClassTool: the fs organ is
// deterministic, so every failure is the worker's own malformed/mistargeted call
// (bad or missing params, a wrong path, a read-before-edit precondition, a no-op
// edit) which it fixes itself next round from the fed-back error. Climbing to a
// costlier MODEL never fixes a deterministic fs error, so these must not trip the
// repeated_tool_error escalation; a floor that thrashes fs usage errors without
// converging is still caught by the (escalatable) no-progress breaker.
func (p *Provider) Dispatch(ctx context.Context, c tool.Call) tool.Result {
	start := p.now()
	root := rootFromContext(ctx)
	c.Params = remapWorktreePaths(root, c.Params)
	val, err := p.handle(root, c)
	latency := p.now().Sub(start).Milliseconds()
	if err != nil {
		return tool.Result{
			Call:       c,
			OK:         false,
			Value:      map[string]any{"error": err.Error()},
			ErrorClass: tool.ClassUsage,
			LatencyMS:  latency,
		}
	}
	return tool.Result{
		Call:       c,
		OK:         true,
		Value:      val,
		ErrorClass: tool.ClassNone,
		LatencyMS:  latency,
	}
}

// remapWorktreePaths rewrites path-like params that reference a file by its source-repo
// location instead of a path relative to the worktree the worker is sandboxed to, so a
// coding worker still lands its read/edit when it (or the bug report it was handed) names
// the file by an absolute source path ("/tmp/proj/go/x.go") or the same with the leading
// slash dropped ("tmp/proj/go/x.go" — which otherwise joins UNDER the worktree into a
// doubled, nonexistent path). It rewrites a path ONLY when the worker's own path fails to
// resolve or misses AND the path uniquely recovers to an existing worktree entry (see
// recoverWorktreePath); a correct path, a genuinely-new file, or an ambiguous tail is left
// untouched. No-op when unsandboxed (root == "") — direct CLI use resolves against the CWD
// exactly as before. The params map is copied only if a value actually changes.
func remapWorktreePaths(root string, params map[string]any) map[string]any {
	if root == "" || len(params) == 0 {
		return params
	}
	out := params
	copied := false
	for _, key := range []string{"file_path", "path", "source", "dest"} {
		raw, ok := params[key].(string)
		if !ok || raw == "" {
			continue
		}
		// Leave a path that already resolves to an existing entry — it is correct.
		if resolved, err := resolveWithin(root, raw); err == nil {
			if _, statErr := os.Stat(resolved); statErr == nil {
				continue
			}
		}
		recovered, ok := recoverWorktreePath(root, raw)
		if !ok || recovered == raw {
			continue
		}
		if !copied { // copy-on-first-write so the caller's map is never mutated
			out = make(map[string]any, len(params))
			for k, v := range params {
				out[k] = v
			}
			copied = true
		}
		out[key] = recovered
	}
	return out
}

// handle dispatches by action, returning the result as a generic map (matching
// the MCP-decoded JSON shape) or an error to be wrapped into a tool_error. root
// is the per-call sandbox root (empty = unsandboxed); every path-taking handler
// resolves its caller-supplied path under it (resolveWithin) so a worker cannot
// touch the filesystem outside its worktree (bug 1081).
func (p *Provider) handle(root string, c tool.Call) (map[string]any, error) {
	switch c.Action {
	case "read":
		return p.handleRead(root, c.Params)
	case "write":
		return p.handleWrite(root, c.Params)
	case "edit":
		return p.handleEdit(root, c.Params)
	case "move":
		return p.handleMove(root, c.Params)
	case "remove":
		return p.handleRemove(root, c.Params)
	case "ls":
		return p.handleLS(root, c.Params)
	case "glob":
		return p.handleGlob(root, c.Params)
	case "grep":
		return p.handleGrep(root, c.Params)
	default:
		return nil, fmt.Errorf("fs.%s: unknown action", c.Action)
	}
}

// asResult marshals a typed result struct to the generic map shape the model
// sees (the MCP path JSON-decodes the toolkit reply into a map; the native
// organ produces the identical shape by round-tripping through JSON, honoring
// each field's omitempty so optional fields appear only when set).
func asResult(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// decodeParams re-decodes the generic params map into a typed param struct,
// routing through each struct's json tags (and any alias UnmarshalJSON).
func decodeParams(params map[string]any, dst any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}
