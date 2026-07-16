package profilehooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"corpos/internal/hooks"
	"corpos/internal/profile"
	"corpos/internal/skills"
)

func skillTree(t *testing.T) *skills.Loader {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("scratchpad-discipline.md", "---\nname: scratchpad-discipline\n---\nKeep a scratchpad.")
	write("content-routing.md", "---\nname: content-routing\n---\nRoute artifacts to homes.")
	return skills.New(dir)
}

func TestSkillInjector_InjectsDeclaredSkillsOnce(t *testing.T) {
	t.Parallel()
	inj := SkillInjector(skillTree(t), 0)
	prof := &profile.JobProfile{Name: "task-lifecycle", Tier: profile.TierLocal, Skills: []string{"scratchpad-discipline"}}

	c1 := &hooks.Context{Kind: hooks.PreUserPrompt, Profile: prof}
	inj(c1)
	if len(c1.SystemPromptAdditions) != 1 {
		t.Fatalf("first fire added %d system prompts, want 1", len(c1.SystemPromptAdditions))
	}
	if !strings.Contains(c1.SystemPromptAdditions[0], "Keep a scratchpad.") {
		t.Errorf("injected block missing the skill body: %s", c1.SystemPromptAdditions[0])
	}

	// Second fire (same session) must NOT re-inject.
	c2 := &hooks.Context{Kind: hooks.PreUserPrompt, Profile: prof}
	inj(c2)
	if len(c2.SystemPromptAdditions) != 0 {
		t.Errorf("skills re-injected on a later turn: %v", c2.SystemPromptAdditions)
	}
}

func TestSkillInjector_NoProfileOrNoSkillsIsNoop(t *testing.T) {
	t.Parallel()
	loader := skillTree(t)

	// No profile.
	c := &hooks.Context{Kind: hooks.PreUserPrompt}
	SkillInjector(loader, 0)(c)
	if len(c.SystemPromptAdditions) != 0 {
		t.Error("no-profile fire should inject nothing")
	}
	// Profile with no skills.
	c = &hooks.Context{Profile: &profile.JobProfile{Name: "git-process", Tier: profile.TierLocal}}
	SkillInjector(loader, 0)(c)
	if len(c.SystemPromptAdditions) != 0 {
		t.Error("no-skills profile should inject nothing")
	}
	// Nil loader.
	c = &hooks.Context{Profile: &profile.JobProfile{Skills: []string{"x"}}}
	SkillInjector(nil, 0)(c)
	if len(c.SystemPromptAdditions) != 0 {
		t.Error("nil loader should inject nothing")
	}
}

func TestSkillInjector_UnknownSkillNamesInjectNothing(t *testing.T) {
	t.Parallel()
	c := &hooks.Context{Profile: &profile.JobProfile{Skills: []string{"does-not-exist"}}}
	SkillInjector(skillTree(t), 0)(c)
	if len(c.SystemPromptAdditions) != 0 {
		t.Errorf("unknown skill names should select nothing, got %v", c.SystemPromptAdditions)
	}
}

