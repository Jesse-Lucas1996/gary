# gary

`gary` is a **mailbox and message queue for AI agents working across different
repos and machines**. Agents register themselves into a shared registry, then
send FIFO messages to each other's queues. It can also **start Claude Code
agents** on any participating machine and feed them their own mail.

It is a single Go binary backed by one SQLite file. On one machine that is the
whole system — no server, no broker, no daemon. Across machines, exactly one
process (`gary serve`) owns that file and the others talk to it over HTTP.

## Mental model

- **Agent** — a named participant (`researcher`, `planner`, `coder`). Registering
  puts it in the registry so others can discover and address it. Names are flat
  and global: one name, one agent, wherever it runs.
- **Mailbox** — every agent has one inbox queue. Messages are delivered FIFO.
- **Message** — one envelope: `from`, `to`, `body`, timestamps, and a lifecycle
  status (`pending → acked`).
- **Hub** — the process running `gary serve`. It owns the SQLite file and is the
  single serializer for every operation.
- **Node** — a machine running `gary node`. It claims spawn requests aimed at it
  and supervises the `claude` processes for the agents it hosts.
- **Spawn** — a request to start a Claude agent on a node: an agent name, a repo,
  a prompt, a model. Never a command (see invariant 9).

The system is still **one SQLite file**. In local mode every `gary` invocation
opens it directly, does its work in a transaction, and exits; concurrency between
agent processes is handled by SQLite in WAL mode. In hub mode the same code runs
inside `gary serve` and everyone else reaches it over HTTP. **Remote mode changes
the transport to the database, never the semantics.**

## Topology

```
  machine A ─┐                        ┌─ gary node --name A ─▶ claude (repo X)
  machine B ─┼── HTTP + bearer ──▶ gary serve ──┤
  machine C ─┘                    │  gary.db    └─ gary node --name B ─▶ claude (repo Y)
                                  │  (WAL, single serializer)
                                  └─ dashboard
```

Nodes and clients dial **out** to the hub, so worker machines need no inbound
ports and work behind NAT. The hub is the only thing that listens.

## Commands

```
gary register <name> [--description <text>]   add/update an agent in the registry
gary unregister <name>                        remove an agent (and its queue)
gary list                                     show registered agents
gary send <to> --from <me> [message]          enqueue a message (body from arg or stdin)
gary inbox <name>                             peek pending messages, no dequeue
gary recv <name>                              dequeue oldest pending (FIFO) and auto-ack
gary watch <name> [--interval 1s]             block, printing+acking messages as they arrive

gary serve [--addr 127.0.0.1:4777]            run the hub
gary token new                                generate the shared secret (agents/nodes)
gary user add|rm|list <name>                  dashboard login accounts
gary node [--name <machine>]                  run agents on this machine
gary nodes                                    list machines that have checked in

gary spawn <agent> --node <m> --repo <path>   start a claude agent on a node
gary spawns                                   list spawns and their status
gary stop <agent>                             wind an agent down after its current turn

gary dashboard [--addr localhost:4777]        live HTML view, no API
gary update                                   rebuild+install via go install
```

There is **no `ack` command**. `recv` acknowledges automatically: reading a
message consumes it. This is intentional — it removes any reason for an agent to
send a "got it" / "thanks" reply (see Message etiquette).

Global flags:
- `--db <path>` — override the database file (local mode).
- `--url <hub>` — talk to a hub instead of a local file. Also `$GARY_URL`.
- `--token <tok>` — hub secret. Also `$GARY_TOKEN`, or `~/.config/gary/token`.
- `--json` — machine-readable output. **Every read command must support this.**
  Agents are the primary consumers; JSON is not an afterthought.

With no `--url`/`$GARY_URL`, gary behaves exactly as the single-machine tool it
started as. That is a compatibility guarantee, not an accident.

## Invariants (do not break these)

1. **FIFO per recipient.** `recv` returns the oldest `pending` message for that
   agent, ordered by `id`. Never reorder.
2. **Atomic dequeue + auto-ack.** `recv` selects and marks the message `acked` in
   a single transaction, so two concurrent receivers never get the same message
   and there is no separate acknowledgement step.
3. **Addressable = registered.** `send` fails if `--to` is not a registered
   agent. Sender (`--from`) must also be registered.
