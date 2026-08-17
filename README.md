# gary

A mailbox and FIFO message queue for AI agents working across different repos and
machines. One Go binary, one SQLite file.

Agents register into a shared registry, discover each other, and drop messages
into each other's queues. Reading a message consumes it (auto-ack), so there is
never a reason to reply "got it" — and `send` refuses acknowledgement-only
messages anyway.

On one machine it's a local file and nothing is running. Across machines, one
process (`gary serve`) owns the file and everyone else talks to it. `gary` can
also **start Claude Code agents** on any participating machine and feed them
their own mail.

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

Requires **Go 1.25+** (`go.mod`; the floor comes from the SQLite driver — this
code itself needs 1.24 for the stdlib `crypto/pbkdf2`). No cgo — the SQLite driver is pure Go, so `CGO_ENABLED=0`
builds work everywhere, and you can cross-compile a static binary for a machine
whose Go is too old to build it:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o gary .
```

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

gary serve [--addr 127.0.0.1:4777]            run the hub
gary token new                                generate the shared secret (agents/nodes)
gary user add|rm|list <name>                  dashboard login accounts
gary node [--name <machine>]                  run agents on this machine
gary nodes                                    list machines that have checked in

gary spawn <agent> --node <m> --repo <path>   start a claude agent on a node
gary spawns                                   list spawns and their status
gary stop <agent>                             wind an agent down after its turn

gary dashboard [--addr localhost:4777]        live HTML view (no API)
gary update                                   rebuild+install via `go install`
```

Global flags: `--db <path>`, `--url <hub>`, `--token <tok>`, `--json` (machine
output on every read command).

Used with no `--url`/`$GARY_URL`, gary behaves exactly like the single-machine
tool it started as.

## Going cross-machine

One machine runs the hub. It owns the SQLite file and is the only thing that
listens on a port; everyone else dials out, so laptops behind NAT need no port
forwarding.

**On the hub:**

```sh
gary token new                          # writes ~/.config/gary/token
gary serve --addr 100.x.x.x:4777        # a tailscale/LAN address, not 0.0.0.0
```

`serve` refuses to bind a public interface without `--insecure`, and refuses any
non-loopback bind without a token. The token can start processes on your
machines — treat it accordingly.

**On every other machine:**

```sh
export GARY_URL=http://100.x.x.x:4777
export GARY_TOKEN=...                   # or copy the token file to ~/.config/gary/token
gary list                               # you're on the shared registry
```

Every command works unchanged from here. `send`, `inbox`, `recv` and `watch` talk
to the hub instead of a local file, and the FIFO and atomic-dequeue guarantees
are identical — the hub is the single serializer either way. `watch` uses
long-polling, so it holds one request open rather than polling once a second.

### Behind an existing nginx + domain

If the hub machine already serves a domain, bind gary to loopback and let nginx
front it under a sub-path — no new port to open, no port forwarding, no extra
certificate, TLS for free:

```sh
gary serve --addr 127.0.0.1:4777
```

Add these two blocks inside the domain's existing `server { listen 443 ... }`
block and reload:

```nginx
location = /gary {
    return 301 /gary/;
}

location /gary/ {
    proxy_pass http://127.0.0.1:4777/;
    proxy_http_version 1.1;
    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    proxy_read_timeout 75s;
    proxy_send_timeout 75s;
    proxy_buffering off;
}
```

Then everywhere else:

```sh
export GARY_URL=https://example.com/gary
```

gary is mount-point agnostic: nginx strips the prefix, and the dashboard and
login redirects are all relative, so the same binary works at `/`, at `/gary/`,
or anywhere else.

Two things in that config are load-bearing. The `proxy_read_timeout` /
`proxy_buffering off` lines — gary holds long-poll requests for up to 30s, and
the nginx defaults break `watch` and `gary node`. And the `/gary` → `/gary/`
redirect, without which the dashboard's relative paths resolve one level too
high.

### Keeping it running

A systemd **user** unit needs no root. Write
`~/.config/systemd/user/gary-hub.service`:

```ini
[Unit]
Description=gary hub
After=network-online.target

[Service]
ExecStart=%h/.local/bin/gary serve --addr 127.0.0.1:4777
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now gary-hub
loginctl enable-linger "$USER"    # keep it running when you are not logged in
```

A node is the same with `ExecStart=%h/.local/bin/gary node --name %l`, plus
`Environment=GARY_URL=...`. Give it `TimeoutStopSec=1800` so an in-flight claude
turn can finish instead of being killed on shutdown. If `claude` isn't on the
unit's `PATH` (a version manager, say), add `--claude-bin` with a *stable* path —
a shim, never a versioned directory that breaks on the next upgrade.

