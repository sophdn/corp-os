package coding

import (
	"context"
	"testing"
	"time"
)

// TestGitRedChecker exercises the production red-now trust anchor against a REAL git Go
// module (the same fixture the red-before-green replay uses): an oracle referencing a
// missing symbol fails to compile (red), while an oracle that passes on the existing tree
// is correctly NOT red (it would be a vacuous acceptance gate).
func TestGitRedChecker(t *testing.T) {
	repo, base := initGoModuleTarget(t) // a Go module with package calc (Add is buggy: a-b)
	gr := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	c := NewGitRedChecker(gr, base, 60*time.Second)

	red, _, err := c.OracleIsRed(context.Background(), AuthoredOracle{
		TestPath:   "calc/missing_accept_test.go",
		TestFunc:   "TestAccept_Missing",
		TestSource: "package calc\nimport \"testing\"\nfunc TestAccept_Missing(t *testing.T){ if Mul(2,3)!=6 { t.Fatal(\"x\") } }\n",
	})
	if err != nil {
		t.Fatalf("red check: %v", err)
	}
	if !red {
		t.Fatal("an oracle referencing a missing symbol must be RED (compile failure)")
	}

	notRed, _, err := c.OracleIsRed(context.Background(), AuthoredOracle{
		TestPath:   "calc/existing_accept_test.go",
		TestFunc:   "TestAccept_Existing",
		TestSource: "package calc\nimport \"testing\"\nfunc TestAccept_Existing(t *testing.T){ if Add(5,3)!=2 { t.Fatal(\"x\") } }\n",
	})
	if err != nil {
		t.Fatalf("green check: %v", err)
	}
	if notRed {
		t.Fatal("an oracle that passes on the existing tree must NOT be red (it is vacuous)")
	}
}
