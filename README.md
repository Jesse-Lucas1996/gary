# gary

A local mailbox and FIFO message queue for AI agents working across different
repos on the **same machine**. One Go binary, one SQLite file, no daemon, no
network.

Agents register into a shared registry, discover each other, and drop messages
into each other's queues. Reading a message consumes it (auto-ack), so there is
never a reason to reply "got it" — and `send` refuses acknowledgement-only
messages anyway.

## Install

**From source (clone):**

```sh
git clone https://github.com/Jesse-Lucas1996/gary.git
cd gary
go build -o gary .
install gary ~/.local/bin/gary   # put it on your PATH
```

**With `go install` (no clone):**

```sh
go install github.com/Jesse-Lucas1996/gary@latest   # lands in $(go env GOPATH)/bin
```

Requires Go 1.24+. No cgo — the SQLite driver is pure Go, so `CGO_ENABLED=0`
builds work everywhere.

**Updating:** `gary update` runs `go install github.com/Jesse-Lucas1996/gary@latest`
for you. If you originally used the clone+`install` method rather than
`go install`, re-copy the rebuilt binary from `$(go env GOPATH)/bin` afterward.

## Commands

```
gary register <name> [--description <text>]   add/update an agent
gary unregister <name>                        remove an agent and its queue
gary list                                     list registered agents
gary send <to> --from <me> [message]          enqueue a message (arg or stdin)
gary inbox <name>                             peek pending messages (no dequeue)
gary recv <name>                              dequeue oldest pending, auto-ack
gary watch <name> [--interval 1s]             block, printing messages as they arrive
gary dashboard [--addr localhost:4777]        serve a live HTML view of agents/messages
gary update                                   rebuild+install the latest gary via `go install`
```

Global flags: `--db <path>`, `--json` (machine output on every read command).

## How delivery actually works

An LLM is not a running process — it's a stateless request/response function. It
does something only when a *harness* (Claude Code, a script, an MCP client) hands
it a prompt. Between calls there is nothing there to receive a push.

So `gary` is passive. Writing a message is just a row in a SQLite file; it wakes
nothing. Even `watch` doesn't get pushed to — it *polls* ("anything new?"). A
message reaches a model's context only when the harness reads it (via `recv`) and
includes it in the next prompt. Every "auto-pickup" below is really *the harness
picking up*, arranged so you don't have to.

The closest thing to a genuine push is exposing `gary` as a **tool**: then the
model itself can call `gary recv` mid-turn and pull its own mail (see below).

## Auto-pickup

`gary` can't push into an agent's context on its own — something has to feed it.
Two ways, depending on how your agent runs:

**A long-running loop** (`watch`): blocks, polls, and prints each message the
moment it lands (auto-acking). Pipe it into whatever drives your agent:

```sh
gary watch planner --json | your-agent-runner
```

**A Claude Code hook** (true hands-off pickup): add a `Stop` hook in the
consuming repo's `.claude/settings.json` so that whenever the agent goes idle it
pulls its next message and keeps working:

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command", "command": "gary recv planner" } ] }
    ]
  }
}
```

The hook's stdout is fed back to the agent, so an incoming message becomes its
next task. Swap `planner` for that repo's agent name.

## Use gary as an agent tool

For any shell-capable agent (like Claude Code) **the CLI is already the tool** —
no MCP server, no wrapper. Just let the agent run `gary`. Tell it, in its system
prompt or `CLAUDE.md`:

> You are the agent `planner`. To reach another agent: `gary send <them> --from
> planner "<request>"`. To pull your own mail: `gary recv planner`. Check it when
> you finish a task or are waiting on someone. Only message when action or info
> is genuinely needed.

To skip approval prompts for these, allowlist them in `.claude/settings.json`:

```json
{ "permissions": { "allow": ["Bash(gary send:*)", "Bash(gary recv:*)", "Bash(gary inbox:*)", "Bash(gary list)"] } }
```

Because the model decides when to run `gary recv`, it pulls its own mail — the
nearest thing to real push, without any external loop.

**MCP:** only worth it for clients that *can't* run a shell (structured tool
schemas instead of bash). It's a separate process wrapping these same commands —
not built yet; open an issue / ask if you need it.

## Example

```sh
gary register researcher --description "digs through docs"
gary register planner    --description "makes the plan"

gary send planner --from researcher "please plan the auth refactor"
gary inbox planner --json
gary recv planner            # returns the message and acks it
```

## Dashboard

`gary dashboard` starts a local HTTP server and serves a page that polls
`/api/state` once a second, so you can watch messages land (pending) and get
consumed (acked) in real time — no daemon left running once you close it.

```sh
gary dashboard --db mydb.db --addr localhost:4777
# open http://localhost:4777
```

## Storage

Single SQLite file (WAL mode), shared by every agent process regardless of repo.
Location: `--db` → `$GARY_DB` → `$XDG_DATA_HOME/gary/gary.db` →
`~/.local/share/gary/gary.db`.

## Agent etiquette (important)

`gary` exists partly to stop agents flooding each other with pleasantries. The
rule agents must follow:

> Send a message **only when the recipient must act or must know something to do
> their job.** A message is a request, an answer, or required information. Never
> send thanks, acknowledgements, "sounds good", or "will do". If nothing is
> needed, send nothing.

`send` enforces the floor: empty and acknowledgement-only messages are rejected.

See `CLAUDE.md` for the full spec, invariants, and data model.
