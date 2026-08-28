package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testToken = "s3cret"

func testHub(t *testing.T) (*Client, *Store) {
	t.Helper()
	s := testStore(t)
	srv := httptest.NewServer(hubHandler(s, serveOpts{token: testToken, api: true, dbPath: "test"}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, testToken), s
}

func TestClientFIFOAndAutoAck(t *testing.T) {
	c, _ := testHub(t)
	for _, n := range []string{"a", "b"} {
		if err := c.RegisterOn(n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{"first", "second", "third"} {
		if _, err := c.Send("a", "b", body, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"first", "second", "third"} {
		m, err := c.Recv("b")
		if err != nil || m == nil {
			t.Fatalf("recv: %v %v", m, err)
		}
		if m.Body != want {
			t.Fatalf("FIFO broken over HTTP: got %q want %q", m.Body, want)
		}
		if m.Status != "acked" || m.AckedAt == nil {
			t.Fatalf("recv did not auto-ack: %+v", m)
		}
	}
	m, err := c.Recv("b")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("empty queue should decode as nil, got %+v", m)
	}
}

func TestClientErrorsKeepTheirIdentity(t *testing.T) {
	c, _ := testHub(t)
	if err := c.RegisterOn("a", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send("a", "a", "thanks!", false); !errors.Is(err, ErrGuard) {
		t.Fatalf("pleasantry over HTTP: got %v, want ErrGuard", err)
	}
	if _, err := c.Send("a", "a", "", false); !errors.Is(err, ErrGuard) {
		t.Fatalf("empty body over HTTP: got %v, want ErrGuard", err)
	}
	if _, err := c.Send("a", "ghost", "real message", false); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("unregistered recipient over HTTP: got %v, want ErrUnregistered", err)
	}
	if _, err := c.Send("ghost", "a", "real message", false); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("unregistered sender over HTTP: got %v, want ErrUnregistered", err)
	}
}

func TestClientCarriesExpectsReply(t *testing.T) {
	c, _ := testHub(t)
	for _, n := range []string{"a", "b"} {
		if err := c.RegisterOn(n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Send("a", "b", "what broke the build?", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send("a", "b", "the build is fixed", false); err != nil {
		t.Fatal(err)
	}
	in, err := c.Inbox("b")
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 2 || !in[0].ExpectsReply || in[1].ExpectsReply {
		t.Fatalf("the reply flag did not survive the wire: %+v", in)
	}
	m, err := c.Recv("b")
	if err != nil {
		t.Fatal(err)
	}
	if !m.ExpectsReply {
		t.Fatalf("recv over HTTP lost the reply flag: %+v", m)
	}
}

func TestClientChannels(t *testing.T) {
	c, _ := testHub(t)
	for _, n := range []string{"planner", "coder", "reviewer"} {
		if err := c.RegisterOn(n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.CreateChannel("standup", "daily status"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"planner", "coder", "reviewer"} {
		if err := c.JoinChannel("standup", n); err != nil {
			t.Fatal(err)
		}
	}
	chans, err := c.Channels()
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 1 || len(chans[0].Members) != 3 {
		t.Fatalf("channel membership did not survive the wire: %+v", chans)
	}

	ids, err := c.Post("planner", "standup", "the parser lands today")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("post over HTTP reached %d agents, want 2", len(ids))
	}
	m, err := c.Recv("coder")
	if err != nil || m == nil {
		t.Fatalf("recv: %v %v", m, err)
	}
	if m.Channel != "standup" || m.ExpectsReply {
		t.Fatalf("post attribution or reply flag lost over the wire: %+v", m)
	}

	if _, err := c.Post("planner", "nope", "real content"); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("unknown channel must arrive as ErrUnregistered, got %v", err)
	}
	if _, err := c.Post("planner", "standup", "thanks!"); !errors.Is(err, ErrGuard) {
		t.Fatalf("the guard must arrive as ErrGuard over the wire, got %v", err)
	}

	if err := c.LeaveChannel("standup", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteChannel("standup"); err != nil {
		t.Fatal(err)
	}
	if chans, _ = c.Channels(); len(chans) != 0 {
		t.Fatalf("channel survived deletion over HTTP: %+v", chans)
	}
}

func TestAuth(t *testing.T) {
	s := testStore(t)
	srv := httptest.NewServer(hubHandler(s, serveOpts{token: testToken, api: true}))
	defer srv.Close()

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "guess"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(srv.URL, tc.token)
			if _, err := c.List(); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("got %v, want ErrUnauthorized", err)
			}
		})
	}
	if _, err := NewClient(srv.URL, testToken).List(); err != nil {
		t.Fatalf("correct token rejected: %v", err)
	}
}

func TestDashboardRequiresToken(t *testing.T) {
	s := testStore(t)
	srv := httptest.NewServer(hubHandler(s, serveOpts{token: testToken, api: true}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/state: got %d, want 401", resp.StatusCode)
	}
}

func TestRecvWaitBlocksUntilMessageArrives(t *testing.T) {
	c, s := testHub(t)
	for _, n := range []string{"a", "b"} {
		if err := c.RegisterOn(n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.Send("a", "b", "arrived late", false)
	}()
	start := time.Now()
	m, err := c.RecvWait(context.Background(), "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("long-poll returned empty despite a message arriving")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("long-poll took %v — it waited out the deadline instead of returning early", elapsed)
	}
}

func TestRecvWaitReturnsEmptyAtDeadline(t *testing.T) {
	c, _ := testHub(t)
	if err := c.RegisterOn("quiet", "", ""); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m, err := c.RecvWait(context.Background(), "quiet", 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("expected nil at deadline, got %+v", m)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("returned before the requested wait elapsed")
	}
}

func TestCheckBind(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		token    string
		insecure bool
		wantErr  bool
	}{
		{"loopback without token is fine", "127.0.0.1:4777", "", false, false},
		{"loopback with token is fine", "127.0.0.1:4777", "t", false, false},
		{"lan address with token", "192.168.1.10:4777", "t", false, false},
		{"tailscale address with token", "100.64.1.2:4777", "t", false, false},
		{"lan address without token", "192.168.1.10:4777", "", false, true},
		{"all interfaces refused", "0.0.0.0:4777", "t", false, true},
		{"empty host refused", ":4777", "t", false, true},
		{"public address refused", "8.8.8.8:4777", "t", false, true},
		{"public address allowed with --insecure", "8.8.8.8:4777", "t", true, false},
		{"malformed addr", "not-an-addr", "t", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBind(tc.addr, tc.token, tc.insecure)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkBind(%q, token=%q, insecure=%v) = %v, wantErr=%v",
					tc.addr, tc.token, tc.insecure, err, tc.wantErr)
			}
		})
	}
}
