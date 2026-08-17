package main

import (
	"context"
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
	Node        string `json:"node"`
	CreatedAt   int64  `json:"created_at"`
	LastSeen    int64  `json:"last_seen"`
}

type Node struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
}

type SpawnSpec struct {
	Agent          string `json:"agent"`
	Node           string `json:"node"`
	Repo           string `json:"repo"`
	Prompt         string `json:"prompt,omitempty"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Force          bool   `json:"force,omitempty"`
}

type Spawn struct {
	ID             int64  `json:"id"`
	Agent          string `json:"agent"`
	Node           string `json:"node"`
	Repo           string `json:"repo"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	PermissionMode string `json:"permission_mode"`
	Status         string `json:"status"`
	SessionID      string `json:"session_id,omitempty"`
	PID            int    `json:"pid,omitempty"`
	Error          string `json:"error,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
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

var (
	ErrGuard        = errors.New("message rejected by send guard")
	ErrUnregistered = errors.New("agent not registered")
	ErrUnauthorized = errors.New("unauthorized")
)

const schema = `
CREATE TABLE IF NOT EXISTS agents (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    from_agent TEXT NOT NULL,
    to_agent   TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    acked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_inbox ON messages(to_agent, status, id);
CREATE TABLE IF NOT EXISTS nodes (
    name       TEXT PRIMARY KEY,
    version    TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS spawns (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agent           TEXT NOT NULL,
    node            TEXT NOT NULL,
    repo            TEXT NOT NULL,
    prompt          TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    permission_mode TEXT NOT NULL DEFAULT 'acceptEdits',
    status          TEXT NOT NULL DEFAULT 'queued',
    session_id      TEXT,
    pid             INTEGER,
    error           TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_claim ON spawns(node, status, id);`

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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	if err := ensureColumn(db, "agents", "node", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return dropSenderFK(db)
}

const messagesTable = `(
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    from_agent TEXT NOT NULL,
    to_agent   TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    acked_at   INTEGER
)`

func dropSenderFK(db *sql.DB) error {
	old, err := hasFK(db, "messages", "from_agent")
	if err != nil || !old {
		return err
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("migrate messages: %w", err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE messages_new ` + messagesTable,
		`INSERT INTO messages_new(id, from_agent, to_agent, body, status, created_at, acked_at)
		 SELECT id, from_agent, to_agent, body, status, created_at, acked_at FROM messages`,
		`DROP TABLE messages`,
		`ALTER TABLE messages_new RENAME TO messages`,
		`CREATE INDEX IF NOT EXISTS idx_inbox ON messages(to_agent, status, id)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate messages: %w", err)
		}
	}
	return tx.Commit()
}

func hasFK(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT "from" FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s keys: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var from string
		if err := rows.Scan(&from); err != nil {
			return false, err
		}
		if from == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

func ensureColumn(db *sql.DB, table, col, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, col, err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// Register upserts an agent, bumping last_seen. Idempotent.
func (s *Store) Register(name, desc string) error { return s.RegisterOn(name, desc, "") }

func (s *Store) RegisterOn(name, desc, node string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("agent name required")
	}
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO agents(name, description, node, created_at, last_seen) VALUES(?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			description=excluded.description,
			node=CASE WHEN excluded.node!='' THEN excluded.node ELSE agents.node END,
			last_seen=excluded.last_seen`,
		name, desc, node, now, now)
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
	rows, err := s.db.Query(`SELECT name, description, node, created_at, last_seen FROM agents ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Name, &a.Description, &a.Node, &a.CreatedAt, &a.LastSeen); err != nil {
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
		return 0, fmt.Errorf("%w: sender %q", ErrUnregistered, from)
	}
	if ok, err := s.exists(to); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("%w: recipient %q", ErrUnregistered, to)
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

func (s *Store) RecvWait(ctx context.Context, name string, d time.Duration) (*Message, error) {
	return pollUntil(ctx, d, func() (*Message, error) { return s.Recv(name) })
}

func pollUntil[T any](ctx context.Context, d time.Duration, fn func() (*T, error)) (*T, error) {
	deadline := time.Now().Add(d)
	for {
		v, err := fn()
		if err != nil || v != nil {
			return v, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		if remaining > pollTick {
			remaining = pollTick
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(remaining):
		}
	}
}

const pollTick = 200 * time.Millisecond

func (s *Store) Heartbeat(name, version string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("node name required")
	}
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO nodes(name, version, created_at, last_seen) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET version=excluded.version, last_seen=excluded.last_seen`,
		name, version, now, now)
	if err != nil {
		return fmt.Errorf("heartbeat %q: %w", name, err)
	}
	return nil
}

func (s *Store) Nodes() ([]Node, error) {
	rows, err := s.db.Query(`SELECT name, version, created_at, last_seen FROM nodes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("nodes: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.Name, &n.Version, &n.CreatedAt, &n.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) Spawn(spec SpawnSpec) (int64, error) {
	if strings.TrimSpace(spec.Agent) == "" || strings.TrimSpace(spec.Node) == "" || strings.TrimSpace(spec.Repo) == "" {
		return 0, fmt.Errorf("spawn requires agent, node and repo")
	}
	if !spec.Force {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM nodes WHERE name=?`, spec.Node).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("%w: node %q (run `gary node --name %s` there, or pass --force)",
				ErrUnregistered, spec.Node, spec.Node)
		}
		var owner string
		err := s.db.QueryRow(`SELECT node FROM agents WHERE name=?`, spec.Agent).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if owner != "" && owner != spec.Node {
			return 0, fmt.Errorf("agent %q already runs on node %q (pass --force to move it)", spec.Agent, owner)
		}
	}
	mode := spec.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`
		INSERT INTO spawns(agent, node, repo, prompt, model, permission_mode, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		spec.Agent, spec.Node, spec.Repo, spec.Prompt, spec.Model, mode, now, now)
	if err != nil {
		return 0, fmt.Errorf("spawn: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) ClaimSpawn(node string) (*Spawn, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var sp Spawn
	err = tx.QueryRow(`
		SELECT id, agent, node, repo, prompt, model, permission_mode, created_at
		FROM spawns WHERE node=? AND status='queued' ORDER BY id LIMIT 1`, node).
		Scan(&sp.ID, &sp.Agent, &sp.Node, &sp.Repo, &sp.Prompt, &sp.Model, &sp.PermissionMode, &sp.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`UPDATE spawns SET status='claimed', updated_at=? WHERE id=?`, now, sp.ID); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sp.Status = "claimed"
	sp.UpdatedAt = now
	return &sp, nil
}

func (s *Store) ClaimSpawnWait(ctx context.Context, node string, d time.Duration) (*Spawn, error) {
	return pollUntil(ctx, d, func() (*Spawn, error) { return s.ClaimSpawn(node) })
}

func (s *Store) UpdateSpawn(id int64, status, sessionID string, pid int, errMsg string) error {
	_, err := s.db.Exec(`
		UPDATE spawns SET
			status     = CASE WHEN ?!='' THEN ? ELSE status END,
			session_id = CASE WHEN ?!='' THEN ? ELSE session_id END,
			pid        = CASE WHEN ?!=0  THEN ? ELSE pid END,
			error      = CASE WHEN ?!='' THEN ? ELSE error END,
			updated_at = ?
		WHERE id=?`,
		status, status, sessionID, sessionID, pid, pid, errMsg, errMsg, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("update spawn %d: %w", id, err)
	}
	return nil
}

func (s *Store) SpawnStatus(id int64) (string, error) {
	var st string
	err := s.db.QueryRow(`SELECT status FROM spawns WHERE id=?`, id).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("spawn %d not found", id)
	}
	return st, err
}

func (s *Store) StopSpawn(agent string) error {
	res, err := s.db.Exec(`
		UPDATE spawns SET status='stopping', updated_at=?
		WHERE agent=? AND status IN ('queued','claimed','running')`, time.Now().Unix(), agent)
	if err != nil {
		return fmt.Errorf("stop %q: %w", agent, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no running spawn for agent %q", agent)
	}
	return nil
}

func (s *Store) Spawns(limit int) ([]Spawn, error) {
	rows, err := s.db.Query(`
		SELECT id, agent, node, repo, prompt, model, permission_mode, status,
		       COALESCE(session_id,''), COALESCE(pid,0), COALESCE(error,''), created_at, updated_at
		FROM spawns ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("spawns: %w", err)
	}
	defer rows.Close()
	var out []Spawn
	for rows.Next() {
		var sp Spawn
		if err := rows.Scan(&sp.ID, &sp.Agent, &sp.Node, &sp.Repo, &sp.Prompt, &sp.Model,
			&sp.PermissionMode, &sp.Status, &sp.SessionID, &sp.PID, &sp.Error, &sp.CreatedAt, &sp.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
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
