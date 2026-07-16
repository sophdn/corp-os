package grounding

import (
	"context"
	"errors"
	"testing"
	"time"

	"corpos/internal/session"
	"corpos/internal/tool"
)

type fakeStore struct {
	tcs    []session.ToolCallRow
	msgs   []session.Message
	tcErr  error
	msgErr error
}

func (f *fakeStore) ToolCalls() ([]session.ToolCallRow, error) { return f.tcs, f.tcErr }
func (f *fakeStore) Messages() ([]session.Message, error)      { return f.msgs, f.msgErr }

type fakeDispatcher struct{ calls []tool.Call }

func (d *fakeDispatcher) Dispatch(_ context.Context, call tool.Call) tool.Result {
	d.calls = append(d.calls, call)
	return tool.Result{OK: true}
}

func searchTC(turn int, action, span, resultJSON string) session.ToolCallRow {
	return session.ToolCallRow{TurnIndex: turn, Surface: "knowledge", Action: action, SpanID: span, ResultJSON: resultJSON, OK: true}
}

func TestScanAndEmit_DetectsAllTiers(t *testing.T) {
	store := &fakeStore{
		tcs: []session.ToolCallRow{
			searchTC(0, "vault_search", "span-A", `{"results":[{"path":"vault/a.md"},{"path":"vault/b.md"},{"path":"vault/c.md"}]}`),
			// a later read of a.md → followed
			{TurnIndex: 1, Surface: "knowledge", Action: "vault_read", ParamsJSON: `{"path":"vault/a.md"}`, OK: true},
		},
		msgs: []session.Message{
			// assistant cites b.md (markdown link) and mentions c.md (plain)
			{TurnIndex: 1, Role: "assistant", Content: "see [B](vault/b.md) and also vault/c.md is relevant"},
		},
	}
	disp := &fakeDispatcher{}
	New(disp, store, "sess-1").scanAndEmit(context.Background())

	got := map[string]string{} // source_ref -> click_kind
	for _, c := range disp.calls {
		if c.Surface != "knowledge" || c.Action != "record_query_interaction" {
			t.Fatalf("unexpected call: %+v", c)
		}
		if c.Params["span_id"] != "span-A" || c.Params["session_id"] != "sess-1" {
			t.Errorf("bad span/session on %+v", c.Params)
		}
		got[c.Params["source_ref"].(string)] = c.Params["click_kind"].(string)
	}
	want := map[string]string{"vault/a.md": "followed", "vault/b.md": "cited", "vault/c.md": "mentioned"}
	for ref, kind := range want {
		if got[ref] != kind {
			t.Errorf("ref %s: got %q, want %q", ref, got[ref], kind)
		}
	}
	if len(disp.calls) != 3 {
		t.Errorf("want 3 interactions, got %d", len(disp.calls))
	}
}

func TestScanAndEmit_SkipsNonGrounding(t *testing.T) {
	store := &fakeStore{
		tcs: []session.ToolCallRow{
			searchTC(0, "vault_search", "", `{"results":[{"path":"vault/a.md"}]}`),                                                // no span → skip
			{TurnIndex: 0, Surface: "work", Action: "task_list", SpanID: "s", OK: true, ResultJSON: `{"results":[{"path":"x"}]}`}, // not a search
			func() session.ToolCallRow {
				tc := searchTC(0, "kiwix_search", "span-z", `{"hits":[{"url":"zim/x"}]}`)
				tc.OK = false
				return tc
			}(), // failed search
		},
		msgs: []session.Message{{TurnIndex: 1, Role: "assistant", Content: "vault/a.md zim/x x"}},
	}
	disp := &fakeDispatcher{}
	New(disp, store, "s").scanAndEmit(context.Background())
	if len(disp.calls) != 0 {
		t.Errorf("expected no emits, got %d: %+v", len(disp.calls), disp.calls)
	}
}

func TestScanAndEmit_StoreErrors(t *testing.T) {
	disp := &fakeDispatcher{}
	New(disp, &fakeStore{tcErr: errors.New("boom")}, "s").scanAndEmit(context.Background())
	New(disp, &fakeStore{msgErr: errors.New("boom")}, "s").scanAndEmit(context.Background())
	if len(disp.calls) != 0 {
		t.Errorf("store errors should yield no emits, got %d", len(disp.calls))
	}
}

func TestDetectClick_TurnOrderingAndRole(t *testing.T) {
	// a mention in the SAME turn as the search (not later) does not count.
	msgs := []session.Message{
		{TurnIndex: 0, Role: "assistant", Content: "vault/a.md"}, // same turn → ignored
		{TurnIndex: 0, Role: "user", Content: "vault/a.md"},      // user, later turn-wise irrelevant
	}
	if got := detectClick("vault/a.md", 0, nil, msgs); got != "" {
		t.Errorf("same-turn mention should not count, got %q", got)
	}
	// a later USER message mentioning it also does not count (only assistant).
	msgs2 := []session.Message{{TurnIndex: 1, Role: "user", Content: "vault/a.md"}}
	if got := detectClick("vault/a.md", 0, nil, msgs2); got != "" {
		t.Errorf("user mention should not count, got %q", got)
	}
	if got := detectClick("", 0, nil, msgs); got != "" {
		t.Errorf("empty ref should be no-op, got %q", got)
	}
}

func TestExtractSourceRefs(t *testing.T) {
	cases := []struct {
		name, json string
		want       int
	}{
		{"vault results path", `{"results":[{"path":"a"},{"path":"b"}]}`, 2},
		{"kiwix hits url", `{"hits":[{"url":"u1"},{"url":"u2"}]}`, 2},
		{"source_ref key", `{"results":[{"source_ref":"sr1"}]}`, 1},
		{"dedup", `{"results":[{"path":"a"},{"path":"a"}]}`, 1},
		{"empty/malformed", `not json`, 0},
		{"no arrays", `{"latency_ms":5}`, 0},
		{"empty string", ``, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractSourceRefs(tc.json); len(got) != tc.want {
				t.Errorf("got %d refs (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}

func TestSessionEndHookAndOptions(t *testing.T) {
	store := &fakeStore{
		tcs:  []session.ToolCallRow{searchTC(0, "vault_search", "span-A", `{"results":[{"path":"vault/a.md"}]}`)},
		msgs: []session.Message{{TurnIndex: 1, Role: "assistant", Content: "vault/a.md"}},
	}
	disp := &fakeDispatcher{}
	r := New(disp, store, "s", WithTimeout(time.Second), WithTimeout(0)) // 0 ignored
	if r.timeout != time.Second {
		t.Errorf("timeout = %v, want 1s", r.timeout)
	}
	r.SessionEndHook()(nil)
	if len(disp.calls) != 1 || disp.calls[0].Params["click_kind"] != "mentioned" {
		t.Errorf("hook should emit one mentioned interaction, got %+v", disp.calls)
	}
}
