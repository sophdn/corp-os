// Package sysorgan is Corp-OS's owned, host-native system surface. It implements
// tool.Provider for the "sys" surface, running the EXEC action in corpos's own
// process on the host — capturing combined output, exit code, and timing — and
// DELEGATING the read-only introspection actions (ps / ports / units /
// containers) to the toolkit provider.
//
// Why host-native exec: the deployed toolkit-server is a distroless image with no
// /bin/sh, so its sys.exec is a policy/availability dead-end — corpos could not
// run the shell-shaped work (build / test / git / discovery) that real agentic
// coding needs (SWAP_VALIDATION §0 exec gate). Running exec in corpos's process
// gives it a real shell + the host toolchain, captured with the same contract the
// model already calls. The introspection actions are unchanged: they forward to
// the toolkit so the "sys" surface stays whole.
//
// exec is gated: the rationale half by dispatch policy upstream, the allowlist
// half here (shells/eval excluded, command substitution rejected). Only
// sandbox=none is implemented; bwrap/podman are rejected with a clear error
// rather than silently ignored (a tracked upgrade).
package sysorgan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"corpos/internal/tool"
)

// Surface is the surface name the model addresses. It matches the toolkit's sys
// surface so the organ is a drop-in owner.
const Surface = "sys"

// introspectionActions are the read-only actions delegated to the toolkit.
var introspectionActions = map[string]bool{
	"ps": true, "ports": true, "units": true, "containers": true,
}

// Provider is the host-native sys organ: a native exec runner + allowlist, plus a
// delegate provider for introspection. The zero value is not ready; use New.
type Provider struct {
	runner   *runner
	allow    commandAllowlist
	delegate tool.Provider // toolkit provider for ps/ports/units/containers
	now      func() time.Time
}

var _ tool.Provider = (*Provider)(nil)

// New builds a ready sys organ. delegate handles the read-only introspection
// actions (typically the toolkit mcp client); it may be nil, in which case those
// actions return a clear error. If the exec runner cannot be constructed (no
// resolvable working directory), exec returns an error per call but the organ
// still delegates introspection.
func New(delegate tool.Provider) *Provider {
	r, _ := newRunner("") // a nil runner surfaces as a per-call exec error
	return &Provider{
		runner:   r,
		allow:    loadAllowlistFromEnv(),
		delegate: delegate,
		now:      time.Now,
	}
}

// Specs advertises the sys surface to the model as one envelope tool.
func (p *Provider) Specs() []tool.Spec {
	return []tool.Spec{{
		Name:        Surface,
		Description: "Owned system surface. exec: run an allowlisted shell command host-native (captured output + exit code + timing; cd persists across calls). go_doc: resolve a Go symbol's real signature + doc via `go doc` (params.symbol, e.g. \"corpos/internal/tool.Spec\", \"session.Create\", or \"bytes.Buffer.Write\") — ground an unfamiliar API instead of guessing its signature. Read-only introspection: ps (processes), ports (listening sockets), units (systemd-user units), containers (podman/docker).",
		InputSchema: envelopeSchema(),
	}}
}

func envelopeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			// The enum lists the surface's actions (sorted) so a job-profile can
			// action-scope this surface (mcp.Project fails closed on an enum-less
			// "thin" spec). exec is native; ps/ports/units/containers delegate.
			"action": map[string]any{
				"type":        "string",
				"description": "the sys action",
				"enum":        []any{"containers", "exec", "go_doc", "ports", "ps", "units"},
			},
			"params":    map[string]any{"type": "object", "description": "action parameters"},
			"rationale": map[string]any{"type": "string", "description": "required for exec"},
		},
		"required": []any{"action"},
	}
}

// Dispatch routes one sys call: exec runs host-native; the introspection actions
// forward to the delegate. Failures are reported as a tool_error Result (never
// out of band), matching the MCP path.
func (p *Provider) Dispatch(ctx context.Context, c tool.Call) tool.Result {
	if introspectionActions[c.Action] {
		if p.delegate == nil {
			return failResult(c, fmt.Sprintf("sys.%s: no toolkit provider is mounted to serve introspection", c.Action), 0)
		}
		return p.delegate.Dispatch(ctx, c)
	}

	start := p.now()
	val, err := p.handle(ctx, c)
	latency := p.now().Sub(start).Milliseconds()
	if err != nil {
		// A sys-organ error is a worker-recoverable usage error (a malformed call:
		// a missing `command` param, a bad cwd), not a model-capability fault — the
		// worker fixes it from the fed-back message next round. Climbing to a costlier
		// MODEL never fixes a malformed sys call, so it must NOT trip the
		// repeated_tool_error escalation; the no-progress breaker still backstops a
		// floor that thrashes without converging. Parallels the fs organ (bug
		// escalate-after-1-…; sys.exec param slips were forcing Coder→Opus escalation).
		// NB: a command that RUNS and exits non-zero is OK=true with exit_code in the
		// payload — only a malformed/undispatchable call reaches here.
		return tool.Result{Call: c, OK: false, Value: map[string]any{"error": err.Error()}, ErrorClass: tool.ClassUsage, LatencyMS: latency}
	}
	return tool.Result{Call: c, OK: true, Value: val, ErrorClass: tool.ClassNone, LatencyMS: latency}
}

func (p *Provider) handle(ctx context.Context, c tool.Call) (map[string]any, error) {
	switch c.Action {
	case "exec":
		return p.handleExec(ctx, c.Params)
	case "go_doc":
		return p.handleGoDoc(ctx, c.Params)
	default:
		return nil, fmt.Errorf("sys.%s: unknown action", c.Action)
	}
}

