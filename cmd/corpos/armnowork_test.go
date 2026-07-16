package main

import (
	"testing"

	"corpos/internal/profile"
)

func TestArmNoWorkAudit(t *testing.T) {
	mutating := &profile.JobProfile{Name: "bug-fix", Tools: []profile.SurfaceScope{{Surface: "fs", Actions: []string{"read", "write", "edit"}}}}
	readonly := &profile.JobProfile{Name: "code-review", Tools: []profile.SurfaceScope{{Surface: "fs", Actions: []string{"read", "grep"}}}}

	if !armNoWorkAudit(mutating) {
		t.Error("a file-mutating profile under a verify gate should arm the no-work audit")
	}
	if armNoWorkAudit(readonly) {
		t.Error("a read-only profile must not arm the no-work audit (it legitimately mutates nothing)")
	}
	if armNoWorkAudit(nil) {
		t.Error("an unprojected run (nil profile) must not arm the no-work audit")
	}
}
