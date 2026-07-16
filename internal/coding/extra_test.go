package coding

import (
	"context"
	"errors"
	"testing"
)

func TestWithModelWorkerOption(t *testing.T) {
	fw := &fakeWorker{}
	o := New(WithModelWorker(fw))
	if o.model != fw {
		t.Fatal("WithModelWorker did not wire the worker")
	}
	// nil is ignored.
	o2 := New(WithModelWorker(nil))
	if o2.model != nil {
		t.Fatal("nil model worker should be ignored")
	}
}

func TestNoopRepoNoops(t *testing.T) {
	r := NoopRepo{Dir: "/work"}
	if sha, err := r.HeadSHA(context.Background()); sha != "" || err != nil {
		t.Fatalf("HeadSHA = %q,%v", sha, err)
	}
	if err := r.FastForward(context.Background(), "x"); err != nil {
		t.Fatalf("FastForward: %v", err)
	}
	if err := r.ResetTo(context.Background(), "x"); err != nil {
		t.Fatalf("ResetTo: %v", err)
	}
	ws, err := r.Open(context.Background(), "", nil)
	if err != nil || ws.Dir() != "/work" {
		t.Fatalf("Open: dir=%q err=%v", ws.Dir(), err)
	}
	if sha, ok, err := ws.Commit(context.Background(), "m"); sha != "" || ok || err != nil {
		t.Fatalf("Commit = %q,%v,%v", sha, ok, err)
	}
	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCommitError(t *testing.T) {
	ws := &fakeWorkspace{dir: t.TempDir(), commitErr: errors.New("commit blew up")}
	repo := &fakeRepo{head: "base", ws: ws}
	o := New(WithRunner(okRunner()), WithRepo(repo))
	at := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"build"}}, Gate: [][]string{{"g"}}}
	st, _ := o.Start(context.Background(), Chain{Slug: "c", TargetRepo: "x", Tasks: []AtomicTask{at}}, "r")
	st = o.RunToCompletion(context.Background(), st)
	if st.Status != ChainFailed || st.ATs[0].WorkerStatus != WorkerCommandError {
		t.Fatalf("commit error should fail the AT, got %q / %q", st.Status, st.ATs[0].WorkerStatus)
	}
}

func TestResolveInputsMissingFieldEmptyOutputs(t *testing.T) {
	o := New()
	state := &RunState{ATs: []ATRecord{{Slug: "a", Status: ATSuccess}}} // nil outputs
	spec := AtomicTask{Slug: "b", Inputs: map[string]InputRef{"in": {From: "a", Field: "f"}}}
	_, err := o.resolveInputs(state, spec)
	if err == nil {
		t.Fatal("want missing-field error")
	}
	if got := availableOutputs(nil); got != "(none)" {
		t.Fatalf("availableOutputs(nil) = %q, want (none)", got)
	}
}
