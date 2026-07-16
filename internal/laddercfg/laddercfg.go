// Package laddercfg holds corpos's model-ladder SIZING POLICY — the pure functions that decide
// how big a run's live-context/skill budget should be for a given model window, and which ladder
// rung a profile's tier rests on. It is lifted out of the cmd/corpos composition root so the
// policy is unit-testable in isolation and main.go keeps wiring, not arithmetic (chain
// corpos-pre-phase-e-honing). No I/O: callers probe the window and pass it in.
package laddercfg

import "corpos/internal/profile"

// MaxLiveContextBudget caps the compaction budget regardless of how wide the model's window is.
// A cloud rung reports its TRUE window (Gemini ~1M, Opus 200k); without this cap, compaction
// would only trip near that window, so the orchestrator's live context could grow to hundreds of
// thousands of tokens before compacting — and that single model call is slow enough to trip the
// request timeout and is costly. ~124k keeps a multi-file coding/orchestrate carry intact (~20×
// the 6144 local-floor budget) while bounding per-call latency — an honest latency policy rather
// than a fake window.
const MaxLiveContextBudget = 124000

// FallbackUnknownWindow sizes compaction for a rung whose window is neither probed nor known (an
// unrecognized model id). A logged last resort so compaction stays ON rather than silently off.
const FallbackUnknownWindow = 128000

// CompactionBudgetForWindow sizes the default compaction budget below the model's context window,
// reserving room for the model's own output and for the gap between corpos' char-based token
// estimate and the model's real tokenizer. The result is capped at MaxLiveContextBudget so a very
// wide window (a 1M-token cloud tier) does not let the live context grow to a latency-killing size
// before compacting. reserve is a quarter of the window, clamped to [1024, 4096] tokens.
func CompactionBudgetForWindow(window int) int {
	reserve := window / 4
	if reserve < 1024 {
		reserve = 1024
	}
	if reserve > 4096 {
		reserve = 4096
	}
	budget := window - reserve
	if budget > MaxLiveContextBudget {
		budget = MaxLiveContextBudget
	}
	if budget < 1 {
		budget = 1
	}
	return budget
}

// SkillBudgetFraction reserves at most this fraction (1/N) of the compaction budget for a worker's
// injected skill text, leaving the rest of the window for the task prompt, tool specs, transcript,
// and tool results. Beyond it, skills inject in their terse tier (skills.SystemPromptWithin). On
// the local floor (8192-tok window → 6144 budget) this caps skills at ~2048 tok, so the coding
// profile's ~8.7k-tok discipline corpus no longer alone overflows the window and forces every
// coding turn up to the strong rung (the floor-fit keystone).
const SkillBudgetFraction = 3

// SkillInjectionBudget is the token ceiling for a worker's injected skill block, derived from the
// model window's compaction budget. A window of 0 (none detected) returns 0 — no cap, full skill
// bodies, the prior behavior.
func SkillInjectionBudget(window int) int {
	if window <= 0 {
		return 0
	}
	return CompactionBudgetForWindow(window) / SkillBudgetFraction
}

// Fidelity is a named point on the context-budget degradation curve (corpos #3099):
// how generously a run provisions the FIXED context components (injected skills /
// context, single tool-result bodies) inside a model window. A small window auto-tunes
// to a lower fidelity — tighter caps, degrade the lowest-priority component first — so
// a tiny local window no longer needs a hand-picked profile to fit; a wide cloud window
// affords Extreme (keep everything verbatim). The four levels ARE the low/mid/high/
// extreme presets the task names; an operator may pin one, else FidelityFor derives it
// from the window.
type Fidelity int

const (
	FidelityLow     Fidelity = iota // tiny windows (< 16k, the local floor): degrade hard
	FidelityMid                     // 16k–64k
	FidelityHigh                    // 64k–200k
	FidelityExtreme                 // >= 200k: keep everything verbatim (caps lifted)
)

// String renders a Fidelity as its preset name (the -context-fidelity flag vocabulary).
func (f Fidelity) String() string {
	switch f {
	case FidelityLow:
		return "low"
	case FidelityMid:
		return "mid"
	case FidelityHigh:
		return "high"
	case FidelityExtreme:
		return "extreme"
	default:
		return "unknown"
	}
}

// ParseFidelity maps a preset name to a Fidelity; ok is false for an unrecognized
// name (the caller then keeps Auto / the window-derived level).
func ParseFidelity(s string) (Fidelity, bool) {
	switch s {
	case "low":
		return FidelityLow, true
	case "mid":
		return FidelityMid, true
	case "high":
		return FidelityHigh, true
	case "extreme":
		return FidelityExtreme, true
	default:
		return FidelityLow, false
	}
}

