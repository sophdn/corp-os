package tool

import "testing"

func TestTallyCountsEachClass(t *testing.T) {
	results := []Result{
		{ErrorClass: ClassNone},
		{ErrorClass: ClassTool},
		{ErrorClass: ClassParse},
		{ErrorClass: ClassTransient},
		{ErrorClass: ClassFatal},
	}
	got := Tally(results)
	// tool_error + parse + transient + fatal all count as tool errors.
	if got.ToolErrors != 4 {
		t.Errorf("ToolErrors = %d, want 4", got.ToolErrors)
	}
	if got.ParseFailures != 1 {
		t.Errorf("ParseFailures = %d, want 1", got.ParseFailures)
	}
	if got.TransientErrors != 1 {
		t.Errorf("TransientErrors = %d, want 1", got.TransientErrors)
	}
}

func TestTallyEmpty(t *testing.T) {
	if got := Tally(nil); got != (ErrorTally{}) {
		t.Errorf("Tally(nil) = %+v, want zero", got)
	}
}

// TestTallyUsageNotCountedAsToolError pins the escalation-classification fix: a
// ClassUsage failure increments UsageErrors but NOT ToolErrors, so a worker's
// recoverable usage slip never trips the repeated_tool_error escalation.
func TestTallyUsageNotCountedAsToolError(t *testing.T) {
	results := []Result{
		{ErrorClass: ClassUsage},
		{ErrorClass: ClassUsage},
		{ErrorClass: ClassNone},
		{ErrorClass: ClassTool},
	}
	got := Tally(results)
	if got.ToolErrors != 1 {
		t.Errorf("ToolErrors = %d, want 1 (only the ClassTool fault; usage excluded)", got.ToolErrors)
	}
	if got.UsageErrors != 2 {
		t.Errorf("UsageErrors = %d, want 2", got.UsageErrors)
	}
}
