package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	desc := fmt.Sprintf("%s agent in %s", runtimeOf(sp), sp.Repo)
	if err := r.b.RegisterOn(sp.Agent, desc, r.node); err != nil {
		r.fail(sp, fmt.Errorf("register: %w", err))
		return
	}
	if _, err := os.Stat(sp.Repo); err != nil {
		r.fail(sp, fmt.Errorf("repo %q: %w", sp.Repo, err))
		return
	}
	_ = r.b.UpdateSpawn(sp.ID, "running", "", 0, "")
	r.report(sp, Activity{}) // a node that died mid-turn left its last step behind
	fmt.Printf("gary node: agent %q running in %s\n", sp.Agent, sp.Repo)

	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	var asked atomic.Bool
	go r.watchStop(waitCtx, sp, &asked, stopWaiting)

	// A requeued spawn carries the session it had, so the agent keeps its context.
	session := sp.SessionID
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
		if fresh, err := r.b.TakeReset(sp.ID); err != nil {
			fmt.Fprintf(os.Stderr, "gary node: %s: reset check: %v\n", sp.Agent, err)
		} else if fresh {
			session = ""
			fmt.Printf("gary node: %s: context reset, starting a fresh session\n", sp.Agent)
		}

		started := time.Now()
		live := Activity{TurnMsg: m.ID, TurnFrom: m.From, TurnStarted: started.Unix()}
		step := func(what string) {
			live.Step, live.StepAt = what, time.Now().Unix()
			r.report(sp, live)
		}
		step("starting " + runtimeOf(sp))
		out, newSession, err := r.turn(ctx, sp, session, m, step)
		if err != nil && ctx.Err() != nil {
			return
		}
		done := Activity{LastTurnAt: time.Now().Unix(), LastTurnSecs: int64(time.Since(started).Round(time.Second).Seconds()),
			LastResult: clip(out, maxResult)}
		if err != nil {
			done.LastResult, done.LastFailed = err.Error(), true
		}
		r.report(sp, done)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gary node: %s: %v\n", sp.Agent, err)
			r.answer(sp.Agent, m, fmt.Sprintf("agent %q failed to handle your message: %v", sp.Agent, err))
			continue
		}
		if newSession != "" && newSession != session {
			session = newSession
			_ = r.b.UpdateSpawn(sp.ID, "", session, 0, "")
		}
		r.answer(sp.Agent, m, out)
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

// report publishes what an agent is doing. It is best-effort: a hub that is
// briefly unreachable, or too old to know the endpoint, costs the dashboard a
// step, never the turn.
func (r *runner) report(sp *Spawn, a Activity) {
	if err := r.b.SetActivity(sp.ID, a); err != nil {
		fmt.Fprintf(os.Stderr, "gary node: %s: activity: %v\n", sp.Agent, err)
	}
}

// turn runs one claude invocation. Output is stream-json so the node sees each
// tool call as it starts and can hand it to step; that is the only way to tell
// a long turn that is working from one blocked on a single command.
func (r *runner) turn(ctx context.Context, sp *Spawn, session string, m *Message, step func(string)) (string, string, error) {
	if runtimeOf(sp) == "codex" {
		return r.codexTurn(ctx, sp, session, m, step)
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", sp.PermissionMode}
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
	cmd.Stdin = strings.NewReader(inbound(m))
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", fmt.Errorf("claude: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("claude: %w", err)
	}
	res, found := readStream(stdout, step)
	if err := cmd.Wait(); err != nil {
		return "", "", fmt.Errorf("claude: %w", err)
	}
	if !found {
		return "", "", fmt.Errorf("claude exited without a result")
	}
	if res.IsError {
		return "", res.SessionID, fmt.Errorf("claude reported an error (%s): %s", res.Subtype, res.Result)
	}
	return res.Result, res.SessionID, nil
}

func runtimeOf(sp *Spawn) string {
	if sp.Runtime == "" {
		return "claude"
	}
	return sp.Runtime
}