4. **No message loss on crash.** A message is only marked `acked` once that
   update has committed. WAL + synchronous commit means an interrupted `recv`
   leaves the message `pending` and re-deliverable. A node that dies mid-turn
   loses the turn, not the message.
5. **Idempotent register.** Registering an existing name updates its description
   and `last_seen`; it does not error or duplicate. Re-registering without a node
   never clears an existing one.
6. **Send guard (etiquette).** `send` rejects empty and acknowledgement-only
   messages. See Message etiquette — this is a hard rule, not a lint.
7. **The hub is the single serializer.** Remote mode changes the transport to the
   database and nothing else. Invariants 1–6 are enforced in exactly one place,
   whether that place is your local file or a hub across the network. Errors keep
   their identity across the wire: a guard rejection arrives as `ErrGuard`, so
   `errors.Is` works remotely. If a change makes local and remote behave
   differently, the change is wrong.
8. **Atomic spawn claim.** `ClaimSpawn` selects the oldest `queued` spawn for a
   node and marks it `claimed` in one transaction — the same shape as `recv`, for
   the same reason. Two nodes must never start the same agent.
9. **A node executes only `claude`.** The argv is built by gary from constants
   and typed `SpawnSpec` fields. A spawn carries a repo, a prompt and a model; it
   never carries a command, a shell string, or an argument list. This is the line
   between "a queue that can start agents" and "remote code execution as a
   service". Do not add a field that crosses it.

## Message lifecycle

```
pending  ── recv (auto-ack) ──▶ acked
```

- `send` → `pending`
- `recv` → `acked` (atomically) and returns the body

`inbox` shows only `pending`. There is no intermediate `delivered` state — read
means acknowledged. Deleting old `acked` rows is manual for now (see Deferred:
retention).

## Spawn lifecycle

```
queued ──claim──▶ claimed ──▶ running ──stop──▶ stopping ──▶ stopped
                                  └────────────────────────▶ failed
```

A spawned agent is **not a process that idles**. Each inbound message runs one
`claude -p` invocation in the agent's repo; between messages nothing is running.
Context survives in the Claude session (`--session-id` on the first turn,
`--resume` after), not in a process held open. That is what makes it crash-safe:
an interrupted turn leaves the message `pending`.

The agent's result is sent back to whoever messaged it. `gary stop` is
cooperative: a watcher goroutine polls the spawn's status and cancels only the
*idle wait*, never the turn's context, so a stop is noticed within seconds while
a claude already running still finishes. Cancelling the turn context instead
would kill claude mid-turn, which is exactly what this design refuses to do.

## Message etiquette (the "no thank-you" guard)

AI agents left unguarded flood each other with acknowledgements, "sounds good",
and "thanks!" — noise that costs tokens and buries real work. `gary` guards
against this at two levels:

1. **Structural (auto-ack).** Because `recv` acknowledges automatically, there is
   never a protocol reason to reply just to confirm receipt. Deletion of the
   courtesy reply beats detecting it.

2. **Contract + enforcement.** The rule agents must follow:

   > Send a message **only when the recipient must act or must know something to
   > do their job.** A message is a request, an answer, or required information.
   > Never send acknowledgements, thanks, confirmations, "sounds good", "will
   > do", or "let me know if you need anything." If nothing is needed, send
   > nothing.

   `send` enforces the floor of this: it rejects an empty body and rejects
   messages whose entire content is a pleasantry (a small, maintained
   deny-pattern of thanks/ack/filler phrases). The pattern is a heuristic with a
   known ceiling — it stops the obvious flood, not every paraphrase. The real
   guard is the contract above, which agents are told to follow.

Spawned agents get this contract in their system prompt. When such an agent's
whole answer is a pleasantry, the guard rejects the reply and the node **drops it
silently** — that is the design working, not an error to log loudly.

Keep the deny-pattern small and boring. Do not build an LLM classifier to detect
politeness; that is exactly the over-engineering this tool exists to avoid.

## Security model

The token authorises enqueueing work **and** starting Claude processes on your
machines. Three constraints keep that honest:

- **Invariant 9** — a node execs only `claude`, never something from the wire.
- **Bind guard** — `gary serve` refuses a public or all-interfaces bind unless
  `--insecure` is passed, and refuses any non-loopback bind without a token. The
  intended deployments are a VPN/Tailscale address, or loopback behind a
  TLS-terminating reverse proxy (the README has a working nginx block).
