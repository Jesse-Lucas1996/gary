package main

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFIFOAndAutoAck(t *testing.T) {
	s := testStore(t)
	mustReg(t, s, "a")
	mustReg(t, s, "b")
	for _, body := range []string{"first", "second", "third"} {
		if _, err := s.Send("a", "b", body); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"first", "second", "third"} {
		m, err := s.Recv("b")
		if err != nil || m == nil {
			t.Fatalf("recv: %v %v", m, err)
		}
		if m.Body != want {
			t.Fatalf("FIFO broken: got %q want %q", m.Body, want)
		}
		if m.Status != "acked" || m.AckedAt == nil {
			t.Fatalf("recv did not auto-ack: %+v", m)
		}
	}
	if m, _ := s.Recv("b"); m != nil {
		t.Fatalf("expected empty queue, got %+v", m)
	}
}

func TestConcurrentRecvDeliversOnce(t *testing.T) {
	s := testStore(t)
	mustReg(t, s, "a")
	mustReg(t, s, "b")
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := s.Send("a", "b", "msg"); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				m, err := s.Recv("b")
				if err != nil {
					t.Error(err)
					return
				}
				if m == nil {
					return
				}
				mu.Lock()
				if seen[m.ID] {
					t.Errorf("message #%d delivered twice", m.ID)
				}
				seen[m.ID] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("delivered %d messages, want %d", len(seen), n)
	}
}

func TestSendToUnregistered(t *testing.T) {
	s := testStore(t)
	mustReg(t, s, "a")
	if _, err := s.Send("a", "ghost", "hello"); err == nil {
		t.Fatal("expected error sending to unregistered recipient")
	}
	if _, err := s.Send("ghost", "a", "hello"); err == nil {
		t.Fatal("expected error sending from unregistered sender")
	}
}

func TestSendGuard(t *testing.T) {
	s := testStore(t)
	mustReg(t, s, "a")
	mustReg(t, s, "b")
	reject := []string{"", "   ", "thanks", "Thanks!", "thank you", "got it", "sounds good", "ok", "👍", "will do."}
	for _, body := range reject {
		if _, err := s.Send("a", "b", body); err == nil {
			t.Errorf("guard should reject %q", body)
		}
	}
	allow := []string{"please review PR #12", "the build failed on line 40", "what is the db path?"}
	for _, body := range allow {
		if _, err := s.Send("a", "b", body); err != nil {
			t.Errorf("guard should allow %q: %v", body, err)
		}
	}
}

func mustReg(t *testing.T, s *Store, name string) {
	t.Helper()
	if err := s.Register(name, ""); err != nil {
		t.Fatal(err)
	}
}

func TestUnregisterAfterSending(t *testing.T) {
	s := testStore(t)
	mustReg(t, s, "sender")
	mustReg(t, s, "recipient")
	if _, err := s.Send("sender", "recipient", "a real message"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unregister("sender"); err != nil {
		t.Fatalf("unregister after sending: %v", err)
	}
	msgs, err := s.Inbox("recipient")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].From != "sender" {
		t.Fatalf("recipient lost mail from a departed sender: %+v", msgs)
	}
}

func TestUnregisterStillDropsOwnQueue(t *testing.T) {
	s := testStore(t)
	mustReg(t, s, "a")
	mustReg(t, s, "b")
	if _, err := s.Send("a", "b", "for b only"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unregister("b"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE to_agent='b'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unregister should still cascade the agent's own inbox, %d rows left", n)
	}
}

const legacySchema = `
CREATE TABLE agents (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);
CREATE TABLE messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    from_agent TEXT NOT NULL REFERENCES agents(name),
    to_agent   TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    acked_at   INTEGER
);
CREATE INDEX idx_inbox ON messages(to_agent, status, id);`

func TestMigratesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"old-sender", "old-recipient"} {
		if _, err := raw.Exec(`INSERT INTO agents(name, description, created_at, last_seen) VALUES(?,'',1,1)`, n); err != nil {
			t.Fatal(err)
		}
	}
	for i, body := range []string{"first", "second", "third"} {
		if _, err := raw.Exec(`INSERT INTO messages(id, from_agent, to_agent, body, status, created_at)
			VALUES(?,?,?,?,'pending',?)`, i+1, "old-sender", "old-recipient", body, i+1); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening a legacy db must migrate it: %v", err)
	}
	defer s.Close()

	msgs, err := s.Inbox("old-recipient")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("migration lost messages: got %d, want 3", len(msgs))
	}
	for i, want := range []string{"first", "second", "third"} {
		if msgs[i].Body != want || msgs[i].ID != int64(i+1) {
			t.Fatalf("migration changed FIFO order or ids: %+v", msgs)
		}
	}
	if err := s.Unregister("old-sender"); err != nil {
		t.Fatalf("legacy db still blocks unregister: %v", err)
	}
	mustReg(t, s, "new-agent")
	id, err := s.Send("new-agent", "old-recipient", "after the migration")
	if err != nil {
		t.Fatal(err)
	}
	if id <= 3 {
		t.Fatalf("autoincrement reused an id after the table swap: got %d", id)
	}

	if err := migrate(s.db); err != nil {
		t.Fatalf("migration must be idempotent: %v", err)
	}
	if again, _ := s.Inbox("old-recipient"); len(again) != 4 {
		t.Fatalf("re-running migrate changed the data: %d messages", len(again))
	}
}
