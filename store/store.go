package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session is a stored session record.
type Session struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    string `json:"createdAt"`
	MessageCount int    `json:"messageCount"`
}

// StoredMessage is a message retrieved from the database.
type StoredMessage struct {
	Seq        int    `json:"seq"`
	Role       string `json:"role"`
	Content    string `json:"content"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`
	Pinned     bool   `json:"pinned"`
	ToolName   string `json:"toolName"`
	ToolArgs   string `json:"toolArgs"`
	ToolResult string `json:"toolResult"`
	Time       string `json:"time"`
}

// NewMessage is the set of fields supplied when persisting one message row.
// Role is one of "user", "assistant", "cot" (a hidden chain-of-thought
// evaluation pass), or "tool" (a single tool call/result).
type NewMessage struct {
	Role       string
	Content    string
	Model      string
	Mode       string
	Pinned     bool
	ToolName   string
	ToolArgs   string
	ToolResult string
}

// PlanStep is one step of a session's plan (see the manage_plan tool).
// Status is one of "pending", "in_progress", "completed", "failed".
type PlanStep struct {
	Seq       int    `json:"seq"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updatedAt"`
}

// GenOptions holds per-session Ollama generation parameters. Pointer fields
// are optional: a nil value means "don't override the model default." Only
// non-nil fields are sent to Ollama in the Options map.
type GenOptions struct {
	NumCtx        *int     `json:"num_ctx,omitempty"`
	NumPredict    *int     `json:"num_predict,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`
}

// ToMap converts set fields into the map[string]any Ollama's Options field
// expects. Returns nil when no field is set, so callers can pass it directly
// to chatRequestOnce without allocating an empty map.
func (g GenOptions) ToMap() map[string]any {
	m := map[string]any{}
	if g.NumCtx != nil {
		m["num_ctx"] = *g.NumCtx
	}
	if g.NumPredict != nil {
		m["num_predict"] = *g.NumPredict
	}
	if g.Temperature != nil {
		m["temperature"] = *g.Temperature
	}
	if g.TopP != nil {
		m["top_p"] = *g.TopP
	}
	if g.TopK != nil {
		m["top_k"] = *g.TopK
	}
	if g.RepeatPenalty != nil {
		m["repeat_penalty"] = *g.RepeatPenalty
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// ArtifactMeta is a lightweight artifact listing (no content) for sidebar display.
type ArtifactMeta struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ContentType string `json:"contentType"`
	CreatedAt   string `json:"createdAt"`
}

// Artifact is a full artifact record, including content.
type Artifact struct {
	ID          string `json:"id"`
	SessionID   string `json:"sessionId"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
	CreatedAt   string `json:"createdAt"`
}

// Store wraps DuckDB-backed session storage and provides thread-safe access.
type Store struct {
	db *sql.DB
	// mu guards currentSession, the only in-memory state here.
	mu sync.Mutex
	// writeMu serializes memory-table writes.
	//
	// It is not belt-and-braces. DuckDB detects write-write conflicts between
	// concurrent transactions and *fails* the loser, and two memory writers are
	// routine: the ingestion queue's worker writes while the chat goroutine searches,
	// and a search recomputes and stores BM25 corpus statistics. Without this, an
	// ingest racing a search raises `Duplicate key "key: stats_dirty" violates
	// primary key constraint` even though the statement is an upsert — observed, not
	// theorized (see TestConcurrentMemoryWrites).
	//
	// Reads deliberately do not take it: DuckDB's MVCC serves them from a snapshot,
	// and holding a lock across search would put ingestion in the path of every
	// query, which is the thing the queue exists to prevent.
	writeMu        sync.Mutex
	currentSession string
}

// lockWrites serializes a memory-table write. Callers must not hold it while
// calling another write method — no method here nests.
func (s *Store) lockWrites() func() {
	s.writeMu.Lock()
	return s.writeMu.Unlock
}

// Path returns the path to the DuckDB file next to config.json / executable.
func Path() string {
	execPath, err := os.Executable()
	if err == nil {
		for _, dir := range []string{filepath.Dir(execPath), "."} {
			p := filepath.Join(dir, "sessions.db")
			if _, statErr := os.Stat(p); statErr == nil {
				return p
			}
		}
	}
	return "./sessions.db"
}

// Open opens (or creates) the DuckDB database at the default location and
// ensures tables exist. Returns a ready Store with an active session selected or
// auto-created.
func Open() (*Store, error) { return OpenAt(Path()) }

// DB exposes the underlying handle. Intended for tests and for ad-hoc queries in
// the memory layer that do not warrant a dedicated method; ordinary callers
// should use the typed methods so access stays serialized and consistent.
func (s *Store) DB() *sql.DB { return s.db }

// OpenAt opens (or creates) the database at an explicit path. Open uses this with
// the default location; tests use it directly to work against a temporary file.
func OpenAt(path string) (*Store, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}

	// Bound the connection pool.
	//
	// `database/sql` defaults to *unlimited* open connections, opening a new one per
	// concurrent query. For a client/server database that is reasonable; for an embedded
	// single-writer engine reached through cgo it is not, because every in-flight query
	// pins an OS thread inside native code for its duration. A burst of concurrent
	// queries therefore turns into a burst of threads sitting in DuckDB, which is both
	// wasteful and a much worse failure mode than waiting.
	//
	// Four is enough for the access pattern: one background worker doing bulk writes
	// (serialized by writeMu anyway) and a handful of short UI reads. Anything beyond
	// that queues in Go, where it is visible and cheap, rather than in cgo.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: db}

	if err := applySchema(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.SetMeta(MetaSchemaVersion, MemorySchemaVersion); err != nil {
		db.Close()
		return nil, err
	}

	// One-time (idempotent) repair of artifacts left behind by the pre-cascade
	// DeleteSession — see cleanupOrphanedArtifacts. Logged, not silent.
	if n, err := s.cleanupOrphanedArtifacts(); err != nil {
		db.Close()
		return nil, err
	} else if n > 0 {
		slog.Info("removed orphaned artifacts whose session no longer exists", "count", n)
	}

	// Pick an existing session or create one.
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("count sessions: %w", err)
	}

	if count == 0 {
		id := s.createSessionInternal("", true)
		s.currentSession = id
	} else {
		err = db.QueryRow(
			"SELECT id FROM sessions ORDER BY created_at DESC LIMIT 1",
		).Scan(&s.currentSession)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("select latest session: %w", err)
		}
	}

	return s, nil
}

