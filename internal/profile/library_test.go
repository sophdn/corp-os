package profile

import (
	"strings"
	"testing"
)

// expectedProfiles is the §4.1 starter library: every name that must ship
// embedded, with whether it is a mechanical (local-tier) leaf profile.
var expectedProfiles = map[string]struct {
	mechanical bool
}{
	"task-lifecycle":         {mechanical: true},
	"file-sort":              {mechanical: true},
	"doc-filing":             {mechanical: true},
	"git-process":            {mechanical: true},
	"atomic-coding-chain":    {mechanical: true},
	"atomic-coding-verifier": {mechanical: true},
	"test-authoring-chain":   {mechanical: true},
	"refactor":               {mechanical: true},
	"code-review":            {},
	"bug-hunt":               {},
	"bug-fix":                {},
	"design":                 {},
	"synthesis":              {},
	"orchestrate":            {},
	"web-research":           {},
}

func TestBuiltinLoadsTheStarterLibrary(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin: %v", err)
	}
	got := reg.Names()
	// Builtin merges the vanilla library with the gitignored userlib overlay, so the
	// registry EQUALS the starter set in a vanilla clone and is a SUPERSET locally
	// (operator profiles add to it). Assert every starter profile is present rather
	// than an exact count.
	if len(got) < len(expectedProfiles) {
		t.Fatalf("Builtin has %d profiles (%v), fewer than the %d starter profiles", len(got), got, len(expectedProfiles))
	}
	for name := range expectedProfiles {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("starter library missing profile %q", name)
		}
	}
}

// TestNoBuiltinProfileDefaultsToStrong codifies the 2026-06-03 policy: Opus
// (tier=strong) is eliminated from the default work set — every starter profile
// defaults to local or mid, and strong is reached only by deliberate, proven
// tuning (or escalation). Flipping a profile to strong is a real decision that
// must update this test on purpose.

// TestOrchestrateIsReadOnlyAndDelegates locks the swap-rehearsal run-15 fix: the
// orchestrator must NOT be able to edit files or run commands, so agent.spawn is the
// ONLY path to implementation and decomposition is structural rather than merely
// prompted. An orchestrator granted write/exec just does the work itself (observed:
// run-15 produced 0 spawns and stalled). It must also carry a delegate posture.
func TestOrchestrateIsReadOnlyAndDelegates(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Get("orchestrate")
	if !ok {
		t.Fatal("orchestrate profile missing")
	}

	allowedFS := map[string]bool{"read": true, "grep": true, "glob": true, "ls": true}
	// Work writes must NOT be in scope: the orchestrator reads context and delegates;
	// an unscoped work surface let it bug_resolve a live bug it never fixed.
	workWrites := map[string]bool{"bug_resolve": true, "bug_reopen": true, "task_complete": true, "task_start": true, "forge": true, "forge_edit": true, "forge_delete": true, "roadmap_set": true, "chain_close": true}
	hasSpawn := false
	for _, ts := range p.Tools {
		switch ts.Surface {
		case "agent":
			for _, a := range ts.Actions {
				if a == "spawn" {
					hasSpawn = true
				}
			}
		case "fs":
			if len(ts.Actions) == 0 {
				t.Error("orchestrate fs scope is whole-surface (includes write/edit) — must be read-only")
			}
			for _, a := range ts.Actions {
				if !allowedFS[a] {
					t.Errorf("orchestrate must not grant fs.%s — read-only (read/grep/glob/ls) so editing requires a spawned worker", a)
				}
			}
		case "sys":
			t.Error("orchestrate must not scope sys — sys.exec would let it implement directly instead of delegating")
		case "work":
			if len(ts.Actions) == 0 {
				t.Error("orchestrate work scope is whole-surface (includes bug_resolve/forge) — must be read-only")
			}
			for _, a := range ts.Actions {
				if workWrites[a] {
					t.Errorf("orchestrate must not grant work.%s — ledger writes belong to workers, not the orchestrator (it fake-resolved a live bug with this)", a)
				}
			}
		}
	}
	if !hasSpawn {
		t.Error("orchestrate must scope agent.spawn (the delegation primitive)")
	}
	if p.SystemPrompt == "" {
		t.Fatal("orchestrate must carry a decompose-and-delegate system prompt")
	}
	// "DECLARE DONE" is the terminal-recognition directive: after a worker reports
	// success and a confirming read, the orchestrator must finish — not spawn a
	// redundant coding-chain to re-verify (the run-23 strong-bound-halt failure).
	// "RUN a build/test/gate command" is the Run-39 regression: the orchestrator must
	// NOT spawn a "run go test to verify" worker (the coding/test-authoring worker's gate
	// already ran; a run-the-gate worker can't run sys.exec and thrashes to Opus, and its
	// blocking spawn eats the orchestrator's per-turn budget → synthesis-turn timeout).
	for _, want := range []string{"agent.spawn", "DELEGATE", "READ-ONLY", "DECLARE DONE", "RUN a build/test/gate command"} {
		if !strings.Contains(p.SystemPrompt, want) {
			t.Errorf("orchestrate system prompt missing the %q directive", want)
		}
	}
}

