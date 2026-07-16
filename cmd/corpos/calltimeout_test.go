package main

import (
	"testing"

	"corpos/internal/model"
)

// TestBuildAdapter_TierAwareCallTimeout pins the cloud-stall fix: the cloud OAC rungs
// (openrouter: Gemini/DeepSeek) get a per-call cap well below the per-turn budget so a stall is
// abandoned early and the loop can retry/escalate, while the local floor keeps a longer cap for
// its slow large-context first call (bug 1123).
func TestBuildAdapter_TierAwareCallTimeout(t *testing.T) {
	cloud, err := buildAdapter("openrouter", "google/gemini-3.1-flash-lite", "")
	if err != nil {
		t.Fatal(err)
	}
	oc, ok := cloud.(*model.OpenAICompat)
	if !ok {
		t.Fatalf("openrouter adapter type = %T, want *model.OpenAICompat", cloud)
	}
	if oc.CallTimeout() != cloudCallTimeout {
		t.Errorf("cloud rung CallTimeout = %v, want the cloud cap %v", oc.CallTimeout(), cloudCallTimeout)
	}

	local, err := buildAdapter("openai", "Qwen", "http://localhost:8081/v1")
	if err != nil {
		t.Fatal(err)
	}
	lc, ok := local.(*model.OpenAICompat)
	if !ok {
		t.Fatalf("local adapter type = %T, want *model.OpenAICompat", local)
	}
	if lc.CallTimeout() <= cloudCallTimeout {
		t.Errorf("local floor CallTimeout = %v, want > cloud cap %v (preserve bug-1123 slow first call)", lc.CallTimeout(), cloudCallTimeout)
	}
}
