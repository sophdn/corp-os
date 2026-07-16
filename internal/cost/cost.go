// Package cost prices model calls and accumulates per-session cost. It prefers
// the PROVIDER-REPORTED cost when a call carries one (OpenRouter returns the
// actual charge in its usage object), and falls back to a static price table —
// with a prompt-cache term — only when the provider reports nothing. The table
// maps a model id to per-1K rates for fresh input, cached input (prompt-cache
// reads, billed at ~0.1x), cache-write input (Anthropic cache creation, ~1.25x),
// and output; unknown models price at 0 and are flagged UNPRICED rather than
// silently invented. Ported from bridge-harness cost.py, then extended for the
// cache model + provider-reported cost (bug cost-ledger-overreports-…).
package cost

import (
	"sort"
	"strings"
	"sync"
)

// rate holds per-1K-token USD rates. cachedInPer1K prices prompt-cache READ
// tokens (re-sent prefix served from cache); cacheWritePer1K prices cache-WRITE
// tokens (Anthropic cache_creation). For providers without a cache split the
// cached/cacheWrite rates are unused (those token counts are zero).
type rate struct{ inPer1K, cachedInPer1K, cacheWritePer1K, outPer1K float64 }

// priceTable holds indicative list prices, recalibrated 2026-06 against the
// published Anthropic rates (Opus 4.8 $5/$25 per M — the prior $15/$75 entry
// over-reported Opus ~3x; Sonnet 4.6 $3/$15; Haiku 4.5 $1/$5). Cache rates use
// Anthropic's standard multipliers: reads at 0.1x input, 5-minute writes at
// 1.25x input. Gemini/OpenRouter list rates are a FALLBACK only — those calls
// carry a provider-reported cost that supersedes the table (see PriceUsage).
// Local and echo tiers are free. Tune as contracts change; the provider-reported
// path is the source of truth where available.
var priceTable = map[string]rate{
	"claude-opus-4-8":                  {0.005, 0.0005, 0.00625, 0.025},
	"claude-opus-4-7":                  {0.005, 0.0005, 0.00625, 0.025},
	"claude-sonnet-4-6":                {0.003, 0.0003, 0.00375, 0.015},
	"claude-haiku-4-5-20251001":        {0.001, 0.0001, 0.00125, 0.005},
	"google/gemini-3.1-flash-lite":     {0.00025, 0.000025, 0.0003125, 0.0015},
	"deepseek-v4-pro":                  {0.0007, 0.00007, 0.000875, 0.0014},
	"deepseek-chat":                    {0.00028, 0.000028, 0.00035, 0.00042},
	"deepseek/deepseek-v3.2":           {0.0002145, 0.00002145, 0.000268, 0.0003218}, // canonicalCodingRung; OpenRouter list ($0.2145/$0.3218 per M) + 0.1x read / 1.25x write
	"qwen2.5-32b":                      {0, 0, 0, 0},
	"Qwen2.5-32B-Instruct-Q4_K_M.gguf": {0, 0, 0, 0},
	"echo":                             {0, 0, 0, 0},
}

// PriceSource records how a cost was derived, so a run-rate built on table
// guesses is distinguishable from one built on real provider charges.
type PriceSource string

const (
	// PricedUnknown is the zero value (no call priced yet).
	PricedUnknown PriceSource = ""
	// PricedProvider means the cost came from the provider's reported charge.
	PricedProvider PriceSource = "provider"
	// PricedTable means the cost came from the static table (an estimate).
	PricedTable PriceSource = "table"
	// PricedUnpriced means neither a provider cost nor a table entry was available.
	PricedUnpriced PriceSource = "unpriced"
	// PricedMixed means a model's calls were priced from more than one source.
	PricedMixed PriceSource = "mixed"
)

// Usage is the token + cost usage for one model call. CachedInputTokens and
// CacheWriteTokens are SUBSETS of InputTokens (InputTokens is the full prompt
// size; fresh = InputTokens − CachedInputTokens − CacheWriteTokens). When
// ProviderReported is true, ProviderCostUSD is the authoritative charge and the
// table is bypassed.
type Usage struct {
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	ProviderCostUSD   float64
	ProviderReported  bool
}