func TestNoBuiltinProfileDefaultsToStrong(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range reg.Names() {
		p, _ := reg.Get(name)
		if p.Tier == TierStrong {
			t.Errorf("profile %q defaults to strong (Opus) — capped at mid until proven (2026-06-03 directive)", name)
		}
		if p.Tier != TierLocal && p.Tier != TierMid {
			t.Errorf("profile %q tier = %q, want local or mid", name, p.Tier)
		}
	}
}

func TestStarterProfilesValidateAndCarryAnEnvelope(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range expectedProfiles {
		p, _ := reg.Get(name)
		if err := p.Validate(); err != nil {
			t.Errorf("%s: invalid: %v", name, err)
		}
		if len(p.Tools) == 0 {
			t.Errorf("%s: declares no tool scope", name)
		}
		if p.Duty == "" {
			t.Errorf("%s: no duty", name)
		}
		// Mechanical leaf profiles run on the local tier; judgment profiles run mid.
		if want.mechanical && p.Tier != TierLocal {
			t.Errorf("%s is mechanical, tier = %q, want local", name, p.Tier)
		}
	}
}

// TestVerifierProfileIsReadOnly enforces the T5 invariant: the independent verifier
// physically cannot edit the code or tests it checks — its fs scope grants NO mutating
// action (write/edit/move/remove). It keeps read-only fs + sys.exec (to run the gate).
func TestVerifierProfileIsReadOnly(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Get("atomic-coding-verifier")
	if !ok {
		t.Fatal("atomic-coding-verifier profile missing")
	}
	mutating := map[string]bool{"write": true, "edit": true, "move": true, "remove": true}
	sawFSRead := false
	for _, scope := range p.Tools {
		if scope.Surface != "fs" {
			continue
		}
		if len(scope.Actions) == 0 {
			t.Fatal("verifier fs scope grants the WHOLE surface (empty actions) — must be read-only")
		}
		for _, a := range scope.Actions {
			if mutating[a] {
				t.Errorf("verifier fs scope grants mutating action %q — it must be read-only", a)
			}
			if a == "read" {
				sawFSRead = true
			}
		}
	}
	if !sawFSRead {
		t.Error("verifier should still grant fs.read (it reads the tree to verify)")
	}
}

// TestAtomicCodingChainCarriesFaithfulReporting enforces T6: the profile's projected
// system prompt carries the faithful-reporting / narration-is-not-execution / verify-
// independently / say-so-if-unverifiable clauses (ported from claude-code prompts.ts).
func TestAtomicCodingChainCarriesFaithfulReporting(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Get("atomic-coding-chain")
	if !ok {
		t.Fatal("atomic-coding-chain profile missing")
	}
	sp := strings.ToLower(p.SystemPrompt)
	if sp == "" {
		t.Fatal("atomic-coding-chain has no system_prompt")
	}
	for _, clause := range []struct{ name, needle string }{
		{"faithful-reporting", "report faithfully"},
		{"narration-is-not-execution", "narration is not execution"},
		{"verify-independently", "verify independently"},
		{"edit-in-place", "edit in place"},
		{"edit-old-new-differ", "must differ"},
		{"minimal-change", "smallest change that turns the gate green"},
		{"focused-investigation", "investigate narrowly"},
		{"stop-when-done", "stop when done"},
		{"say-so-if-unverifiable", "unverified"},
	} {
		if !strings.Contains(sp, clause.needle) {
			t.Errorf("atomic-coding-chain system prompt missing the %s clause (%q)", clause.name, clause.needle)
		}
	}
	// A coding worker needs more tool rounds than a generic worker to read → locate →
	// edit → verify in one conversation (the t3b dogfood was cut off at the default 12).
	if p.MaxToolRounds != 24 {
		t.Errorf("atomic-coding-chain max_tool_rounds = %d, want 24", p.MaxToolRounds)
	}
}

