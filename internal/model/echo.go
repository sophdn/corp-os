package model

import (
	"context"

	"corpos/internal/tool"
)

// Echo is a deterministic, scriptable Adapter for offline tests and CI. It
// returns its scripted responses in order; once the script is exhausted it ends
// the turn by echoing the last user message, so a driven loop always terminates.
type Echo struct {
	model  string
	script []Response
	next   int
}

// NewEcho builds an Echo adapter for the given model id that returns the
// scripted responses in order.
func NewEcho(model string, script ...Response) *Echo {
	return &Echo{model: model, script: script}
}

var _ Adapter = (*Echo)(nil)

// Model returns the adapter's model id.
func (e *Echo) Model() string { return e.model }

// Available always reports true — Echo needs no backend.
func (e *Echo) Available() bool { return true }

// Complete returns the next scripted response, or a final end_turn echoing the
// last user message once the script is exhausted.
func (e *Echo) Complete(_ context.Context, messages []ChatMessage, _ []tool.Spec) (Response, error) {
	if e.next < len(e.script) {
		r := e.script[e.next]
		e.next++
		if r.Model == "" {
			r.Model = e.model
		}
		return r, nil
	}
	return Response{
		Model:      e.model,
		Text:       lastUserContent(messages),
		StopReason: StopEndTurn,
	}, nil
}

// lastUserContent returns the content of the most recent user message, or "".
func lastUserContent(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			return messages[i].Content
		}
	}
	return ""
}
