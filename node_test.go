package main

import (
	"context"
	"errors"
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

	out, session, err := r.turn(context.Background(), sp, "", &Message{From: "planner", Body: "make a file"}, func(string) {})
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
		"ARG=--output-format", "ARG=stream-json", "ARG=--verbose",
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

	if _, _, err := r.turn(context.Background(), sp, "sess-existing", &Message{From: "p", Body: "hi"}, func(string) {}); err != nil {
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

	if _, err := s.Send("planner", "coder", "create hello.txt", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reply delivered to sender", func() bool {
		msgs, _ := s.Inbox("planner")
		return len(msgs) == 1 && msgs[0].Body == "created hello.txt"
	})
	if msgs, _ := s.Inbox("planner"); msgs[0].ExpectsReply {
		t.Fatal("a reply must not itself expect a reply, or two agents volley forever")
	}
	eventually(t, "finished turn recorded as activity", func() bool {
		sp, _ := s.Spawns(1)
		return len(sp) == 1 && sp[0].LastTurnAt != 0
	})
	sp, _ := s.Spawns(1)
	if a := sp[0].Activity; a.TurnMsg != 0 || a.Step != "" || a.LastResult != "created hello.txt" || a.LastFailed {
		t.Fatalf("after a turn the agent should read idle with the result kept, got %+v", a)
	}

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

func TestInboundStatesWhetherAReplyIsExpected(t *testing.T) {
	awaited := inbound(&Message{From: "planner", Body: "make a file", ExpectsReply: true})
	if !strings.Contains(awaited, "waiting on your answer") {
		t.Errorf("an awaited message must say so:\n%s", awaited)
	}
	forget := inbound(&Message{From: "planner", Body: "make a file"})
	if !strings.Contains(forget, "No reply is expected") {
		t.Errorf("a fire-and-forget message must say so:\n%s", forget)
	}
	post := inbound(&Message{From: "planner", Body: "make a file", Channel: "standup"})
	if !strings.Contains(post, "standup") || !strings.Contains(post, "No reply is expected") {
		t.Errorf("a channel post must name its channel and expect no reply:\n%s", post)
	}

	for _, framed := range []string{awaited, forget, post} {
		if !strings.Contains(framed, "make a file") || !strings.Contains(framed, "planner") {
			t.Errorf("framing dropped the message or its sender:\n%s", framed)
		}
	}
}

// The loop breaker: a turn nobody asked for an answer from must not emit a
// message, or two agents keep triggering each other's turns forever.
func TestNodeDropsResultWhenNoReplyExpected(t *testing.T) {
	s := testStore(t)
	bin, record := stubClaude(t, "created hello.txt")
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

	if _, err := s.Send("planner", "coder", "create hello.txt", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn to run", func() bool {
		b, _ := os.ReadFile(record)
		return strings.Contains(string(b), "create hello.txt")
	})
	// The turn ran and returned a non-empty result; the reply, had there been
	// one, would already be enqueued by the time the next turn could start.
	time.Sleep(time.Second)
	if msgs, _ := s.Inbox("planner"); len(msgs) != 0 {
		t.Fatalf("an unrequested result must not become a message: %+v", msgs)
	}

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

func TestReadStreamReportsToolCallsAsSteps(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"let me look"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"while pgrep -f x;\n do sleep 5; done","description":"wait"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`not json at all`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a"}},{"type":"tool_use","name":"Grep","input":{"pattern":"b"}}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`,
	}, "\n")
	var steps []string
	res, found := readStream(strings.NewReader(stream), func(s string) { steps = append(steps, s) })
	if !found || res.Result != "done" || res.SessionID != "s1" {
		t.Fatalf("result: found=%v %+v", found, res)
	}
	want := []string{"writing", "Bash: while pgrep -f x; do sleep 5; done", "thinking", "Read: /a (+1 more in parallel)"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Fatalf("steps:\n got %q\nwant %q", steps, want)
	}
}

func TestReadStreamWithoutResult(t *testing.T) {
	if _, found := readStream(strings.NewReader(`{"type":"system"}`), func(string) {}); found {
		t.Fatal("a stream with no result event must not count as a result")
	}
}

func TestReadCodexStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll run that."}}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'ls'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'ls'","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"pong"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1}}`,
	}, "\n")
	var steps []string
	res := readCodexStream(strings.NewReader(stream), func(s string) { steps = append(steps, s) })
	if !res.done || res.err != "" || res.last != "pong" || res.thread != "t1" {
		t.Fatalf("result: %+v", res)
	}
	want := []string{"writing", "Bash: /bin/bash -lc 'ls'", "writing"}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Fatalf("steps:\n got %q\nwant %q", steps, want)
	}

	failed := readCodexStream(strings.NewReader(`{"type":"thread.started","thread_id":"t2"}
{"type":"turn.failed","error":{"message":"429 Too Many Requests"}}`), func(string) {})
	if failed.done || failed.err != "429 Too Many Requests" || failed.thread != "t2" {
		t.Fatalf("failed turn: %+v", failed)
	}
}

