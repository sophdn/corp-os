package model

import (
	"context"
	"testing"

	"corpos/internal/tool"
)

func TestEchoReturnsScriptedThenDefault(t *testing.T) {
	scripted := Response{
		ToolCalls:  []tool.Call{{ID: "c1", Surface: "work", Action: "chain_state"}},
		StopReason: StopToolUse,
	}
	e := NewEcho("qwen", scripted)
	ctx := context.Background()
	msgs := []ChatMessage{{Role: RoleUser, Content: "hello"}}

	r1, err := e.Complete(ctx, msgs, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if r1.StopReason != StopToolUse || len(r1.ToolCalls) != 1 {
		t.Errorf("scripted response wrong: %+v", r1)
	}
	if r1.Model != "qwen" {
		t.Errorf("Model not defaulted: %q", r1.Model)
	}

	r2, err := e.Complete(ctx, msgs, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if r2.StopReason != StopEndTurn || r2.Text != "hello" {
		t.Errorf("default echo wrong: %+v", r2)
	}

	if e.Model() != "qwen" || !e.Available() {
		t.Errorf("Model/Available wrong: %q %v", e.Model(), e.Available())
	}
}

func TestEchoDefaultWithNoUserMessage(t *testing.T) {
	e := NewEcho("qwen")
	r, err := e.Complete(context.Background(), []ChatMessage{{Role: RoleSystem, Content: "sys"}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if r.Text != "" {
		t.Errorf("Text = %q, want empty", r.Text)
	}
}
