// Package session persists one agent session's conversation state in SQLite.
// One DB file per session (<dir>/<run_id>.db); the store is the single writer.
// The loop appends messages and records tool calls / cost / turns through it;
// Open reopens a DB for resume or inspection. Schema is versioned via
// PRAGMA user_version. Ported from bridge-harness state.py.
//
// NB (flag F4): this is LOCAL session state — conversation, telemetry, cost —
// NOT the toolkit-server event ledger. The substrate ledger stays remote over
// MCP; the two are never conflated.
package session

import (
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// SchemaVersion gates migrations; opening a DB written by a different version
// errors. v2 added the sub-orchestration tree columns (parent_run_id, profile,
// duty) so a worker session links to the parent that spawned it. v3 added
// selection_json — the deterministic prompt→profile matcher's logged features
// (signals/shapes/score/fallback) for the later ML matcher dataset (corpos #3096).
const SchemaVersion = 3

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const schemaSQL = `
CREATE TABLE session (
	run_id TEXT PRIMARY KEY, created_at TEXT NOT NULL, model_cheap TEXT NOT NULL,
	model_strong TEXT NOT NULL, project TEXT NOT NULL, skills_digest TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'open',
	parent_run_id TEXT, profile TEXT NOT NULL DEFAULT '', duty TEXT NOT NULL DEFAULT '',
	selection_json TEXT NOT NULL DEFAULT '');
CREATE TABLE message (
	id INTEGER PRIMARY KEY AUTOINCREMENT, turn_index INTEGER NOT NULL, role TEXT NOT NULL,
	content TEXT NOT NULL, model TEXT, created_at TEXT NOT NULL);
CREATE TABLE tool_call (
	id INTEGER PRIMARY KEY AUTOINCREMENT, turn_index INTEGER NOT NULL, surface TEXT NOT NULL,
	action TEXT NOT NULL, params_json TEXT NOT NULL, result_json TEXT NOT NULL, ok INTEGER NOT NULL,
	error_class TEXT, latency_ms INTEGER NOT NULL, model TEXT, span_id TEXT, created_at TEXT NOT NULL);
CREATE TABLE escalation (
	id INTEGER PRIMARY KEY AUTOINCREMENT, turn_index INTEGER NOT NULL, edge TEXT NOT NULL,
	trigger TEXT, from_model TEXT NOT NULL, to_model TEXT NOT NULL, event_id TEXT, created_at TEXT NOT NULL);
CREATE TABLE cost (
	id INTEGER PRIMARY KEY AUTOINCREMENT, turn_index INTEGER NOT NULL, model TEXT NOT NULL,
	input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, usd REAL NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE turn (
	turn_index INTEGER PRIMARY KEY, model TEXT NOT NULL, started_at TEXT NOT NULL,
	ended_at TEXT, signals_json TEXT);
`

// Header is the session's identity + tier config (the one session row).
type Header struct {
	RunID        string
	CreatedAt    string
	ModelCheap   string
	ModelStrong  string
	Project      string
	SkillsDigest string
	Status       string
	// ParentRunID links a worker session to the parent that spawned it (empty for
	// a root session) — the sub-orchestration tree edge (T6). Profile and Duty
	// record which job-profile the worker ran under and the duty it was given.
	ParentRunID string
	Profile     string
	Duty        string
	// Selection is the deterministic prompt→profile matcher's logged decision for a
	// top-level run that auto-selected its profile (corpos #3096): a JSON object of
	// the prompt-features → chosen-profile → score/fallback that, joined with this
	// session's cost/tool_call rows + final status, is one labeled row for the later
	// ML matcher (#3098). Empty for an explicit -profile run or a spawned worker.
	Selection string
}

// Message is one conversation message.
type Message struct {
	ID        int64
	TurnIndex int
	Role      string
	Content   string
	Model     string
	CreatedAt string
}

// CostRow is one persisted model-call cost row (one per Complete call).
type CostRow struct {
	TurnIndex    int
	Model        string
	InputTokens  int
	OutputTokens int
	USD          float64
	CreatedAt    string
}

// ToolCallRow is one persisted dispatch telemetry row (one per tool dispatch).
type ToolCallRow struct {
	TurnIndex  int
	Surface    string
	Action     string
	ParamsJSON string
	ResultJSON string
	OK         bool
	ErrorClass string
	LatencyMS  int
	Model      string
	SpanID     string
	CreatedAt  string
}

// TurnRow is one persisted turn row (StartTurn opens it, EndTurn closes it).
type TurnRow struct {
	TurnIndex   int
	Model       string
	StartedAt   string
	EndedAt     string
	SignalsJSON string
}

// Store is read/write access to one session's SQLite DB.
type Store struct {
	db    *sql.DB
	runID string
	path  string
}

// NewRunID mints a ULID-style, time-sortable, filename-safe 26-char id.
func NewRunID() string {
	v := big.NewInt(time.Now().UnixMilli())
	v.Lsh(v, 80)
	rb := make([]byte, 10)
	_, _ = crand.Read(rb)
	v.Or(v, new(big.Int).SetBytes(rb))
	mask := big.NewInt(0x1f)
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = crockford[new(big.Int).And(v, mask).Int64()]
		v.Rsh(v, 5)
	}
	return string(out)
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single-writer SQLite — avoid "database is locked"
	return db, nil
}