// Price returns the USD cost of one call at the table's FRESH input rate (no
// cache term, no provider cost). Retained for callers that have only plain
// input/output counts; the loop ledger uses PriceUsage. Unknown models price 0.
func Price(model string, inputTokens, outputTokens int) float64 {
	r, ok := priceTable[model]
	if !ok {
		return 0
	}
	return float64(inputTokens)/1000*r.inPer1K + float64(outputTokens)/1000*r.outPer1K
}

// PriceUsage prices one call, preferring the provider-reported cost and falling
// back to the table with the cache split. It returns the USD cost and the source
// the price came from (provider | table | unpriced).
func PriceUsage(model string, u Usage) (float64, PriceSource) {
	if u.ProviderReported {
		return u.ProviderCostUSD, PricedProvider
	}
	r, ok := priceTable[model]
	if !ok {
		return 0, PricedUnpriced
	}
	fresh := u.InputTokens - u.CachedInputTokens - u.CacheWriteTokens
	if fresh < 0 {
		fresh = 0 // a provider that double-counts cache against input can't go negative
	}
	usd := float64(fresh)/1000*r.inPer1K +
		float64(u.CachedInputTokens)/1000*r.cachedInPer1K +
		float64(u.CacheWriteTokens)/1000*r.cacheWritePer1K +
		float64(u.OutputTokens)/1000*r.outPer1K
	return usd, PricedTable
}

// IsPriced reports whether the model has a price-table entry (else cost is
// reported UNPRICED rather than guessed). A provider-reported call is always
// priced regardless of the table.
func IsPriced(model string) bool {
	_, ok := priceTable[model]
	return ok
}

// Tier is a model's rung on the cost-routed ladder (§4.6 / SWAP_VALIDATION
// §5). The taxonomy mirrors the runtime tierSet: local (free, on-device bulk),
// mid (hosted cheap/escalation), strong (the frontier rung — Opus, kept rare).
// It exists so a run-rate report can show per-tier share and make "Opus is
// < 5% of spend" a one-glance check without hand-classifying model ids.
type Tier string

const (
	// TierUnknown is an unrecognised model id (classify it before trusting a
	// tier rollup, the way an UNPRICED model flags the price table).
	TierUnknown Tier = ""
	// TierLocal is a free on-device rung (Qwen, echo) — the intended bulk.
	TierLocal Tier = "local"
	// TierMid is a hosted cheap/escalation rung (Gemini-Flash-Lite, Haiku,
	// DeepSeek) — carries the work the floor can't, still far below frontier.
	TierMid Tier = "mid"
	// TierStrong is the frontier rung (Opus, Sonnet) — the escalation top the
	// swap-validation gate keeps under 5% of spend.
	TierStrong Tier = "strong"
)

// tierTable classifies the known price-table model ids onto the ladder. Ids
// absent here fall to Classify's family heuristic, then TierUnknown.
var tierTable = map[string]Tier{
	"claude-opus-4-8":                  TierStrong,
	"claude-opus-4-7":                  TierStrong,
	"claude-sonnet-4-6":                TierStrong,
	"claude-haiku-4-5-20251001":        TierMid,
	"google/gemini-3.1-flash-lite":     TierMid,
	"deepseek-v4-pro":                  TierMid,
	"deepseek-chat":                    TierMid,
	"deepseek/deepseek-v3.2":           TierMid,
	"qwen2.5-32b":                      TierLocal,
	"Qwen2.5-32B-Instruct-Q4_K_M.gguf": TierLocal,
	"echo":                             TierLocal,
}

// tierFamily maps a lowercased model-id substring to a tier, so a drifted or
// suffixed id (a new Opus snapshot, a re-quantised Qwen) still classifies
// without a table edit. Order matters: the first match wins, so the frontier
// families are checked before the cheaper ones.
var tierFamily = []struct {
	needle string
	tier   Tier
}{
	{"opus", TierStrong},
	{"sonnet", TierStrong},
	{"haiku", TierMid},
	{"gemini", TierMid},
	{"deepseek", TierMid},
	{"qwen", TierLocal},
	{".gguf", TierLocal},
	{"echo", TierLocal},
}

