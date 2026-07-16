package agent

import (
	"context"
	"strings"
	"testing"

	"corpos/internal/model"
	"corpos/internal/profile"
	"corpos/internal/tool"
)

// readForever is an adapter that ALWAYS emits a read-only fs.grep tool call and
// never spawns — the orchestrate over-investigation signature (bug 1072). It
// records every transcript it is asked to complete so a test can inspect whether
// the spawn-forcing reminder was injected near the tail.
type readForever struct{ seen [][]model.ChatMessage }

func (r *readForever) Model() string   { return "readforever" }
func (r *readForever) Available() bool { return true }
func (r *readForever) Complete(_ context.Context, msgs []model.ChatMessage, _ []tool.Spec) (model.Response, error) {
	r.seen = append(r.seen, append([]model.ChatMessage(nil), msgs...))
	return model.Response{
		Model:      "readforever",
		ToolCalls:  []tool.Call{{ID: "g", Surface: "fs", Action: "grep"}},
		StopReason: model.StopToolUse,
	}, nil
}

// orchestrateProfile is a minimal spawn-capable profile: it scopes the agent
// surface (so spawnCapable() is true) plus read-only fs — the orchestrate shape.
func orchestrateProfile() *profile.JobProfile {
	return &profile.JobProfile{
		Name: "orchestrate",
		Tier: profile.TierMid,
		Tools: []profile.SurfaceScope{
			{Surface: "agent", Actions: []string{"spawn"}},
			{Surface: "fs", Actions: []string{"read", "grep"}},
		},
	}
}

// leafProfile is a non-spawn-capable worker profile (no agent surface).
func leafProfile() *profile.JobProfile {
	return &profile.JobProfile{
		Name:  "atomic-coding-chain",
		Tier:  profile.TierLocal,
		Tools: []profile.SurfaceScope{{Surface: "fs", Actions: []string{"read", "write", "edit"}}},
	}
}

