// Package escalation is corpos' client half of the toolkit's orchestrator-tier
// escalation contract (docs/ORCHESTRATOR_ESCALATION.md). corpos owns the policy
// (the router decides WHEN to escalate); this package emits the decision as an
// EscalationProposed event and reads the effective per-trigger thresholds — both
// over MCP to the toolkit's admin surface, which keeps the event type + config
// table as surfaces. It is sans-IO over an injected Dispatcher so the loop can
// emit through the same toolkit-server client and tests inject a fake.
package escalation

import (
	"context"
	"fmt"

	"corpos/internal/router"
	"corpos/internal/tool"
)

// Dispatcher is the subset of the MCP client this package needs: one Dispatch
// call. *mcp.Client satisfies it; tests inject a fake.
type Dispatcher interface {
	Dispatch(ctx context.Context, call tool.Call) tool.Result
}

// Proposal is one EscalationProposed payload — the escalate edge the router
// produced, shaped for the admin.escalation_propose action.
type Proposal struct {
	Trigger        string
	FromModel      string
	ToModel        string
	SessionID      string
	TurnIndex      int
	StateBefore    string
	StateAfter     string
	TriggerDetail  string
	FiredThreshold *float64
	ProjectID      string
	Reason         string
}

// Emitter emits an escalation proposal and returns the event id. The loop holds
// an Emitter and calls it on each escalate edge; a nil Emitter means local-only
// telemetry (no event lands).
type Emitter interface {
	Propose(ctx context.Context, p Proposal) (eventID string, err error)
}

// Client is the toolkit-backed escalation surface: it emits EscalationProposed
// via admin.escalation_propose and reads the effective per-trigger thresholds via
// admin.escalation_threshold_list. It owns no state.
type Client struct {
	d Dispatcher
}

// New builds a Client over a Dispatcher (an *mcp.Client in production).
func New(d Dispatcher) *Client { return &Client{d: d} }

// Propose emits one EscalationProposed event and returns its event id.
func (c *Client) Propose(ctx context.Context, p Proposal) (string, error) {
	params := map[string]any{
		"trigger":      p.Trigger,
		"from_model":   p.FromModel,
		"to_model":     p.ToModel,
		"session_id":   p.SessionID,
		"turn_index":   p.TurnIndex,
		"state_before": p.StateBefore,
		"state_after":  p.StateAfter,
	}
	if p.TriggerDetail != "" {
		params["trigger_detail"] = p.TriggerDetail
	}
	if p.FiredThreshold != nil {
		params["fired_threshold"] = *p.FiredThreshold
	}
	if p.ProjectID != "" {
		params["project_id"] = p.ProjectID
	}
	if p.Reason != "" {
		params["reason"] = p.Reason
	}

	res := c.d.Dispatch(ctx, tool.Call{
		Surface: "admin", Action: "escalation_propose", Params: params, Rationale: p.Reason,
	})
	if !res.OK {
		return "", fmt.Errorf("escalation_propose: %s", errText(res.Value))
	}
	m, ok := res.Value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("escalation_propose: unexpected response %T", res.Value)
	}
	id, _ := m["event_id"].(string)
	return id, nil
}

// Thresholds fetches the effective per-trigger escalation config for the project,
// building a router.Config. On any failure (unreachable toolkit, unexpected shape)
// it returns router.DefaultConfig along with the error, so a caller can log the
// degradation and still run with the built-in defaults.
func (c *Client) Thresholds(ctx context.Context, projectID string) (router.Config, error) {
	params := map[string]any{}
	if projectID != "" {
		params["project_id"] = projectID
	}
	res := c.d.Dispatch(ctx, tool.Call{
		Surface: "admin", Action: "escalation_threshold_list", Params: params,
	})
	if !res.OK {
		return router.DefaultConfig(), fmt.Errorf("escalation_threshold_list: %s", errText(res.Value))
	}
	rows, ok := res.Value.([]any)
	if !ok {
		return router.DefaultConfig(), fmt.Errorf("escalation_threshold_list: unexpected response %T", res.Value)
	}

	cfg := router.Config{Triggers: map[router.Trigger]router.TriggerConfig{}}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := row["trigger_kind"].(string)
		if kind == "" {
			continue
		}
		cfg.Triggers[router.Trigger(kind)] = router.TriggerConfig{
			ThresholdValue: toFloat(row["threshold_value"]),
			Enabled:        toBool(row["enabled"]),
		}
		if k := toInt(row["de_escalation_turns"]); k > cfg.DeEscalationTurns {
			cfg.DeEscalationTurns = k
		}
	}
	if len(cfg.Triggers) == 0 {
		return router.DefaultConfig(), nil // empty table → built-in defaults
	}
	if cfg.DeEscalationTurns == 0 {
		cfg.DeEscalationTurns = 2
	}
	return cfg, nil
}

// errText pulls a human message out of a toolkit error Value ({"error": …}),
// falling back to a generic render.
func errText(v any) string {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["error"].(string); ok {
			return s
		}
	}
	return fmt.Sprintf("%v", v)
}

// toFloat coerces a JSON-decoded number (float64) or int to float64.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

// toInt coerces a JSON-decoded number to int.
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// toBool coerces a JSON-decoded bool (or numeric 1/0) to bool.
func toBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case int:
		return b != 0
	default:
		return false
	}
}