// applySchema creates and migrates every table, idempotently.
//
// One function rather than three calls at the open site so that the sequence cannot
// drift: tests build a Store directly and previously reproduced this DDL by hand,
// which silently missed the Phase 7 migration and failed with a binder error on a
// column that existed everywhere except in tests.
func applySchema(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}
	if _, err := db.Exec(memorySchemaSQL); err != nil {
		return fmt.Errorf("create memory tables: %w", err)
	}
	// Columns added after the first release. Idempotent, so it runs unconditionally
	// rather than being gated on the recorded schema version — see
	// memoryMigrationSQL.
	if _, err := db.Exec(memoryMigrationSQL); err != nil {
		return fmt.Errorf("migrate memory tables: %w", err)
	}
	return nil
}

// schemaSQL is the full schema, applied idempotently on every Open. Kept as a
// package-level constant so tests can build a store against a temporary path
// without duplicating the DDL.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS sessions (
	id        TEXT PRIMARY KEY,
	title     TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	session_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	model      TEXT NOT NULL DEFAULT '',
	mode       TEXT NOT NULL DEFAULT '',
	pinned     BOOLEAN NOT NULL DEFAULT true,
	role       TEXT NOT NULL,
	content    TEXT NOT NULL,
	tool_name  TEXT NOT NULL DEFAULT '',
	tool_args  TEXT NOT NULL DEFAULT '',
	tool_result TEXT NOT NULL DEFAULT '',
	timestamp  TIMESTAMP NOT NULL,
	PRIMARY KEY (session_id, seq)
);
CREATE TABLE IF NOT EXISTS artifacts (
	id           TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	title        TEXT NOT NULL DEFAULT '',
	content      TEXT NOT NULL,
	content_type TEXT NOT NULL DEFAULT 'text',
	created_at   TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS plan_steps (
	session_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	content    TEXT NOT NULL,
	status     TEXT NOT NULL DEFAULT 'pending',
	updated_at TIMESTAMP NOT NULL,
	PRIMARY KEY (session_id, seq)
);
CREATE TABLE IF NOT EXISTS session_tool_disabled (
	session_id TEXT NOT NULL,
	tool_name  TEXT NOT NULL,
	PRIMARY KEY (session_id, tool_name)
);
CREATE TABLE IF NOT EXISTS session_gen_options (
	session_id   TEXT PRIMARY KEY,
	options_json TEXT NOT NULL DEFAULT '{}'
);`

// Close shuts down the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateSession inserts a new session and makes it active. Returns its ID.
func (s *Store) CreateSession(title string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if title == "" {
		title = "New Chat"
	}
	id := s.createSessionInternal(title, false)
	s.currentSession = id
	return id, nil
}

// CurrentSession returns the ID of the active session.
func (s *Store) CurrentSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentSession
}

// SwitchSession changes the active session to an existing one.
func (s *Store) SwitchSession(id string) error {
	var exists bool
	err := s.db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", id,
	).Scan(&exists)
	if err != nil || !exists {
		return fmt.Errorf("session %s not found", id)
	}
	s.mu.Lock()
	s.currentSession = id
	s.mu.Unlock()
	return nil
}

// DeleteSession removes a session, its messages, plan, artifacts, and every
// piece of memory derived from any of them.
//
// Two behaviors worth calling out. First, artifacts now cascade: previously they
// carried a session_id but were never deleted, so they survived as orphans —
// unintentional, and fixed here (see cleanupOrphanedArtifacts for the one-time
// repair of rows already orphaned). Second, memory derived from this session
// dies with it, while directory-sourced memory is untouched because it has no
// session_id — that asymmetry is deliberate.
//
// Ordering matters: memory rows are removed before the artifacts they were
// derived from, so the cascade can still resolve source_ref -> artifact.
//
// If the deleted session was current, a fresh replacement is created so there is
// always an active session.
func (s *Store) DeleteSession(id string) error {
	var exists bool
	err := s.db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", id,
	).Scan(&exists)
	if err != nil || !exists {
		return fmt.Errorf("session %s not found", id)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	s.mu.Lock()
	wasCurrent := s.currentSession == id
	s.mu.Unlock()

	// Memory first, while the artifacts it was derived from still exist.
	if err := deleteMemoryForSessionTx(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM artifacts WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("delete artifacts: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM messages WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM plan_steps WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("delete plan: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM session_tool_disabled WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("delete tool settings: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM session_gen_options WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("delete gen options: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	// The corpus shrank, so BM25's df/N/avgdl now describe a corpus that no
	// longer exists. Recompute lazily on next search rather than trying to
	// decrement correctly here.
	if err := markStatsDirtyTx(tx); err != nil {
		return err
	}

	if wasCurrent {
		newID := s.createSessionInternal("", true)
		s.mu.Lock()
		s.currentSession = newID
		s.mu.Unlock()
	}

	return tx.Commit()
}

// HasMessages reports whether any messages have been persisted for a session.
func (s *Store) HasMessages(sessionID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM messages WHERE session_id = ?)", sessionID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check messages: %w", err)
	}
	return exists, nil
}

// SaveMessage persists a message for the given session and returns its seq.
// The lock spans both the seq lookup and the insert — releasing it in
// between would let two concurrent calls read the same MAX(seq)+1 and then
// both insert, colliding on the (session_id, seq) primary key.
func (s *Store) SaveMessage(sessionID string, msg NewMessage) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var seq int
	err := s.db.QueryRow(
		"SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE session_id = ?", sessionID,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("get seq: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(
		`INSERT INTO messages (session_id, seq, role, content, model, mode, pinned, tool_name, tool_args, tool_result, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, seq, msg.Role, msg.Content, msg.Model, msg.Mode, msg.Pinned, msg.ToolName, msg.ToolArgs, msg.ToolResult, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	return seq, nil
}

// SetMessagePinned toggles whether a message is included in the context sent
// to the model on future turns. The message row itself is never deleted.
func (s *Store) SetMessagePinned(sessionID string, seq int, pinned bool) error {
	_, err := s.db.Exec(
		"UPDATE messages SET pinned = ? WHERE session_id = ? AND seq = ?",
		pinned, sessionID, seq,
	)
	return err
}

// GetSessions returns all sessions ordered newest first. The message count
// only counts user/assistant turns — internal cot/tool rows are excluded so
// the count reflects actual conversation length, not bookkeeping detail.
func (s *Store) GetSessions() ([]Session, error) {
	rows, err := s.db.Query(`
SELECT s.id, COALESCE(s.title,''), s.created_at, COALESCE(mm.msg_count,0) AS msg_count
FROM sessions s
LEFT JOIN (
	SELECT session_id, COUNT(m.session_id) AS msg_count, MAX(m.timestamp) AS timestamp
	FROM messages m
	WHERE m.role IN ('user', 'assistant')
	GROUP BY m.session_id
) AS mm ON mm.session_id = s.id
ORDER BY COALESCE(mm.timestamp, s.created_at) DESC`)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	sessions := make([]Session, 0)
	for rows.Next() {
		var ss Session
		if err := rows.Scan(&ss.ID, &ss.Title, &ss.CreatedAt, &ss.MessageCount); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, ss)
	}
	return sessions, nil
}