// Classify returns a model's ladder tier: the exact price-table classification
// when known, else a family-substring heuristic, else TierUnknown. It is the
// single home for model→tier knowledge so the run-rate report does not
// hand-classify ids (acceptance: reuse existing plumbing, no parallel path).
func Classify(model string) Tier {
	if t, ok := tierTable[model]; ok {
		return t
	}
	lower := strings.ToLower(model)
	for _, f := range tierFamily {
		if strings.Contains(lower, f.needle) {
			return f.tier
		}
	}
	return TierUnknown
}

// ModelTotal is a per-model cost rollup. CachedInputTokens/CacheWriteTokens are
// the cache split of InputTokens. Priced stays true when the cost is trustworthy
// (provider-reported or table-priced); PricedFrom carries the finer marker.
type ModelTotal struct {
	Model             string
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	USD               float64
	Priced            bool
	PricedFrom        PriceSource
}

// Ledger accumulates running cost, per model call.
type Ledger struct {
	perModel map[string]*ModelTotal
}

// NewLedger returns an empty cost ledger.
func NewLedger() *Ledger {
	return &Ledger{perModel: map[string]*ModelTotal{}}
}

// Add prices and accumulates one model call; returns that call's USD cost. It
// consumes the provider-reported cost when present, else the table (with cache
// rates), and records which source priced the rollup.
func (l *Ledger) Add(model string, u Usage) float64 {
	usd, src := PriceUsage(model, u)
	t := l.perModel[model]
	if t == nil {
		t = &ModelTotal{Model: model}
		l.perModel[model] = t
	}
	t.InputTokens += u.InputTokens
	t.CachedInputTokens += u.CachedInputTokens
	t.CacheWriteTokens += u.CacheWriteTokens
	t.OutputTokens += u.OutputTokens
	t.USD += usd
	t.PricedFrom = combineSource(t.PricedFrom, src)
	t.Priced = t.PricedFrom != PricedUnpriced && t.PricedFrom != PricedUnknown
	return usd
}

// combineSource merges a rollup's existing price source with a new call's: the
// first source wins, identical sources stay, and differing sources become Mixed
// (a model priced partly from real charges and partly from table guesses).
func combineSource(have, add PriceSource) PriceSource {
	if have == PricedUnknown {
		return add
	}
	if have == add {
		return have
	}
	return PricedMixed
}

// TotalUSD is the running total across every model.
func (l *Ledger) TotalUSD() float64 {
	var sum float64
	for _, t := range l.perModel {
		sum += t.USD
	}
	return sum
}

// Breakdown returns per-model rollups, most expensive first.
func (l *Ledger) Breakdown() []ModelTotal {
	out := make([]ModelTotal, 0, len(l.perModel))
	for _, t := range l.perModel {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].USD > out[j].USD })
	return out
}

// Meter is a concurrency-safe cumulative cost accumulator with a ceiling, shared
// across a spawn TREE — the orchestrator loop plus every worker it spawns — so the
// run-level cost ceiling (-max-cost-usd) bounds CUMULATIVE tree spend, not just one
// loop's own ledger (bug 1124). A delegating orchestrator's own ledger sees only its
// cheap planning calls while the spawned workers spend the money on separate child
// ledgers, so a per-loop breaker leaves the run unbounded; threading one Meter through
// every loop and the spawn tool makes the ceiling whole-tree. Each loop adds its priced
// calls to the shared Meter and checks Exceeded before its next model call; the spawn
// tool checks it before dispatching another worker. The zero value is unusable — build
// with NewMeter. A nil *Meter is inert at every call site (callers nil-guard), so a
// non-tree run keeps the per-loop ledger breaker unchanged.
type Meter struct {
	ceiling float64 // immutable after construction; no lock needed to read
	mu      sync.Mutex
	total   float64
}

// NewMeter builds a shared tree-cost meter with the given USD ceiling. A ceiling <= 0
// yields a meter that tracks cumulative spend but never reports Exceeded (no ceiling).
func NewMeter(ceilingUSD float64) *Meter {
	return &Meter{ceiling: ceilingUSD}
}

// Add accumulates one priced call's USD into the shared tree total and returns the
// new cumulative total.
func (m *Meter) Add(usd float64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total += usd
	return m.total
}

// Total is the cumulative tree cost accrued so far.
func (m *Meter) Total() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Ceiling is the configured USD ceiling (<= 0 means no ceiling).
func (m *Meter) Ceiling() float64 { return m.ceiling }

