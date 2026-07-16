package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"corpos/internal/runrate"
	"corpos/internal/session"
)

// runInspectSession dumps one persisted session's transcript + cost + tool_call
// telemetry by run id (read-only). It returns a process exit code. This is the
// single-session DETAIL view; runInspect (main.go) renders the sub-orchestration
// TREE across a root run's spawned workers — the two are complementary.
func runInspectSession(dir, runID string, stdout, stderr io.Writer) int {
	st, err := session.Open(dir, runID)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: inspect-session %s: %v\n", runID, err)
		return 1
	}
	defer func() { _ = st.Close() }()

	h, err := st.HeaderRow()
	if err != nil {
		fmt.Fprintf(stderr, "corpos: inspect-session %s: header: %v\n", runID, err)
		return 1
	}
	msgs, err := st.Messages()
	if err != nil {
		fmt.Fprintf(stderr, "corpos: inspect-session %s: messages: %v\n", runID, err)
		return 1
	}
	costs, err := st.Costs()
	if err != nil {
		fmt.Fprintf(stderr, "corpos: inspect-session %s: cost: %v\n", runID, err)
		return 1
	}
	tcs, err := st.ToolCalls()
	if err != nil {
		fmt.Fprintf(stderr, "corpos: inspect-session %s: tool_calls: %v\n", runID, err)
		return 1
	}

	fmt.Fprintf(stdout, "session %s\n", h.RunID)
	fmt.Fprintf(stdout, "  created %s  project %s  status %s\n", h.CreatedAt, h.Project, h.Status)
	fmt.Fprintf(stdout, "  models: cheap=%s strong=%s\n", h.ModelCheap, h.ModelStrong)

	fmt.Fprintf(stdout, "transcript (%d messages):\n", len(msgs))
	for _, m := range msgs {
		fmt.Fprintf(stdout, "  [turn %d %s] %s\n", m.TurnIndex, m.Role, oneLine(m.Content))
	}

	fmt.Fprintf(stdout, "cost (%d rows):\n", len(costs))
	for _, c := range costs {
		fmt.Fprintf(stdout, "  turn %d  %s  %d in + %d out tok → $%.4f\n",
			c.TurnIndex, c.Model, c.InputTokens, c.OutputTokens, c.USD)
	}
	renderTiers(stdout, tierRollupOfCosts(costs))

	fmt.Fprintf(stdout, "tool_calls (%d rows):\n", len(tcs))
	for _, t := range tcs {
		status := "ok"
		if !t.OK {
			status = t.ErrorClass
			if status == "" {
				status = "error"
			}
		}
		fmt.Fprintf(stdout, "  turn %d  %s.%s  %s  %dms%s\n",
			t.TurnIndex, t.Surface, t.Action, status, t.LatencyMS, spanSuffix(t.SpanID))
	}
	return 0
}

// tierRollupOfCosts folds one session's cost rows (per-turn) into per-model
// spend and rolls that onto the ladder, so the single-run view shows the same
// per-tier share as the window report without a parallel accounting path.
func tierRollupOfCosts(costs []session.CostRow) []runrate.TierSpend {
	byModel := map[string]*runrate.ModelSpend{}
	var order []string
	for _, c := range costs {
		m := byModel[c.Model]
		if m == nil {
			m = &runrate.ModelSpend{Model: c.Model}
			byModel[c.Model] = m
			order = append(order, c.Model)
		}
		m.InputTokens += c.InputTokens
		m.OutputTokens += c.OutputTokens
		m.USD += c.USD
	}
	per := make([]runrate.ModelSpend, 0, len(order))
	for _, name := range order {
		per = append(per, *byModel[name])
	}
	return runrate.RollupTiers(per)
}

// runRunRate projects the monthly API run-rate from the session telemetry under
// dir over [since, until] per criteria §5 (read-only). Empty sinceStr defaults to
// the earliest session; empty untilStr defaults to now. Returns an exit code.
func runRunRate(dir, sinceStr, untilStr string, now time.Time, stdout, stderr io.Writer) int {
	sessions, err := scanSessions(dir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: run-rate: %v\n", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Fprintf(stderr, "corpos: run-rate: no session DBs under %s\n", dir)
		return 1
	}

	since, until, err := resolvePeriod(sinceStr, untilStr, sessions, now)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: run-rate: %v\n", err)
		return 2
	}

	rep, err := runrate.Project(sessions, since, until)
	if err != nil {
		fmt.Fprintf(stderr, "corpos: run-rate: %v\n", err)
		return 1
	}
	renderReport(stdout, rep)
	return 0
}

