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
	Role    string `json:"role"`
	Content string `json:"content"`
	Time    string `json:"time"`
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
	role       TEXT NOT NULL,
	content    TEXT NOT NULL,
	timestamp  TIMESTAMP NOT NULL,
	PRIMARY KEY (session_id, seq)
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

// SaveMessage persists a message for the given session.
func (s *Store) SaveMessage(sessionID string, role, content string) error {
	s.mu.Lock()
	var seq int
	err := s.db.QueryRow(
		"SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE session_id = ?", sessionID,
	).Scan(&seq)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("get seq: %w", err)
	}
	s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(
		"INSERT INTO messages (session_id, seq, role, content, timestamp) VALUES (?, ?, ?, ?, ?)",
		sessionID, seq, role, content, now,
	)
	return err
}

// GetSessions returns all sessions ordered newest first.
func (s *Store) GetSessions() ([]Session, error) {
	rows, err := s.db.Query(`
SELECT s.id, COALESCE(s.title,''), s.created_at, COALESCE(mm.msg_count,0) AS msg_count
FROM sessions s
LEFT JOIN (
	SELECT session_id, COUNT(m.session_id) AS msg_count, MAX(m.timestamp) AS timestamp
	FROM messages m
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
		"SELECT role, content, timestamp FROM messages WHERE session_id = ? ORDER BY seq",
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	msgs := make([]StoredMessage, 0)
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.Role, &m.Content, &m.Time); err != nil {
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

// createSessionInternal inserts a row into the sessions table and returns its ID.
func (s *Store) createSessionInternal(title string, _ bool) string {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	s.db.Exec("INSERT INTO sessions (id, title, created_at) VALUES (?, ?, ?)", id, title, now)
	return id
}
