package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/router"
	"corpos/internal/tool"
)

// recordingFS is a tool provider that records every dispatch and serves a
// configurable-size payload for fs.read (so a small toolResultCap decides
// truncation), succeeding for everything else.
type recordingFS struct {
	readPayload string
	calls       []tool.Call
}

func (p *recordingFS) Dispatch(_ context.Context, c tool.Call) tool.Result {
	p.calls = append(p.calls, c)
	if c.Surface == "fs" && c.Action == "read" {
		return tool.Result{Call: c, OK: true, Value: map[string]any{"content": p.readPayload}}
	}
	return tool.Result{Call: c, OK: true, Value: map[string]any{"ok": true}}
}

func (p *recordingFS) dispatched(surface, action string) bool {
	for _, c := range p.calls {
		if c.Surface == surface && c.Action == action {
			return true
		}
	}
	return false
}

// scriptModel emits a fixed sequence of responses (one per turn-round), holding the
// last response once exhausted.
type scriptModel struct {
	i    int
	resp []model.Response
}

func (a *scriptModel) Model() string   { return "m" }
func (a *scriptModel) Available() bool { return true }
func (a *scriptModel) Complete(context.Context, []model.ChatMessage, []tool.Spec) (model.Response, error) {
	r := a.resp[a.i]
	if a.i < len(a.resp)-1 {
		a.i++
	}
	return r, nil
}

func readCall(fp string) tool.Call {
	return tool.Call{ID: "r", Surface: "fs", Action: "read", Params: map[string]any{"file_path": fp}}
}
func writeCall(fp string) tool.Call {
	return tool.Call{ID: "w", Surface: "fs", Action: "write", Params: map[string]any{"file_path": fp, "content": "x"}}
}
func toolResp(c tool.Call) model.Response {
	return model.Response{Model: "m", ToolCalls: []tool.Call{c}, StopReason: model.StopToolUse}
}
func doneResp() model.Response {
	return model.Response{Model: "m", Text: "done", StopReason: model.StopEndTurn}
}

func writeDispatch(res []tool.Result) (tool.Result, bool) {
	for _, r := range res {
		if r.Call.Action == "write" {
			return r, true
		}
	}
	return tool.Result{}, false
}

// TestTruncatedReadBlocksWholeFileWrite is the run-9 regression: a whole-file
// fs.write to a path whose read result was truncated is refused (the unseen
// remainder would be lost), and the write never reaches the provider.
func TestTruncatedReadBlocksWholeFileWrite(t *testing.T) {
	fp := "/repo/read.go"
	prov := &recordingFS{readPayload: strings.Repeat("Z", 200)} // 200 chars > cap 50 → truncated
	m := &scriptModel{resp: []model.Response{toolResp(readCall(fp)), toolResp(writeCall(fp)), doneResp()}}
	l := New(router.New(m, m), prov, nil, WithToolResultCap(50))

	res, err := l.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if prov.dispatched("fs", "write") {
		t.Fatal("a whole-file write after a truncated read must NOT reach the provider")
	}
	w, ok := writeDispatch(res.Dispatches)
	if !ok {
		t.Fatal("expected a write dispatch result (the blocked synthesis)")
	}
	if w.OK {
		t.Error("the blocked write must be a tool error, not OK")
	}
	if v, _ := w.Value.(map[string]any); v["fs_guard"] != "truncated_view" {
		t.Errorf("blocked write should carry fs_guard=truncated_view, got %v", w.Value)
	}
}

// TestUntruncatedWholeReadAllowsWrite: a whole-file read that fits (no truncation)
// leaves the path clean, so the follow-up whole-file write proceeds to the provider.
func TestUntruncatedWholeReadAllowsWrite(t *testing.T) {
	fp := "/repo/small.go"
	prov := &recordingFS{readPayload: "tiny"} // < cap 50 → not truncated
	m := &scriptModel{resp: []model.Response{toolResp(readCall(fp)), toolResp(writeCall(fp)), doneResp()}}
	l := New(router.New(m, m), prov, nil, WithToolResultCap(50))

	if _, err := l.Run(context.Background(), "fix it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !prov.dispatched("fs", "write") {
		t.Fatal("a write after a complete, untruncated read must proceed to the provider")
	}
}