// Create makes a new session DB and writes its header row. h.RunID may be empty
// (one is minted).
func Create(dir string, h Header) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	runID := h.RunID
	if runID == "" {
		runID = NewRunID()
	}
	path := filepath.Join(dir, runID+".db")
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("session DB already exists: %s", path)
	}
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		_ = db.Close()
		return nil, err
	}
	_, err = db.Exec(
		`INSERT INTO session (run_id, created_at, model_cheap, model_strong, project, skills_digest, status, parent_run_id, profile, duty, selection_json)
		 VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?)`,
		runID, nowStamp(), h.ModelCheap, h.ModelStrong, h.Project, h.SkillsDigest, nullable(h.ParentRunID), h.Profile, h.Duty, h.Selection)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, runID: runID, path: path}, nil
}

// Open reopens an existing session DB (resume/inspect); errors on a missing DB
// or a schema-version mismatch.
func Open(dir, runID string) (*Store, error) {
	path := filepath.Join(dir, runID+".db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no session DB at %s", path)
	}
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		_ = db.Close()
		return nil, err
	}
	if v != SchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("session DB schema version %d != supported %d", v, SchemaVersion)
	}
	return &Store{db: db, runID: runID, path: path}, nil
}

// RunID returns the session run id.
func (s *Store) RunID() string { return s.runID }

// Path returns the backing DB path.
func (s *Store) Path() string { return s.path }

// AppendMessage appends one message and returns its row id.
func (s *Store) AppendMessage(turnIndex int, role, content, model string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO message (turn_index, role, content, model, created_at) VALUES (?, ?, ?, ?, ?)`,
		turnIndex, role, content, nullable(model), nowStamp())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordToolCall records one MCP dispatch as a telemetry row.
func (s *Store) RecordToolCall(turnIndex int, surface, action, paramsJSON, resultJSON string, ok bool, errorClass string, latencyMS int, model, spanID string) error {
	_, err := s.db.Exec(
		`INSERT INTO tool_call (turn_index, surface, action, params_json, result_json, ok, error_class, latency_ms, model, span_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		turnIndex, surface, action, paramsJSON, resultJSON, boolToInt(ok), nullable(errorClass), latencyMS, nullable(model), nullable(spanID), nowStamp())
	return err
}

