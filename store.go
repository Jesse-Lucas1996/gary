package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the whole persistence layer: one SQLite file, plain functions.
type Store struct{ db *sql.DB }

type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   int64  `json:"created_at"`
	LastSeen    int64  `json:"last_seen"`
}

type Message struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Body      string `json:"body"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	AckedAt   *int64 `json:"acked_at,omitempty"`
}

var ErrGuard = errors.New("message rejected by send guard")

const schema = `
CREATE TABLE IF NOT EXISTS agents (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    from_agent TEXT NOT NULL REFERENCES agents(name),
    to_agent   TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    acked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_inbox ON messages(to_agent, status, id);`

// DBPath resolves the database file: --db flag > $GARY_DB > XDG > ~/.local/share.
func DBPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv("GARY_DB"); env != "" {
		return env, nil
	}
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "gary", "gary.db"), nil
}

// Open opens (creating if needed) the DB at path with WAL + foreign keys, migrates.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	// _txlock=immediate: recv reads-then-writes in one txn; taking the write lock
	// upfront lets busy_timeout serialize concurrent receivers instead of failing
	// with SQLITE_BUSY_SNAPSHOT on the read→write upgrade.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Register upserts an agent, bumping last_seen. Idempotent.
func (s *Store) Register(name, desc string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO agents(name, description, created_at, last_seen) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET description=excluded.description, last_seen=excluded.last_seen`,
		name, desc, now, now)
	if err != nil {
		return fmt.Errorf("register %q: %w", name, err)
	}
	return nil
}

// Unregister removes an agent; ON DELETE CASCADE drops its queue.
func (s *Store) Unregister(name string) error {
	res, err := s.db.Exec(`DELETE FROM agents WHERE name=?`, name)
	if err != nil {
		return fmt.Errorf("unregister %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent %q not registered", name)
	}
	return nil
}

func (s *Store) List() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT name, description, created_at, last_seen FROM agents ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Name, &a.Description, &a.CreatedAt, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) exists(name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM agents WHERE name=?`, name).Scan(&n)
	return n > 0, err
}

// Send enqueues a message after validating both agents and the etiquette guard.
func (s *Store) Send(from, to, body string) (int64, error) {
	if err := checkGuard(body); err != nil {
		return 0, err
	}
	if ok, err := s.exists(from); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("sender %q not registered", from)
	}
	if ok, err := s.exists(to); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("recipient %q not registered", to)
	}
	res, err := s.db.Exec(`INSERT INTO messages(from_agent, to_agent, body, created_at) VALUES(?,?,?,?)`,
		from, to, body, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("send: %w", err)
	}
	return res.LastInsertId()
}

// Inbox returns pending messages for name, FIFO, without consuming them.
func (s *Store) Inbox(name string) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, from_agent, to_agent, body, status, created_at, acked_at
		FROM messages WHERE to_agent=? AND status='pending' ORDER BY id`, name)
	if err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Body, &m.Status, &m.CreatedAt, &m.AckedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Recent returns the most recent messages across all agents, newest first,
// for the dashboard's live view. Read-only — does not touch message status.
func (s *Store) Recent(limit int) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, from_agent, to_agent, body, status, created_at, acked_at
		FROM messages ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Body, &m.Status, &m.CreatedAt, &m.AckedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Recv atomically dequeues the oldest pending message and marks it acked.
// Returns (nil, nil) when the queue is empty.
func (s *Store) Recv(name string) (*Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var m Message
	err = tx.QueryRow(`
		SELECT id, from_agent, to_agent, body, created_at
		FROM messages WHERE to_agent=? AND status='pending' ORDER BY id LIMIT 1`, name).
		Scan(&m.ID, &m.From, &m.To, &m.Body, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`UPDATE messages SET status='acked', acked_at=? WHERE id=?`, now, m.ID); err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.Status = "acked"
	m.AckedAt = &now
	return &m, nil
}

// checkGuard is the "no thank-you" send guard: rejects empty and pleasantry-only
// bodies. Heuristic with a known ceiling — stops the obvious flood, not every
// paraphrase. The real guard is the contract agents follow (see CLAUDE.md).
// small deny-pattern by design; do NOT grow this into a classifier.
var pleasantry = regexp.MustCompile(`(?i)^\W*(thanks?( you)?|thx|ty|got it|sounds good|will do|no problem|np|ok(ay)?|cool|great|awesome|nice|perfect|understood|acknowledged|ack|received|roger|cheers|👍|🙏|👌)[\s!.]*$`)

func checkGuard(body string) error {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return fmt.Errorf("%w: empty message", ErrGuard)
	}
	if pleasantry.MatchString(trimmed) {
		return fmt.Errorf("%w: acknowledgement-only message (send only when action or info is needed)", ErrGuard)
	}
	return nil
}
