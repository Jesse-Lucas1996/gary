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

## Commands

```
gary register <name> [--description <text>]   add/update an agent
gary unregister <name>                        remove an agent and its queue
gary list                                     list registered agents
gary send <to> --from <me> [message]          enqueue a message (arg or stdin)
gary inbox <name>                             peek pending messages (no dequeue)
gary recv <name>                              dequeue oldest pending, auto-ack
gary watch <name> [--interval 1s]             block, printing messages as they arrive
```

Global flags: `--db <path>`, `--json` (machine output on every read command).

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

## Example

```sh
gary register researcher --description "digs through docs"
gary register planner    --description "makes the plan"

gary send planner --from researcher "please plan the auth refactor"
gary inbox planner --json
gary recv planner            # returns the message and acks it
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
