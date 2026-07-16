package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateRecordAndResume(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, Header{ModelCheap: "qwen", ModelStrong: "haiku", Project: "mcp-servers", SkillsDigest: "abc",
		Profile: "code-review", Duty: "review the diff", Selection: `{"profile":"code-review","score":4,"fallback":false}`})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.RunID() == "" || s.Path() == "" {
		t.Error("empty run id / path")
	}
	if n, err := s.NextTurnIndex(); err != nil || n != 0 {
		t.Errorf("NextTurnIndex on fresh store = %d (err %v), want 0", n, err)
	}

	if _, err := s.AppendMessage(0, "user", "hello", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(0, "assistant", "hi", "qwen"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTurn(0, "qwen"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordToolCall(0, "work", "chain_state", "{}", `{"ok":true}`, true, "", 12, "qwen", "sp1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(0, "qwen", 10, 5, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(0, `{"tool_errors":0}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("closed"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.NextTurnIndex(); err != nil || n != 1 {
		t.Errorf("NextTurnIndex = %d (err %v), want 1", n, err)
	}

	runID := s.RunID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir, runID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	msgs, err := r.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content != "hello" || msgs[1].Model != "qwen" {
		t.Errorf("messages = %+v", msgs)
	}
	h, err := r.HeaderRow()
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "closed" || h.Project != "mcp-servers" || h.SkillsDigest != "abc" {
		t.Errorf("header = %+v", h)
	}
	if h.Profile != "code-review" || h.Duty != "review the diff" ||
		h.Selection != `{"profile":"code-review","score":4,"fallback":false}` {
		t.Errorf("selection/profile/duty round-trip = %+v", h)
	}
}

func TestNewRunIDUniqueAndLength(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if a == b {
		t.Error("run ids should differ")
	}
	if len(a) != 26 {
		t.Errorf("run id length = %d, want 26", len(a))
	}
}

func TestOpenMissingErrors(t *testing.T) {
	if _, err := Open(t.TempDir(), "NOPE"); err == nil {
		t.Error("want error for a missing DB")
	}
}

func TestCreateDuplicateErrors(t *testing.T) {
	dir := t.TempDir()
	h := Header{RunID: "FIXEDID", Project: "p"}
	s, err := Create(dir, h)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Create(dir, h); err == nil {
		t.Error("want error creating a duplicate run id")
	}
}

func TestSchemaVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, Header{RunID: "VERID", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(dir, "VERID"); err == nil {
		t.Error("want a schema-version mismatch error")
	}
}

// TestOperationsAfterCloseError exercises every writer/reader's error path: once
// the DB is closed, all of them must surface an error rather than panic.
func TestOperationsAfterCloseError(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, Header{RunID: "CLOSED", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AppendMessage(0, "user", "x", "m"); err == nil {
		t.Error("AppendMessage after close should error")
	}
	if err := s.RecordToolCall(0, "w", "a", "{}", "{}", false, "tool_error", 1, "m", "sp"); err == nil {
		t.Error("RecordToolCall after close should error")
	}
	if err := s.RecordCost(0, "m", 1, 1, 0); err == nil {
		t.Error("RecordCost after close should error")
	}
	if err := s.StartTurn(0, "m"); err == nil {
		t.Error("StartTurn after close should error")
	}
	if err := s.EndTurn(0, "{}"); err == nil {
		t.Error("EndTurn after close should error")
	}
	if err := s.SetStatus("x"); err == nil {
		t.Error("SetStatus after close should error")
	}
	if _, err := s.HeaderRow(); err == nil {
		t.Error("HeaderRow after close should error")
	}
	if _, err := s.Messages(); err == nil {
		t.Error("Messages after close should error")
	}
	if _, err := s.NextTurnIndex(); err == nil {
		t.Error("NextTurnIndex after close should error")
	}
}

func TestCreateCannotMakeDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// dir resolves under a regular file → MkdirAll fails.
	if _, err := Create(filepath.Join(f, "sub"), Header{Project: "p"}); err == nil {
		t.Error("want an error when the session dir cannot be created")
	}
}

func TestCostToolCallTurnReaders(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, Header{RunID: "READERS", Project: "mcp-servers", ModelCheap: "qwen", ModelStrong: "haiku"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.StartTurn(0, "qwen"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(0, "qwen", 100, 50, 0.25); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordToolCall(0, "work", "chain_state", `{"c":"x"}`, `{"ok":true}`, true, "", 7, "qwen", "sp1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordToolCall(0, "fs", "read", `{}`, `{}`, false, "tool_error", 3, "qwen", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.EndTurn(0, `{"tool_errors":1,"parse_failures":0}`); err != nil {
		t.Fatal(err)
	}

	costs, err := s.Costs()
	if err != nil {
		t.Fatalf("Costs: %v", err)
	}
	if len(costs) != 1 || costs[0].InputTokens != 100 || costs[0].OutputTokens != 50 || costs[0].USD != 0.25 || costs[0].Model != "qwen" {
		t.Fatalf("Costs = %+v", costs)
	}
	if costs[0].CreatedAt == "" {
		t.Errorf("cost row missing created_at")
	}

	tcs, err := s.ToolCalls()
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(tcs) != 2 {
		t.Fatalf("want 2 tool_calls, got %d", len(tcs))
	}
	if !tcs[0].OK || tcs[0].Surface != "work" || tcs[0].Action != "chain_state" || tcs[0].LatencyMS != 7 || tcs[0].SpanID != "sp1" {
		t.Errorf("tool_call[0] = %+v", tcs[0])
	}
	if tcs[1].OK || tcs[1].ErrorClass != "tool_error" || tcs[1].SpanID != "" {
		t.Errorf("tool_call[1] = %+v", tcs[1])
	}

	turns, err := s.Turns()
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(turns) != 1 || turns[0].Model != "qwen" || turns[0].StartedAt == "" || turns[0].EndedAt == "" {
		t.Fatalf("Turns = %+v", turns)
	}
	if turns[0].SignalsJSON != `{"tool_errors":1,"parse_failures":0}` {
		t.Errorf("turn signals = %q", turns[0].SignalsJSON)
	}
}

func TestReadersAfterCloseError(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, Header{RunID: "RDCLOSED", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := s.Costs(); err == nil {
		t.Error("Costs after close: want error")
	}
	if _, err := s.ToolCalls(); err == nil {
		t.Error("ToolCalls after close: want error")
	}
	if _, err := s.Turns(); err == nil {
		t.Error("Turns after close: want error")
	}
}
