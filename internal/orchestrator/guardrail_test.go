package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"corpos/internal/profile"
)

// guardrailRegistry has the two real profiles the guardrail arbitrates between:
// atomic-coding-chain (protects *_test.go) and test-authoring-chain (protects
// production, excepts *_test.go).
func guardrailRegistry(t *testing.T) *profile.Registry {
	t.Helper()
	dir := t.TempDir()
	write := func(name, toml string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(toml), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("atomic-coding-chain.toml", "name = \"atomic-coding-chain\"\nduty = \"fix one task\"\ntier = \"local\"\nprotect_paths = [\"**/*_test.go\"]\n")
	write("test-authoring-chain.toml", "name = \"test-authoring-chain\"\nduty = \"author a test\"\ntier = \"local\"\nprotect_paths = [\"**/*.go\", \"!**/*_test.go\"]\n")
	reg, err := profile.Load(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

// TestResolveProfile_GuaranteeHoldsOnBothPaths is the chain-379 task-4 invariant: the single
// resolveProfile chokepoint enforces "a test-authoring duty never lands on a test-protecting
// profile" for BOTH the explicit-name path (via the in-chokepoint guardrail) and the auto-router
// path (via the router) — so the orchestrator needs no downstream re-check.
func TestResolveProfile_GuaranteeHoldsOnBothPaths(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("done"), guardrailRegistry(t), WithRouter(leafRouter()))
	duty := "add a unit test covering the parser"

	// Explicit-name path: a test-protecting profile is redirected to the test-authoring profile.
	r, _, ok := sp.resolveProfile(context.Background(), spawnCall(map[string]any{}), "atomic-coding-chain", duty)
	if !ok || r.name != "test-authoring-chain" || r.guardrailRedirect == "" {
		t.Fatalf("explicit path: r=%+v ok=%v, want test-authoring-chain with a redirect recorded", r, ok)
	}

	// Auto-router path (omitted profile): the router routes the test-authoring duty there too.
	r2, _, ok := sp.resolveProfile(context.Background(), spawnCall(map[string]any{}), "", duty)
	if !ok || r2.name != "test-authoring-chain" {
		t.Fatalf("auto path: r=%+v ok=%v, want test-authoring-chain", r2, ok)
	}
}

// TestGuardrail_RedirectsExplicitTestAuthoringMisassignment is the rehearsal regression:
// when the orchestrator EXPLICITLY names the test-protecting coding profile for a
// test-authoring duty, the guardrail redirects it to the test-authoring profile (which
// can actually write the *_test.go), instead of letting the worker thrash protect-path
// denials up the ladder to Opus.
func TestGuardrail_RedirectsExplicitTestAuthoringMisassignment(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("done"), guardrailRegistry(t), WithRouter(leafRouter()))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{
		"profile": "atomic-coding-chain", // the explicit mis-assignment
		"duty":    "add a unit test covering the parser",
	}))
	if !res.OK {
		t.Fatalf("spawn failed: %+v", res.Value)
	}
	v := res.Value.(map[string]any)
	if v["profile"] != "test-authoring-chain" {
		t.Fatalf("guardrail should redirect to test-authoring-chain, got profile=%v", v["profile"])
	}
	if v["profile_guardrail"] == nil {
		t.Fatalf("the redirect should be recorded in profile_guardrail telemetry, got %+v", v)
	}
}

// TestGuardrail_RedirectsExplicitTestRevisionMisassignment is the bug 1101 regression:
// the decompose-admin rehearsal spawned a test-STRENGTHENING follow-up ("the test you
// created is too simple, improve it") and explicitly named atomic-coding-chain, which
// protects *_test.go — so the worker could not edit the very test it was told to improve
// and thrashed protect-path denials up to Opus. The revision-aware guardrail must redirect
// it to the test-authoring profile just like a fresh-authoring duty.
func TestGuardrail_RedirectsExplicitTestRevisionMisassignment(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("done"), guardrailRegistry(t), WithRouter(leafRouter()))
	res := sp.Dispatch(context.Background(), spawnCall(map[string]any{
		"profile": "atomic-coding-chain", // the explicit mis-assignment
		"duty":    "the parser_test.go is too simple, strengthen it to cover the error path",
	}))
	if !res.OK {
		t.Fatalf("spawn failed: %+v", res.Value)
	}
	v := res.Value.(map[string]any)
	if v["profile"] != "test-authoring-chain" {
		t.Fatalf("guardrail should redirect a test-revision duty to test-authoring-chain, got profile=%v", v["profile"])
	}
	if v["profile_guardrail"] == nil {
		t.Fatalf("the redirect should be recorded in profile_guardrail telemetry, got %+v", v)
	}
}

// TestGuardrail_LeavesCompatibleAssignmentsAlone: a real bug-fix stays on the coding
// profile (it must keep test protection), and a test-authoring duty already on the
// test-authoring profile is not redirected (no loop).
func TestGuardrail_LeavesCompatibleAssignmentsAlone(t *testing.T) {
	sp := NewSpawnProvider(leafSpawner("done"), guardrailRegistry(t), WithRouter(leafRouter()))

	// A bug-fix duty (production-fix signal) stays on atomic-coding-chain.
	bugfix := sp.Dispatch(context.Background(), spawnCall(map[string]any{
		"profile": "atomic-coding-chain",
		"duty":    "fix the bug in the parser so the failing test passes",
	}))
	if v := bugfix.Value.(map[string]any); v["profile"] != "atomic-coding-chain" || v["profile_guardrail"] != nil {
		t.Fatalf("a bug-fix duty must stay on atomic-coding-chain (keep test protection), got %+v", v)
	}

	// A test-authoring duty already on the right profile is not redirected.
	ta := sp.Dispatch(context.Background(), spawnCall(map[string]any{
		"profile": "test-authoring-chain",
		"duty":    "add a unit test covering the parser",
	}))
	if v := ta.Value.(map[string]any); v["profile"] != "test-authoring-chain" || v["profile_guardrail"] != nil {
		t.Fatalf("a test-authoring duty already on the right profile must not be redirected, got %+v", v)
	}
}
