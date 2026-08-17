package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func stubClaude(t *testing.T, result string) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	record = filepath.Join(dir, "record")
	script := fmt.Sprintf(`#!/bin/sh
{
  echo "CWD=$PWD"
  for a in "$@"; do echo "ARG=$a"; done
  echo "STDIN=$(cat)"
} >> %q
printf '{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":"sess-abc"}\n'
`, record, result)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func TestTurnBuildsClaudeInvocation(t *testing.T) {
	s := testStore(t)
	bin, record := stubClaude(t, "did the thing")
	repo := t.TempDir()
	r := &runner{b: s, node: "n1", claudeBin: bin, running: map[string]bool{}}
	sp := &Spawn{ID: 1, Agent: "coder", Node: "n1", Repo: repo, PermissionMode: "acceptEdits", Model: "sonnet"}

	out, session, err := r.turn(context.Background(), sp, "", &Message{From: "planner", Body: "make a file"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "did the thing" {
		t.Fatalf("result: got %q", out)
	}
	if session != "sess-abc" {
		t.Fatalf("session: got %q, want the id claude reported", session)
	}

	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"CWD=" + repo,
		"ARG=-p",
		"ARG=--output-format", "ARG=json",
		"ARG=--permission-mode", "ARG=acceptEdits",
		"ARG=--session-id",
		"ARG=--model", "ARG=sonnet",
		"STDIN=", "make a file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("invocation missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ARG=--resume") {
		t.Error("first turn should pin --session-id, not --resume")
	}
}

func TestTurnResumesExistingSession(t *testing.T) {
	s := testStore(t)
	bin, record := stubClaude(t, "ok")
	r := &runner{b: s, node: "n1", claudeBin: bin, running: map[string]bool{}}
	sp := &Spawn{ID: 1, Agent: "coder", Node: "n1", Repo: t.TempDir(), PermissionMode: "acceptEdits"}

	if _, _, err := r.turn(context.Background(), sp, "sess-existing", &Message{From: "p", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(record)
	got := string(b)
	if !strings.Contains(got, "ARG=--resume") || !strings.Contains(got, "ARG=sess-existing") {
		t.Errorf("later turns must resume the session\ngot:\n%s", got)
	}
	if strings.Contains(got, "ARG=--session-id") {
		t.Error("--session-id and --resume must not both be passed")
	}
}

func TestReplySuppressesPleasantry(t *testing.T) {
	s := testStore(t)
	mustReg(t, s, "coder")
	mustReg(t, s, "planner")
	r := &runner{b: s, node: "n1", running: map[string]bool{}}

	r.reply("coder", "planner", "Thanks!")
	msgs, err := s.Inbox("planner")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("pleasantry reply should be dropped, got %+v", msgs)
	}

	r.reply("coder", "planner", "the build fails at line 40")
	msgs, _ = s.Inbox("planner")
	if len(msgs) != 1 {
		t.Fatalf("real reply should be delivered, got %d messages", len(msgs))
	}
}

func TestNodeRunsSpawnEndToEnd(t *testing.T) {
	s := testStore(t)
	bin, _ := stubClaude(t, "created hello.txt")
	repo := t.TempDir()
	mustReg(t, s, "planner")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runNode(ctx, s, "n1", bin, time.Minute) }()

	eventually(t, "node heartbeat", func() bool {
		n, _ := s.Nodes()
		return len(n) == 1
	})

	if _, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "n1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent registered by node", func() bool {
		agents, _ := s.List()
		for _, a := range agents {
			if a.Name == "coder" && a.Node == "n1" {
				return true
			}
		}
		return false
	})

	if _, err := s.Send("planner", "coder", "create hello.txt"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reply delivered to sender", func() bool {
		msgs, _ := s.Inbox("planner")
		return len(msgs) == 1 && msgs[0].Body == "created hello.txt"
	})

	if err := s.StopSpawn("coder"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "spawn marked stopped", func() bool {
		st, _ := s.SpawnStatus(1)
		return st == "stopped"
	})

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runNode did not exit after cancellation")
	}
}

func TestSpawnRejectsUnknownNode(t *testing.T) {
	s := testStore(t)
	if _, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "nope", Repo: "/tmp"}); err == nil {
		t.Fatal("spawning onto a node that never checked in should fail")
	}
	if _, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "nope", Repo: "/tmp", Force: true}); err != nil {
		t.Fatalf("--force should bypass the node check: %v", err)
	}
}

func TestSpawnRejectsAgentOnAnotherNode(t *testing.T) {
	s := testStore(t)
	for _, n := range []string{"n1", "n2"} {
		if err := s.Heartbeat(n, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RegisterOn("coder", "", "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "n2", Repo: "/tmp"}); err == nil {
		t.Fatal("the registry is flat, so one name must not run on two nodes")
	}
	if _, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "n1", Repo: "/tmp"}); err != nil {
		t.Fatalf("re-spawning on the same node should be allowed: %v", err)
	}
}

func TestConcurrentClaimSpawnHandsOutOnce(t *testing.T) {
	s := testStore(t)
	if err := s.Heartbeat("n1", "test"); err != nil {
		t.Fatal(err)
	}
	const n = 30
	for i := 0; i < n; i++ {
		if _, err := s.Spawn(SpawnSpec{Agent: fmt.Sprintf("a%d", i), Node: "n1", Repo: "/tmp", Force: true}); err != nil {
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
				sp, err := s.ClaimSpawn("n1")
				if err != nil {
					t.Error(err)
					return
				}
				if sp == nil {
					return
				}
				mu.Lock()
				if seen[sp.ID] {
					t.Errorf("spawn #%d claimed twice", sp.ID)
				}
				seen[sp.ID] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("claimed %d spawns, want %d", len(seen), n)
	}
}

func TestRegisterPreservesNode(t *testing.T) {
	s := testStore(t)
	if err := s.RegisterOn("coder", "spawned", "n1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Register("coder", "re-registered by hand"); err != nil {
		t.Fatal(err)
	}
	agents, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Node != "n1" {
		t.Fatalf("plain register wiped the node: %+v", agents[0])
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