// RecordCost records one model call's token usage and priced cost.
func (s *Store) RecordCost(turnIndex int, model string, inputTokens, outputTokens int, usd float64) error {
	_, err := s.db.Exec(
		`INSERT INTO cost (turn_index, model, input_tokens, output_tokens, usd, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		turnIndex, model, inputTokens, outputTokens, usd, nowStamp())
	return err
}

// RecordEscalation records one tier-change edge (the router moving a turn onto a
// different model rung): edge is "escalate" or "deescalate", trigger summarizes
// the signal, and from/to name the models. eventID is optional (a substrate
// escalation event id when one is emitted).
func (s *Store) RecordEscalation(turnIndex int, edge, trigger, fromModel, toModel, eventID string) error {
	_, err := s.db.Exec(
		`INSERT INTO escalation (turn_index, edge, trigger, from_model, to_model, event_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		turnIndex, edge, nullable(trigger), fromModel, toModel, nullable(eventID), nowStamp())
	return err
}

// StartTurn opens a turn row (idempotent on re-open).
func (s *Store) StartTurn(turnIndex int, model string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO turn (turn_index, model, started_at) VALUES (?, ?, ?)`,
		turnIndex, model, nowStamp())
	return err
}

// EndTurn closes a turn row, stamping the observed signals.
func (s *Store) EndTurn(turnIndex int, signalsJSON string) error {
	_, err := s.db.Exec(
		`UPDATE turn SET ended_at = ?, signals_json = ? WHERE turn_index = ?`,
		nowStamp(), signalsJSON, turnIndex)
	return err
}

// SetStatus updates the session header status.
func (s *Store) SetStatus(status string) error {
	_, err := s.db.Exec(`UPDATE session SET status = ? WHERE run_id = ?`, status, s.runID)
	return err
}

// HeaderRow reads the session header row.
func (s *Store) HeaderRow() (Header, error) {
	var h Header
	var digest, parent sql.NullString
	err := s.db.QueryRow(
		`SELECT run_id, created_at, model_cheap, model_strong, project, skills_digest, status, parent_run_id, profile, duty, selection_json FROM session WHERE run_id = ?`,
		s.runID).Scan(&h.RunID, &h.CreatedAt, &h.ModelCheap, &h.ModelStrong, &h.Project, &digest, &h.Status, &parent, &h.Profile, &h.Duty, &h.Selection)
	if err != nil {
		return Header{}, err
	}
	h.SkillsDigest = digest.String
	h.ParentRunID = parent.String
	return h, nil
}

// Messages returns all messages in insertion order.
func (s *Store) Messages() ([]Message, error) {
	rows, err := s.db.Query(`SELECT id, turn_index, role, content, model, created_at FROM message ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		var m Message
		var model sql.NullString
		if err := rows.Scan(&m.ID, &m.TurnIndex, &m.Role, &m.Content, &model, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Model = model.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// NextTurnIndex returns max(turn_index)+1 over the turn rows (0 when none).
func (s *Store) NextTurnIndex() (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(turn_index) FROM turn`).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64) + 1, nil
}

// Escalation is one recorded tier-change edge.
type Escalation struct {
	TurnIndex int
	Edge      string
	Trigger   string
	FromModel string
	ToModel   string
}

// CostTotal sums this session's priced model-call cost.
func (s *Store) CostTotal() (float64, error) {
	var v float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(usd), 0) FROM cost`).Scan(&v)
	return v, err
}

// Counts returns this session's turn and tool-call totals.
func (s *Store) Counts() (turns, toolCalls int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM turn`).Scan(&turns); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM tool_call`).Scan(&toolCalls)
	return turns, toolCalls, err
}

// Escalations returns this session's tier-change edges in turn order.
func (s *Store) Escalations() ([]Escalation, error) {
	rows, err := s.db.Query(`SELECT turn_index, edge, trigger, from_model, to_model FROM escalation ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Escalation
	for rows.Next() {
		var e Escalation
		var trigger sql.NullString
		if err := rows.Scan(&e.TurnIndex, &e.Edge, &trigger, &e.FromModel, &e.ToModel); err != nil {
			return nil, err
		}
		e.Trigger = trigger.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// Costs returns all cost rows in insertion order.
func (s *Store) Costs() ([]CostRow, error) {
	rows, err := s.db.Query(`SELECT turn_index, model, input_tokens, output_tokens, usd, created_at FROM cost ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CostRow
	for rows.Next() {
		var c CostRow
		if err := rows.Scan(&c.TurnIndex, &c.Model, &c.InputTokens, &c.OutputTokens, &c.USD, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ToolCalls returns all tool_call rows in insertion order.
func (s *Store) ToolCalls() ([]ToolCallRow, error) {
	rows, err := s.db.Query(
		`SELECT turn_index, surface, action, params_json, result_json, ok, error_class, latency_ms, model, span_id, created_at FROM tool_call ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ToolCallRow
	for rows.Next() {
		var t ToolCallRow
		var ok int
		var errorClass, model, spanID sql.NullString
		if err := rows.Scan(&t.TurnIndex, &t.Surface, &t.Action, &t.ParamsJSON, &t.ResultJSON, &ok, &errorClass, &t.LatencyMS, &model, &spanID, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.OK = ok != 0
		t.ErrorClass = errorClass.String
		t.Model = model.String
		t.SpanID = spanID.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// Turns returns all turn rows ordered by turn index.
func (s *Store) Turns() ([]TurnRow, error) {
	rows, err := s.db.Query(`SELECT turn_index, model, started_at, ended_at, signals_json FROM turn ORDER BY turn_index`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TurnRow
	for rows.Next() {
		var t TurnRow
		var endedAt, signals sql.NullString
		if err := rows.Scan(&t.TurnIndex, &t.Model, &t.StartedAt, &endedAt, &signals); err != nil {
			return nil, err
		}
		t.EndedAt = endedAt.String
		t.SignalsJSON = signals.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// Close closes the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
