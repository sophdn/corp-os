// Package runrate projects a monthly API run-rate from persisted session cost
// telemetry, implementing the measurement methodology locked in
// docs/SWAP_VALIDATION_CRITERIA.md §5: sum the paid (non-free) spend S across the
// sessions in a period of D elapsed days, then project the monthly run-rate as
// S / D × 30. The aggregation is sans-IO — callers read the session DBs and hand
// this package the per-session cost rows plus the [since, until] window; pricing
// classification (free vs UNPRICED vs paid) comes from internal/cost.
package runrate

import (
	"fmt"
	"sort"
	"time"

	"corpos/internal/cost"
	"corpos/internal/session"
)

// daysPerMonth is the §5 projection constant (S / D × 30).
const daysPerMonth = 30.0

// hoursPerDay converts an elapsed duration to days.
const hoursPerDay = 24.0

// Session is one session's cost telemetry plus the instant it started — the unit
// the period filter (CreatedAt ∈ [since, until]) selects on.
type Session struct {
	RunID     string
	CreatedAt time.Time
	Costs     []session.CostRow
}

// ModelSpend is a per-model token + USD rollup over the period. Priced is false
// when the model has no internal/cost price-table entry, meaning its USD is an
// untrustworthy zero (an UNPRICED model — see §5).
type ModelSpend struct {
	Model        string
	InputTokens  int
	OutputTokens int
	USD          float64
	Priced       bool
}

// Report is the projected run-rate over a period.
type Report struct {
	// Since and Until bound the period (inclusive).
	Since time.Time
	Until time.Time
	// ElapsedDays is D — the period length in days (Until − Since).
	ElapsedDays float64
	// Sessions is how many sessions fell within the period.
	Sessions int
	// PaidUSD is S — the summed paid (non-free) spend over the period.
	PaidUSD float64
	// ProjectedMonthlyUSD is the §5 projection: S / D × 30.
	ProjectedMonthlyUSD float64
	// PerModel is the per-model rollup, most expensive first.
	PerModel []ModelSpend
	// UnpricedModels lists models seen with no price-table entry; their spend is
	// an untrustworthy zero and the table needs an entry before the number is
	// trustworthy (§5).
	UnpricedModels []string
	// PerTier rolls PerModel up onto the local/mid/strong ladder with each
	// tier's share of the period's tokens and USD — the one-glance "is the
	// frontier rare?" view. Ladder order (cheap→frontier); empty tiers omitted.
	PerTier []TierSpend
}

// TierSpend is a per-tier token + USD rollup with that tier's share of the
// window's total tokens and total USD. Share fields are fractions in [0,1].
type TierSpend struct {
	Tier         cost.Tier
	InputTokens  int
	OutputTokens int
	USD          float64
	// USDShare is this tier's fraction of total USD over the window (0 when the
	// window has no priced spend).
	USDShare float64
	// TokenShare is this tier's fraction of total (input+output) tokens.
	TokenShare float64
}

// FrontierUSDShareThreshold is the swap-validation gate: the strong (frontier /
// Opus) rung must stay under this fraction of spend (§3 / SWAP_VALIDATION §5).
const FrontierUSDShareThreshold = 0.05

// tierOrder is the ladder order tiers render in (cheap→frontier), unknown last.
var tierOrder = []cost.Tier{cost.TierLocal, cost.TierMid, cost.TierStrong, cost.TierUnknown}

// RollupTiers classifies per-model spend onto the local/mid/strong ladder and
// computes each tier's share of total tokens and total USD. The result is in
// ladder order with empty tiers omitted. It is the shared core behind both the
// session-window report and the single-run inspect view (no parallel path).
func RollupTiers(per []ModelSpend) []TierSpend {
	acc := map[cost.Tier]*TierSpend{}
	var totalUSD float64
	var totalTok int
	for _, m := range per {
		t := cost.Classify(m.Model)
		s := acc[t]
		if s == nil {
			s = &TierSpend{Tier: t}
			acc[t] = s
		}
		s.InputTokens += m.InputTokens
		s.OutputTokens += m.OutputTokens
		s.USD += m.USD
		totalUSD += m.USD
		totalTok += m.InputTokens + m.OutputTokens
	}
	out := make([]TierSpend, 0, len(acc))
	for _, t := range tierOrder {
		s := acc[t]
		if s == nil {
			continue
		}
		if totalUSD > 0 {
			s.USDShare = s.USD / totalUSD
		}
		if totalTok > 0 {
			s.TokenShare = float64(s.InputTokens+s.OutputTokens) / float64(totalTok)
		}
		out = append(out, *s)
	}
	return out
}

// FrontierShare returns the strong (frontier / Opus) tier's share of USD and of
// tokens over a tier rollup — the two numbers the swap-validation gate reads.
// Both are 0 when no strong-tier spend is present.
func FrontierShare(tiers []TierSpend) (usdShare, tokenShare float64) {
	for _, t := range tiers {
		if t.Tier == cost.TierStrong {
			return t.USDShare, t.TokenShare
		}
	}
	return 0, 0
}

// Project aggregates the sessions that fall within [since, until] and projects
// the monthly run-rate per criteria §5. It errors when the period is not
// positive (D ≤ 0), since the projection divides by D.
func Project(sessions []Session, since, until time.Time) (Report, error) {
	days := until.Sub(since).Hours() / hoursPerDay
	if days <= 0 {
		return Report{}, fmt.Errorf("run-rate period must be positive: since %s is not before until %s",
			since.Format(time.RFC3339), until.Format(time.RFC3339))
	}

	perModel := map[string]*ModelSpend{}
	var order []string // first-seen order, for stable UnpricedModels
	counted := 0
	for _, s := range sessions {
		if s.CreatedAt.Before(since) || s.CreatedAt.After(until) {
			continue
		}
		counted++
		for _, c := range s.Costs {
			m := perModel[c.Model]
			if m == nil {
				m = &ModelSpend{Model: c.Model, Priced: cost.IsPriced(c.Model)}
				perModel[c.Model] = m
				order = append(order, c.Model)
			}
			m.InputTokens += c.InputTokens
			m.OutputTokens += c.OutputTokens
			m.USD += c.USD
		}
	}

	var paid float64
	var unpriced []string
	for _, name := range order {
		ms := perModel[name]
		paid += ms.USD
		if !ms.Priced {
			unpriced = append(unpriced, name)
		}
	}

	per := make([]ModelSpend, 0, len(order))
	for _, name := range order {
		per = append(per, *perModel[name])
	}
	sort.Slice(per, func(i, j int) bool {
		if per[i].USD != per[j].USD {
			return per[i].USD > per[j].USD
		}
		return per[i].Model < per[j].Model
	})

	return Report{
		Since:               since,
		Until:               until,
		ElapsedDays:         days,
		Sessions:            counted,
		PaidUSD:             paid,
		ProjectedMonthlyUSD: paid / days * daysPerMonth,
		PerModel:            per,
		UnpricedModels:      unpriced,
		PerTier:             RollupTiers(per),
	}, nil
}

// ParseDate parses a period bound as either a date-only stamp (2006-01-02, taken
// as UTC midnight) or a full RFC3339 timestamp. The date-only form is the
// ergonomic case for the day-granularity validation period.
func ParseDate(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid date %q: want YYYY-MM-DD or RFC3339", s)
}
