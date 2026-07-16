// Package coding gives corpos a native, gate-verified atomic coding chain: a goal
// is decomposed into ordered atomic tasks (ATs); each AT runs through a worker
// behind an orchestrator-owned gate; on gate failure an operator escalates an
// intervention; green ATs integrate onto a single branch. It subsumes the Python
// atomic-tasks research bench (see docs/ATOMIC_CODING_CHAIN.md for the behavioral
// contract and the concept map).
//
// The package is COMPOSITION over the existing corpos runtime, not a parallel
// orchestrator: a model worker is an agent.Spawner-spawned scoped sub-agent
// (writing files through the fs tool.Provider, never fenced-block prose); the
// operator seat is the internal/router escalation ladder; gate-bypass safety is
// internal/risk. This file defines the spec types (the chain/AT domain model) and
// their validation invariants.
package coding

import (
	"fmt"
	"path/filepath"
	"strings"
)

// WorkerKind discriminates how an AT is executed.
type WorkerKind string

const (
	// WorkerDeterministic runs one fixed command, then the gate. One-shot.
	WorkerDeterministic WorkerKind = "deterministic"
	// WorkerModel drives a model in a write→gate→revise loop. Files are written
	// through fs tool calls (the agent loop's tool.Provider), not parsed prose.
	WorkerModel WorkerKind = "model"
)

// validWorkerKinds is the closed worker taxonomy.
var validWorkerKinds = map[WorkerKind]bool{WorkerDeterministic: true, WorkerModel: true}

// InputRef references a named output produced by an upstream AT.
type InputRef struct {
	// From is the slug of the upstream AT producing the output.
	From string `toml:"from" json:"from"`
	// Field is the name of that AT's output_contract extraction.
	Field string `toml:"field" json:"field"`
}

// Assertion is a command that must exit 0 as a post-success post-condition.
type Assertion struct {
	Name        string   `toml:"name" json:"name"`
	Command     []string `toml:"command" json:"command"`
	Description string   `toml:"description,omitempty" json:"description,omitempty"`
}

// ExtractionFormat is how an extraction's stdout is interpreted.
type ExtractionFormat string

const (
	// FormatString trims trailing whitespace from stdout.
	FormatString ExtractionFormat = "string"
	// FormatJSON parses stdout as JSON.
	FormatJSON ExtractionFormat = "json"
)

// Extraction is a command whose stdout becomes a named typed output that
// downstream ATs may reference via InputRef.
type Extraction struct {
	Name    string           `toml:"name" json:"name"`
	Command []string         `toml:"command" json:"command"`
	Format  ExtractionFormat `toml:"format,omitempty" json:"format,omitempty"`
}

// OutputContract is the post-success verification: assertions (must all exit 0)
// and extractions (stdout → named outputs). Both run against the post-success
// workspace.
type OutputContract struct {
	Assertions  []Assertion  `toml:"assertions,omitempty" json:"assertions,omitempty"`
	Extractions []Extraction `toml:"extractions,omitempty" json:"extractions,omitempty"`
}

// WorkerConfig is the per-AT worker configuration. Kind discriminates the union;
// the Command/Timeout fields apply to a deterministic worker, the SystemPrompt/
// InlineSiblingDocs fields to a model worker.
type WorkerConfig struct {
	Kind WorkerKind `toml:"kind" json:"kind"`

	// Deterministic worker:
	Command        []string `toml:"command,omitempty" json:"command,omitempty"`
	TimeoutSeconds int      `toml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`

	// Model worker:
	SystemPrompt      string `toml:"system_prompt,omitempty" json:"system_prompt,omitempty"`
	InlineSiblingDocs bool   `toml:"inline_sibling_docs,omitempty" json:"inline_sibling_docs,omitempty"`
}

// AtomicTask is one atomic, gate-verified unit of work.
type AtomicTask struct {
	// Slug is unique within the chain.
	Slug string `toml:"slug" json:"slug"`
	// Goal is the natural-language instruction to the worker.
	Goal string `toml:"goal" json:"goal"`
	// Inputs are named references to upstream ATs' extracted outputs.
	Inputs map[string]InputRef `toml:"inputs,omitempty" json:"inputs,omitempty"`
	// Workspace is the glob allowlist of writable paths (empty = no writes).
	Workspace []string `toml:"workspace,omitempty" json:"workspace,omitempty"`
	// Protected is the glob set the worker must NOT modify — typically the gate's
	// own oracle test files. A worker attempt that touches a protected path is a
	// gate-integrity violation and is rejected (the gate is immutable).
	Protected []string `toml:"protected,omitempty" json:"protected,omitempty"`
	// AllowTestOnlyDiff opts a genuine test-authoring AT out of the test-only-diff
	// fake-green guard. For a bug-fix AT (the default) a diff that touches only test
	// files means the production code was never fixed, so a green gate is hollow and
	// the run is rejected (WorkerTestOnlyDiff). Set true only when the AT's job IS to
	// add tests against already-correct code.
	AllowTestOnlyDiff bool `toml:"allow_test_only_diff,omitempty" json:"allow_test_only_diff,omitempty"`
	// Gate is the ordered list of commands (argv vectors) that verify success.
	Gate [][]string `toml:"gate,omitempty" json:"gate,omitempty"`
	// Oracles are acceptance-test files (repo-relative path → Go source) the
	// orchestrator SEEDS into the worktree and commits BEFORE the worker runs, so the
	// gate has its red-before-green oracle present. This is the executable end of the
	// gate-authoring bridge: a feature has no pre-existing test, so the authored oracle
	// is carried here rather than expected to already exist on the tree. Every oracle
	// path MUST be covered by Protected (the worker cannot edit its own oracle) and be
	// a clean relative path — validate() enforces both; an unprotected or escaping
	// seeded oracle is a fake-green hole.
	Oracles map[string]string `toml:"oracles,omitempty" json:"oracles,omitempty"`
	// OutputContract is the post-success verification + extraction.
	OutputContract OutputContract `toml:"output_contract,omitempty" json:"output_contract,omitempty"`
	// Worker selects + configures how the AT runs.
	Worker WorkerConfig `toml:"worker" json:"worker"`
	// ConventionsRef are paths whose contents are injected into a model worker's
	// prompt (meaningful only for the model worker kind).
	ConventionsRef []string `toml:"conventions_ref,omitempty" json:"conventions_ref,omitempty"`
	// MaxIterations bounds the write→gate→revise loop (≥1).
	MaxIterations int `toml:"max_iterations,omitempty" json:"max_iterations,omitempty"`
}

