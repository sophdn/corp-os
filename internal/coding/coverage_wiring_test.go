package coding

import (
	"context"
	"testing"
	"time"
)

// greenChain is a one-AT chain whose deterministic worker writes a file and whose gate
// (real `test -f`) passes — a clean gate-green to layer the Tier-2 advisory on.
func greenChain(dir string) Chain {
	return Chain{Slug: "cov", TargetRepo: dir, Tasks: []AtomicTask{{
		Slug:   "write",
		Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"sh", "-c", "printf hi > out.txt"}},
		Gate:   [][]string{{"test", "-f", "out.txt"}},
	}}}
}

func runGreen(t *testing.T, o *Orchestrator, dir string) *RunState {
	t.Helper()
	st, err := o.Start(context.Background(), greenChain(dir), "run")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainSuccess {
		t.Fatalf("chain status = %q, want success (diag: %s)", st.Status, st.ATs[0].Diagnostic)
	}
	return st
}

// A "proposed" Tier-2 grade attaches a NON-blocking advisory flag: the AT still succeeds
// (coverage never hard-fails a real green), and the advisory rides on the AT's flags.
func TestCoverageWiring_ProposedAttachesAdvisoryButStaysSuccess(t *testing.T) {
	o := New(WithCoverageGrade())
	o.coverageFn = func(context.Context, Runner, string, string, time.Duration) CoverageGrade {
		return CoverageGrade{Verdict: "proposed", Advisory: "gate green, but changed lines x.go:12 not exercised"}
	}
	st := runGreen(t, o, t.TempDir())

	if st.ATs[0].WorkerStatus != WorkerSuccess {
		t.Fatalf("a proposed grade must NOT change the status; got %q", st.ATs[0].WorkerStatus)
	}
	f, ok := findFlag(st.ATs[0].Flags, FlagCoverageAdvisory)
	if !ok {
		t.Fatalf("a proposed grade must attach FlagCoverageAdvisory; flags = %+v", st.ATs[0].Flags)
	}
	if f.Detail == "" {
		t.Fatal("the advisory flag should carry the advisory detail")
	}
}

// A "confirmed" Tier-2 grade adds no flag — the green is fully substantiated.
func TestCoverageWiring_ConfirmedAddsNoFlag(t *testing.T) {
	o := New(WithCoverageGrade())
	o.coverageFn = func(context.Context, Runner, string, string, time.Duration) CoverageGrade {
		return CoverageGrade{Verdict: "confirmed"}
	}
	st := runGreen(t, o, t.TempDir())
	if _, ok := findFlag(st.ATs[0].Flags, FlagCoverageAdvisory); ok {
		t.Fatal("a confirmed grade must not attach a coverage advisory")
	}
}

// Without WithCoverageGrade the path is inert (the pre-feature behavior): no coverage run,
// no flag — so every existing caller and test is unaffected.
func TestCoverageWiring_DisabledByDefault(t *testing.T) {
	o := New() // no WithCoverageGrade
	if o.coverageFn != nil {
		t.Fatal("coverage grading must be off by default")
	}
	st := runGreen(t, o, t.TempDir())
	if _, ok := findFlag(st.ATs[0].Flags, FlagCoverageAdvisory); ok {
		t.Fatal("disabled coverage grading must never attach a flag")
	}
}
