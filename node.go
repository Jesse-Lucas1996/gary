package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type runner struct {
	b         Backend
	node      string
	claudeBin string
	turnLimit time.Duration

	mu      sync.Mutex
	running map[string]bool
}

const (
	heartbeatEvery = 30 * time.Second
	claimWait      = 20 * time.Second
	stopPoll       = 2 * time.Second
	recvWait       = 20 * time.Second
)

func runNode(ctx context.Context, b Backend, node, claudeBin string, turnLimit time.Duration) error {
	if node == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("no --name and cannot read hostname: %w", err)
		}
		node = h
	}
	if _, err := exec.LookPath(claudeBin); err != nil {
		return fmt.Errorf("%q not found on PATH — a node needs Claude Code installed and "+
			"authenticated (or pass --claude-bin): %w", claudeBin, err)
	}
	r := &runner{b: b, node: node, claudeBin: claudeBin, turnLimit: turnLimit, running: map[string]bool{}}

	if err := b.Heartbeat(node, version); err != nil {
		return fmt.Errorf("cannot reach hub: %w", err)
	}
	fmt.Printf("gary node %q up (claude: %s)\n", node, r.claudeBin)

	var wg sync.WaitGroup
	go r.heartbeatLoop(ctx)

	for ctx.Err() == nil {
		sp, err := r.b.ClaimSpawnWait(ctx, r.node, claimWait)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(os.Stderr, "gary node: claim: %v\n", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if sp == nil {
			continue
		}
		if !r.claim(sp.Agent) {
			_ = r.b.UpdateSpawn(sp.ID, "failed", "", 0,
				fmt.Sprintf("agent %q is already running on this node — `gary stop %s` first", sp.Agent, sp.Agent))
			continue
		}
		wg.Add(1)
		go func(sp *Spawn) {
			defer wg.Done()
			defer r.release(sp.Agent)
			r.supervise(ctx, sp)
		}(sp)
	}

	fmt.Println("\ngary node: shutting down, waiting for in-flight turns...")
	wg.Wait()
	return nil
}

func (r *runner) claim(agent string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[agent] {
		return false
	}
	r.running[agent] = true
	return true
}

func (r *runner) release(agent string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, agent)
}

func (r *runner) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.b.Heartbeat(r.node, version); err != nil {
				fmt.Fprintf(os.Stderr, "gary node: heartbeat: %v\n", err)
			}
		}
	}
}

func (r *runner) supervise(ctx context.Context, sp *Spawn) {
	desc := fmt.Sprintf("claude agent in %s", sp.Repo)
	if err := r.b.RegisterOn(sp.Agent, desc, r.node); err != nil {
		r.fail(sp, fmt.Errorf("register: %w", err))
		return
	}
	if _, err := os.Stat(sp.Repo); err != nil {
		r.fail(sp, fmt.Errorf("repo %q: %w", sp.Repo, err))
		return
	}
	_ = r.b.UpdateSpawn(sp.ID, "running", "", 0, "")
	fmt.Printf("gary node: agent %q running in %s\n", sp.Agent, sp.Repo)

	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	var asked atomic.Bool
	go r.watchStop(waitCtx, sp, &asked, stopWaiting)

	session := ""
	for ctx.Err() == nil && !asked.Load() {
		m, err := r.b.RecvWait(waitCtx, sp.Agent, recvWait)
		if err != nil {
			if ctx.Err() != nil || asked.Load() {
				break
			}
			fmt.Fprintf(os.Stderr, "gary node: %s: recv: %v\n", sp.Agent, err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if m == nil {
			continue
		}
		fmt.Printf("gary node: %s <- %s (#%d)\n", sp.Agent, m.From, m.ID)

		out, newSession, err := r.turn(ctx, sp, session, m)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "gary node: %s: %v\n", sp.Agent, err)
			r.reply(sp.Agent, m.From, fmt.Sprintf("agent %q failed to handle your message: %v", sp.Agent, err))
			continue
		}
		if newSession != "" && newSession != session {
			session = newSession
			_ = r.b.UpdateSpawn(sp.ID, "", session, 0, "")
		}
		r.reply(sp.Agent, m.From, out)
	}

	if asked.Load() {
		_ = r.b.UpdateSpawn(sp.ID, "stopped", "", 0, "")
		fmt.Printf("gary node: agent %q stopped\n", sp.Agent)
	}
}

func (r *runner) watchStop(ctx context.Context, sp *Spawn, asked *atomic.Bool, cancel context.CancelFunc) {
	t := time.NewTicker(stopPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := r.b.SpawnStatus(sp.ID)
			if err == nil && (st == "stopping" || st == "stopped") {
				asked.Store(true)
				cancel()
				return
			}
		}
	}
}

func (r *runner) turn(ctx context.Context, sp *Spawn, session string, m *Message) (string, string, error) {
	args := []string{"-p", "--output-format", "json", "--permission-mode", sp.PermissionMode}
	if session == "" {
		args = append(args, "--session-id", newSessionID())
	} else {
		args = append(args, "--resume", session)
	}
	if sp.Model != "" {
		args = append(args, "--model", sp.Model)
	}
	args = append(args, "--append-system-prompt", systemPrompt(sp))

	if r.turnLimit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.turnLimit)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, r.claudeBin, args...)
	cmd.Dir = sp.Repo
	cmd.Stdin = strings.NewReader(fmt.Sprintf("Message from agent %q:\n\n%s", m.From, m.Body))
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("claude: %w", err)
	}

	var res struct {
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Subtype   string `json:"subtype"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", fmt.Errorf("parse claude output: %w", err)
	}
	if res.IsError {
		return "", res.SessionID, fmt.Errorf("claude reported an error (%s): %s", res.Subtype, res.Result)
	}
	return res.Result, res.SessionID, nil
}

func systemPrompt(sp *Spawn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the gary agent %q, running on node %q.\n\n", sp.Agent, sp.Node)
	b.WriteString("Each message you receive is from another agent. Your final response is " +
		"sent back to whoever messaged you, so answer with the result itself — not a status update.\n\n")
	fmt.Fprintf(&b, "To reach another agent: `gary send <them> --from %s \"<request>\"`. "+
		"`gary list` shows who exists.\n\n", sp.Agent)
	b.WriteString("Message only when the recipient must act or must know something to do their job. " +
		"Never send thanks, acknowledgements, or confirmations — reading a message already acks it, " +
		"and gary rejects pleasantry-only messages.\n")
	if strings.TrimSpace(sp.Prompt) != "" {
		fmt.Fprintf(&b, "\n%s\n", sp.Prompt)
	}
	return b.String()
}

func (r *runner) reply(from, to, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	_, err := r.b.Send(from, to, body)
	switch {
	case err == nil:
	case errors.Is(err, ErrGuard):
		fmt.Printf("gary node: %s -> %s suppressed (pleasantry-only)\n", from, to)
	default:
		fmt.Fprintf(os.Stderr, "gary node: reply %s -> %s: %v\n", from, to, err)
	}
}

func (r *runner) fail(sp *Spawn, err error) {
	fmt.Fprintf(os.Stderr, "gary node: spawn %d (%s): %v\n", sp.ID, sp.Agent, err)
	_ = r.b.UpdateSpawn(sp.ID, "failed", "", 0, err.Error())
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