// TestTruncatedReadAllowsEdit: fs.edit is surgical and never blocked, even when the
// path's read was truncated (the run-9 steer — edit is the right tool here).
func TestTruncatedReadAllowsEdit(t *testing.T) {
	fp := "/repo/read.go"
	prov := &recordingFS{readPayload: strings.Repeat("Z", 200)}
	editCall := tool.Call{ID: "e", Surface: "fs", Action: "edit", Params: map[string]any{"file_path": fp}}
	m := &scriptModel{resp: []model.Response{toolResp(readCall(fp)), toolResp(editCall), doneResp()}}
	l := New(router.New(m, m), prov, nil, WithToolResultCap(50))

	if _, err := l.Run(context.Background(), "fix it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !prov.dispatched("fs", "edit") {
		t.Fatal("fs.edit must never be blocked by the truncated-view guard")
	}
}

// TestTruncatedViewHelpers covers the path/range/taint helpers directly across their
// branches (the 95% floor runs tight).
func TestTruncatedViewHelpers(t *testing.T) {
	// fsCallPath: file_path, path alias, neither, nil params.
	if got := fsCallPath(tool.Call{Params: map[string]any{"file_path": "a"}}); got != "a" {
		t.Errorf("file_path: got %q", got)
	}
	if got := fsCallPath(tool.Call{Params: map[string]any{"path": "b"}}); got != "b" {
		t.Errorf("path alias: got %q", got)
	}
	if got := fsCallPath(tool.Call{Params: map[string]any{"other": "c"}}); got != "" {
		t.Errorf("no path param: got %q", got)
	}
	if got := fsCallPath(tool.Call{}); got != "" {
		t.Errorf("nil params: got %q", got)
	}

	// fsReadIsWholeFile + numParam across number kinds.
	whole := tool.Call{Params: map[string]any{"file_path": "a"}}
	if !fsReadIsWholeFile(whole) {
		t.Error("no offset/limit must be a whole-file read")
	}
	if fsReadIsWholeFile(tool.Call{Params: map[string]any{"limit": float64(10)}}) {
		t.Error("a limit makes the read ranged")
	}
	if fsReadIsWholeFile(tool.Call{Params: map[string]any{"offset": int(5)}}) {
		t.Error("an offset>1 makes the read ranged")
	}
	if numParam(tool.Call{Params: map[string]any{"n": int64(7)}}, "n") != 7 {
		t.Error("int64 param")
	}
	if numParam(tool.Call{}, "n") != 0 {
		t.Error("missing param → 0")
	}

	// trackTruncatedView: taint on truncated read; a ranged untruncated read does NOT
	// clear it; a whole untruncated read does; non-fs and pathless are no-ops.
	l := New(router.New(capEcho(), capEcho()), vProvider{}, nil, WithToolResultCap(50))
	big := strings.Repeat("Z", 200)
	l.trackTruncatedView(readCall("/f"), big) // truncated → taint
	if l.truncatedWriteReason(writeCall("/f")) == "" {
		t.Fatal("a truncated read should taint the path so a whole-file write is refused")
	}
	l.trackTruncatedView(tool.Call{Surface: "fs", Action: "read", Params: map[string]any{"file_path": "/f", "offset": float64(5)}}, "tiny")
	if l.truncatedWriteReason(writeCall("/f")) == "" {
		t.Error("a ranged untruncated read must NOT clear the taint")
	}
	l.trackTruncatedView(readCall("/f"), "tiny") // whole untruncated → clear
	if l.truncatedWriteReason(writeCall("/f")) != "" {
		t.Error("a whole untruncated read should clear the taint")
	}
	l.trackTruncatedView(tool.Call{Surface: "work", Action: "read", Params: map[string]any{"file_path": "/f"}}, big) // non-fs noop
	l.trackTruncatedView(tool.Call{Surface: "fs", Action: "read"}, big)                                              // no path noop
	if l.truncatedWriteReason(writeCall("/f")) != "" {
		t.Error("non-fs / pathless reads must not taint")
	}
	// truncatedWriteReason no-ops: a write with no path, and a non-write fs call.
	if l.truncatedWriteReason(tool.Call{Surface: "fs", Action: "write"}) != "" {
		t.Error("a pathless write has nothing to block")
	}
	if l.truncatedWriteReason(readCall("/f")) != "" {
		t.Error("a read is never blocked by the write guard")
	}
}