// Exceeded reports whether the cumulative tree cost has reached the ceiling. It is
// always false when the ceiling is <= 0, so a tracking-only meter never trips.
func (m *Meter) Exceeded() bool {
	if m.ceiling <= 0 {
		return false
	}
	return m.Total() >= m.ceiling
}

// SpawnBudget bounds the COUNT of workers an orchestrator may spawn across a run's whole
// spawn tree — the count analogue of Meter's cost ceiling. The tree-cost meter alone is a
// blunt, late backstop against over-decomposition (it lets many real workers run before the
// spend ceiling trips); a spawn count caps the fan-out directly and far earlier, with an
// actionable "stop decomposing" signal. It is shared across the tree (one budget, threaded
// to the single SpawnProvider the aggregator mounts), so nested spawns draw from the same
// pool. A cap <= 0 means unbounded (tracking only) — the prior behavior.
type SpawnBudget struct {
	cap   int // immutable after construction
	mu    sync.Mutex
	count int
}

// NewSpawnBudget builds a shared spawn-count budget. A cap <= 0 yields a budget that counts
// spawns but never refuses (no bound), so wiring it is always safe.
func NewSpawnBudget(cap int) *SpawnBudget {
	return &SpawnBudget{cap: cap}
}

// Reserve accounts for one spawn and reports whether it is permitted. With no cap (<= 0) it
// always permits and still tallies. Once the count has reached the cap it returns false
// WITHOUT incrementing, so a refused spawn does not consume budget and Count reflects only
// spawns that actually ran.
func (b *SpawnBudget) Reserve() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cap > 0 && b.count >= b.cap {
		return false
	}
	b.count++
	return true
}

// Count is the number of spawns reserved so far.
func (b *SpawnBudget) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// Cap is the configured spawn ceiling (<= 0 means unbounded).
func (b *SpawnBudget) Cap() int { return b.cap }

// StrongTurnBudget bounds the total number of STRONG-rung (Opus) turns served across a run's whole
// spawn TREE — the turn analogue of Meter's cost ceiling and SpawnBudget's spawn count. A per-worker
// WithBoundedTop caps a SINGLE worker's Opus turns, but a stuck atom that respawns N workers hands
// each a FRESH per-worker bound, so the tree serves N×bound Opus turns re-climbing the same dead goal
// (bug 1165: 5 coding workers each climbed to Opus for one trivial bug, ~86% of spend). One
// StrongTurnBudget, threaded to every worker's router, POOLS those turns: once the run has served the
// cap of strong turns, a further climb onto the strong rung is refused tree-wide, so a respawn can no
// longer re-climb Opus. It bounds TOTAL strong turns (atom-agnostic), sidestepping bug-1151's "no
// stable per-atom key" objection the same way -max-cost-usd bounds total spend without one. A cap <= 0
// means unbounded (tracking only), matching -max-cost-usd's default-off shape, so wiring it is always
// safe. Concurrency-safe: the tree may run workers in parallel and draw from one pool.
type StrongTurnBudget struct {
	cap   int // immutable after construction; no lock needed to read
	mu    sync.Mutex
	count int
}

// NewStrongTurnBudget builds a shared strong-turn budget. A cap <= 0 yields a budget that tallies
// served strong turns but never reports Exhausted (tracking-only).
func NewStrongTurnBudget(cap int) *StrongTurnBudget { return &StrongTurnBudget{cap: cap} }

// Exhausted reports whether the run has served its cap of strong-rung turns. Always false when the
// cap is <= 0. It is a non-mutating peek — the router calls it before deciding to serve the bounded
// top rung, then Take()s when it actually does.
func (b *StrongTurnBudget) Exhausted() bool {
	if b.cap <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count >= b.cap
}

// Take tallies one served strong-rung turn. The router calls it when it routes a turn onto the strong
// rung, so Count reflects Opus turns actually served, not climbs merely attempted.
func (b *StrongTurnBudget) Take() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.count++
}

// Count is the number of strong-rung turns served so far.
func (b *StrongTurnBudget) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// Cap is the configured strong-turn ceiling (<= 0 means unbounded).
func (b *StrongTurnBudget) Cap() int { return b.cap }