func TestSkillInjector_BudgetForcesTerseTier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := "---\nname: big-skill\ndescription: a big discipline\n---\n" +
		"## Core\nThe one load-bearing rule.\n\n## Details\n" +
		strings.Repeat("verbose detail line\n", 200)
	if err := os.WriteFile(filepath.Join(dir, "big-skill.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := skills.New(dir)
	prof := &profile.JobProfile{Name: "x", Tier: profile.TierLocal, Skills: []string{"big-skill"}}

	c := &hooks.Context{Kind: hooks.PreUserPrompt, Profile: prof}
	SkillInjector(loader, 64)(c) // tiny window → terse tier
	if len(c.SystemPromptAdditions) != 1 {
		t.Fatalf("want 1 addition, got %d", len(c.SystemPromptAdditions))
	}
	got := c.SystemPromptAdditions[0]
	if !strings.Contains(got, "The one load-bearing rule.") {
		t.Errorf("terse tier should carry the ## Core: %q", got)
	}
	if strings.Contains(got, "verbose detail line") {
		t.Errorf("terse tier must not carry the full body")
	}
}

func sampleEnvelope() map[string]any {
	return map[string]any{
		"intent": map[string]any{"shape": "verify"},
		"references": []any{
			map[string]any{"token": "c1", "shape": "chain_slug"},
			map[string]any{"token": "b1", "shape": "bug_slug"},
			map[string]any{"token": "p1", "shape": "path"},
			map[string]any{"token": "l1", "shape": "library"},
			"malformed-not-a-map",
		},
	}
}

func TestPruneEnvelope_KeepsOnlyDeclaredShapes(t *testing.T) {
	t.Parallel()
	got := PruneEnvelope(sampleEnvelope(), []string{"chain_slug", "bug_slug"})
	refs := got["references"].([]any)
	if len(refs) != 2 {
		t.Fatalf("kept %d references, want 2 (chain_slug + bug_slug)", len(refs))
	}
	for _, r := range refs {
		shape := r.(map[string]any)["shape"].(string)
		if shape != "chain_slug" && shape != "bug_slug" {
			t.Errorf("kept a non-declared shape %q", shape)
		}
	}
	// The rest of the envelope survives.
	if _, ok := got["intent"]; !ok {
		t.Error("prune dropped the intent block")
	}
}

func TestPruneEnvelope_EmptyShapesPrunesAll(t *testing.T) {
	t.Parallel()
	got := PruneEnvelope(sampleEnvelope(), nil)
	if refs := got["references"].([]any); len(refs) != 0 {
		t.Errorf("empty shapes should prune all references, kept %d", len(refs))
	}
}

func TestPruneEnvelope_MalformedReturnedUnchanged(t *testing.T) {
	t.Parallel()
	env := map[string]any{"no_references": true}
	got := PruneEnvelope(env, []string{"chain_slug"})
	if _, ok := got["references"]; ok {
		t.Error("an envelope with no references array should be returned unchanged")
	}
}

func TestContextPruner_PrunesMetadataPayload(t *testing.T) {
	t.Parallel()
	prune := ContextPruner()
	c := &hooks.Context{
		Profile:  &profile.JobProfile{Name: "task-lifecycle", ContextShapes: []string{"chain_slug"}},
		Metadata: map[string]any{MetadataKeyParseContext: sampleEnvelope()},
	}
	prune(c)
	env := c.Metadata[MetadataKeyParseContext].(map[string]any)
	if refs := env["references"].([]any); len(refs) != 1 {
		t.Fatalf("pruned payload has %d references, want 1 (chain_slug)", len(refs))
	}
}

func TestContextInjector_RendersReferences(t *testing.T) {
	t.Parallel()
	c := &hooks.Context{Metadata: map[string]any{MetadataKeyParseContext: map[string]any{
		"references": []any{
			map[string]any{"token": "corpos-x", "shape": "chain_slug",
				"top_candidates": []any{map[string]any{"ID": "corpos-x", "Title": "chain corpos-x in mcp-servers"}}},
			map[string]any{"token": "bug-y", "shape": "bug_slug", "presented_as": "open bug bug-y — something broke"},
		},
	}}}
	ContextInjector()(c)
	if len(c.SystemPromptAdditions) != 1 {
		t.Fatalf("added %d system prompts, want 1", len(c.SystemPromptAdditions))
	}
	s := c.SystemPromptAdditions[0]
	for _, want := range []string{"corpos-x", "[chain_slug]", "chain corpos-x in mcp-servers", "bug-y", "[bug_slug]", "open bug bug-y"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered block missing %q:\n%s", want, s)
		}
	}
}