// goDocParams is the typed param struct for sys.go_doc.
type goDocParams struct {
	Symbol string `json:"symbol"`
	Cwd    string `json:"cwd,omitempty"`
}

// goDocTimeoutMS bounds a go-doc lookup. Loading a package's docs is fast; a longer run
// means a dependency is building or something is wrong, so fail fast rather than hang.
const goDocTimeoutMS = 30_000

// handleGoDoc resolves a Go symbol's real signature + documentation via `go doc`, so the
// coding worker can GROUND an unfamiliar API instead of guessing it (bug 1090: a worker
// hallucinated session.NewStore / tool.Spec.Actions / agent.WithSurface and shipped five
// non-compiling guesses with no type-aware tool to resolve them). It is a BOUNDED lookup —
// only `go doc`, only a symbol-shaped argument (no flags, no shell metacharacters), gated
// through the same exec allowlist for defense in depth — not a general exec.
func (p *Provider) handleGoDoc(ctx context.Context, params map[string]any) (map[string]any, error) {
	var gp goDocParams
	if err := decodeParams(params, &gp); err != nil {
		return nil, fmt.Errorf("sys.go_doc: invalid params: %w", err)
	}
	if p.runner == nil {
		return nil, errors.New("sys.go_doc: runner unavailable (no resolvable working directory)")
	}
	sym := strings.TrimSpace(gp.Symbol)
	if sym == "" {
		return nil, errors.New(`sys.go_doc requires symbol (e.g. "corpos/internal/tool.Spec", "session.Create", or "bytes.Buffer.Write")`)
	}
	if err := validateGoDocSymbol(sym); err != nil {
		return nil, fmt.Errorf("sys.go_doc: %w", err)
	}
	command := "go doc " + sym
	if err := p.allow.permit(command); err != nil { // defense in depth; the head is `go`
		return nil, fmt.Errorf("sys.go_doc: %w", err)
	}
	res, err := p.runner.run(ctx, command, runOptions{Cwd: gp.Cwd, TimeoutMS: goDocTimeoutMS})
	if err != nil {
		return nil, err
	}
	return asResult(res)
}

// validateGoDocSymbol restricts the argument to a Go package path and/or symbol so the
// bounded lookup cannot become arbitrary `go doc` flag use or shell injection: letters,
// digits, and . / _ - only, at most two space-separated tokens (an optional package path
// and a symbol), neither a flag.
func validateGoDocSymbol(sym string) error {
	if len(sym) > 200 {
		return errors.New("symbol is too long")
	}
	fields := strings.Fields(sym)
	if len(fields) > 2 {
		return errors.New("symbol takes at most two tokens: an optional package path and a symbol")
	}
	for _, tok := range fields {
		if strings.HasPrefix(tok, "-") {
			return errors.New("flags are not allowed; pass only a package path and/or symbol")
		}
	}
	for _, r := range sym {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '/' || r == '_' || r == '-' || r == ' ':
		default:
			return fmt.Errorf("unsupported character %q (allowed: letters, digits, . / _ - and a single space)", r)
		}
	}
	return nil
}

// execParams is the typed param struct for sys.exec.
type execParams struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
	Sandbox   string `json:"sandbox,omitempty"`
}

// handleExec gates and runs a command host-native.
func (p *Provider) handleExec(ctx context.Context, params map[string]any) (map[string]any, error) {
	// Salvage a list-shaped command (["go","test","./..."]) to one shell string BEFORE decode
	// (bug 1113): a weak model commonly passes the argv as a list, which would otherwise fail
	// the string decode below. Matches the risk gate's execCommandOf salvage, so the gate and
	// the handler agree on the command instead of the gate approving a salvaged string the
	// handler then rejects.
	if _, isStr := params["command"].(string); !isStr {
		if cmd := tool.CommandString(params["command"]); cmd != "" {
			params["command"] = cmd
		}
	}
	var ep execParams
	if err := decodeParams(params, &ep); err != nil {
		return nil, fmt.Errorf("sys.exec: invalid params: %w", err)
	}
	if p.runner == nil {
		return nil, errors.New("sys.exec: runner unavailable (no resolvable working directory)")
	}
	if strings.TrimSpace(ep.Command) == "" {
		return nil, errors.New("sys.exec requires command")
	}
	if ep.Sandbox != "" && ep.Sandbox != "none" {
		return nil, fmt.Errorf("sys.exec: sandbox %q is not supported by the native organ (only sandbox=none); run host-native or use the toolkit for sandboxed exec", ep.Sandbox)
	}
	if err := p.allow.permit(ep.Command); err != nil {
		return nil, fmt.Errorf("sys.exec: %w", err)
	}
	res, err := p.runner.run(ctx, ep.Command, runOptions{Cwd: ep.Cwd, TimeoutMS: ep.TimeoutMS})
	if err != nil {
		return nil, err
	}
	return asResult(res)
}

// failResult builds a worker-recoverable usage-error Result whose Value carries
// the message (ClassUsage, like the main Dispatch error path — escalating the
// model cannot fix a malformed/unservable sys call, so it must not escalate).
func failResult(c tool.Call, msg string, latency int64) tool.Result {
	return tool.Result{Call: c, OK: false, Value: map[string]any{"error": msg}, ErrorClass: tool.ClassUsage, LatencyMS: latency}
}

// asResult marshals a typed result struct to the generic map shape the model sees.
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

// decodeParams re-decodes the generic params map into a typed param struct.
func decodeParams(params map[string]any, dst any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}
