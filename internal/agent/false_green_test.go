package agent

import (
	"context"
	"testing"
	"time"

	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

func fgGreenGate() *VerifyGate {
	return &VerifyGate{Command: []string{"x"}, run: func(context.Context, []string, string, time.Duration) (int, string) { return 0, "ok" }}
}

func mutatingProfile() *profile.JobProfile {
	return &profile.JobProfile{Name: "coder", Tier: profile.TierLocal,
		Tools: []profile.SurfaceScope{{Surface: "fs", Actions: []string{"read", "write", "edit"}}}}
}

// TestWorkAudit_NoOpDoneClaimRefused is the false-green regression (chain 365 dogfood): a
// coding run under a verify gate that makes ZERO file mutations and claims done must be
// REFUSED (Result.Fabricated), not ride a gate that passes vacuously on unchanged code.
func TestWorkAudit_NoOpDoneClaimRefused(t *testing.T) {
	m := model.NewEcho("qwen", model.Response{Text: "I analyzed it; all done.", StopReason: model.StopEndTurn})
	res, err := New(single(m), &fakeProvider{}, nil,
		WithProfile(mutatingProfile()), WithVerify(fgGreenGate()),
		WithWorkAudit(WorkAudit{RequireMutation: true}), WithFabricationReprompts(0),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated == "" {
		t.Fatalf("a no-mutation done-claim under a verify gate must be refused (Fabricated), got clean: text=%q", res.Text)
	}
}

// TestWorkAudit_MutationPasses is the positive control: a run that DID mutate a file and
// passes the gate is a clean success (the audit must not over-fire on real work).
func TestWorkAudit_MutationPasses(t *testing.T) {
	m := model.NewEcho("qwen",
		model.Response{ToolCalls: []tool.Call{{ID: "c1", Surface: "fs", Action: "write",
			Params: map[string]any{"path": "x.go", "content": "package x"}}}, StopReason: model.StopToolUse},
		model.Response{Text: "done", StopReason: model.StopEndTurn},
	)
	res, err := New(single(m), &fakeProvider{}, nil,
		WithProfile(mutatingProfile()), WithVerify(fgGreenGate()),
		WithWorkAudit(WorkAudit{RequireMutation: true}), WithFabricationReprompts(0),
	).Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fabricated != "" {
		t.Fatalf("a run that mutated a file must not be flagged no-work, got Fabricated=%q", res.Fabricated)
	}
}