// TestSpawnCapableDetection exercises every spawn-capable shape: a profile scoping
// the agent surface action-explicitly, one scoping the whole agent surface (no
// Actions), an agent surface WITHOUT the spawn action, a non-agent profile, and a
// nil profile. The memo means a second call must agree.
func TestSpawnCapableDetection(t *testing.T) {
	cases := []struct {
		name string
		p    *profile.JobProfile
		want bool
	}{
		{"explicit spawn action", orchestrateProfile(), true},
		{"whole agent surface", &profile.JobProfile{Name: "o", Tier: profile.TierMid,
			Tools: []profile.SurfaceScope{{Surface: "agent"}}}, true},
		{"agent surface no spawn action", &profile.JobProfile{Name: "o", Tier: profile.TierMid,
			Tools: []profile.SurfaceScope{{Surface: "agent", Actions: []string{"inspect"}}}}, false},
		{"leaf worker", leafProfile(), false},
		{"nil profile", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []Option{}
			if tc.p != nil {
				opts = append(opts, WithProfile(tc.p))
			}
			l := New(single(&readForever{}), &fakeProvider{}, nil, opts...)
			if got := l.spawnCapable(); got != tc.want {
				t.Fatalf("spawnCapable() = %v, want %v", got, tc.want)
			}
			if got := l.spawnCapable(); got != tc.want { // memoised second call agrees
				t.Fatalf("memoised spawnCapable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpawnForcingFiresForOrchestrateNeverSpawns: a spawn-capable (orchestrate)
// agent that reads forever without ever calling agent.spawn gets a forcing
// reminder injected near the transcript tail once it crosses the budget. This is
// the structural must-spawn guard for bug 1072 — prompt guidance alone proved
// insufficient, so the loop deterministically forces the issue.
func TestSpawnForcingFiresForOrchestrateNeverSpawns(t *testing.T) {
	rec := &readForever{}
	loop := New(single(rec), &fakeProvider{}, nil,
		WithProfile(orchestrateProfile()),
		WithMaxRounds(10),
		WithSpawnForcing(2),
	)

	if _, err := loop.Run(context.Background(), "fix the failing test in dispatch.go"); err == nil {
		t.Fatal("a read-forever orchestrator should hit max rounds")
	}

	// The forcing reminder must have been surfaced near the tail at some model turn
	// once the read budget was crossed.
	fired := false
	for _, msgs := range rec.seen {
		if len(msgs) > 0 && strings.HasPrefix(msgs[len(msgs)-1].Content, spawnForceMarker) {
			fired = true
			break
		}
	}
	if !fired {
		t.Fatal("spawn-forcing reminder should fire for a spawn-capable agent that never spawns")
	}

	// Never stacked: at most one forcing reminder lives in the transcript.
	count := 0
	for _, m := range loop.Transcript() {
		if strings.HasPrefix(m.Content, spawnForceMarker) {
			count++
		}
	}
	if count > 1 {
		t.Errorf("spawn-forcing reminders must not stack: found %d", count)
	}
}

// TestSpawnForcingDoesNotRefireAfterSpawn: the must-spawn guard is a PRE-first-spawn
// backstop. Once the orchestrator has delegated, the post-spawn confirming reads the
// declare-done path needs must NOT re-arm the nudge — re-firing it nags the orchestrator
// back into a redundant spawn and burns the frontier rung to a strong-bound halt (bug
// spawn-now-nudge-fires-on-post-success-confirming-reads). With spawnForceEvery=2 and
// THREE post-spawn read rounds, the pre-fix code re-fired at round 3; it must not.
func TestSpawnForcingDoesNotRefireAfterSpawn(t *testing.T) {
	m := model.NewEcho("orch",
		model.Response{ToolCalls: []tool.Call{{ID: "s", Surface: "agent", Action: "spawn"}}, StopReason: model.StopToolUse}, // round 0: delegate
		model.Response{ToolCalls: []tool.Call{{ID: "c0", Surface: "fs", Action: "ls"}}, StopReason: model.StopToolUse},      // round 1: confirm
		model.Response{ToolCalls: []tool.Call{{ID: "c1", Surface: "fs", Action: "grep"}}, StopReason: model.StopToolUse},    // round 2: confirm
		model.Response{ToolCalls: []tool.Call{{ID: "c2", Surface: "fs", Action: "grep"}}, StopReason: model.StopToolUse},    // round 3: confirm (pre-fix re-fired here)
		model.Response{Text: "done", StopReason: model.StopEndTurn},                                                         // round 4: declare done
	)
	loop := New(single(m), &fakeProvider{}, nil,
		WithProfile(orchestrateProfile()),
		WithMaxRounds(10),
		WithSpawnForcing(2),
	)
	res, err := loop.Run(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
	// No spawn-force reminder anywhere: it must never re-arm on post-spawn reads.
	for _, msg := range loop.Transcript() {
		if strings.HasPrefix(msg.Content, spawnForceMarker) {
			t.Error("must-spawn guard re-fired on post-spawn confirming reads — it should be a pre-first-spawn backstop only")
		}
	}
}

// TestSpawnForcingSilentForLeafWorker: a non-spawn-capable worker (no agent
// surface) NEVER sees the forcing reminder even when it loops on reads — the
// guard is scoped to spawn-capable profiles so normal workers are unaffected.
func TestSpawnForcingSilentForLeafWorker(t *testing.T) {
	rec := &readForever{}
	loop := New(single(rec), &fakeProvider{}, nil,
		WithProfile(leafProfile()),
		WithMaxRounds(10),
		WithSpawnForcing(2),
	)

	if _, err := loop.Run(context.Background(), "edit dispatch.go"); err == nil {
		t.Fatal("a read-forever worker should hit max rounds")
	}

	for _, m := range loop.Transcript() {
		if strings.HasPrefix(m.Content, spawnForceMarker) {
			t.Fatal("a leaf (non-spawn-capable) worker must never get the spawn-forcing reminder")
		}
	}
}

// TestSpawnForcingClearsAfterSpawn: once the orchestrator DOES spawn, the forcing
// reminder is cleared and the round counter resets, so a post-spawn synthesis turn
// is not nagged.
func TestSpawnForcingClearsAfterSpawn(t *testing.T) {
	// Round 0: read. Round 1: read (crosses the budget -> reminder injected for
	// round 2). Round 2: agent.spawn (clears it). Round 3: answer.
	m := model.NewEcho("orch",
		model.Response{ToolCalls: []tool.Call{{ID: "r0", Surface: "fs", Action: "grep"}}, StopReason: model.StopToolUse},
		model.Response{ToolCalls: []tool.Call{{ID: "r1", Surface: "fs", Action: "read"}}, StopReason: model.StopToolUse},
		model.Response{ToolCalls: []tool.Call{{ID: "s", Surface: "agent", Action: "spawn"}}, StopReason: model.StopToolUse},
		model.Response{Text: "synthesized", StopReason: model.StopEndTurn},
	)
	loop := New(single(m), &fakeProvider{}, nil,
		WithProfile(orchestrateProfile()),
		WithMaxRounds(10),
		WithSpawnForcing(2),
	)
	res, err := loop.Run(context.Background(), "fix the failing test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "synthesized" {
		t.Errorf("Text = %q, want synthesized", res.Text)
	}
	// After the spawn cleared it, the final transcript carries no dangling reminder.
	for _, msg := range loop.Transcript() {
		if strings.HasPrefix(msg.Content, spawnForceMarker) {
			t.Error("the forcing reminder should be cleared once the orchestrator spawns")
		}
	}
}
