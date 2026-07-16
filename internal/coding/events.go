package coding

// Event sourcing: a coding chain's RunState is a PROJECTION over an append-only
// log of typed events, not a bespoke stored struct. The orchestrator emits a delta
// event at every state transition; Fold replays the log to reconstruct RunState;
// resume re-folds; a branch_fix is a fork in the log (a supersede + an insert);
// and a long chain compacts by collapsing each AT to its terminal delta.
//
// The Emitter is a seam: the default is no-op; a SliceEmitter records in memory
// (resume / tests); a toolkit-ledger sink (LedgerEmitter, wired at deploy) lands
// the same events on toolkit-server's append-only event store so corpos reuses the
// substrate ledger rather than building a parallel one (invariant F4).

// EventKind is the typed event discriminator.
type EventKind string

const (
	// EvChainStarted seeds the run: its metadata + the ordered AT slugs.
	EvChainStarted EventKind = "chain_started"
	// EvATStatus is the full current state of one AT after a transition (it carries
	// the whole record, so Fold replaces by slug — the delta is idempotent).
	EvATStatus EventKind = "at_status"
	// EvATInserted inserts a new AT record at its Position (a branch_fix branch).
	EvATInserted EventKind = "at_inserted"
	// EvChainStatus records a chain-level transition + the current position.
	EvChainStatus EventKind = "chain_status"
)

// Event is one delta on the coding-chain log. Only the fields relevant to Kind are
// set; the zero values of the rest are ignored by Fold.
type Event struct {
	Kind EventKind

	// EvChainStarted:
	RunID             string
	ChainSlug         string
	TargetRepo        string
	BaseBranch        string
	IntegrationBranch string
	Seeds             []ATRecord // the initial AT records (slug + spec), in order

	// EvATStatus / EvATInserted: the full record state.
	AT ATRecord

	// EvChainStatus:
	Status       ChainStatus
	FailedATSlug string
	Position     int
}

// Emitter receives coding-chain events. Implementations persist or forward them.
type Emitter interface {
	Emit(Event)
}

// NoopEmitter discards events (the default — telemetry off).
type NoopEmitter struct{}

// Emit discards the event.
func (NoopEmitter) Emit(Event) {}

// SliceEmitter records events in memory; its log is the source for Fold / resume.
type SliceEmitter struct {
	Events []Event
}

// Emit appends the event to the in-memory log.
func (s *SliceEmitter) Emit(e Event) { s.Events = append(s.Events, e) }

// Fold replays a coding-chain event log into the RunState it projects. It is the
// single source of truth for chain state: the same fold reconstructs state on
// resume, after a crash, or for inspection.
func Fold(events []Event) *RunState {
	s := &RunState{}
	for _, e := range events {
		switch e.Kind {
		case EvChainStarted:
			s.RunID = e.RunID
			s.ChainSlug = e.ChainSlug
			s.TargetRepo = e.TargetRepo
			s.BaseBranch = e.BaseBranch
			s.IntegrationBranch = e.IntegrationBranch
			s.Status = ChainPending
			s.ATs = make([]ATRecord, len(e.Seeds))
			copy(s.ATs, e.Seeds)
		case EvATStatus:
			if ar := s.findAT(e.AT.Slug); ar != nil {
				*ar = e.AT
			}
		case EvATInserted:
			pos := e.AT.Position
			if pos < 0 || pos > len(s.ATs) {
				pos = len(s.ATs)
			}
			s.ATs = append(s.ATs[:pos:pos], append([]ATRecord{e.AT}, s.ATs[pos:]...)...)
			s.renumber()
		case EvChainStatus:
			s.Status = e.Status
			s.FailedATSlug = e.FailedATSlug
			s.CurrentPosition = e.Position
		}
	}
	return s
}

// Compact returns a fold-equivalent but smaller log: the chain_started event, the
// LAST status delta per AT (a green AT collapses to its single terminal record),
// any insert deltas, and the last chain_status. Fold(Compact(log)) == Fold(log).
// This is the compaction that keeps a long coding chain's context bounded — only
// the terminal record of each settled AT plus the active frontier survive, and the
// full history stays re-expandable from the uncompacted log.
func Compact(events []Event) []Event {
	var out []Event
	var started *Event
	var lastChainStatus *Event
	lastAT := map[string]int{}    // slug → index in out
	inserted := map[string]bool{} // slugs introduced by an insert (kept as inserts)

	for i := range events {
		e := events[i]
		switch e.Kind {
		case EvChainStarted:
			cp := e
			started = &cp
		case EvATInserted:
			inserted[e.AT.Slug] = true
			// Fold an insert + its later status into one insert carrying the latest record.
			if idx, ok := lastAT[e.AT.Slug]; ok {
				out[idx] = e
			} else {
				out = append(out, e)
				lastAT[e.AT.Slug] = len(out) - 1
			}
		case EvATStatus:
			rec := e
			if inserted[e.AT.Slug] {
				rec.Kind = EvATInserted // keep the slug as an insert so Fold creates it
			}
			if idx, ok := lastAT[e.AT.Slug]; ok {
				out[idx] = rec
			} else {
				out = append(out, rec)
				lastAT[e.AT.Slug] = len(out) - 1
			}
		case EvChainStatus:
			cp := e
			lastChainStatus = &cp
		}
	}

	compacted := make([]Event, 0, len(out)+2)
	if started != nil {
		compacted = append(compacted, *started)
	}
	compacted = append(compacted, out...)
	if lastChainStatus != nil {
		compacted = append(compacted, *lastChainStatus)
	}
	return compacted
}
