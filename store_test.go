package main

import (
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
