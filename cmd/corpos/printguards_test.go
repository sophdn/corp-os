package main

import (
	"strings"
	"testing"

	"corpos/internal/agent"
	"corpos/internal/profile"
)

// A read-only (no verify gate) profile renders every guard OFF with the coding-worker note.
func TestPrintGuardSet_ReadOnly(t *testing.T) {
	var b strings.Builder
	printGuardSet(&b, &profile.JobProfile{Name: "code-review"})
	out := b.String()
	for _, name := range []string{"work-audit", "required-read", "fake-green", "scaffold-fab"} {
		if !strings.Contains(out, name) {
			t.Errorf("output should enumerate %q; got:\n%s", name, out)
		}
	}
	if strings.Contains(out, "[ON ]") {
		t.Errorf("a gate-less profile should arm no guards; got:\n%s", out)
	}
	if !strings.Contains(out, "NOTE:") {
		t.Errorf("a gate-less profile should explain the coding-worker activation; got:\n%s", out)
	}
}

// A coding profile (declaring a verify gate) arms every guard ON.
func TestPrintGuardSet_CodingProfile(t *testing.T) {
	var b strings.Builder
	printGuardSet(&b, &profile.JobProfile{
		Name:          "atomic-coding-chain",
		VerifyCommand: []string{"go", "test", "./..."},
	})
	out := b.String()
	if n := strings.Count(out, "[ON ]"); n != 4 {
		t.Fatalf("a coding profile should arm all 4 guards; got %d ON in:\n%s", n, out)
	}
	if strings.Contains(out, "[off]") {
		t.Errorf("a coding profile should not leave a guard off; got:\n%s", out)
	}
	if !strings.Contains(out, "scaffold-fab") {
		t.Errorf("the scaffold-fabrication guard should be enumerated; got:\n%s", out)
	}
}

// The nil-profile (unprojected) view still enumerates the catalog (all off).
func TestPrintGuardSet_NilProfile(t *testing.T) {
	var b strings.Builder
	printGuardSet(&b, nil)
	if !strings.Contains(b.String(), "full (unprojected)") {
		t.Fatalf("nil profile should render the unprojected label; got:\n%s", b.String())
	}
}

// guardActiveFor mirrors the wiring: both stages arm only when a gate can run.
func TestGuardActiveFor(t *testing.T) {
	for _, g := range agent.GuardCatalog() {
		if guardActiveFor(g, false) {
			t.Errorf("%s should be OFF without a gate", g.Name())
		}
		if !guardActiveFor(g, true) {
			t.Errorf("%s should be ON with a gate", g.Name())
		}
	}
}