// Window thresholds for the auto fidelity classification (criterion 2: a small window
// auto-tunes the fixed cost down without a manual profile choice).
const (
	fidelityMidWindow     = 16384
	fidelityHighWindow    = 65536
	fidelityExtremeWindow = 200000
)

// FidelityFor classifies a model window into its default fidelity preset. An
// unknown window (<= 0) maps to FidelityHigh as a neutral default — AllocateAt then
// disables the artificial caps anyway (no detection → no synthetic ceiling).
func FidelityFor(window int) Fidelity {
	switch {
	case window <= 0:
		return FidelityHigh
	case window < fidelityMidWindow:
		return FidelityLow
	case window < fidelityHighWindow:
		return FidelityMid
	case window < fidelityExtremeWindow:
		return FidelityHigh
	default:
		return FidelityExtreme
	}
}

// Allocation is the budget split for one run's context window (corpos #3099): the
// transcript (compaction) budget plus the caps on each fixed component. Every field
// is derived from the window + fidelity by Allocate/AllocateAt, so the compaction
// budget, the skill-injection cap, the injected-context cap, and the per-result cap
// all come from ONE place instead of scattered call-site arithmetic. A cap of 0 means
// "uncapped" (the prior behavior), used when no window was detected or at Extreme.
type Allocation struct {
	Window           int
	Fidelity         Fidelity
	CompactionBudget int // transcript budget: window − output reserve, capped (the proven formula)
	SkillBudget      int // ceiling on injected skill text
	InjectBudget     int // ceiling on injected parse_context / memory text
	PerResultCap     int // ceiling (tokens) on what a single tool RESULT may add; 0 only when no window detected
}

// injectDivisor sizes the injected-context cap as 1/N of the compaction budget, N
// shrinking the cap as fidelity drops (degrade the lowest-priority component first).
func injectDivisor(f Fidelity) int {
	switch f {
	case FidelityLow:
		return 8
	case FidelityMid:
		return 6
	case FidelityExtreme:
		return 3
	default: // FidelityHigh
		return 4
	}
}

// perResultCapFloor keeps even a low-fidelity per-result cap usable (a single result
// smaller than this is never truncated).
const perResultCapFloor = 512

// perResultCap sizes the single-result ceiling as a fraction of the compaction budget
// that grows with fidelity: a single result may use 1/4 of the transcript budget at
// Low, rising to the WHOLE budget at Extreme (a wide window keeps tool results verbatim
// — overflow is then compaction's job, not the per-result guard's). Always positive for
// a detected window, floored at perResultCapFloor.
func perResultCap(compaction int, f Fidelity) int {
	div := 2 // FidelityHigh
	switch f {
	case FidelityLow:
		div = 4
	case FidelityMid:
		div = 3
	case FidelityExtreme:
		div = 1
	}
	c := compaction / div
	if c < perResultCapFloor {
		c = perResultCapFloor
	}
	return c
}

// Allocate sizes a run's full context budget from the model window alone, picking the
// fidelity preset automatically (FidelityFor). It is the single entry point cmd/corpos
// uses to derive the compaction budget, the skill/inject caps, and the per-result cap.
func Allocate(window int) Allocation {
	return AllocateAt(window, FidelityFor(window))
}

// AllocateAt sizes a run's context budget at an explicitly chosen fidelity (the
// operator's -context-fidelity override). The compaction budget and skill budget stay
// window-derived (the proven, non-regressing formulas); fidelity governs the injected-
// context and per-result caps. A window of 0 (none detected) yields an all-uncapped
// allocation with compaction off, exactly the prior behavior.
func AllocateAt(window int, f Fidelity) Allocation {
	a := Allocation{Window: window, Fidelity: f}
	if window <= 0 {
		return a
	}
	a.CompactionBudget = CompactionBudgetForWindow(window)
	a.SkillBudget = SkillInjectionBudget(window)
	a.InjectBudget = a.CompactionBudget / injectDivisor(f)
	a.PerResultCap = perResultCap(a.CompactionBudget, f)
	return a
}

// FloorForTier maps the active profile's Tier to its rung index, clamping down to the nearest
// lower rung that is actually configured. A nil profile (unprojected run) rests on the base rung.
func FloorForTier(p *profile.JobProfile, midIdx, strongIdx int) int {
	if p == nil {
		return 0
	}
	switch p.Tier {
	case profile.TierMid:
		if midIdx >= 0 {
			return midIdx
		}
	case profile.TierStrong:
		switch {
		case strongIdx >= 0:
			return strongIdx
		case midIdx >= 0:
			return midIdx
		}
	}
	return 0
}
