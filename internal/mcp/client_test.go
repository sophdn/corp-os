package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"corpos/internal/tool"
)

var errSentinel = errors.New("sentinel")

func TestDispatchClassification(t *testing.T) {
	cases := []struct {
		name      string
		transport Transport
		wantOK    bool
		wantClass tool.ErrorClass
		wantSpan  string
	}{
		{
			name: "success map with span",
			transport: func(context.Context, string, map[string]any) (any, error) {
				return map[string]any{"id": float64(329), "span_id": "sp-1"}, nil
			},
			wantOK: true, wantClass: tool.ClassNone, wantSpan: "sp-1",
		},
		{
			name:      "success slice",
			transport: func(context.Context, string, map[string]any) (any, error) { return []any{"a", "b"}, nil },
			wantOK:    true, wantClass: tool.ClassNone,
		},
		{
			name: "tool error body",
			transport: func(context.Context, string, map[string]any) (any, error) {
				return map[string]any{"error": "boom"}, nil
			},
			wantOK: false, wantClass: tool.ClassTool,
		},
		{
			name:      "transient",
			transport: func(context.Context, string, map[string]any) (any, error) { return nil, transportError{errSentinel} },
			wantOK:    false, wantClass: tool.ClassTransient,
		},
		{
			name:      "parse failure",
			transport: func(context.Context, string, map[string]any) (any, error) { return nil, parseError{"bad json"} },
			wantOK:    false, wantClass: tool.ClassParse,
		},
		{
			name:      "fatal scalar response",
			transport: func(context.Context, string, map[string]any) (any, error) { return 42, nil },
			wantOK:    false, wantClass: tool.ClassFatal,
		},
		{
			name:      "fatal unknown error",
			transport: func(context.Context, string, map[string]any) (any, error) { return nil, errSentinel },
			wantOK:    false, wantClass: tool.ClassFatal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New("http://example.invalid", WithTransport(tc.transport))
			got := c.Dispatch(context.Background(), tool.Call{Surface: "work", Action: "chain_state"})
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", got.OK, tc.wantOK)
			}
			if got.ErrorClass != tc.wantClass {
				t.Errorf("ErrorClass = %q, want %q", got.ErrorClass, tc.wantClass)
			}
			if tc.wantSpan != "" && got.SpanID != tc.wantSpan {
				t.Errorf("SpanID = %q, want %q", got.SpanID, tc.wantSpan)
			}
		})
	}
}

func TestDispatchBuildsEnvelope(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := New("http://x", WithProject("mcp-servers"), WithTransport(
		func(_ context.Context, path string, body map[string]any) (any, error) {
			gotPath, gotBody = path, body
			return map[string]any{"ok": true}, nil
		}))
	c.Dispatch(context.Background(), tool.Call{
		Surface: "work", Action: "forge", Params: map[string]any{"x": 1}, Rationale: "because",
	})
	if gotPath != "/mcp/work" {
		t.Errorf("path = %q, want /mcp/work", gotPath)
	}
	if gotBody["action"] != "forge" {
		t.Errorf("action = %v, want forge", gotBody["action"])
	}
	if gotBody["rationale"] != "because" {
		t.Errorf("rationale = %v, want because", gotBody["rationale"])
	}
	if gotBody["project"] != "mcp-servers" {
		t.Errorf("project = %v, want mcp-servers", gotBody["project"])
	}
	if _, ok := gotBody["params"].(map[string]any); !ok {
		t.Errorf("params not a map: %T", gotBody["params"])
	}
}

func TestDispatchOmitsEmptyRationaleAndProject(t *testing.T) {
	var gotBody map[string]any
	c := New("http://x", WithTransport(func(_ context.Context, _ string, body map[string]any) (any, error) {
		gotBody = body
		return map[string]any{}, nil
	}))
	c.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "read", Params: map[string]any{"path": "/x"}})
	if _, ok := gotBody["rationale"]; ok {
		t.Error("rationale should be omitted when empty")
	}
	if _, ok := gotBody["project"]; ok {
		t.Error("project should be omitted when unset")
	}
}

func TestHTTPTransportSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/work" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"slug":"agent-os-bootstrap","span_id":"sp-9"}`))
	}))
	defer srv.Close()

	got := New(srv.URL).Dispatch(context.Background(), tool.Call{Surface: "work", Action: "chain_state"})
	if !got.OK {
		t.Fatalf("OK = false, result = %+v", got)
	}
	if got.SpanID != "sp-9" {
		t.Errorf("SpanID = %q, want sp-9", got.SpanID)
	}
}

func TestHTTPTransportParseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 300))) // non-JSON, long → exercises truncate
	}))
	defer srv.Close()

	got := New(srv.URL).Dispatch(context.Background(), tool.Call{Surface: "work", Action: "x"})
	if got.ErrorClass != tool.ClassParse {
		t.Errorf("ErrorClass = %q, want parse_failure", got.ErrorClass)
	}
}

func TestHTTPTransportTransient(t *testing.T) {
	// 127.0.0.1:1 refuses fast → the default transport surfaces a transient error.
	c := New("http://127.0.0.1:1", WithHTTPClient(&http.Client{Timeout: 2 * time.Second}))
	got := c.Dispatch(context.Background(), tool.Call{Surface: "work", Action: "x"})
	if got.ErrorClass != tool.ClassTransient {
		t.Errorf("ErrorClass = %q, want transient", got.ErrorClass)
	}
}

func TestHTTPTransportShortParseFailure(t *testing.T) {
	// A short non-JSON body exercises truncate's len(b) <= n branch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	got := New(srv.URL).Dispatch(context.Background(), tool.Call{Surface: "work", Action: "x"})
	if got.ErrorClass != tool.ClassParse {
		t.Errorf("ErrorClass = %q, want parse_failure", got.ErrorClass)
	}
}

func TestDefaultTransportMarshalError(t *testing.T) {
	// A channel can't be JSON-marshaled, so the default transport fails at the
	// marshal step (before any HTTP call) → classified fatal.
	c := New("http://x")
	got := c.Dispatch(context.Background(), tool.Call{
		Surface: "work", Action: "x", Params: map[string]any{"bad": make(chan int)},
	})
	if got.ErrorClass != tool.ClassFatal {
		t.Errorf("ErrorClass = %q, want fatal", got.ErrorClass)
	}
}

func TestDefaultTransportBadURL(t *testing.T) {
	// A control character in the URL makes http.NewRequestWithContext fail →
	// surfaced as a transient transport error.
	c := New("http://\x7f-bad")
	got := c.Dispatch(context.Background(), tool.Call{Surface: "work", Action: "x"})
	if got.ErrorClass != tool.ClassTransient {
		t.Errorf("ErrorClass = %q, want transient", got.ErrorClass)
	}
}
