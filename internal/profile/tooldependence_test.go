package profile

import (
	"strings"
	"testing"
)

// fs/sys are the tool-execution surfaces; write/edit/exec are the actions that
// make a profile unable to do its job from the substrate alone (it must mutate
// the filesystem or run a command). These fixtures mirror the library profiles.
func fsScope(actions ...string) SurfaceScope  { return SurfaceScope{Surface: "fs", Actions: actions} }
func sysScope(actions ...string) SurfaceScope { return SurfaceScope{Surface: "sys", Actions: actions} }

func TestToolDependent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		p    JobProfile
		want bool
	}{
		{
			name: "coding profile (fs write/edit + sys exec) is tool-dependent",
			p:    JobProfile{Name: "atomic-coding-chain", Tier: TierLocal, Tools: []SurfaceScope{fsScope("read", "write", "edit", "grep", "glob", "ls"), sysScope("exec")}},
			want: true,
		},
		{
			name: "fs write alone is tool-dependent",
			p:    JobProfile{Name: "writer", Tier: TierLocal, Tools: []SurfaceScope{fsScope("read", "write")}},
			want: true,
		},
		{
			name: "fs edit alone is tool-dependent",
			p:    JobProfile{Name: "editor", Tier: TierLocal, Tools: []SurfaceScope{fsScope("edit")}},
			want: true,
		},
		{
			name: "sys exec alone (no fs) is tool-dependent",
			p:    JobProfile{Name: "git-process", Tier: TierLocal, Tools: []SurfaceScope{sysScope("exec")}},
			want: true,
		},
		{
			name: "whole-fs scope (empty actions = all, includes write) is tool-dependent",
			p:    JobProfile{Name: "whole-fs", Tier: TierLocal, Tools: []SurfaceScope{fsScope()}},
			want: true,
		},
		{
			name: "whole-sys scope (empty actions = all, includes exec) is tool-dependent",
			p:    JobProfile{Name: "whole-sys", Tier: TierLocal, Tools: []SurfaceScope{sysScope()}},
			want: true,
		},
		{
			name: "read-only fs (read/grep/glob/ls) is NOT tool-dependent",
			p:    JobProfile{Name: "synthesis", Tier: TierMid, Tools: []SurfaceScope{fsScope("read", "grep", "glob", "ls")}},
			want: false,
		},
		{
			name: "sys read-only introspection (ps/ports, no exec) is NOT tool-dependent",
			p:    JobProfile{Name: "watcher", Tier: TierLocal, Tools: []SurfaceScope{sysScope("ps", "ports")}},
			want: false,
		},
		{
			name: "no fs/sys at all (work-only) is NOT tool-dependent",
			p:    JobProfile{Name: "task-lifecycle", Tier: TierLocal, Tools: []SurfaceScope{{Surface: "work"}}},
			want: false,
		},
		{
			name: "no tools at all is NOT tool-dependent",
			p:    JobProfile{Name: "empty", Tier: TierLocal},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.ToolDependent(); got != tc.want {
				t.Errorf("ToolDependent() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToollessAbort(t *testing.T) {
	t.Parallel()
	coding := JobProfile{Name: "atomic-coding-chain", Tier: TierLocal, Tools: []SurfaceScope{fsScope("read", "write", "edit"), sysScope("exec")}}
	readonly := JobProfile{Name: "synthesis", Tier: TierMid, Tools: []SurfaceScope{fsScope("read", "grep")}}

	tests := []struct {
		name          string
		p             *JobProfile
		projected     int
		degraded      bool
		wantAbort     bool
		wantReasonHas string
	}{
		{
			name:          "tool-dependent + 0 projected + degraded specs → ABORT (MCP unreachable)",
			p:             &coding,
			projected:     0,
			degraded:      true,
			wantAbort:     true,
			wantReasonHas: "unreachable",
		},
		{
			name:          "tool-dependent + 0 projected + NOT degraded → ABORT (empty catalog)",
			p:             &coding,
			projected:     0,
			degraded:      false,
			wantAbort:     true,
			wantReasonHas: "atomic-coding-chain",
		},
		{
			name:      "tool-dependent + N>0 projected → ok",
			p:         &coding,
			projected: 2,
			degraded:  false,
			wantAbort: false,
		},
		{
			name:      "read-only profile + 0 projected → ok (may still run)",
			p:         &readonly,
			projected: 0,
			degraded:  true,
			wantAbort: false,
		},
		{
			name:      "no profile (unprojected full surface) → ok",
			p:         nil,
			projected: 0,
			degraded:  true,
			wantAbort: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			abort, reason := ToollessAbort(tc.p, tc.projected, tc.degraded)
			if abort != tc.wantAbort {
				t.Fatalf("ToollessAbort abort = %v, want %v (reason %q)", abort, tc.wantAbort, reason)
			}
			if tc.wantAbort {
				if reason == "" {
					t.Fatalf("ToollessAbort aborted but gave no reason")
				}
				if tc.wantReasonHas != "" && !strings.Contains(reason, tc.wantReasonHas) {
					t.Errorf("ToollessAbort reason = %q, want it to contain %q", reason, tc.wantReasonHas)
				}
			} else if reason != "" {
				t.Errorf("ToollessAbort did not abort but gave reason %q", reason)
			}
		})
	}
}
