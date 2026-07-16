package routing

import (
	"context"
	"testing"

	"corpos/internal/tool"
)

// stubProvider returns a fixed result for any dispatch.
type stubProvider struct{ res tool.Result }

func (s stubProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	r := s.res
	r.Call = c
	return r
}

func okResult(v any) tool.Result { return tool.Result{OK: true, Value: v} }

func TestMCPClassifyExtractsLabel(t *testing.T) {
	p := stubProvider{res: okResult(map[string]any{"label": "chain-execution", "latency_ms": 12.0})}
	c := NewMCPClassifier(p)
	label, err := c.Classify(context.Background(), "do a chain task")
	if err != nil || label != "chain-execution" {
		t.Errorf("Classify = %q (err %v), want chain-execution", label, err)
	}
}

func TestMCPClassifyDispatchFailure(t *testing.T) {
	p := stubProvider{res: tool.Result{OK: false, Value: map[string]any{"error": "down"}}}
	if _, err := NewMCPClassifier(p).Classify(context.Background(), "x"); err == nil {
		t.Error("a failed dispatch should error")
	}
}

func TestMCPClassifyNonObjectBody(t *testing.T) {
	p := stubProvider{res: okResult("not an object")}
	if _, err := NewMCPClassifier(p).Classify(context.Background(), "x"); err == nil {
		t.Error("a non-object body should error")
	}
}

func TestMCPClassifyMissingLabel(t *testing.T) {
	p := stubProvider{res: okResult(map[string]any{"latency_ms": 5.0})}
	if _, err := NewMCPClassifier(p).Classify(context.Background(), "x"); err == nil {
		t.Error("a body with no label should error")
	}
}
