package coding

import (
	"context"
	"os"
	"testing"
	"time"

	"corpos/internal/cost"
	"corpos/internal/model"
)

// liveAdapters builds the real operator tiers from env keys (source
// ~/.config/corpos/corpos.env first). The mid tier is Gemini-Flash-Lite via the
// OpenAI-compatible OpenRouter endpoint; the strong tier is Opus via Anthropic.
func liveAdapters(t *testing.T) (mid, strong model.Adapter) {
	t.Helper()
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 (and source corpos.env) to run live operator-tier measurements")
	}
	or := os.Getenv("OPENROUTER_API_KEY")
	an := os.Getenv("ANTHROPIC_API_KEY")
	if or == "" || an == "" {
		t.Skip("OPENROUTER_API_KEY and ANTHROPIC_API_KEY required for the live operator measurement")
	}
	mid = model.NewOpenAICompat("google/gemini-3.1-flash-lite", "https://openrouter.ai/api/v1",
		model.WithOACKey(or), model.WithOACRequireKey())
	strong = model.NewAnthropic("claude-opus-4-8", model.WithAnthropicKey(an), model.WithAnthropicMaxTokens(2048))
	return mid, strong
}

// compileFailureContext is a realistic operator context: the worker's attempt
// references a method (Add) that no AT has built yet — the classic Finding-0
// whack-a-mole failure. Current-package files are ground truth (only Open exists).
func compileFailureContext() OperatorContext {
	return OperatorContext{
		FailedATSlug: "store-open-impl",
		Goal:         "Implement an idempotent Close() on the SQLite store in internal/store/store.go.",
		WorkerStatus: WorkerGateFailure,
		Diagnostic:   "gate command \"go build ./internal/store\" exited 2\n# corpos/internal/store\ninternal/store/store.go:21:9: s.Add undefined (type *Store has no field or method Add)",
		GateTails:    "internal/store/store.go:21:9: s.Add undefined (type *Store has no field or method Add)\n[build failed]",
		PackageFiles: "// internal/store/store.go\npackage store\n\nimport \"database/sql\"\n\ntype Store struct { db *sql.DB }\n\nfunc Open(ctx context.Context, path string) (*Store, error) { /* ... */ return &Store{}, nil }\n\nfunc (s *Store) Close() error { return s.db.Close() }\n",
		Diff:         "+func (s *Store) Close() error {\n+\ts.Add() // ensure flushed\n+\treturn s.db.Close()\n+}\n",
		ClassifyHint: "compile error in build/vet; suggests branch_fix",
	}
}

// testAssertionContext: the impl compiles and is correct; a test references an
// unexported symbol — a test-shape problem the operator should fix with edit, NOT
// by weakening the impl.
func testAssertionContext() OperatorContext {
	return OperatorContext{
		FailedATSlug: "cli-delete-tests",
		Goal:         "Add a table-driven test for the delete command in internal/cli/cli_test.go.",
		WorkerStatus: WorkerGateFailure,
		Diagnostic:   "gate command \"go test ./internal/cli\" exited 2\ninternal/cli/cli_test.go:30:14: undefined: sqliteStore (cannot refer to unexported name)",
		GateTails:    "internal/cli/cli_test.go:30:14: undefined: sqliteStore\n[build failed]",
		PackageFiles: "// internal/cli/cli.go\npackage cli\n\nfunc Run(args []string) int { /* dispatch add/list/complete/delete */ return 0 }\n\n// the store is constructed via store.Open(ctx, path); there is no exported sqliteStore.\n",
		Diff:         "+func TestDelete(t *testing.T) {\n+\ts := &sqliteStore{} // wrong: unexported, not the constructor\n+\t_ = s\n+}\n",
		ClassifyHint: "tests fail to compile; suggests edit with a tighter spec",
	}
}