// codexTurn is turn for a codex agent: `codex exec --json`, resumed by thread id.
// codex has no appended system prompt, so a fresh session gets gary's framing as
// the head of its first message and keeps it in the thread from then on.
func (r *runner) codexTurn(ctx context.Context, sp *Spawn, session string, m *Message, step func(string)) (string, string, error) {
	args := []string{"exec"}
	if session != "" {
		args = append(args, "resume", session)
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if sp.PermissionMode == "bypassPermissions" {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else if session == "" {
		args = append(args, "--sandbox", "workspace-write") // resume takes no --sandbox; the thread keeps it
	}
	if sp.Model != "" {
		args = append(args, "--model", sp.Model)
	}
	args = append(args, "-")

	prompt := inbound(m)
	if session == "" {
		prompt = systemPrompt(sp) + "\n---\n\n" + prompt
	}
	if r.turnLimit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.turnLimit)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Dir = sp.Repo
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", fmt.Errorf("codex: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("codex: %w", err)
	}
	res := readCodexStream(stdout, step)
	if err := cmd.Wait(); err != nil {
		if res.err != "" {
			return "", res.thread, fmt.Errorf("codex: %s", res.err)
		}
		return "", res.thread, fmt.Errorf("codex: %w", err)
	}
	if res.err != "" {
		return "", res.thread, fmt.Errorf("codex reported an error: %s", res.err)
	}
	if !res.done {
		return "", res.thread, fmt.Errorf("codex exited without finishing the turn")
	}
	return res.last, res.thread, nil
}

type codexResult struct {
	thread, last, err string
	done              bool
}

// readCodexStream is readStream for codex's JSONL events. The turn's result is
// its last agent_message; earlier ones are narration and become steps.
func readCodexStream(rd io.Reader, step func(string)) codexResult {
	var res codexResult
	br := bufio.NewReader(rd)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var ev struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
				Message  string `json:"message"`
				Error    struct {
					Message string `json:"message"`
				} `json:"error"`
				Item struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Command string `json:"command"`
				} `json:"item"`
			}
			if json.Unmarshal(line, &ev) == nil {
				switch ev.Type {
				case "thread.started":
					res.thread = ev.ThreadID
				case "turn.completed":
					res.done = true
				case "turn.failed":
					res.err = ev.Error.Message
				case "error":
					res.err = ev.Message
				case "item.started":
					if ev.Item.Type == "command_execution" {
						step(clip("Bash: "+strings.Join(strings.Fields(ev.Item.Command), " "), maxStep))
					} else if ev.Item.Type != "" {
						step(ev.Item.Type)
					}
				case "item.completed":
					if ev.Item.Type == "agent_message" {
						res.last = ev.Item.Text
						step("writing")
					}
				}
			}
		}
		if err != nil {
			return res
		}
	}
}

type streamResult struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Subtype   string `json:"subtype"`
}