// GetMessages returns all messages for a given session in order.
func (s *Store) GetMessages(sessionID string) ([]StoredMessage, error) {
	rows, err := s.db.Query(
		`SELECT seq, role, content, model, mode, pinned, tool_name, tool_args, tool_result, timestamp
		 FROM messages WHERE session_id = ? ORDER BY seq`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	msgs := make([]StoredMessage, 0)
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.Seq, &m.Role, &m.Content, &m.Model, &m.Mode, &m.Pinned, &m.ToolName, &m.ToolArgs, &m.ToolResult, &m.Time); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// RenameSession updates the title of an existing session.
func (s *Store) RenameSession(id string, title string) error {
	if title == "" {
		title = "New Chat"
	}
	_, err := s.db.Exec("UPDATE sessions SET title = ? WHERE id = ?", title, id)
	return err
}

// CreateArtifact persists a new artifact for the given session and returns its ID.
func (s *Store) CreateArtifact(sessionID, title, content, contentType string) (string, error) {
	if title == "" {
		title = "Untitled"
	}
	if contentType == "" {
		contentType = "text"
	}
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		"INSERT INTO artifacts (id, session_id, title, content, content_type, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, sessionID, title, content, contentType, now,
	)
	if err != nil {
		return "", fmt.Errorf("insert artifact: %w", err)
	}
	return id, nil
}

