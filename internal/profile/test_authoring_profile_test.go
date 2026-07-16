package profile

import (
	"testing"

	"corpos/internal/pathglob"
)

// TestTestAuthoringProfileProtectsProductionNotTests is the bug 1089 contract: the
// embedded test-authoring-chain profile must permit *_test.go writes while denying
// production .go writes, the inverse of atomic-coding-chain. It asserts the loaded
// profile's protect_paths, evaluated through the shared matcher, behave that way — so
// the spawner's dispatch guard lets the worker author its test but not edit production.
func TestTestAuthoringProfileProtectsProductionNotTests(t *testing.T) {
	reg, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	p, ok := reg.Get("test-authoring-chain")
	if !ok {
		t.Fatal("test-authoring-chain profile not embedded")
	}
	if len(p.ProtectPaths) == 0 {
		t.Fatal("test-authoring-chain must declare protect_paths")
	}

	// Production source is protected (the worker cannot change behavior to pass a test).
	for _, prod := range []string{"internal/agent/loop.go", "cmd/corpos/main.go"} {
		if !pathglob.IsProtected(prod, p.ProtectPaths) {
			t.Errorf("production path %q must be protected for a test-authoring worker", prod)
		}
	}
	// Test files are writable (the deliverable).
	for _, test := range []string{"internal/agent/loop_test.go", "internal/coding/spec_test.go"} {
		if pathglob.IsProtected(test, p.ProtectPaths) {
			t.Errorf("test path %q must be writable for a test-authoring worker", test)
		}
	}

	// And it is genuinely the INVERSE of atomic-coding-chain (which protects tests).
	ac, ok := reg.Get("atomic-coding-chain")
	if !ok {
		t.Fatal("atomic-coding-chain profile not embedded")
	}
	if !pathglob.IsProtected("x/y_test.go", ac.ProtectPaths) {
		t.Error("atomic-coding-chain should still protect *_test.go (unchanged)")
	}
}
