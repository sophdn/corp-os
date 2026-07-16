package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		p       JobProfile
		wantErr bool
	}{
		{
			name: "valid mechanical profile",
			p: JobProfile{
				Name: "task-lifecycle", Tier: TierLocal,
				Tools: []SurfaceScope{{Surface: "work", Actions: []string{"task_read"}}},
			},
		},
		{
			name: "valid whole-surface scope (empty actions)",
			p:    JobProfile{Name: "design", Tier: TierStrong, Tools: []SurfaceScope{{Surface: "knowledge"}}},
		},
		{name: "no name", p: JobProfile{Tier: TierLocal}, wantErr: true},
		{name: "blank name", p: JobProfile{Name: "   ", Tier: TierLocal}, wantErr: true},
		{name: "no tier", p: JobProfile{Name: "x"}, wantErr: true},
		{name: "unknown tier", p: JobProfile{Name: "x", Tier: Tier("frontier")}, wantErr: true},
		{
			name:    "empty surface in scope",
			p:       JobProfile{Name: "x", Tier: TierLocal, Tools: []SurfaceScope{{Surface: " "}}},
			wantErr: true,
		},
		{
			name: "duplicate surface in scope",
			p: JobProfile{Name: "x", Tier: TierLocal, Tools: []SurfaceScope{
				{Surface: "work", Actions: []string{"task_read"}},
				{Surface: "work", Actions: []string{"task_complete"}},
			}},
			wantErr: true,
		},
		{name: "mid tier ok", p: JobProfile{Name: "orchestrate", Tier: TierMid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.p.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSurfaces(t *testing.T) {
	t.Parallel()
	p := JobProfile{Tools: []SurfaceScope{{Surface: "work"}, {Surface: "fs"}, {Surface: "knowledge"}}}
	got := p.Surfaces()
	want := []string{"fs", "knowledge", "work"} // sorted
	if len(got) != len(want) {
		t.Fatalf("Surfaces() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Surfaces()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

const sampleProfile = `
name = "task-lifecycle"
duty = "transition chains, tasks, bugs on the work ledger"
tier = "local"
skills = ["scratchpad-discipline"]
context_shapes = ["chain_slug", "task_slug", "bug_slug"]

[[tools]]
surface = "work"
actions = ["task_read", "task_list", "task_start", "task_complete", "record"]
`

func TestLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "task-lifecycle.toml", sampleProfile)

	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", reg.Len())
	}
	p, ok := reg.Get("task-lifecycle")
	if !ok {
		t.Fatal("Get(task-lifecycle) not found")
	}
	if p.Tier != TierLocal {
		t.Errorf("Tier = %q, want local", p.Tier)
	}
	if len(p.Tools) != 1 || p.Tools[0].Surface != "work" {
		t.Fatalf("Tools = %+v, want one work scope", p.Tools)
	}
	if len(p.Tools[0].Actions) != 5 {
		t.Errorf("actions = %v, want 5", p.Tools[0].Actions)
	}
	if len(p.Skills) != 1 || p.Skills[0] != "scratchpad-discipline" {
		t.Errorf("Skills = %v", p.Skills)
	}
	if len(p.ContextShapes) != 3 {
		t.Errorf("ContextShapes = %v, want 3", p.ContextShapes)
	}
}

func TestLoadAbsentDir(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("Load(absent dir) = nil error, want error")
	}
}

func TestLoadIgnoresNonTOMLAndSubdirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "task-lifecycle.toml", sampleProfile)
	writeFile(t, dir, "README.md", "# not a profile")
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 (non-toml + subdir ignored)", reg.Len())
	}
}

func TestLoadMalformedTOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "bad.toml", "name = \"x\"\nthis is not = = toml")
	if _, err := Load(dir); err == nil {
		t.Fatal("Load(malformed) = nil error, want parse error")
	}
}

func TestLoadInvalidProfileRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "bad.toml", `name = "x"`+"\n"+`tier = "frontier"`)
	if _, err := Load(dir); err == nil {
		t.Fatal("Load(invalid tier) = nil error, want validation error")
	}
}

func TestLoadDuplicateNameRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "a.toml", sampleProfile)
	writeFile(t, dir, "b.toml", sampleProfile) // same name = duplicate
	if _, err := Load(dir); err == nil {
		t.Fatal("Load(duplicate name) = nil error, want error")
	}
}

func TestRegistryGetMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "p.toml", sampleProfile)
	reg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("Get(nope) = found, want not found")
	}
	names := reg.Names()
	if len(names) != 1 || names[0] != "task-lifecycle" {
		t.Fatalf("Names() = %v", names)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