// GetArtifactsForSession returns metadata (no content) for all artifacts in a
// session, ordered newest first.
func (s *Store) GetArtifactsForSession(sessionID string) ([]ArtifactMeta, error) {
	rows, err := s.db.Query(
		"SELECT id, title, content_type, created_at FROM artifacts WHERE session_id = ? ORDER BY created_at DESC",
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query artifacts: %w", err)
	}
	defer rows.Close()

	artifacts := make([]ArtifactMeta, 0)
	for rows.Next() {
		var m ArtifactMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.ContentType, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		artifacts = append(artifacts, m)
	}
	return artifacts, nil
}

// GetArtifact returns the full record (including content) for a single artifact.
func (s *Store) GetArtifact(id string) (Artifact, error) {
	var a Artifact
	err := s.db.QueryRow(
		"SELECT id, session_id, title, content, content_type, created_at FROM artifacts WHERE id = ?", id,
	).Scan(&a.ID, &a.SessionID, &a.Title, &a.Content, &a.ContentType, &a.CreatedAt)
	if err != nil {
		return Artifact{}, fmt.Errorf("get artifact %s: %w", id, err)
	}
	return a, nil
}

// SetPlan replaces the entire plan for a session with the given steps.
// manage_plan is full-replace semantics (the model always sends the complete
// list), but this upserts rather than deleting-then-reinserting every row:
// DuckDB's ART index (used for the (session_id, seq) primary key) has a known
// limitation where deleting and reinserting the same key within a single
// transaction can raise a spurious constraint violation — see the "known
// index limitations" section of https://duckdb.org/docs/sql/indexes — which
// is exactly what a plain clear-and-reinsert hit here in practice, since a
// step's (session_id, seq) is usually unchanged between calls. Only seqs that
// no longer exist in the new, shorter plan are actually deleted, and those
// are by definition disjoint from the seqs just upserted, so no key is ever
// deleted and reinserted in the same transaction.
func (s *Store) SetPlan(sessionID string, steps []PlanStep) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, step := range steps {
		if _, err := tx.Exec(
			`INSERT INTO plan_steps (session_id, seq, content, status, updated_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (session_id, seq) DO UPDATE SET
			   content = excluded.content, status = excluded.status, updated_at = excluded.updated_at`,
			sessionID, step.Seq, step.Content, step.Status, now,
		); err != nil {
			return fmt.Errorf("upsert plan step: %w", err)
		}
	}

	if _, err := tx.Exec("DELETE FROM plan_steps WHERE session_id = ? AND seq > ?", sessionID, len(steps)); err != nil {
		return fmt.Errorf("trim plan: %w", err)
	}

	return tx.Commit()
}