// TestCodingProfilesScopeSelfTestRuns (corrected after rehearsal run-2): the coding and
// test-authoring workers must NOT try to self-run build/test — they have no sys.exec (the
// operator-seat organ owns the gate and runs it after declare-done, feeding failures back).
// The old chain-378 approach (keep exec, steer the worker to package-scoped self-runs)
// BACKFIRED: sys.exec is risk-blocked, so the worker followed the "scope the run" instruction,
// hit a tool_error, and thrashed instead of relying on the gate (run-2: 10-min non-convergence
// on real bug 1020). So the guard now enforces the corrected design — the prompt says "don't
// self-run, rely on the auto-gate after declare-done", and the profile grants no sys.exec.
func TestCodingProfilesScopeSelfTestRuns(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"atomic-coding-chain", "test-authoring-chain"} {
		p, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s profile missing", name)
		}
		sp := strings.ToLower(p.SystemPrompt)
		// The worker must be told NEVER to run build/test itself…
		if !strings.Contains(sp, "never try to run") {
			t.Errorf("%s prompt must tell the worker NEVER to run build/test itself", name)
		}
		// …and pointed at the auto-gate that runs after declare-done.
		if !strings.Contains(sp, "declare done") || !strings.Contains(sp, "gate") {
			t.Errorf("%s prompt must steer the worker to the auto-gate after declare-done", name)
		}
		// The profile must NOT grant sys.exec — that is the risk-blocked tool the worker
		// thrashed on; it is removed so the worker relies on the organ gate.
		for _, ss := range p.Tools {
			if ss.Surface != "sys" {
				continue
			}
			for _, a := range ss.Actions {
				if a == "exec" {
					t.Errorf("%s must NOT grant sys.exec (worker relies on the organ gate, not self-exec)", name)
				}
			}
		}
	}
}

// TestCodingProfilesTellWorkerToCopyOldStringVerbatim enforces lever #2 of the
// fs.edit-reliability fix (Run-55). The ground-truth failure was reproduction drift:
// the worker regenerated old_string from memory and abbreviated it, so the exact
// match missed even with the whole file visible. The prompt must steer it to COPY
// old_string verbatim (never retype from memory), keep it a short unique snippet, and
// — when an edit misses — copy the closest-actual-text hint fs.edit now returns rather
// than guess again. Paired with the nearest-text hint (commit ebc799c), this converts
// a drifted edit into an immediate verbatim retry instead of a grep-and-re-read hunt.
func TestCodingProfilesTellWorkerToCopyOldStringVerbatim(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"atomic-coding-chain", "test-authoring-chain"} {
		p, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s profile missing", name)
		}
		sp := strings.ToLower(p.SystemPrompt)
		for _, clause := range []struct{ name, needle string }{
			{"copy-verbatim", "copy old_string verbatim"},
			{"not-from-memory", "never retype it from memory"},
			{"shortest-unique", "shortest unique snippet"},
			{"use-the-hint", "closest actual text"},
			{"insertion-anchor", "one short unique existing line"},
		} {
			if !strings.Contains(sp, clause.needle) {
				t.Errorf("%s prompt missing the %s clause (%q) — the copy-verbatim discipline (lever #2)", name, clause.name, clause.needle)
			}
		}
	}
}

// TestMechanicalLifecycleCarriesNoBroadWriteSurface enforces the §4.4.2 invariant
// for the transition worker: task-lifecycle may carry the lifecycle/record writes
// but NOT the broad forge/roadmap/trained_model/suggestion write surface.
func TestMechanicalLifecycleCarriesNoBroadWriteSurface(t *testing.T) {
	t.Parallel()
	reg, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := reg.Get("task-lifecycle")
	var work []string
	for _, ts := range p.Tools {
		if ts.Surface == "work" {
			work = ts.Actions
		}
	}
	if work == nil {
		t.Fatal("task-lifecycle has no work scope")
	}
	forbidden := []string{"forge", "forge_edit", "forge_delete", "roadmap_set", "roadmap_insert", "trained_model_promote", "suggestion_resolve"}
	have := map[string]bool{}
	for _, a := range work {
		have[a] = true
	}
	for _, f := range forbidden {
		if have[f] {
			t.Errorf("task-lifecycle leaks the broad write action %q (lifecycle subset only — §4.4.2)", f)
		}
	}
	// It must still carry the lifecycle essentials.
	for _, need := range []string{"task_complete", "task_start", "record"} {
		if !have[need] {
			t.Errorf("task-lifecycle missing lifecycle action %q", need)
		}
	}
}
