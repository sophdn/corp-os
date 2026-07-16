package routing

import (
	"context"
	"fmt"

	"corpos/internal/tool"
)

// classifySurface/classifyAction name the toolkit-server rubric the router uses
// as its routing input.
const (
	classifySurface = "measure"
	classifyAction  = "classify_session_routing_trigger"
)

// MCPClassifier is the production Classifier: it dispatches
// measure.classify_session_routing_trigger over a tool.Provider (the same MCP
// aggregator the loop drives) and reads the rubric's label off the result. It
// reuses the existing classifier infra rather than hand-rolling routing (the task
// constraint).
type MCPClassifier struct {
	provider tool.Provider
}

// NewMCPClassifier builds a classifier over the MCP provider.
func NewMCPClassifier(p tool.Provider) *MCPClassifier {
	return &MCPClassifier{provider: p}
}

var _ Classifier = (*MCPClassifier)(nil)

// Classify dispatches the routing rubric for the duty and returns its label. A
// failed dispatch, a non-map body, or a missing/empty label is an error — the
// Router turns that into the fallback profile.
func (c *MCPClassifier) Classify(ctx context.Context, duty string) (string, error) {
	res := c.provider.Dispatch(ctx, tool.Call{
		Surface: classifySurface,
		Action:  classifyAction,
		Params:  map[string]any{"user_input": duty},
	})
	if !res.OK {
		return "", fmt.Errorf("classify dispatch failed: %v", res.Value)
	}
	body, ok := res.Value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("classify returned a non-object body (%T)", res.Value)
	}
	label, ok := body["label"].(string)
	if !ok || label == "" {
		return "", fmt.Errorf("classify returned no label (body=%v)", body)
	}
	return label, nil
}