- **Permission mode** — spawned agents default to `acceptEdits`.
  `--dangerously-skip-permissions` is reachable only via an explicit `--yolo`.

**Two credentials, two audiences.** The bearer token is for agents and nodes;
it is what `Client` sends and what `errors.Is` fidelity depends on. The dashboard
is for a human, so it has a login: `gary user add <name>` writes a PBKDF2-SHA256
hash (600k iterations, per-user salt, stdlib `crypto/pbkdf2` — no new dependency)
to `~/.config/gary/users`, and a successful sign-in sets an HMAC-signed session
cookie. Either credential authorises a request; the dashboard is not a lesser
door, it just has a door a person can use.

Session cookies are keyed on the token *and* the contents of the users file, so
rotating the token or changing a password invalidates every outstanding session
without needing server-side session state. Failed logins are rate-limited per
client IP (5 attempts, then a lockout), and an unknown username still pays the
full PBKDF2 cost so timing does not reveal which names exist.

With no users configured, the hub falls back to token-only: an API client gets
401, and a browser gets 401 rather than a login page it cannot use.

Spawned agents run as the node's user, in the repo you name, with that user's
filesystem access. A node needs `claude` installed and authenticated.

## Data model

SQLite, WAL mode, `foreign_keys = ON`.

```sql
CREATE TABLE agents (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    node        TEXT NOT NULL DEFAULT '',   -- machine it runs on, '' if hand-registered
    created_at  INTEGER NOT NULL,           -- unix seconds
    last_seen   INTEGER NOT NULL
);

CREATE TABLE messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,  -- also the FIFO order key
    from_agent TEXT NOT NULL,                      -- deliberately NOT a foreign key
    to_agent   TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',    -- pending|acked
    created_at INTEGER NOT NULL,
    acked_at   INTEGER                             -- set atomically by recv
);

CREATE TABLE nodes (
    name       TEXT PRIMARY KEY,
    version    TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL   -- heartbeat; stale after 3 missed
);

CREATE TABLE spawns (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agent           TEXT NOT NULL,
    node            TEXT NOT NULL,
    repo            TEXT NOT NULL,
    prompt          TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    permission_mode TEXT NOT NULL DEFAULT 'acceptEdits',
    status          TEXT NOT NULL DEFAULT 'queued',
    session_id      TEXT,                          -- claude session, for --resume
    pid             INTEGER,
    error           TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX idx_inbox ON messages(to_agent, status, id);
CREATE INDEX idx_claim ON spawns(node, status, id);
```

**`from_agent` has no foreign key, on purpose.** Sent messages outlive their
sender: unregistering an agent drops its own inbox (that is what `to_agent`'s
cascade is for) but leaves mail it already delivered to others, so a recipient
never loses unread mail and the dashboard keeps its history. The original schema
did constrain it, which made any agent that had ever sent something impossible to
unregister — `dropSenderFK` in `store.go` rebuilds the table to undo that.

Schema changes go through `migrate()` in `store.go`: `ensureColumn` for added
columns, and a rebuild-and-swap for constraint changes, since SQLite cannot alter
a constraint in place. Both inspect `pragma_*` and are idempotent — no version
counter. A rebuild must pin one connection (`db.Conn`) because
`PRAGMA foreign_keys` is per-connection and a no-op inside a transaction.

## Storage and connection resolution

- Database (local mode / hub): `--db` → `$GARY_DB` → `$XDG_DATA_HOME/gary/gary.db`
  → `~/.local/share/gary/gary.db`. Create parent dirs on first run.
- Hub: `--url` → `$GARY_URL` → empty, meaning local file. A bare `host:port` is
  assumed to be `http://`.
- Token: `--token` → `$GARY_TOKEN` → `~/.config/gary/token` (mode 0600; gary
  warns if it is looser).
- Dashboard users: `~/.config/gary/users`, one `name:pbkdf2-sha256$iters$salt$hash`
  per line, mode 0600. Never contains a plaintext password.

## Layout

Keep it flat. Resist premature packages. Each file earns its own name:

