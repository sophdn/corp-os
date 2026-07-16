package router

import "testing"

func cfgAll() Config { return DefaultConfig() }

// fl is a float64-pointer helper for the low_confidence signal.
func fl(v float64) *float64 { return &v }

func TestDefaultConfigMatchesMigration080(t *testing.T) {
	c := DefaultConfig()
	if c.DeEscalationTurns != 2 {
		t.Errorf("K = %d, want 2", c.DeEscalationTurns)
	}
	want := map[Trigger]float64{
		TriggerRetryExhaustion:   2,
		TriggerLowConfidence:     0.35,
		TriggerRepeatedToolError: 3,
		TriggerParseFailure:      2,
		TriggerExplicitHandoff:   1,
	}
	for k, v := range want {
		tc, ok := c.Triggers[k]
		if !ok || !tc.Enabled || tc.ThresholdValue != v {
			t.Errorf("trigger %s = %+v, want enabled threshold %v", k, tc, v)
		}
	}
}

func TestDetectPriorityOrder(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()))
	// All five fire at once; explicit_handoff is highest priority.
	e := r.Observe(Signals{
		ToolErrors: 9, ParseFailures: 9, RetriesUsed: 9, ExplicitHandoff: 9, Confidence: fl(0.0),
	})
	if e.Direction != EdgeEscalate || e.Trigger != TriggerExplicitHandoff {
		t.Fatalf("highest-priority trigger should win: got %+v", e)
	}
}

func TestDetectPrioritySkipsDisabled(t *testing.T) {
	c := cfgAll()
	c.Triggers[TriggerExplicitHandoff] = TriggerConfig{ThresholdValue: 1, Enabled: false}
	c.Triggers[TriggerRetryExhaustion] = TriggerConfig{ThresholdValue: 2, Enabled: false}
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(c))
	// explicit_handoff + retry disabled; parse_failure (next in priority) wins.
	e := r.Observe(Signals{ParseFailures: 9, RetriesUsed: 9, ExplicitHandoff: 9})
	if e.Trigger != TriggerParseFailure {
		t.Fatalf("disabled triggers must be skipped: got %s", e.Trigger)
	}
}

func TestLowConfidenceFloorAndNil(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(Config{
		Triggers: map[Trigger]TriggerConfig{TriggerLowConfidence: {ThresholdValue: 0.35, Enabled: true}},
	}))
	if e := r.Observe(Signals{Confidence: nil}); e.Direction != EdgeNone {
		t.Error("nil (unmeasured) confidence must never fire low_confidence")
	}
	if e := r.Observe(Signals{Confidence: fl(0.5)}); e.Direction != EdgeNone {
		t.Error("confidence at/above the floor must not fire")
	}
	e := r.Observe(Signals{Confidence: fl(0.34)})
	if e.Direction != EdgeEscalate || e.Trigger != TriggerLowConfidence {
		t.Errorf("confidence below the floor must fire low_confidence: got %+v", e)
	}
}

func TestRetryExhaustionAndExplicitHandoffTriggers(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(Config{
		Triggers: map[Trigger]TriggerConfig{TriggerRetryExhaustion: {ThresholdValue: 2, Enabled: true}},
	}))
	if e := r.Observe(Signals{RetriesUsed: 2}); e.Trigger != TriggerRetryExhaustion {
		t.Errorf("retries_used>=threshold should fire retry_exhaustion: got %+v", e)
	}

	r2 := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(Config{
		Triggers: map[Trigger]TriggerConfig{TriggerExplicitHandoff: {ThresholdValue: 1, Enabled: true}},
	}))
	if e := r2.Observe(Signals{ExplicitHandoff: 1}); e.Trigger != TriggerExplicitHandoff {
		t.Errorf("explicit_handoff>=threshold should fire: got %+v", e)
	}
}

func TestEscalateEdgePayloadFields(t *testing.T) {
	r := NewLadder(localMidStrong(true), 0, WithConfig(cfgAll()))
	e := r.Observe(Signals{ToolErrors: 5, Detail: "extra"})
	if e.Direction != EdgeEscalate {
		t.Fatalf("want escalate edge, got %s", e.Direction)
	}
	if e.FromModel != "local" || e.ToModel != "mid" {
		t.Errorf("edge models = %s→%s, want local→mid", e.FromModel, e.ToModel)
	}
	if e.StateBefore != "cheap" || e.StateAfter != "escalated" {
		t.Errorf("states = %s→%s, want cheap→escalated", e.StateBefore, e.StateAfter)
	}
	if e.Trigger != TriggerRepeatedToolError || e.FiredThreshold != 3 {
		t.Errorf("trigger/threshold = %s/%v, want repeated_tool_error/3", e.Trigger, e.FiredThreshold)
	}
	if e.Detail != "tool_errors=5 extra" {
		t.Errorf("detail = %q, want %q", e.Detail, "tool_errors=5 extra")
	}
	// A second climb (mid→strong) is above the floor: state_before is escalated.
	e2 := r.Observe(Signals{ToolErrors: 5})
	if e2.StateBefore != "escalated" || e2.FromModel != "mid" || e2.ToModel != "strong" {
		t.Errorf("second climb = %s %s→%s, want escalated mid→strong", e2.StateBefore, e2.FromModel, e2.ToModel)
	}
}

