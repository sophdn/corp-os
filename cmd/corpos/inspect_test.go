package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"corpos/internal/session"
)

// seedSession writes one session DB with a message, a cost row, a tool_call row,
// and a closed turn — the shape the loop persists.
func seedSession(t *testing.T, dir, runID, model string, usd float64) {
	t.Helper()
	st, err := session.Create(dir, session.Header{RunID: runID, Project: "mcp-servers", ModelCheap: model, ModelStrong: model})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.AppendMessage(0, "user", "do the thing", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.StartTurn(0, model); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordToolCall(0, "work", "chain_state", `{"chain":"x"}`, `{"ok":true}`, true, "", 12, model, "sp1"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCost(0, model, 1000, 500, usd); err != nil {
		t.Fatal(err)
	}
	if err := st.EndTurn(0, `{"tool_errors":0,"parse_failures":0}`); err != nil {
		t.Fatal(err)
	}
}

func TestRunInspectSession(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, "RUNAAA", "claude-haiku-4-5-20251001", 0.0060)

	var out, errb bytes.Buffer
	if code := runInspectSession(dir, "RUNAAA", &out, &errb); code != 0 {
		t.Fatalf("runInspectSession code = %d, stderr=%q", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"session RUNAAA", "transcript (1 messages)", "do the thing",
		"cost (1 rows)", "claude-haiku-4-5-20251001", "tool_calls (1 rows)", "work.chain_state", "span=sp1"} {
		if !strings.Contains(s, want) {
			t.Errorf("inspect output missing %q\n%s", want, s)
		}
	}
}

func TestRunInspectSessionMissing(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runInspectSession(t.TempDir(), "NOPE", &out, &errb); code != 1 {
		t.Errorf("runInspectSession missing code = %d, want 1", code)
	}
}

func TestRunRunRate(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, "RUNAAA", "claude-haiku-4-5-20251001", 7.0)
	seedSession(t, dir, "RUNBBB", "claude-haiku-4-5-20251001", 7.0)
	seedSession(t, dir, "RUNFREE", "qwen2.5-32b", 0)

	var out, errb bytes.Buffer
	// since 14 days ago, until defaults to now (a full timestamp, so today's
	// freshly-created sessions are in-period). S = $7 + $7 = $14 (exact, regardless
	// of D); the exact S/D×30 projection is covered by runrate.TestProjectMethodology.
	now := time.Now().UTC()
	since := now.AddDate(0, 0, -14).Format("2006-01-02")
	if code := runRunRate(dir, since, "", now, &out, &errb); code != 0 {
		t.Fatalf("runRunRate code = %d, stderr=%q", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"corpos run-rate", "paid spend S = $14.0000", "projected monthly run-rate",
		"claude-haiku-4-5-20251001", "qwen2.5-32b", "[free]"} {
		if !strings.Contains(s, want) {
			t.Errorf("run-rate output missing %q\n%s", want, s)
		}
	}
}

func TestRunRunRateNoSessions(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runRunRate(t.TempDir(), "", "", time.Now(), &out, &errb); code != 1 {
		t.Errorf("runRunRate empty-dir code = %d, want 1", code)
	}
}

func TestRunRunRateBadDate(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, "RUNAAA", "claude-haiku-4-5-20251001", 1.0)
	var out, errb bytes.Buffer
	if code := runRunRate(dir, "garbage", "", time.Now(), &out, &errb); code != 2 {
		t.Errorf("runRunRate bad-date code = %d, want 2", code)
	}
}

func TestRunRunRateDefaultsPeriod(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, "RUNAAA", "claude-haiku-4-5-20251001", 3.0)
	var out, errb bytes.Buffer
	// Empty since/until → earliest-session..now; just assert it projects without error.
	if code := runRunRate(dir, "", "", time.Now().UTC(), &out, &errb); code != 0 {
		t.Fatalf("runRunRate defaults code = %d, stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "projected monthly run-rate") {
		t.Errorf("missing projection:\n%s", out.String())
	}
}

// TestInspectTierRenderPass exercises the per-tier rollup + frontier-gate line on
// a mid-tier (Haiku) session: the frontier share is 0%, so the gate reads PASS.
func TestInspectTierRenderPass(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, "RUNMID", "claude-haiku-4-5-20251001", 0.0060)
	var out, errb bytes.Buffer
	if code := runInspectSession(dir, "RUNMID", &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr=%q", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"per-tier (token & cost share)", "mid", "of spend", "PASS"} {
		if !strings.Contains(s, want) {
			t.Errorf("tier render missing %q\n%s", want, s)
		}
	}
	if strings.Contains(s, "FAIL") {
		t.Errorf("a Haiku-only session must not FAIL the frontier gate\n%s", s)
	}
}

// TestInspectTierRenderFailAndUnknown covers the FAIL verdict (an Opus-dominated
// session breaches the <5% gate) and the unknown-tier render branch.
func TestInspectTierRenderFailAndUnknown(t *testing.T) {
	dir := t.TempDir()
	st, err := session.Create(dir, session.Header{RunID: "RUNOPUS", Project: "corpos", ModelCheap: "qwen2.5-32b", ModelStrong: "claude-opus-4-8"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for turn, row := range []struct {
		model string
		usd   float64
	}{
		{"claude-opus-4-8", 9.0},
		{"mystery-model-xyz", 0}, // unknown tier branch
	} {
		if err := st.StartTurn(turn, row.model); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordCost(turn, row.model, 1000, 500, row.usd); err != nil {
			t.Fatal(err)
		}
		if err := st.EndTurn(turn, `{"tool_errors":0,"parse_failures":0}`); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	var out, errb bytes.Buffer
	if code := runInspectSession(dir, "RUNOPUS", &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr=%q", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"strong", "unknown", "FAIL"} {
		if !strings.Contains(s, want) {
			t.Errorf("opus/unknown render missing %q\n%s", want, s)
		}
	}
}
