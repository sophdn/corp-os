package main

import (
	"context"
	"testing"

	"corpos/internal/fsorgan"
	"corpos/internal/mcp"
	"corpos/internal/sysorgan"
	"corpos/internal/tool"
)

// stubProvider is a no-op tool.Provider standing in for a toolkit-http client.
type stubProvider struct{}

func (stubProvider) Dispatch(context.Context, tool.Call) tool.Result {
	return tool.Result{OK: true}
}

// TestMountNativeFS_OwnsFSSurface verifies the T4 cutover: after mounting, the
// "fs" surface is offered exactly once (by the native organ), the toolkit's "fs"
// spec is stripped, and the aggregator routes fs to the organ without collision.
func TestMountNativeFS_OwnsFSSurface(t *testing.T) {
	toolkit := mcp.Server{
		Name:     "toolkit",
		Provider: stubProvider{},
		Specs: []tool.Spec{
			{Name: "work"},
			{Name: "fs"}, // the toolkit-advertised fs that must be stripped
			{Name: "knowledge"},
		},
	}

	servers := mountNativeFS([]mcp.Server{toolkit})

	// Exactly one server now offers "fs", and it is the native organ.
	var fsOwners int
	for _, s := range servers {
		for _, spec := range s.Specs {
			if spec.Name == fsorgan.Surface {
				fsOwners++
				if s.Name != "fs-native" {
					t.Fatalf("fs surface offered by %q, want fs-native", s.Name)
				}
			}
		}
	}
	if fsOwners != 1 {
		t.Fatalf("fs offered by %d servers, want exactly 1", fsOwners)
	}

	// The aggregator accepts the mounted set (no surface collision) and routes
	// an fs call to the native organ — proven by the organ's typed error on a
	// missing file_path (the stub toolkit would have returned OK).
	agg, err := mcp.NewAggregator(servers...)
	if err != nil {
		t.Fatalf("aggregator rejected the mounted set: %v", err)
	}
	res := agg.Dispatch(context.Background(), tool.Call{Surface: "fs", Action: "read", Params: map[string]any{}})
	if res.OK {
		t.Fatal("fs.read with no file_path should fail via the native organ, not the OK stub")
	}
}

// TestMountNativeSys_OwnsSysSurface verifies the T5 cutover: the "sys" surface is
// owned by the native organ (exec routed in-process, gated), the toolkit's "sys"
// spec is stripped, and introspection delegates to the toolkit provider.
func TestMountNativeSys_OwnsSysSurface(t *testing.T) {
	delegate := &recordingProvider{}
	toolkit := mcp.Server{
		Name:     "toolkit",
		Provider: delegate,
		Specs:    []tool.Spec{{Name: "work"}, {Name: "sys"}, {Name: "knowledge"}},
	}

	servers := mountNativeSys([]mcp.Server{toolkit}, delegate)

	var sysOwners int
	for _, s := range servers {
		for _, spec := range s.Specs {
			if spec.Name == sysorgan.Surface {
				sysOwners++
				if s.Name != "sys-native" {
					t.Fatalf("sys surface offered by %q, want sys-native", s.Name)
				}
			}
		}
	}
	if sysOwners != 1 {
		t.Fatalf("sys offered by %d servers, want exactly 1", sysOwners)
	}

	agg, err := mcp.NewAggregator(servers...)
	if err != nil {
		t.Fatalf("aggregator rejected the mounted set: %v", err)
	}

	// exec routes to the native organ: a disallowed (shell) command is gate-rejected
	// in-process — the delegate would have returned OK.
	exec := agg.Dispatch(context.Background(), tool.Call{
		Surface: "sys", Action: "exec", Params: map[string]any{"command": "bash -c x"},
	})
	if exec.OK {
		t.Fatal("sys.exec of a shell should be gate-rejected by the native organ")
	}
	if delegate.calls != 0 {
		t.Fatal("exec must not reach the delegate")
	}

	// introspection routes through the native organ to the delegate.
	ps := agg.Dispatch(context.Background(), tool.Call{Surface: "sys", Action: "ps"})
	if !ps.OK || delegate.calls != 1 || delegate.lastAction != "ps" {
		t.Fatalf("sys.ps should delegate to the toolkit: ok=%v calls=%d action=%q", ps.OK, delegate.calls, delegate.lastAction)
	}
}

// recordingProvider counts calls and returns OK, standing in for the toolkit.
type recordingProvider struct {
	calls      int
	lastAction string
}

func (p *recordingProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	p.calls++
	p.lastAction = c.Action
	return tool.Result{Call: c, OK: true, Value: map[string]any{"ok": true}}
}

// TestNativeOrgansProjectable proves the native organs can be action-scoped by a
// job-profile (e.g. atomic-coding-chain's fs:[read,write,...] + sys:[exec]).
// Without an action enum on their specs, mcp.Project fails closed and the coding
// profile projects 0 surfaces — corpos then runs toolless.
func TestNativeOrgansProjectable(t *testing.T) {
	servers := mountNativeSys(mountNativeFS([]mcp.Server{}), nil)
	var specs []tool.Spec
	for _, s := range servers {
		specs = append(specs, s.Specs...)
	}
	scope := mcp.Scope{
		"fs":  []string{"read", "write", "edit", "grep", "glob", "ls"},
		"sys": []string{"exec"},
	}
	projected := mcp.Project(specs, scope)
	surfs := map[string]bool{}
	for _, p := range projected {
		surfs[p.Name] = true
	}
	if !surfs["fs"] || !surfs["sys"] {
		t.Fatalf("native fs+sys organs must survive an action-scoped projection; got surfaces %v (coding profile would project 0)", surfs)
	}
}
