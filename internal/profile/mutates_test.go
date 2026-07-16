package profile

import "testing"

func TestMutatesFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		tools []SurfaceScope
		want  bool
	}{
		{"fs write+edit (bug-fix/coder)", []SurfaceScope{{Surface: "fs", Actions: []string{"read", "write", "edit"}}}, true},
		{"fs read-only (code-review)", []SurfaceScope{{Surface: "fs", Actions: []string{"read", "grep", "glob", "ls"}}}, false},
		{"whole fs surface", []SurfaceScope{{Surface: "fs"}}, true},
		{"fs move only", []SurfaceScope{{Surface: "fs", Actions: []string{"move"}}}, true},
		{"fs remove only", []SurfaceScope{{Surface: "fs", Actions: []string{"remove"}}}, true},
		{"sys exec only (git-process) — not an fs write", []SurfaceScope{{Surface: "sys", Actions: []string{"exec"}}}, false},
		{"no fs at all (orchestrate-ish)", []SurfaceScope{{Surface: "work"}, {Surface: "agent", Actions: []string{"spawn"}}}, false},
		{"no tools", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (JobProfile{Name: "p", Tools: c.tools}).MutatesFiles(); got != c.want {
				t.Errorf("MutatesFiles()=%v, want %v", got, c.want)
			}
		})
	}
}

// TestMutatorSurfaceDropped pins the bug-1080 fail-loud predicate: a file-mutating profile
// whose PROJECTED surfaces no longer include fs (the enum-less raw fs spec was dropped under
// action-scoping while sys survived) is a misconfiguration; a profile that either doesn't
// mutate or whose fs survived is not.
func TestMutatorSurfaceDropped(t *testing.T) {
	t.Parallel()
	coder := &JobProfile{Name: "atomic-coding-chain", Tools: []SurfaceScope{
		{Surface: "fs", Actions: []string{"read", "write", "edit"}},
		{Surface: "sys", Actions: []string{"exec"}},
	}}
	readonly := &JobProfile{Name: "code-review", Tools: []SurfaceScope{
		{Surface: "fs", Actions: []string{"read", "grep"}},
	}}
	cases := []struct {
		name      string
		p         *JobProfile
		projected []string
		want      bool
	}{
		{"coder, fs dropped, sys survived → dropped (the 1080 trap)", coder, []string{"sys"}, true},
		{"coder, fs survived → ok", coder, []string{"fs", "sys"}, false},
		{"coder, nothing projected → dropped", coder, nil, true},
		{"read-only profile, fs dropped → not a mutator, exempt", readonly, []string{"sys"}, false},
		{"nil profile → false", nil, []string{"sys"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MutatorSurfaceDropped(c.p, c.projected); got != c.want {
				t.Errorf("MutatorSurfaceDropped()=%v, want %v", got, c.want)
			}
		})
	}
}
