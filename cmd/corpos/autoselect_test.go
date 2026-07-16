package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"corpos/internal/profile"
	"corpos/internal/tool"
)

// envProvider stands in for the toolkit: it answers parse_context with a one-shape
// envelope and refuses everything else, so the auto-select path can be driven without
// a live substrate. shape="" makes parse_context fail (the down-substrate case).
type envProvider struct{ shape string }

func (e envProvider) Dispatch(_ context.Context, c tool.Call) tool.Result {
	if c.Surface == "knowledge" && c.Action == "parse_context" && e.shape != "" {
		return tool.Result{OK: true, Value: map[string]any{
			"references": []any{map[string]any{"shape": e.shape, "token": "x"}},
		}}
	}
	return tool.Result{OK: false}
}

func TestAutoSelectProfile_NoPromptUsesDefault(t *testing.T) {
	name, sel := autoSelectProfile(context.Background(), envProvider{}, "", "orchestrate", "", time.Second, io.Discard)
	if name != "orchestrate" || sel != "" {
		t.Fatalf("no-prompt → default with no dataset row, got (%q, %q)", name, sel)
	}
}

func TestAutoSelectProfile_KeywordMatchAndDataset(t *testing.T) {
	name, sel := autoSelectProfile(context.Background(), envProvider{}, "review the diff for correctness", "orchestrate", "", time.Second, io.Discard)
	if name != "code-review" {
		t.Fatalf("want code-review, got %q", name)
	}
	var got profile.Selection
	if err := json.Unmarshal([]byte(sel), &got); err != nil {
		t.Fatalf("selection JSON did not unmarshal: %v (%s)", err, sel)
	}
	if got.Profile != "code-review" || got.Fallback || got.Score == 0 {
		t.Fatalf("dataset row = %+v, want non-fallback code-review with score>0", got)
	}
}

func TestAutoSelectProfile_DownSubstrateStillSelects(t *testing.T) {
	// parse_context fails (shape="") → selection rests on keyword signals alone.
	name, _ := autoSelectProfile(context.Background(), envProvider{shape: ""}, "commit and push", "orchestrate", "", time.Second, io.Discard)
	if name != "git-process" {
		t.Fatalf("want git-process via keywords with no envelope, got %q", name)
	}
}

func TestAutoSelectProfile_NoMatchFallsBack(t *testing.T) {
	name, sel := autoSelectProfile(context.Background(), envProvider{}, "hello there friend", "orchestrate", "", time.Second, io.Discard)
	if name != "orchestrate" {
		t.Fatalf("no-match → default orchestrate, got %q", name)
	}
	var got profile.Selection
	if err := json.Unmarshal([]byte(sel), &got); err != nil {
		t.Fatalf("selection JSON did not unmarshal: %v", err)
	}
	if !got.Fallback {
		t.Fatalf("no-match should record fallback=true, got %+v", got)
	}
}
