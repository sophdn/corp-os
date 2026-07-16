package mcp

import (
	"context"
	"testing"

	"corpos/internal/tool"
)

// stubProvider returns a fixed result for any call, recording the last call it saw.
type stubProvider struct {
	res  tool.Result
	seen tool.Call
}

func (s *stubProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	s.seen = c
	return s.res
}

func bugReadCall() tool.Call { return tool.Call{Surface: "work", Action: "bug_read"} }

// A successful bug_read has its resolution-status fields relocated under ledger_status
// and a ground_truth_directive attached, while the bug's substance is preserved — so a
// decomposing agent no longer reads a bare top-level status='fixed' as a premise (bug 1145).
func TestGroundTruthReconcilerFoldsBugReadStatus(t *testing.T) {
	inner := &stubProvider{res: tool.Result{OK: true, Value: map[string]any{
		"slug":                "some-bug",
		"problem_statement":   "the thing is broken",
		"acceptance_criteria": "the thing works",
		"status":              "fixed",
		"resolution_kind":     "fixed",
		"resolved_commit_sha": "abc123",
		"resolved_at":         "2026-07-01T00:00:00Z",
	}}}
	res := NewGroundTruthReconciler(inner).Dispatch(context.Background(), bugReadCall())

	m, ok := res.Value.(map[string]any)
	if !ok {
		t.Fatalf("Value is not a map: %T", res.Value)
	}
	// Status fields are gone from the top level...
	for _, f := range []string{"status", "resolution_kind", "resolved_commit_sha", "resolved_at"} {
		if _, present := m[f]; present {
			t.Errorf("top-level %q must be relocated, still present", f)
		}
	}
	// ...and relocated under ledger_status.
	ls, ok := m["ledger_status"].(map[string]any)
	if !ok {
		t.Fatalf("ledger_status missing or wrong type: %T", m["ledger_status"])
	}
	if ls["status"] != "fixed" || ls["resolved_commit_sha"] != "abc123" {
		t.Errorf("relocated ledger_status = %v, want the original status fields", ls)
	}
	// The directive is attached and the substance is preserved.
	if _, present := m["ground_truth_directive"]; !present {
		t.Error("ground_truth_directive not attached")
	}
	if m["problem_statement"] != "the thing is broken" || m["acceptance_criteria"] != "the thing works" {
		t.Errorf("bug substance not preserved: %v", m)
	}
}

// A non-bug_read call passes through untouched.
func TestGroundTruthReconcilerIgnoresOtherCalls(t *testing.T) {
	orig := map[string]any{"status": "fixed", "slug": "x"}
	inner := &stubProvider{res: tool.Result{OK: true, Value: orig}}
	res := NewGroundTruthReconciler(inner).Dispatch(context.Background(), tool.Call{Surface: "work", Action: "bug_list"})
	m := res.Value.(map[string]any)
	if _, present := m["ledger_status"]; present {
		t.Error("a non-bug_read call must not be reconciled")
	}
	if m["status"] != "fixed" {
		t.Error("a non-bug_read call's status must be left in place")
	}
}

// A failed bug_read (OK=false, e.g. an error body) is not reconciled.
func TestGroundTruthReconcilerIgnoresFailedDispatch(t *testing.T) {
	inner := &stubProvider{res: tool.Result{OK: false, Value: map[string]any{"error": "not found", "status": "fixed"}}}
	res := NewGroundTruthReconciler(inner).Dispatch(context.Background(), bugReadCall())
	m := res.Value.(map[string]any)
	if _, present := m["ledger_status"]; present {
		t.Error("a failed dispatch must not be reconciled")
	}
}

// A bug_read whose Value is not a JSON object is passed through.
func TestGroundTruthReconcilerIgnoresNonMapValue(t *testing.T) {
	inner := &stubProvider{res: tool.Result{OK: true, Value: "a plain string"}}
	res := NewGroundTruthReconciler(inner).Dispatch(context.Background(), bugReadCall())
	if res.Value != "a plain string" {
		t.Errorf("non-map Value must pass through, got %v", res.Value)
	}
}

// A record carrying none of the resolution-status fields is left unchanged (no empty
// ledger_status object, no directive).
func TestGroundTruthReconcilerLeavesStatuslessRecord(t *testing.T) {
	inner := &stubProvider{res: tool.Result{OK: true, Value: map[string]any{"slug": "open-bug", "problem_statement": "x"}}}
	res := NewGroundTruthReconciler(inner).Dispatch(context.Background(), bugReadCall())
	m := res.Value.(map[string]any)
	if _, present := m["ledger_status"]; present {
		t.Error("a statusless record must not gain a ledger_status object")
	}
	if _, present := m["ground_truth_directive"]; present {
		t.Error("a statusless record must not gain a directive")
	}
}

// The transform is idempotent: a second pass finds the fields already relocated and
// makes no further change (guards against a double-wrap re-nesting the status).
func TestGroundTruthReconcilerIdempotent(t *testing.T) {
	inner := &stubProvider{res: tool.Result{OK: true, Value: map[string]any{
		"slug": "b", "status": "fixed", "resolution_kind": "fixed",
	}}}
	rec := NewGroundTruthReconciler(inner)
	first := rec.Dispatch(context.Background(), bugReadCall())
	// Feed the first result back through a second reconciler.
	inner2 := &stubProvider{res: first}
	second := NewGroundTruthReconciler(inner2).Dispatch(context.Background(), bugReadCall())
	m := second.Value.(map[string]any)
	ls, ok := m["ledger_status"].(map[string]any)
	if !ok {
		t.Fatalf("ledger_status lost on second pass: %T", m["ledger_status"])
	}
	if _, nested := ls["ledger_status"]; nested {
		t.Error("second pass re-nested ledger_status inside itself")
	}
	if ls["status"] != "fixed" {
		t.Errorf("ledger_status.status = %v after second pass, want fixed", ls["status"])
	}
}
