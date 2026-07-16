package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordEscalationAndReaders(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, Header{Project: "p", Profile: "orchestrate"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.StartTurn(0, "google/gemini-3.1-flash-lite"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(0, "claude-opus-4-8", 1000, 100, 0.0225); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordToolCall(0, "work", "chain_state", "{}", "{}", true, "", 7, "gem", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEscalation(1, "escalate", "tool_errors=1", "google/gemini-3.1-flash-lite", "claude-opus-4-8", ""); err != nil {
		t.Fatal(err)
	}

	total, err := s.CostTotal()
	if err != nil || total != 0.0225 {
		t.Errorf("CostTotal = %v (err %v), want 0.0225", total, err)
	}
	turns, calls, err := s.Counts()
	if err != nil || turns != 1 || calls != 1 {
		t.Errorf("Counts = %d turns, %d calls (err %v), want 1,1", turns, calls, err)
	}
	esc, err := s.Escalations()
	if err != nil || len(esc) != 1 {
		t.Fatalf("Escalations = %+v (err %v), want 1", esc, err)
	}
	if esc[0].Edge != "escalate" || esc[0].FromModel != "google/gemini-3.1-flash-lite" || esc[0].ToModel != "claude-opus-4-8" || esc[0].Trigger != "tool_errors=1" {
		t.Errorf("escalation row = %+v", esc[0])
	}
}

func TestLoadTreeRollsUpCostAndStructure(t *testing.T) {
	dir := t.TempDir()
	root, err := Create(dir, Header{Project: "p", Profile: "orchestrate"})
	if err != nil {
		t.Fatal(err)
	}
	rootID := root.RunID()
	if err := root.RecordCost(0, "claude-opus-4-8", 1000, 100, 0.0225); err != nil {
		t.Fatal(err)
	}
	if err := root.RecordEscalation(1, "escalate", "tool_errors=1", "gem", "opus", ""); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()

	a, err := Create(dir, Header{Project: "p", Profile: "task-lifecycle", ParentRunID: rootID, Duty: "leaf A"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RecordCost(0, "qwen", 10, 5, 0); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()

	b, err := Create(dir, Header{Project: "p", Profile: "task-lifecycle", ParentRunID: rootID, Duty: "leaf B"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RecordCost(0, "google/gemini-3.1-flash-lite", 100, 20, 0.0002); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()

	// A junk DB file must be skipped, not fail the whole tree.
	if err := os.WriteFile(filepath.Join(dir, "BOGUS.db"), []byte("not a sqlite db"), 0o644); err != nil {
		t.Fatal(err)
	}

	tree, err := LoadTree(dir, rootID)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if tree.Size() != 3 {
		t.Errorf("tree size = %d, want 3", tree.Size())
	}
	if len(tree.Children) != 2 {
		t.Fatalf("root children = %d, want 2", len(tree.Children))
	}
	if tree.Header.Profile != "orchestrate" {
		t.Errorf("root profile = %q", tree.Header.Profile)
	}
	if len(tree.Escalations) != 1 {
		t.Errorf("root escalations = %d, want 1", len(tree.Escalations))
	}
	if got := tree.TreeCostUSD(); got < 0.0226 || got > 0.0228 {
		t.Errorf("tree cost = %v, want ~0.0227", got)
	}
	// Children are ordered by (time-sortable) run id; both link to the root.
	for _, c := range tree.Children {
		if c.Header.ParentRunID != rootID {
			t.Errorf("child %s parent = %q, want %q", c.Header.RunID, c.Header.ParentRunID, rootID)
		}
		if c.Header.Duty == "" {
			t.Errorf("child %s has no duty", c.Header.RunID)
		}
	}
}

func TestLoadTreeMissingRootErrors(t *testing.T) {
	if _, err := LoadTree(t.TempDir(), "NOSUCHROOT"); err == nil {
		t.Error("LoadTree with no matching root should error")
	}
}

func TestHeaderRoundTripsTreeColumns(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, Header{Project: "p", ParentRunID: "PARENT1", Profile: "bug-hunt", Duty: "find the leak"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.HeaderRow()
	if err != nil {
		t.Fatal(err)
	}
	if h.ParentRunID != "PARENT1" || h.Profile != "bug-hunt" || h.Duty != "find the leak" {
		t.Errorf("header tree columns = %+v", h)
	}
	_ = s.Close()
}
