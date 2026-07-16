package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"corpos/internal/model"
)

// initEmptyPkgModuleTarget builds a temp git repo that is a real Go module with an empty
// target package (the feature absent), so an authored oracle referencing the unbuilt symbol
// fails to compile there (red). Returns the repo dir.
func initEmptyPkgModuleTarget(t *testing.T, pkgDir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(cleanGitEnv(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/feat\n\ngo 1.26\n")
	// The package exists but is empty of the feature symbol, so the oracle's reference
	// is a missing-identifier compile error (red) rather than a missing-package error.
	write(filepath.Join(pkgDir, "doc.go"), "package "+filepath.Base(pkgDir)+"\n")
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "base module")
	return dir
}

// TestLiveOracleAuthorRedOnQwen is the empirical payoff for the generative half of the
// prose→argv seam: the REAL local floor (Qwen2.5-32B) authors a Go acceptance oracle from
// a plain prose assertion, and the deterministic trust anchors hold live — the source
// parses, declares the exact acceptance func, and FAILS on the unbuilt tree (the
// anti-vacuous-oracle red-now check). Gated on CORPOS_LIVE + the local model.
func TestLiveOracleAuthorRedOnQwen(t *testing.T) {
	if os.Getenv("CORPOS_LIVE") == "" {
		t.Skip("set CORPOS_LIVE=1 to author an oracle against the local floor")
	}
	qwen := model.NewOpenAICompat("Qwen2.5-32B-Instruct-Q4_K_M.gguf", "http://localhost:8081/v1")
	if !qwen.Available() {
		t.Skip("local Qwen not available")
	}
	const pkgDir = "internal/declnum"
	repo := initEmptyPkgModuleTarget(t, pkgDir)
	gr := NewGitRepo(ExecRunner{}, repo, t.TempDir())
	checker := NewGitRedChecker(gr, gitHead(t, repo), 90*time.Second)

	author := NewOracleAuthor(qwen, checker, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	oracle, err := author.Author(ctx, PlanTask{
		Slug:      "parse-decimal",
		Goal:      "Add ParseDecimal(s string) (int, error) that parses a base-10 integer string.",
		Assertion: `ParseDecimal("42") returns (42, nil) and ParseDecimal("x") returns a non-nil error`,
	}, PackageTarget{Dir: pkgDir, PackageName: "declnum"}, nil)
	if err != nil {
		t.Fatalf("live oracle author did not converge: %v", err)
	}
	t.Logf("authored oracle %s (%s):\n%s", oracle.TestFunc, oracle.TestPath, oracle.TestSource)
	if oracle.TestFunc != "TestAccept_ParseDecimal" {
		t.Errorf("unexpected test func %q", oracle.TestFunc)
	}
	// The convergence already required RED-now (the checker passed); re-assert it directly.
	red, detail, err := checker.OracleIsRed(ctx, oracle)
	if err != nil {
		t.Fatalf("red recheck: %v", err)
	}
	if !red {
		t.Fatalf("authored oracle is NOT red on the unbuilt tree (vacuous): %s", detail)
	}
}

func gitHead(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	cmd.Env = cleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	return trimNL(string(out))
}
