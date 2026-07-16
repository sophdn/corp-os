package coding

import (
	"context"
	"testing"

	"corpos/internal/agent"
	"corpos/internal/hooks"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

func fireGuard(guard hooks.Func, c *tool.Call) *hooks.Context {
	ctx := &hooks.Context{Kind: hooks.PreToolUse, ToolCall: c}
	guard(ctx)
	return ctx
}

func TestProtectedPathGuard_DeniesProtectedWrite(t *testing.T) {
	guard := ProtectedPathGuard([]string{"internal/fs/acceptance_test.go"})
	got := fireGuard(guard, &tool.Call{Surface: "fs", Action: "write",
		Params: map[string]any{"path": "internal/fs/acceptance_test.go"}})
	if !got.DenyToolCall {
		t.Fatal("write to a protected acceptance path should be denied at dispatch")
	}
	if got.DenyReason == "" {
		t.Fatal("a denial must carry a reason")
	}
}

func TestProtectedPathGuard_DeniesViaFilePathSpelling(t *testing.T) {
	guard := ProtectedPathGuard([]string{"gate/**"})
	got := fireGuard(guard, &tool.Call{Surface: "fs", Action: "edit",
		Params: map[string]any{"file_path": "gate/oracle_test.go"}})
	if !got.DenyToolCall {
		t.Fatal("edit to a protected path (file_path spelling) should be denied")
	}
}

func TestProtectedPathGuard_AllowsUnprotectedWrite(t *testing.T) {
	guard := ProtectedPathGuard([]string{"gate/**"})
	got := fireGuard(guard, &tool.Call{Surface: "fs", Action: "write",
		Params: map[string]any{"path": "internal/fs/read.go"}})
	if got.DenyToolCall {
		t.Fatalf("write outside the protected set must be allowed: %s", got.DenyReason)
	}
}

func TestProtectedPathGuard_AllowsRead(t *testing.T) {
	guard := ProtectedPathGuard([]string{"**"})
	got := fireGuard(guard, &tool.Call{Surface: "fs", Action: "read",
		Params: map[string]any{"path": "gate/oracle_test.go"}})
	if got.DenyToolCall {
		t.Fatal("a read must never be denied by the protected-path guard")
	}
}

func TestProtectedPathGuard_NoToolCallOrEmptySet(t *testing.T) {
	// Nil tool call → no-op.
	if got := fireGuard(ProtectedPathGuard([]string{"**"}), nil); got.DenyToolCall {
		t.Fatal("nil tool call should be a no-op")
	}
	// Empty protected set → no-op even for an fs.write.
	got := fireGuard(ProtectedPathGuard(nil), &tool.Call{Surface: "fs", Action: "write",
		Params: map[string]any{"path": "anything.go"}})
	if got.DenyToolCall {
		t.Fatal("empty protected set should deny nothing")
	}
}

// ModelWorker injects the protected-path guard when the AT names protected paths (the opt
// branch is exercised; the guard's behavior is covered above and at the agent layer).
func TestModelWorkerInjectsProtectedGuard(t *testing.T) {
	fs := &fakeSpawner{res: agent.Result{Text: "did it"}}
	w := &ModelWorker{spawner: fs, profile: &profile.JobProfile{Name: "coding"}}
	at := AtomicTask{Slug: "fix", Worker: WorkerConfig{Kind: WorkerModel}, Protected: []string{"gate/**"}}
	res := w.Attempt(context.Background(), at, "/work", Feedback{})
	if res.CommandErr != nil {
		t.Fatalf("unexpected error: %v", res.CommandErr)
	}
}