func TestSaturatedTopRungEmitsNoEdge(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()))
	if e := r.Observe(Signals{ToolErrors: 9}); e.Direction != EdgeEscalate {
		t.Fatalf("first climb should escalate, got %s", e.Direction)
	}
	// At the top rung a fired trigger is saturated: no edge.
	if e := r.Observe(Signals{ToolErrors: 9}); e.Direction != EdgeNone {
		t.Errorf("a fired trigger at the top rung must emit no edge, got %+v", e)
	}
}

func TestDeescalateEdgeCarriesNoTrigger(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(cfgAll()))
	r.Observe(Signals{ToolErrors: 9}) // escalate
	if e := r.Observe(Signals{}); e.Direction != EdgeNone {
		t.Fatal("one clean turn should not de-escalate yet")
	}
	e := r.Observe(Signals{}) // K=2 clean turns → descend
	if e.Direction != EdgeDeescalate {
		t.Fatalf("want de-escalate edge, got %s", e.Direction)
	}
	if e.Trigger != "" {
		t.Errorf("de-escalation must carry no trigger, got %q", e.Trigger)
	}
	if e.StateBefore != "escalated" || e.StateAfter != "de_escalated" {
		t.Errorf("states = %s→%s, want escalated→de_escalated", e.StateBefore, e.StateAfter)
	}
	if e.FromModel != "strong" || e.ToModel != "cheap" {
		t.Errorf("models = %s→%s, want strong→cheap", e.FromModel, e.ToModel)
	}
}

func TestWithConfigOverridesK(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true}, WithConfig(Config{
		Triggers:          map[Trigger]TriggerConfig{TriggerRepeatedToolError: {ThresholdValue: 1, Enabled: true}},
		DeEscalationTurns: 1,
	}))
	r.Observe(Signals{ToolErrors: 1}) // escalate
	if e := r.Observe(Signals{}); e.Direction != EdgeDeescalate {
		t.Errorf("K=1 should de-escalate after one clean turn, got %s", e.Direction)
	}
}

func TestWithConfigEmptyKeepsExisting(t *testing.T) {
	// WithConfig with a zero DeEscalationTurns and nil Triggers must not clobber
	// the WithEscalation-seeded config applied before it.
	r := New(stub{"cheap", true}, stub{"strong", true},
		WithEscalation(1, 3), WithConfig(Config{}))
	r.Observe(Signals{ToolErrors: 1}) // escalate (repeated_tool_error still enabled)
	r.Observe(Signals{})              // clean 1
	r.Observe(Signals{})              // clean 2 (K=3, not yet)
	if r.State() != StateEscalated {
		t.Error("empty WithConfig must preserve the prior K=3 and trigger config")
	}
}

func TestConfigWithRepeatedToolError(t *testing.T) {
	base := DefaultConfig()
	got := base.WithRepeatedToolError(1)
	if tc := got.Triggers[TriggerRepeatedToolError]; tc.ThresholdValue != 1 || !tc.Enabled {
		t.Errorf("repeated_tool_error not pinned: %+v", tc)
	}
	// Other triggers + K are preserved.
	if got.Triggers[TriggerParseFailure] != base.Triggers[TriggerParseFailure] {
		t.Error("other triggers must be preserved")
	}
	if got.DeEscalationTurns != base.DeEscalationTurns {
		t.Error("K must be preserved")
	}
	// The copy is independent of the source.
	if base.Triggers[TriggerRepeatedToolError].ThresholdValue != 3 {
		t.Error("source config must not be mutated")
	}
}

func TestNoConfigNeverFires(t *testing.T) {
	r := New(stub{"cheap", true}, stub{"strong", true})
	if e := r.Observe(Signals{ToolErrors: 99, ParseFailures: 99, ExplicitHandoff: 99}); e.Direction != EdgeNone {
		t.Error("a router with no enabled triggers must never escalate")
	}
}
