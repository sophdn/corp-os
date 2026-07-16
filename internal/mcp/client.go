// Package mcp is the toolkit-server tool provider for Corp-OS: a thin net/http
// client that dispatches tool calls over toolkit-server's bespoke
// POST /mcp/<surface> route — a plain {action, params, …} JSON dispatch table,
// NOT MCP-spec JSON-RPC (see the T3 client-choice deliverable). It implements
// tool.Provider, so it is one swappable provider behind the loop's dispatch seam.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"corpos/internal/tool"
)

// defaultTimeout is the per-request timeout for the default HTTP transport.
const defaultTimeout = 30 * time.Second

// Transport is the injectable seam: it POSTs a JSON body to a /mcp path and
// returns the decoded JSON (a map, slice, or scalar). Tests inject a fake so the
// whole dispatch path runs with no server; the default hits the live daemon.
type Transport func(ctx context.Context, path string, body map[string]any) (any, error)

// transportError marks a network/timeout failure (classified ClassTransient).
type transportError struct{ err error }

func (e transportError) Error() string { return e.err.Error() }

// parseError marks an undecodable response body (classified ClassParse).
type parseError struct{ msg string }

func (e parseError) Error() string { return e.msg }

// Client dispatches tool calls to a running toolkit-server over HTTP.
type Client struct {
	baseURL    string
	project    string
	httpClient *http.Client
	transport  Transport
}

// Option configures a Client.
type Option func(*Client)

// WithProject sets the default project scope sent on calls; the substrate
// requires a project on write actions.
func WithProject(project string) Option {
	return func(c *Client) { c.project = project }
}

// WithHTTPClient sets the *http.Client used by the default transport.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithTransport overrides the transport entirely (tests inject a fake).
func WithTransport(t Transport) Option {
	return func(c *Client) { c.transport = t }
}

// New builds a Client for the toolkit-server daemon at baseURL (e.g.
// DefaultToolkitURL, "http://localhost:3001").
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	if c.transport == nil {
		c.transport = c.httpTransport
	}
	return c
}

// ensure *Client satisfies the provider seam at compile time.
var _ tool.Provider = (*Client)(nil)

// Dispatch sends one tool call and classifies the outcome. It never returns a
// Go error — failures are reported via tool.Result.ErrorClass.
func (c *Client) Dispatch(ctx context.Context, call tool.Call) tool.Result {
	body := map[string]any{"action": call.Action, "params": paramsOrEmpty(call.Params)}
	if call.Rationale != "" {
		body["rationale"] = call.Rationale
	}
	if c.project != "" {
		body["project"] = c.project
	}

	start := time.Now()
	resp, err := c.transport(ctx, "/mcp/"+call.Surface, body)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		return tool.Fail(call, classify(err), err.Error(), latency)
	}
	switch v := resp.(type) {
	case map[string]any:
		if _, isErr := v["error"]; isErr {
			return tool.Result{Call: call, OK: false, Value: resp, ErrorClass: tool.ClassTool, LatencyMS: latency, SpanID: spanID(v)}
		}
		return tool.Result{Call: call, OK: true, Value: resp, ErrorClass: tool.ClassNone, LatencyMS: latency, SpanID: spanID(v)}
	case []any:
		return tool.Result{Call: call, OK: true, Value: resp, ErrorClass: tool.ClassNone, LatencyMS: latency}
	default:
		return tool.Fail(call, tool.ClassFatal, fmt.Sprintf("unexpected response type %T", resp), latency)
	}
}

// classify maps a transport error to its dispatch class.
func classify(err error) tool.ErrorClass {
	var te transportError
	var pe parseError
	switch {
	case errors.As(err, &te):
		return tool.ClassTransient
	case errors.As(err, &pe):
		return tool.ClassParse
	default:
		return tool.ClassFatal
	}
}

// spanID extracts a string span_id from a decoded map response, if present.
func spanID(m map[string]any) string {
	if s, ok := m["span_id"].(string); ok {
		return s
	}
	return ""
}

// paramsOrEmpty returns params, or an empty map when nil (the substrate expects
// a params object).
func paramsOrEmpty(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

// httpTransport is the default Transport: POST the JSON body to baseURL+path and
// decode the JSON response.
func (c *Client) httpTransport(ctx context.Context, path string, body map[string]any) (any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, transportError{err}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, transportError{err}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, transportError{err}
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, parseError{fmt.Sprintf("non-JSON response (status %d): %s", resp.StatusCode, truncate(data, 200))}
	}
	return out, nil
}

// truncate shortens a byte slice for error messages.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