// GetPlan returns the current plan for a session in step order — empty if no
// plan has been created yet.
func (s *Store) GetPlan(sessionID string) ([]PlanStep, error) {
	rows, err := s.db.Query(
		"SELECT seq, content, status, updated_at FROM plan_steps WHERE session_id = ? ORDER BY seq",
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query plan: %w", err)
	}
	defer rows.Close()

	steps := make([]PlanStep, 0)
	for rows.Next() {
		var p PlanStep
		if err := rows.Scan(&p.Seq, &p.Content, &p.Status, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan plan step: %w", err)
		}
		steps = append(steps, p)
	}
	return steps, nil
}

// GetDisabledTools returns the names of tools the user has explicitly disabled
// for the given session. An absent row means the tool is enabled (default).
func (s *Store) GetDisabledTools(sessionID string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT tool_name FROM session_tool_disabled WHERE session_id = ? ORDER BY tool_name",
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get disabled tools: %w", err)
	}
	defer rows.Close()
	var tools []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tool name: %w", err)
		}
		tools = append(tools, name)
	}
	return tools, rows.Err()
}

// SetToolEnabled enables or disables a tool for a session. A disabled tool
// has a row in session_tool_disabled; enabling it removes that row.
func (s *Store) SetToolEnabled(sessionID, toolName string, enabled bool) error {
	defer s.lockWrites()()
	if enabled {
		_, err := s.db.Exec(
			"DELETE FROM session_tool_disabled WHERE session_id = ? AND tool_name = ?",
			sessionID, toolName,
		)
		return err
	}
	_, err := s.db.Exec(
		"INSERT INTO session_tool_disabled (session_id, tool_name) VALUES (?, ?) ON CONFLICT DO NOTHING",
		sessionID, toolName,
	)
	return err
}

// GetSessionGenOptions returns the persisted generation options for a session.
// Returns a zero-value GenOptions (all fields nil) if none have been set.
func (s *Store) GetSessionGenOptions(sessionID string) (GenOptions, error) {
	var raw string
	err := s.db.QueryRow(
		"SELECT options_json FROM session_gen_options WHERE session_id = ?", sessionID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return GenOptions{}, nil
	}
	if err != nil {
		return GenOptions{}, fmt.Errorf("get session gen options: %w", err)
	}
	var opts GenOptions
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		return GenOptions{}, fmt.Errorf("parse session gen options: %w", err)
	}
	return opts, nil
}

// SetSessionGenOptions persists generation options for a session, replacing
// any previously stored values.
func (s *Store) SetSessionGenOptions(sessionID string, opts GenOptions) error {
	data, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("marshal gen options: %w", err)
	}
	defer s.lockWrites()()
	_, err = s.db.Exec(
		`INSERT INTO session_gen_options (session_id, options_json) VALUES (?, ?)
		 ON CONFLICT (session_id) DO UPDATE SET options_json = excluded.options_json`,
		sessionID, string(data),
	)
	return err
}

// createSessionInternal inserts a row into the sessions table and returns its ID.
func (s *Store) createSessionInternal(title string, _ bool) string {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	s.db.Exec("INSERT INTO sessions (id, title, created_at) VALUES (?, ?, ?)", id, title, now)
	return id
}