// scanSessions reads every *.db session under dir into a runrate.Session (run id,
// start instant, cost rows). Unreadable / version-mismatched DBs are skipped with
// a warning rather than failing the whole aggregation.
func scanSessions(dir string, stderr io.Writer) ([]runrate.Session, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []runrate.Session
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".db") {
			continue
		}
		runID := strings.TrimSuffix(name, ".db")
		st, err := session.Open(dir, runID)
		if err != nil {
			fmt.Fprintf(stderr, "corpos: run-rate: skip %s: %v\n", name, err)
			continue
		}
		h, herr := st.HeaderRow()
		costs, cerr := st.Costs()
		_ = st.Close()
		if herr != nil || cerr != nil {
			fmt.Fprintf(stderr, "corpos: run-rate: skip %s: %v\n", name, firstErr(herr, cerr))
			continue
		}
		created, perr := time.Parse(time.RFC3339Nano, h.CreatedAt)
		if perr != nil {
			fmt.Fprintf(stderr, "corpos: run-rate: skip %s: bad created_at %q: %v\n", name, h.CreatedAt, perr)
			continue
		}
		out = append(out, runrate.Session{RunID: runID, CreatedAt: created, Costs: costs})
	}
	return out, nil
}

// resolvePeriod fills the period bounds, defaulting since to the earliest session
// and until to now when the flags are empty.
func resolvePeriod(sinceStr, untilStr string, sessions []runrate.Session, now time.Time) (since, until time.Time, err error) {
	if sinceStr != "" {
		if since, err = runrate.ParseDate(sinceStr); err != nil {
			return time.Time{}, time.Time{}, err
		}
	} else {
		since = earliest(sessions)
	}
	if untilStr != "" {
		if until, err = runrate.ParseDate(untilStr); err != nil {
			return time.Time{}, time.Time{}, err
		}
	} else {
		until = now.UTC()
	}
	return since, until, nil
}

// earliest returns the earliest session start instant.
func earliest(sessions []runrate.Session) time.Time {
	min := sessions[0].CreatedAt
	for _, s := range sessions[1:] {
		if s.CreatedAt.Before(min) {
			min = s.CreatedAt
		}
	}
	return min
}

// renderReport writes the run-rate report: the period, the paid spend S, the §5
// projection, the per-model rollup (free / UNPRICED flagged), and an UNPRICED
// warning when the number is untrustworthy.
func renderReport(stdout io.Writer, rep runrate.Report) {
	fmt.Fprintf(stdout, "corpos run-rate — period %s → %s (%.1f days), %d session(s)\n",
		rep.Since.Format("2006-01-02"), rep.Until.Format("2006-01-02"), rep.ElapsedDays, rep.Sessions)
	fmt.Fprintf(stdout, "  paid spend S = $%.4f\n", rep.PaidUSD)
	fmt.Fprintf(stdout, "  projected monthly run-rate = $%.2f/mo   (S / %.1f days × 30)\n",
		rep.ProjectedMonthlyUSD, rep.ElapsedDays)
	fmt.Fprintf(stdout, "per-model:\n")
	for _, m := range rep.PerModel {
		tag := ""
		switch {
		case !m.Priced:
			tag = " [UNPRICED]"
		case m.USD == 0:
			tag = " [free]"
		}
		fmt.Fprintf(stdout, "  %s: %d in + %d out tok → $%.4f%s\n",
			m.Model, m.InputTokens, m.OutputTokens, m.USD, tag)
	}
	renderTiers(stdout, rep.PerTier)
	if len(rep.UnpricedModels) > 0 {
		fmt.Fprintf(stdout, "! UNPRICED models present (spend untrustworthy, add a price-table entry): %s\n",
			strings.Join(rep.UnpricedModels, ", "))
	}
}

// renderTiers writes the per-tier (local/mid/strong) token + USD share rollup
// and the headline frontier-share gate line that makes "Opus is < 5% of spend"
// a one-glance check. It is shared by the session-window report and the
// single-run inspect view. A nil/empty rollup prints nothing.
func renderTiers(stdout io.Writer, tiers []runrate.TierSpend) {
	if len(tiers) == 0 {
		return
	}
	fmt.Fprintf(stdout, "per-tier (token & cost share):\n")
	for _, t := range tiers {
		name := string(t.Tier)
		if t.Tier == "" {
			name = "unknown"
		}
		fmt.Fprintf(stdout, "  %-7s %d in + %d out tok (%.1f%% of tok) → $%.4f (%.1f%% of spend)\n",
			name, t.InputTokens, t.OutputTokens, t.TokenShare*100, t.USD, t.USDShare*100)
	}
	usdShare, tokShare := runrate.FrontierShare(tiers)
	verdict := "PASS"
	if usdShare > runrate.FrontierUSDShareThreshold {
		verdict = "FAIL"
	}
	fmt.Fprintf(stdout, "frontier (strong tier — Opus) share: %.1f%% of spend, %.1f%% of tok — target <%.0f%% of spend: %s\n",
		usdShare*100, tokShare*100, runrate.FrontierUSDShareThreshold*100, verdict)
}

// oneLine collapses a multi-line message body to a single line for the transcript
// listing.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// spanSuffix renders a non-empty span id as a trailing annotation.
func spanSuffix(span string) string {
	if span == "" {
		return ""
	}
	return "  span=" + span
}

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