State is the single SQLite file, so backup is a file copy.

## Running Claude agents

A machine that should *run* agents also runs a node:

```sh
gary node --name laptop     # long-polls the hub for work
```

That machine needs `claude` installed and authenticated (OAuth login, or
`ANTHROPIC_API_KEY` in the systemd unit's environment). Then, from anywhere:

```sh
gary spawn reviewer --node laptop --repo ~/work/api \
    --prompt "You review diffs. Be specific about line numbers."

gary send reviewer --from planner "review the diff on branch auth-refactor"
gary inbox planner          # the review comes back here
```

Each inbound message runs one `claude -p` turn in the agent's repo, and the
result is sent back to whoever asked. Between messages **nothing is running** —
context lives in the Claude session (`--session-id`, then `--resume`), not in an
idle process. So an interrupted turn leaves the message `pending` and
re-deliverable rather than lost.

`gary stop <agent>` is cooperative. A watcher on the node polls the spawn's
status and interrupts only the *idle wait*, so a stop lands within a couple of
seconds — but a `claude` already mid-turn is left alone to finish. Nothing is
ever killed part-way through a turn.

**Safety.** A node only ever executes the `claude` binary, with an argument list
gary builds itself — a spawn carries a repo, a prompt and a model, never a
command. Spawned agents default to `--permission-mode acceptEdits`; the
skip-permissions mode needs an explicit `--yolo`. They run as the node's user
with that user's filesystem access.

## How delivery actually works

An LLM is not a running process — it's a stateless request/response function. It
does something only when a *harness* (Claude Code, a script, an MCP client) hands
it a prompt. Between calls there is nothing there to receive a push.

So the queue itself is passive. Writing a message is just a row in SQLite; it
wakes nothing. A message reaches a model's context only when a harness reads it
and includes it in the next prompt.

`gary node` **is** that harness — for spawned agents, delivery really is
end-to-end: a message lands, a node picks it up, Claude runs. For Claude Code
sessions you started yourself, you still need one of the two hookups below.

## Auto-pickup for sessions you run yourself

**A long-running loop** (`watch`): blocks, prints each message the moment it lands
(auto-acking). Pipe it into whatever drives your agent:

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

The hub serves a live view at its own address — including under a sub-path, so
`https://example.com/gary/` works as-is. `gary dashboard` runs the same page
without the API, straight off a local file.

It shows machines and their heartbeats, the long-running agents on each with
their queue depth and a stop button, the registry, and messages landing (pending)
and being consumed (acked) in real time.

```sh
gary dashboard --db mydb.db --addr localhost:4777
# open http://localhost:4777
```

### Signing in

Agents and nodes authenticate with the shared token. People get a login instead:

```sh
gary user add admin       # prompts for a password, twice
```

The password is stored as a PBKDF2-SHA256 hash (600k iterations, per-user salt)
in `~/.config/gary/users`, mode 0600 — never in plaintext, and never in this
repository. Visiting the dashboard then redirects to a sign-in page and sets a
signed session cookie that lasts 30 days; "sign out" is in the top right.

Changing a password or rotating the token invalidates every existing session.
Failed logins are rate-limited per IP.

If no users are configured, the dashboard stays token-only — useful on loopback,
not much use in a browser.

## Storage

Single SQLite file (WAL mode). Location: `--db` → `$GARY_DB` →
`$XDG_DATA_HOME/gary/gary.db` → `~/.local/share/gary/gary.db`. On a hub, that one
file is the whole system's state.

Connection: `--url` → `$GARY_URL` → local file. Token: `--token` → `$GARY_TOKEN`
→ `~/.config/gary/token`. Dashboard logins: `~/.config/gary/users` (hashes only,
mode 0600).

Both config files are mode 0600 and gary warns if they are looser.

## Agent etiquette (important)

`gary` exists partly to stop agents flooding each other with pleasantries. The
rule agents must follow:

> Send a message **only when the recipient must act or must know something to do
> their job.** A message is a request, an answer, or required information. Never
> send thanks, acknowledgements, "sounds good", or "will do". If nothing is
> needed, send nothing.

`send` enforces the floor: empty and acknowledgement-only messages are rejected.
Spawned agents get this rule in their system prompt, and if one replies with
nothing but a pleasantry the node drops it silently.

See `CLAUDE.md` for the full spec, invariants, and data model.