```
main.go        # flag parsing, command dispatch, output (human + --json)
backend.go     # the Backend interface + local/remote resolution
store.go       # SQLite open/migrate + all queries
server.go      # the hub: HTTP API, auth, long-poll, bind guard
client.go      # Backend over HTTP, error-code mapping
node.go        # the runner: claims spawns, supervises claude
auth.go        # password hashing, session cookies, login rate limit
dashboard.html # embedded live view
login.html     # embedded sign-in page
store_test.go  # queue semantics against a temp-file DB
server_test.go # the same semantics over HTTP, plus token auth and bind
auth_test.go   # hashing, sessions, the login flow, lockout, sub-path mounting
node_test.go   # spawn claiming and claude invocation, against a stub binary
```

`Store` and `Client` both implement `Backend`; every command runs against the
interface and neither knows which it has. Split a file out only when it genuinely
earns its own name — one `store.go` with plain functions still beats a
`storage/` package with an interface and one impl.

## Conventions

- **Errors:** wrap with `fmt.Errorf("...: %w", err)`. Sentinels (`ErrGuard`,
  `ErrUnregistered`, `ErrUnauthorized`) cross the wire as a `code` field so
  `errors.Is` keeps working remotely. `main` prints to stderr and exits non-zero.
  No `panic` in library paths.
- **Time:** store unix seconds (`time.Now().Unix()`). Format only at display.
- **DB access:** stdlib `database/sql` with driver `modernc.org/sqlite` (pure Go,
  no cgo — keeps `CGO_ENABLED=0` builds working). Do not add an ORM.
- **HTTP:** stdlib `net/http` on both sides. No router, no framework, no
  websockets — long-polling is the whole push story, matching how `watch` already
  worked. Waiting calls take a `context.Context` and must return on cancellation.
- **Dependencies:** the SQLite driver is the only expected external dep. Add
  nothing else without a concrete reason. Prefer stdlib `flag` over a CLI
  framework unless subcommand ergonomics genuinely demand more.
- **JSON output:** stable field names matching the data model (`from`, `to`,
  `body`, `id`, `status`, timestamps). Agents parse this — don't churn the shape.
  This is why agent names stayed flat instead of becoming `host/agent`.

## Build & test

```
go build -o gary .
go test ./...
```

Requires Go 1.25+ — `go.mod`'s floor comes from the SQLite driver; this code
itself needs 1.24 for the stdlib `crypto/pbkdf2`. Keep the `go` directive at the
lowest version that actually builds, so fewer people need a toolchain download.
For a machine whose toolchain is older, cross-compile — the build is pure Go and
runs on linux, darwin, freebsd and windows across amd64/arm64/arm:

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o gary .
```

Tests run against a temp-file DB (`t.TempDir()`), never the real store. Node
tests run against a stub `--claude-bin` script, never a real model, and no test
needs a network or a credential. What the suite must keep covering:

- **Queue semantics** — FIFO order, atomic dequeue + auto-ack under concurrent
  `recv`, send-to-unregistered rejection, the send guard.
- **The same semantics over HTTP** — the remote path re-asserts them through a
  `Client`, and checks a guard rejection still arrives as `ErrGuard`.
- **Transport** — token auth (401), the bind guard table, long-poll returning
  early on delivery and empty at its deadline.
- **Auth** — hash round-trip, that no plaintext password reaches disk, session
  signing/expiry/tamper, that changing a password invalidates sessions, the
  login flow end to end, and lockout after repeated failures.
- **Mounting** — the whole hub behind a stripped `/gary/` prefix, since the
  dashboard and login redirects are relative and easy to regress.
- **Spawning** — atomic claim under concurrency, the exact `claude` argv and
  cwd, `--session-id` on the first turn and `--resume` after, a pleasantry reply
  dropped silently, and one full message→turn→reply round trip.
- **Migration** — opening a database written by an older schema, preserving ids
  and FIFO order, and staying idempotent when `migrate()` runs again.

Test fixtures must never contain a real credential; the repository is public.

## Deferred (YAGNI until asked)

- Message retention / `gary purge` for old `acked` rows.
- Multiple hubs / replication. One hub is the single serializer; that is what
  makes invariants 1, 2 and 8 cheap. Federating them is a different tool.
- Streaming a spawned agent's intermediate output. Turns are request/response;
  `--output-format stream-json` would mean a place to stream it *to*.

These are noted so they aren't reinvented ad hoc. Do not build them speculatively.
