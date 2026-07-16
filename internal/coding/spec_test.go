package coding

import "testing"

// detTask builds a minimal valid deterministic AT.
func detTask(slug string) AtomicTask {
	return AtomicTask{Slug: slug, Goal: "g", Worker: WorkerConfig{Kind: WorkerDeterministic, Command: []string{"true"}}}
}

func TestChainValidateRejectsEmpty(t *testing.T) {
	if err := (Chain{Slug: "c"}).Validate(); err == nil {
		t.Fatal("want error for zero tasks")
	}
}

func TestChainValidateRejectsDuplicateSlugs(t *testing.T) {
	c := Chain{Slug: "c", Tasks: []AtomicTask{detTask("a"), detTask("a")}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for duplicate slug")
	}
}

func TestChainValidateRejectsEmptySlug(t *testing.T) {
	c := Chain{Slug: "c", Tasks: []AtomicTask{detTask("")}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for empty slug")
	}
}

func TestChainValidateRejectsForwardInputRef(t *testing.T) {
	a := detTask("a")
	a.Inputs = map[string]InputRef{"x": {From: "b", Field: "f"}} // b is later
	c := Chain{Slug: "c", Tasks: []AtomicTask{a, detTask("b")}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for forward input ref")
	}
}

func TestChainValidateRejectsSelfInputRef(t *testing.T) {
	a := detTask("a")
	a.Inputs = map[string]InputRef{"x": {From: "a", Field: "f"}}
	c := Chain{Slug: "c", Tasks: []AtomicTask{a}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for self input ref")
	}
}

func TestChainValidateAcceptsBackwardInputRef(t *testing.T) {
	b := detTask("b")
	b.Inputs = map[string]InputRef{"x": {From: "a", Field: "f"}}
	c := Chain{Slug: "c", Tasks: []AtomicTask{detTask("a"), b}}
	if err := c.Validate(); err != nil {
		t.Fatalf("backward ref should be valid: %v", err)
	}
}

func TestChainValidateRejectsUnknownWorkerKind(t *testing.T) {
	a := detTask("a")
	a.Worker = WorkerConfig{Kind: "bogus"}
	c := Chain{Slug: "c", Tasks: []AtomicTask{a}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for unknown worker kind")
	}
}

func TestChainValidateRejectsConventionsRefOnNonModel(t *testing.T) {
	a := detTask("a")
	a.ConventionsRef = []string{"x.md"}
	c := Chain{Slug: "c", Tasks: []AtomicTask{a}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for conventions_ref on deterministic worker")
	}
}

func TestChainValidateRejectsDeterministicWithoutCommand(t *testing.T) {
	a := AtomicTask{Slug: "a", Worker: WorkerConfig{Kind: WorkerDeterministic}}
	c := Chain{Slug: "c", Tasks: []AtomicTask{a}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for deterministic worker with no command")
	}
}

func TestChainValidateRejectsNegativeMaxIterations(t *testing.T) {
	a := detTask("a")
	a.MaxIterations = -1
	c := Chain{Slug: "c", Tasks: []AtomicTask{a}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for negative max_iterations")
	}
}

func TestChainValidateAcceptsModelWithConventions(t *testing.T) {
	a := AtomicTask{Slug: "a", Goal: "g", Worker: WorkerConfig{Kind: WorkerModel}, ConventionsRef: []string{"x.md"}}
	c := Chain{Slug: "c", Tasks: []AtomicTask{a}}
	if err := c.Validate(); err != nil {
		t.Fatalf("model worker with conventions should be valid: %v", err)
	}
}

func TestMaxIterationsDefault(t *testing.T) {
	if got := (AtomicTask{}).maxIterations(); got != DefaultMaxIterations {
		t.Fatalf("default max_iterations = %d, want %d", got, DefaultMaxIterations)
	}
	if got := (AtomicTask{MaxIterations: 3}).maxIterations(); got != 3 {
		t.Fatalf("max_iterations = %d, want 3", got)
	}
}

func TestExtractionFormatDefault(t *testing.T) {
	if got := (Extraction{}).extractionFormat(); got != FormatString {
		t.Fatalf("default format = %q, want string", got)
	}
	if got := (Extraction{Format: FormatJSON}).extractionFormat(); got != FormatJSON {
		t.Fatalf("format = %q, want json", got)
	}
}

func TestTaskBySlug(t *testing.T) {
	c := Chain{Tasks: []AtomicTask{detTask("a"), detTask("b")}}
	if _, ok := c.taskBySlug("b"); !ok {
		t.Fatal("want b found")
	}
	if _, ok := c.taskBySlug("z"); ok {
		t.Fatal("want z not found")
	}
}