// Chain is an ordered list of ATs over a single target git repo.
type Chain struct {
	Slug       string       `toml:"slug" json:"slug"`
	TargetRepo string       `toml:"target_repo" json:"target_repo"`
	BaseBranch string       `toml:"base_branch,omitempty" json:"base_branch,omitempty"`
	Tasks      []AtomicTask `toml:"tasks" json:"tasks"`
}

// DefaultMaxIterations is the write→gate→revise ceiling applied when an AT omits
// MaxIterations.
const DefaultMaxIterations = 5

// Validate checks a chain is well-formed and returns the first problem found:
// at least one task, unique slugs, every input referencing an EARLIER task (no
// forward or self references), valid worker kinds, and per-AT field consistency.
func (c Chain) Validate() error {
	if len(c.Tasks) == 0 {
		return fmt.Errorf("chain %q must have at least one task", c.Slug)
	}
	seen := map[string]bool{}
	for i := range c.Tasks {
		t := c.Tasks[i]
		if strings.TrimSpace(t.Slug) == "" {
			return fmt.Errorf("chain %q has a task at position %d with no slug", c.Slug, i)
		}
		if seen[t.Slug] {
			return fmt.Errorf("chain %q has duplicate task slug %q", c.Slug, t.Slug)
		}
		if err := t.validate(seen); err != nil {
			return err
		}
		seen[t.Slug] = true
	}
	return nil
}

// validate checks one AT against the set of slugs already seen earlier in the
// chain (so input references can only point backward).
func (t AtomicTask) validate(earlier map[string]bool) error {
	if !validWorkerKinds[t.Worker.Kind] {
		return fmt.Errorf("task %q has unknown worker kind %q (want deterministic|model)", t.Slug, t.Worker.Kind)
	}
	if t.Worker.Kind != WorkerModel && len(t.ConventionsRef) > 0 {
		return fmt.Errorf("task %q: conventions_ref is meaningful only for a model worker, got %q", t.Slug, t.Worker.Kind)
	}
	if t.Worker.Kind == WorkerDeterministic && len(t.Worker.Command) == 0 {
		return fmt.Errorf("task %q: a deterministic worker requires a command", t.Slug)
	}
	if t.MaxIterations < 0 {
		return fmt.Errorf("task %q: max_iterations must be >= 1, got %d", t.Slug, t.MaxIterations)
	}
	for path := range t.Oracles {
		if clean := filepath.Clean(path); path == "" || clean != path || filepath.IsAbs(path) || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("task %q: oracle path %q must be a clean repo-relative path (no absolute, no \"..\")", t.Slug, path)
		}
		if !matchesAny(path, t.Protected) {
			return fmt.Errorf("task %q: oracle path %q is not covered by protected — a seeded oracle the worker can edit is a fake-green hole", t.Slug, path)
		}
	}
	for name, ref := range t.Inputs {
		if ref.From == t.Slug {
			return fmt.Errorf("task %q input %q references itself", t.Slug, name)
		}
		if !earlier[ref.From] {
			return fmt.Errorf("task %q input %q references %q, which is not an earlier task in the chain", t.Slug, name, ref.From)
		}
	}
	return nil
}

// maxIterations returns the effective write→gate→revise ceiling for an AT.
func (t AtomicTask) maxIterations() int {
	if t.MaxIterations <= 0 {
		return DefaultMaxIterations
	}
	return t.MaxIterations
}

// extractionFormat returns the effective format for an extraction (string default).
func (e Extraction) extractionFormat() ExtractionFormat {
	if e.Format == FormatJSON {
		return FormatJSON
	}
	return FormatString
}

// taskBySlug returns the task with the given slug, or false.
func (c Chain) taskBySlug(slug string) (AtomicTask, bool) {
	for i := range c.Tasks {
		if c.Tasks[i].Slug == slug {
			return c.Tasks[i], true
		}
	}
	return AtomicTask{}, false
}
