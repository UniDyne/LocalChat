package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session is a stored session record.
type Session struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	CreatedAt      string `json:"createdAt"`
	MessageCount int  `json:"messageCount"`
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
	Role        string
	Content     string
	Model       string
	Mode        string
	Pinned      bool
	ToolName    string
	ToolArgs    string
	ToolResult  string
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
	mu sync.Mutex
	currentSession string
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

// Open opens (or creates) the DuckDB database and ensures tables exist.
// Returns a ready Store with an active session selected or auto-created.
func Open() (*Store, error) {
	db, err := sql.Open("duckdb", Path())
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}

	s := &Store{db: db}

	createSQL := `
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
);`

	if _, err := db.Exec(createSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
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

// DeleteSession removes a session and its messages. If it was the current
// session, create a fresh replacement so there's always an active session.
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

	if _, err := tx.Exec("DELETE FROM messages WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete session: %w", err)
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
func (s *Store) SaveMessage(sessionID string, msg NewMessage) (int, error) {
	s.mu.Lock()
	var seq int
	err := s.db.QueryRow(
		"SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE session_id = ?", sessionID,
	).Scan(&seq)
	if err != nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("get seq: %w", err)
	}
	s.mu.Unlock()

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

// createSessionInternal inserts a row into the sessions table and returns its ID.
func (s *Store) createSessionInternal(title string, _ bool) string {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	s.db.Exec("INSERT INTO sessions (id, title, created_at) VALUES (?, ?, ?)", id, title, now)
	return id
}