// readStream consumes claude's stream-json output to EOF — stopping early would
// leave claude blocked on a full pipe — turning tool calls into steps and
// returning the final result event. Lines it cannot parse are skipped.
func readStream(rd io.Reader, step func(string)) (streamResult, bool) {
	var res streamResult
	found := false
	br := bufio.NewReader(rd)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var ev struct {
				Type    string `json:"type"`
				Message struct {
					Content []struct {
						Type  string          `json:"type"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
					} `json:"content"`
				} `json:"message"`
				streamResult
			}
			if json.Unmarshal(line, &ev) == nil {
				switch ev.Type {
				case "result":
					res, found = ev.streamResult, true
				case "assistant":
					var tools []string
					wrote := false
					for _, c := range ev.Message.Content {
						switch c.Type {
						case "tool_use":
							tools = append(tools, describeTool(c.Name, c.Input))
						case "text":
							wrote = true
						}
					}
					switch {
					case len(tools) == 1:
						step(tools[0])
					case len(tools) > 1:
						step(fmt.Sprintf("%s (+%d more in parallel)", tools[0], len(tools)-1))
					case wrote:
						step("writing")
					}
				case "user":
					step("thinking")
				}
			}
		}
		if err != nil {
			return res, found
		}
	}
}

// describeTool renders a tool call as one line, naming the thing it acts on —
// for Bash the command itself, since that is what you need to see when it hangs.
func describeTool(name string, raw json.RawMessage) string {
	var in struct {
		Command     string `json:"command"`
		FilePath    string `json:"file_path"`
		Path        string `json:"path"`
		Pattern     string `json:"pattern"`
		URL         string `json:"url"`
		Query       string `json:"query"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &in)
	for _, v := range []string{in.Command, in.FilePath, in.Pattern, in.URL, in.Query, in.Description, in.Path} {
		if v = strings.Join(strings.Fields(v), " "); v != "" {
			return clip(name+": "+v, maxStep)
		}
	}
	return name
}

const (
	maxStep   = 240
	maxResult = 4000
)

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func systemPrompt(sp *Spawn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the gary agent %q, running on node %q.\n\n", sp.Agent, sp.Node)
	b.WriteString("Each message you receive is from another agent. Some ask for an answer back " +
		"and some do not; each one tells you which it is.\n\n")
	fmt.Fprintf(&b, "To reach another agent: `gary send <them> --from %s \"<request>\"`. "+
		"Add `--expect-reply` only when you genuinely cannot continue without their answer: it "+
		"turns their result into a new message to you and costs them a turn. `gary list` shows "+
		"who exists.\n\n", sp.Agent)
	fmt.Fprintf(&b, "To reach a whole channel: `gary post <channel> --from %s \"<update>\"`. "+
		"`gary channels` shows which exist and who is on each. A post never asks for a reply and "+
		"reaches every member, costing each of them a turn.\n\n", sp.Agent)
	b.WriteString("Message only when the recipient must act or must know something to do their job. " +
		"Never send thanks, acknowledgements, or confirmations — reading a message already acks it, " +
		"and gary rejects pleasantry-only messages.\n")
	if strings.TrimSpace(sp.Prompt) != "" {
		fmt.Fprintf(&b, "\n%s\n", sp.Prompt)
	}
	return b.String()
}

// inbound frames a message for the turn, including whether the agent's final
// response is going anywhere. Agents pad status updates when they assume someone
// is listening; telling them nobody is keeps the turn's output honest.
func inbound(m *Message) string {
	head := fmt.Sprintf("Message from agent %q", m.From)
	note := "No reply is expected: your final response will not be sent to anyone. Act on this " +
		"message, and message someone yourself only if they need something from you."
	if m.ExpectsReply {
		note = fmt.Sprintf("%s is waiting on your answer: your final response is sent back to them, "+
			"so answer with the result itself — not a status update.", m.From)
	}
	if m.Channel != "" {
		head = fmt.Sprintf("Message from agent %q on channel %q, which went to every member", m.From, m.Channel)
		note += fmt.Sprintf(" In particular, do not post to %q just because you received this: "+
			"a post costs every member a turn, so post only what all of them need.", m.Channel)
	}
	return fmt.Sprintf("%s:\n\n%s\n\n---\n%s", head, m.Body, note)
}

// answer routes a turn's result. It goes back to the sender only if that sender
// asked for it; otherwise the turn ends here and the result is printed for the
// operator. This is invariant 10, and it is what stops two agents volleying
// forever: an unrequested result never becomes another agent's inbound message.
func (r *runner) answer(agent string, m *Message, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	if !m.ExpectsReply {
		fmt.Printf("gary node: %s finished #%d from %s (no reply expected):\n%s\n", agent, m.ID, m.From, body)
		return
	}
	r.reply(agent, m.From, body)
}

func (r *runner) reply(from, to, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	// false is load-bearing: a reply never expects a reply, so a turn's output
	// can never cause another turn.
	_, err := r.b.Send(from, to, body, false)
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