// TestLiveMidOperatorDiagnoses measures the MID tier (Gemini-Flash-Lite) in the
// operator seat on two real failure shapes. It validates the spike's load-bearing
// finding live: the cheap tier OWNS diagnosis (emits a valid, well-targeted op),
// at trivial cost. Decisions + cost are logged.
func TestLiveMidOperatorDiagnoses(t *testing.T) {
	mid, _ := liveAdapters(t)
	op := ModelOperator{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name string
		octx OperatorContext
		want map[OperatorOp]bool // acceptable ops (diagnosis is "correct" if within this set)
	}{
		{"compile-failure", compileFailureContext(), map[OperatorOp]bool{OpBranchFix: true, OpEdit: true}},
		{"test-shape", testAssertionContext(), map[OperatorOp]bool{OpEdit: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec, usage, err := op.Decide(ctx, mid, tc.octx)
			if err != nil {
				t.Fatalf("mid decide: %v", err)
			}
			c := cost.Price(mid.Model(), usage.InputTokens, usage.OutputTokens)
			t.Logf("MID decision: op=%s target=%s reason=%q tokens=%d/%d cost=$%.6f",
				dec.Op, dec.TargetAT, dec.Reason, usage.InputTokens, usage.OutputTokens, c)
			if !tc.want[dec.Op] {
				t.Errorf("mid mis-diagnosed: op=%s, acceptable=%v", dec.Op, tc.want)
			}
			if dec.Op == OpEdit && dec.Goal == "" {
				t.Error("edit decision must carry a corrected goal")
			}
		})
	}
}

// TestLiveStrongOperatorAuthors measures the STRONG tier (Opus) on the hard
// authoring case — it should produce a valid, precise edit. Confirms the rung is a
// real authoring escalation, not just a re-diagnosis.
func TestLiveStrongOperatorAuthors(t *testing.T) {
	_, strong := liveAdapters(t)
	op := ModelOperator{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dec, usage, err := op.Decide(ctx, strong, testAssertionContext())
	if err != nil {
		t.Fatalf("strong decide: %v", err)
	}
	c := cost.Price(strong.Model(), usage.InputTokens, usage.OutputTokens)
	t.Logf("STRONG decision: op=%s reason=%q tokens=%d/%d cost=$%.6f",
		dec.Op, dec.Reason, usage.InputTokens, usage.OutputTokens, c)
	if dec.Op != OpEdit || dec.Goal == "" {
		t.Errorf("strong should author a precise edit, got op=%s goal-empty=%v", dec.Op, dec.Goal == "")
	}
}

// TestLiveCoderRungAuthors measures the DeepSeek coder rung on the hard-authoring
// case (task 5): it should land a valid, precise edit like Opus, but far cheaper —
// the lever that collapses the Opus share. Logs the coder cost for comparison with
// the Opus cost from TestLiveStrongOperatorAuthors.
func TestLiveCoderRungAuthors(t *testing.T) {
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 (and source corpos.env)")
	}
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY required for the coder-rung measurement")
	}
	coder := model.NewOpenAICompat("deepseek-chat", "https://api.deepseek.com/v1",
		model.WithOACKey(key), model.WithOACRequireKey())
	op := ModelOperator{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dec, usage, err := op.Decide(ctx, coder, testAssertionContext())
	if err != nil {
		t.Fatalf("coder decide: %v", err)
	}
	c := cost.Price("deepseek-chat", usage.InputTokens, usage.OutputTokens)
	opusEstimate := cost.Price("claude-opus-4-8", usage.InputTokens, usage.OutputTokens)
	t.Logf("CODER (deepseek) decision: op=%s reason=%q tokens=%d/%d cost=$%.6f (same tokens on Opus would be $%.6f, ~%.0fx)",
		dec.Op, dec.Reason, usage.InputTokens, usage.OutputTokens, c, opusEstimate, opusEstimate/maxF(c, 1e-9))
	if dec.Op != OpEdit || dec.Goal == "" {
		t.Errorf("coder rung should author a precise edit, got op=%s goal-empty=%v", dec.Op, dec.Goal == "")
	}
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