func TestSpawnRuntime(t *testing.T) {
	s := testStore(t)
	if err := s.Heartbeat("n1", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Spawn(SpawnSpec{Agent: "a", Node: "n1", Repo: "/tmp", Runtime: "gemini"}); err == nil {
		t.Fatal("an unknown runtime must be refused")
	}
	if _, err := s.Spawn(SpawnSpec{Agent: "a", Node: "n1", Repo: "/tmp", Runtime: "codex"}); err != nil {
		t.Fatal(err)
	}
	sp, err := s.ClaimSpawn("n1")
	if err != nil || sp == nil || sp.Runtime != "codex" {
		t.Fatalf("claimed %+v, %v", sp, err)
	}
}

func TestSetActivityKeepsLastTurnWhenGoingIdle(t *testing.T) {
	s := testStore(t)
	if err := s.Heartbeat("n1", "test"); err != nil {
		t.Fatal(err)
	}
	id, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "n1", Repo: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	steps := []Activity{
		{TurnMsg: 7, TurnFrom: "planner", TurnStarted: 100, Step: "Bash: ls", StepAt: 101},
		{LastTurnAt: 130, LastTurnSecs: 30, LastResult: "boom", LastFailed: true},
		{},
	}
	for _, a := range steps {
		if err := s.SetActivity(id, a); err != nil {
			t.Fatal(err)
		}
	}
	sp, _ := s.Spawns(1)
	got := sp[0].Activity
	want := Activity{LastTurnAt: 130, LastTurnSecs: 30, LastResult: "boom", LastFailed: true}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestReclaimedSpawnKeepsItsSession(t *testing.T) {
	s := testStore(t)
	if err := s.Heartbeat("n1", "test"); err != nil {
		t.Fatal(err)
	}
	id, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "n1", Repo: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSpawn(id, "queued", "sess-kept", 0, ""); err != nil {
		t.Fatal(err)
	}
	sp, err := s.ClaimSpawn("n1")
	if err != nil || sp == nil {
		t.Fatalf("claim: %v %v", sp, err)
	}
	if sp.SessionID != "sess-kept" {
		t.Fatalf("claimed spawn lost its session: %q", sp.SessionID)
	}
}

func TestClearChannelWipesPostsAndResetsMembers(t *testing.T) {
	s := testStore(t)
	for _, n := range []string{"lead", "coder", "other"} {
		mustReg(t, s, n)
	}
	if err := s.Heartbeat("n1", "test"); err != nil {
		t.Fatal(err)
	}
	coder, _ := s.Spawn(SpawnSpec{Agent: "coder", Node: "n1", Repo: "/tmp"})
	other, _ := s.Spawn(SpawnSpec{Agent: "other", Node: "n1", Repo: "/tmp"})
	for _, id := range []int64{coder, other} {
		if err := s.UpdateSpawn(id, "running", "sess-old", 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateChannel("room", ""); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"lead", "coder"} {
		if err := s.JoinChannel("room", a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Post("lead", "room", "build the parser"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send("lead", "other", "a direct message", false); err != nil {
		t.Fatal(err)
	}

	n, err := s.ClearChannel("room")
	if err != nil || n != 1 {
		t.Fatalf("clear: deleted %d, err %v", n, err)
	}
	if msgs, _ := s.Inbox("coder"); len(msgs) != 0 {
		t.Fatalf("unread post survived the clear: %+v", msgs)
	}
	if msgs, _ := s.Inbox("other"); len(msgs) != 1 {
		t.Fatal("clearing a channel must not touch direct messages")
	}
	if _, err := s.ClearChannel("nope"); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("clearing an unknown channel: %v", err)
	}

	for id, want := range map[int64]bool{coder: true, other: false} {
		got, err := s.TakeReset(id)
		if err != nil || got != want {
			t.Fatalf("spawn %d reset: got %v (%v), want %v", id, got, err, want)
		}
	}
	if again, _ := s.TakeReset(coder); again {
		t.Fatal("a reset must be consumed once")
	}
	sp, _ := s.Spawns(10)
	for _, x := range sp {
		if x.ID == coder && x.SessionID != "" {
			t.Fatalf("taking the reset must drop the session, still %q", x.SessionID)
		}
	}
}

func TestResetSessionNeedsALiveAgent(t *testing.T) {
	s := testStore(t)
	if err := s.ResetSession("ghost"); err == nil {
		t.Fatal("resetting an agent with no running spawn should fail")
	}
}

func TestNodeStartsFreshSessionAfterReset(t *testing.T) {
	s := testStore(t)
	bin, record := stubClaude(t, "done")
	mustReg(t, s, "planner")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runNode(ctx, s, "n1", bin, time.Minute)
	eventually(t, "node heartbeat", func() bool { n, _ := s.Nodes(); return len(n) == 1 })
	if _, err := s.Spawn(SpawnSpec{Agent: "coder", Node: "n1", Repo: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "agent registered", func() bool { a, _ := s.List(); return len(a) == 2 })

	turns := func() int { b, _ := os.ReadFile(record); return strings.Count(string(b), "CWD=") }
	send := func(body string, want int) {
		t.Helper()
		if _, err := s.Send("planner", "coder", body, false); err != nil {
			t.Fatal(err)
		}
		eventually(t, "turn "+body, func() bool { return turns() == want })
	}
	send("one", 1)
	send("two", 2)
	if err := s.ResetSession("coder"); err != nil {
		t.Fatal(err)
	}
	send("three", 3)

	b, _ := os.ReadFile(record)
	calls := strings.Split(string(b), "CWD=")[1:]
	for i, want := range []string{"ARG=--session-id", "ARG=--resume", "ARG=--session-id"} {
		if !strings.Contains(calls[i], want) {
			t.Errorf("turn %d should use %s:\n%s", i+1, want, calls[i])
		}
	}
}
