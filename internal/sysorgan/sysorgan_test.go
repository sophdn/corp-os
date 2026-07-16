package sysorgan

import (
	"context"
	"strings"
	"testing"
	"time"

	"corpos/internal/tool"
)

// stubDelegate records the calls it receives and returns a marker result.
type stubDelegate struct {
	lastSurface string
	lastAction  string
}

func (s *stubDelegate) Dispatch(_ context.Context, c tool.Call) tool.Result {
	s.lastSurface = c.Surface
	s.lastAction = c.Action
	return tool.Result{Call: c, OK: true, Value: map[string]any{"delegated": c.Action}}
}

func execDispatch(p *Provider, command string, extra map[string]any) tool.Result {
	params := map[string]any{"command": command}
	for k, v := range extra {
		params[k] = v
	}
	return p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "exec", Params: params})
}

func mustMap(t *testing.T, r tool.Result) map[string]any {
	t.Helper()
	if !r.OK {
		t.Fatalf("expected ok, got error: %v", r.Value)
	}
	return r.Value.(map[string]any)
}

func errMsg(r tool.Result) string {
	m, _ := r.Value.(map[string]any)
	s, _ := m["error"].(string)
	return s
}

func TestSpecs(t *testing.T) {
	specs := New(nil).Specs()
	if len(specs) != 1 || specs[0].Name != Surface {
		t.Fatalf("specs = %+v", specs)
	}
	if !strings.Contains(specs[0].Description, "exec") {
		t.Fatal("description should mention exec")
	}
	if specs[0].InputSchema["required"].([]any)[0] != "action" {
		t.Fatal("schema requires action")
	}
}

func TestDispatch_ExecSuccess(t *testing.T) {
	m := mustMap(t, execDispatch(New(nil), "echo hi", nil))
	if m["output"].(string) != "hi" {
		t.Fatalf("output = %v", m["output"])
	}
	if int(m["exit_code"].(float64)) != 0 {
		t.Fatalf("exit_code = %v", m["exit_code"])
	}
}

func TestDispatch_ExecGatedRejection(t *testing.T) {
	r := execDispatch(New(nil), "bash -c 'echo x'", nil)
	if r.OK || !strings.Contains(errMsg(r), "allowlist") {
		t.Fatalf("shell should be gate-rejected, got %v", r.Value)
	}
	// A gate rejection is worker-recoverable (try an allowed command) and must NOT
	// escalate — a costlier model is gated identically (bug escalate-after-1-…).
	if r.ErrorClass != tool.ClassUsage {
		t.Fatalf("error class = %q, want usage_error", r.ErrorClass)
	}
}

func TestDispatch_ExecRejectsSandbox(t *testing.T) {
	r := execDispatch(New(nil), "echo x", map[string]any{"sandbox": "bwrap"})
	if r.OK || !strings.Contains(errMsg(r), "sandbox") {
		t.Fatalf("bwrap sandbox should be rejected, got %v", r.Value)
	}
	// sandbox=none is accepted.
	if !execDispatch(New(nil), "echo x", map[string]any{"sandbox": "none"}).OK {
		t.Fatal("sandbox=none should be accepted")
	}
}

func TestDispatch_ExecRequiresCommand(t *testing.T) {
	r := New(nil).Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "exec", Params: map[string]any{}})
	if r.OK || !strings.Contains(errMsg(r), "requires command") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestDispatch_ExecInvalidParams(t *testing.T) {
	r := New(nil).Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "exec", Params: map[string]any{"command": 5}})
	if r.OK || !strings.Contains(errMsg(r), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

// TestDispatch_ExecSalvagesListCommand is the bug-1113 fix: a weak model that passes the argv
// as a LIST (["echo","hi"]) instead of one shell string is salvaged to a string and runs,
// instead of failing the decode — so the worker's malformed go-test verify actually executes
// (Run-49: list-malformed commands wasted rounds and tipped the worker into retry-exhaustion).
func TestDispatch_ExecSalvagesListCommand(t *testing.T) {
	m := mustMap(t, New(nil).Dispatch(context.Background(),
		tool.Call{Surface: Surface, Action: "exec", Params: map[string]any{"command": []any{"echo", "hi"}}}))
	if m["output"].(string) != "hi" {
		t.Fatalf("a list-shaped command must be salvaged and run; output = %v", m["output"])
	}
}

func TestDispatch_ExecRunnerUnavailable(t *testing.T) {
	p := &Provider{runner: nil, allow: loadAllowlist(""), now: time.Now}
	r := execDispatch(p, "echo x", nil)
	if r.OK || !strings.Contains(errMsg(r), "runner unavailable") {
		t.Fatalf("nil runner should error, got %v", r.Value)
	}
}

func TestDispatch_ExecParamsMarshalError(t *testing.T) {
	// A channel value cannot be JSON-marshaled → decodeParams marshal-error path.
	r := New(nil).Dispatch(context.Background(), tool.Call{
		Surface: Surface, Action: "exec",
		Params: map[string]any{"command": "echo x", "extra": make(chan int)},
	})
	if r.OK || !strings.Contains(errMsg(r), "invalid params") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestDispatch_UnknownAction(t *testing.T) {
	r := New(nil).Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "frob"})
	if r.OK || !strings.Contains(errMsg(r), "unknown action") {
		t.Fatalf("error = %v", r.Value)
	}
}

func TestDispatch_IntrospectionDelegates(t *testing.T) {
	d := &stubDelegate{}
	p := New(d)
	for _, action := range []string{"ps", "ports", "units", "containers"} {
		r := p.Dispatch(context.Background(), tool.Call{Surface: Surface, Action: action})
		if !r.OK {
			t.Fatalf("%s delegation failed: %v", action, r.Value)
		}
		if d.lastAction != action || d.lastSurface != Surface {
			t.Fatalf("delegate saw surface=%q action=%q, want sys/%s", d.lastSurface, d.lastAction, action)
		}
		if r.Value.(map[string]any)["delegated"] != action {
			t.Fatalf("delegate result not forwarded: %v", r.Value)
		}
	}
}

func TestDispatch_IntrospectionNilDelegate(t *testing.T) {
	r := New(nil).Dispatch(context.Background(), tool.Call{Surface: Surface, Action: "ps"})
	if r.OK || !strings.Contains(errMsg(r), "no toolkit provider") {
		t.Fatalf("nil delegate ps should error, got %v", r.Value)
	}
}

func TestDispatch_LatencyFromClock(t *testing.T) {
	p := New(nil)
	times := []time.Time{time.Unix(0, 0), time.Unix(0, 5*int64(time.Millisecond))}
	i := 0
	p.now = func() time.Time {
		tm := times[i]
		if i < len(times)-1 {
			i++
		}
		return tm
	}
	r := execDispatch(p, "echo hi", nil)
	if r.LatencyMS != 5 {
		t.Fatalf("latency = %d, want 5", r.LatencyMS)
	}
}

func TestSpecs_ActionEnumProjectable(t *testing.T) {
	props := New(nil).Specs()[0].InputSchema["properties"].(map[string]any)
	action := props["action"].(map[string]any)
	enum, ok := action["enum"].([]any)
	if !ok || len(enum) == 0 {
		t.Fatal("action must carry an enum so a job-profile can action-scope the sys surface (mcp.Project fails closed otherwise)")
	}
	got := map[string]bool{}
	for _, e := range enum {
		got[e.(string)] = true
	}
	for _, want := range []string{"exec", "go_doc", "ps", "ports", "units", "containers"} {
		if !got[want] {
			t.Errorf("action enum missing %q", want)
		}
	}
}
