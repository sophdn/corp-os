package agent

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	"corpos/internal/tool"
)

// Scaffold-fabrication guard (bug corpos-worker-fabricates-module-to-satisfy-a-nonfunctional-
// verify-gate, dogfood 2026-06-11). When a go build/test gate runs in a dir that has NO buildable
// module, the worker can make the gate pass by MANUFACTURING the thing the gate checks: fabricate
// a dummy go.mod (+ a placeholder .go file) INTO the verify-dir so `go build ./...` succeeds. That
// is a fake-green-adjacent gaming response — making the gate green by creating its input rather
// than fixing the real code (or surfacing that the gate is non-functional).
//
// Part A (verifyGateRunnable, a spawn-time check) fails fast on a non-runnable gate so the
// worker is never handed a broken gate to game in the first place. This guard is the second,
// independent layer the bug also asks for: even if the fail-fast is bypassed (a gate that becomes
// non-runnable mid-run, or a dir check that a fabricated scaffold would otherwise satisfy), a
// GREEN gate whose pass depends on a worker-WRITTEN build-scaffold in the verify-dir is NOT a clean
// done. Like workaudit/fakegreen it is computed by the loop from the REAL dispatch record AFTER
// the gate runs (the unforgeable-footer idiom), so the worker cannot revise it by narrating
// success.
//
// It fires at StageFakeGreen (after a PASSED gate) when the turn's dispatch record contains a
// successful fs.write/edit of a build-scaffold file (go.mod, or go.sum/go.work — the module
// manifest the go gate keys off) whose path lands INSIDE the verify-dir. A worker editing a real
// go.mod elsewhere in the tree (legitimate dependency work) does not match — only a manifest
// written into the gate's own working dir, which is the fabrication signature.

// buildScaffoldNames are the module-manifest files whose presence makes a go build/test gate
// runnable — the exact artifacts the 2026-06-11 worker fabricated. Writing one of these INTO the
// verify-dir is manufacturing the gate's input. go.mod is the load-bearing one; go.sum/go.work are
// the sibling manifests a fuller fabrication would add.
var buildScaffoldNames = map[string]bool{
	"go.mod":  true,
	"go.sum":  true,
	"go.work": true,
}

// isBuildScaffoldPath reports whether p names a build-scaffold manifest (by base name).
func isBuildScaffoldPath(p string) bool {
	return buildScaffoldNames[path.Base(filepath.ToSlash(p))]
}

// withinDir reports whether target lands inside (or at) dir. Both are normalized to forward
// slashes; an empty dir means the process CWD, so every relative target is "within" it (the
// worker writes into the corpos process tree the gate also runs in). A path that escapes dir via
// .. is not within it.
func withinDir(dir, target string) bool {
	t := filepath.ToSlash(target)
	if dir == "" {
		// Gate runs in the process CWD: a relative scaffold path is within it; an absolute
		// path is only within it if it is under the CWD, which we cannot resolve sans-IO, so
		// treat a bare relative manifest as within (the fabrication case) and leave absolute
		// paths to the explicit-dir branch below.
		return !path.IsAbs(t)
	}
	d := strings.TrimRight(filepath.ToSlash(dir), "/")
	if t == d {
		return true
	}
	if !path.IsAbs(t) {
		// A relative scaffold path is resolved against the gate's working dir, so it is within
		// the verify-dir by construction.
		return true
	}
	return strings.HasPrefix(t, d+"/")
}

// scaffoldFabricationVerdict returns a non-empty verdict when the turn's dispatch record shows the
// worker SUCCESSFULLY wrote a build-scaffold manifest (go.mod/go.sum/go.work) into the verify-dir
// — the fabrication-to-pass signature. An empty verdict means no scaffold write landed in the
// gate's dir (the green is clean on this axis). Only the manifest base name + its containing dir
// matter, so it is a pure function over the dispatches + the gate's dir.
func scaffoldFabricationVerdict(dispatches []tool.Result, verifyDir string) string {
	for _, d := range dispatches {
		if !isMutatingWrite(d) {
			continue
		}
		p := toolCallPath(d.Call)
		if p == "" || !isBuildScaffoldPath(p) {
			continue
		}
		if withinDir(verifyDir, p) {
			return "fabricated a build-scaffold (" + path.Base(filepath.ToSlash(p)) +
				") in the verify-dir to pass the gate — fix the real code, don't manufacture what the gate checks"
		}
	}
	return ""
}

// ScaffoldFabricationGuard is a fake-green-stage Guard: consulted ONLY after a verify gate PASSED,
// it refuses a green the worker manufactured by writing a build-scaffold (go.mod) into the
// verify-dir. VerifyDir is the gate's working dir (empty → the process CWD), the boundary a
// fabricated manifest is measured against. It carries only that config; Assess wraps the pure
// scaffoldFabricationVerdict.
type ScaffoldFabricationGuard struct {
	// VerifyDir is the gate's working directory — a build-scaffold written here is the
	// fabrication signal (empty → the process CWD).
	VerifyDir string
}

func (ScaffoldFabricationGuard) Name() string      { return "scaffold-fab" }
func (ScaffoldFabricationGuard) Stage() GuardStage { return StageFakeGreen }
func (ScaffoldFabricationGuard) Describe() string {
	return "refuses a PASSED verify gate when the worker wrote a build-scaffold (go.mod) into the verify-dir to manufacture the gate's input instead of fixing the real code"
}
func (g ScaffoldFabricationGuard) Assess(_ context.Context, in GuardInput) GuardVerdict {
	if v := scaffoldFabricationVerdict(in.Dispatches, g.VerifyDir); v != "" {
		return fail(v)
	}
	return pass()
}