func TestContextInjector_SummaryVariants(t *testing.T) {
	t.Parallel()
	longTitle := strings.Repeat("x", 140)
	c := &hooks.Context{Metadata: map[string]any{MetadataKeyParseContext: map[string]any{
		"references": []any{
			// long title → truncated with an ellipsis
			map[string]any{"token": "a", "shape": "s1", "top_candidates": []any{map[string]any{"Title": longTitle}}},
			// title empty, ID fallback
			map[string]any{"token": "b", "shape": "s2", "top_candidates": []any{map[string]any{"ID": "id-b"}}},
			// no candidates, no presented_as → token+shape only (empty summary)
			map[string]any{"token": "c", "shape": "s3"},
		},
	}}}
	ContextInjector()(c)
	s := c.SystemPromptAdditions[0]
	if !strings.Contains(s, "…") {
		t.Errorf("long title should be truncated with an ellipsis: %s", s)
	}
	if strings.Contains(s, longTitle) {
		t.Error("the full over-long title should not appear")
	}
	if !strings.Contains(s, "id-b") {
		t.Errorf("ID fallback missing: %s", s)
	}
	if !strings.Contains(s, "c [s3]") {
		t.Errorf("a reference with no summary should still render token+shape: %s", s)
	}
}

func TestContextInjector_NoopCases(t *testing.T) {
	t.Parallel()
	inj := ContextInjector()
	// No metadata.
	c := &hooks.Context{}
	inj(c)
	// Metadata without the key.
	c = &hooks.Context{Metadata: map[string]any{"other": 1}}
	inj(c)
	// Empty references.
	c = &hooks.Context{Metadata: map[string]any{MetadataKeyParseContext: map[string]any{"references": []any{}}}}
	inj(c)
	// References that are all malformed (no token).
	c = &hooks.Context{Metadata: map[string]any{MetadataKeyParseContext: map[string]any{"references": []any{map[string]any{"shape": "x"}}}}}
	inj(c)
	if len(c.SystemPromptAdditions) != 0 {
		t.Errorf("no-op cases should inject nothing, got %v", c.SystemPromptAdditions)
	}
}

// TestPrunerThenInjector is the real pipeline: a full envelope is pruned to the
// profile's shapes, then only the survivors are rendered into the system prompt.
func TestPrunerThenInjector(t *testing.T) {
	t.Parallel()
	c := &hooks.Context{
		Profile:  &profile.JobProfile{Name: "task-lifecycle", ContextShapes: []string{"chain_slug"}},
		Metadata: map[string]any{MetadataKeyParseContext: sampleEnvelope()},
	}
	ContextPruner()(c)   // keeps only chain_slug
	ContextInjector()(c) // renders the survivors
	if len(c.SystemPromptAdditions) != 1 {
		t.Fatalf("want 1 injected block, got %d", len(c.SystemPromptAdditions))
	}
	s := c.SystemPromptAdditions[0]
	if !strings.Contains(s, "chain_slug") || !strings.Contains(s, "c1") {
		t.Errorf("expected the chain_slug reference rendered: %s", s)
	}
	for _, gone := range []string{"bug_slug", "path", "library"} {
		if strings.Contains(s, gone) {
			t.Errorf("pruned shape %q leaked into the injected block: %s", gone, s)
		}
	}
}

func TestContextPruner_NoopWhenNoProfileOrNoPayload(t *testing.T) {
	t.Parallel()
	prune := ContextPruner()
	// No profile.
	c := &hooks.Context{Metadata: map[string]any{MetadataKeyParseContext: sampleEnvelope()}}
	prune(c)
	if env := c.Metadata[MetadataKeyParseContext].(map[string]any); len(env["references"].([]any)) != 5 {
		t.Error("no-profile fire should leave the payload untouched")
	}
	// Profile but no metadata.
	c = &hooks.Context{Profile: &profile.JobProfile{ContextShapes: []string{"chain_slug"}}}
	prune(c) // must not panic
	// Profile + metadata without the parse_context key.
	c = &hooks.Context{Profile: &profile.JobProfile{}, Metadata: map[string]any{"other": 1}}
	prune(c)
	if _, ok := c.Metadata[MetadataKeyParseContext]; ok {
		t.Error("pruner invented a parse_context key")
	}
}
