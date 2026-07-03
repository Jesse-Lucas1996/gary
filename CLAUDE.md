# gary

`gary` is a **local mailbox and message queue for AI agents working across
different repos on the same machine**. Agents register themselves into a shared
registry, then send FIFO messages to each other's queues. It is a single Go
binary backed by one SQLite file — no server, no broker, no daemon, no network.

Scope is deliberately **one machine, many repos**. That is why the single shared
SQLite file is enough: every agent process, whatever repo it runs in, opens the
same DB. Cross-machine is explicitly out of scope (see Deferred).

## Mental model

- **Agent** — a named participant (`researcher`, `planner`, `coder`). Registering
  puts it in the registry so others can discover and address it.
- **Mailbox** — every agent has one inbox queue. Messages are delivered FIFO.
- **Message** — one envelope: `from`, `to`, `body`, timestamps, and a lifecycle
  status (`pending → delivered → acked`).

The whole system is **one SQLite file**. Every `gary` invocation opens it, does
its work in a transaction, and exits. Concurrency between separate agent
processes is handled by SQLite in WAL mode — this is why there is no daemon.
Add a daemon only when agents must talk **across machines** (see Deferred).

## Commands

```
gary register <name> [--description <text>]   # add/update an agent in the registry
gary unregister <name>                         # remove an agent (and its queue)
gary list                                      # show registered agents
gary send <to> --from <me> [message]           # enqueue a message (body from arg or stdin)
gary inbox <name>                              # peek pending messages, no dequeue
gary recv <name>                               # dequeue oldest pending (FIFO) and auto-ack
gary watch <name> [--interval 1s]              # block, printing+acking messages as they arrive
```

There is **no `ack` command**. `recv` acknowledges automatically: reading a
message consumes it. This is intentional — it removes any reason for an agent to
send a "got it" / "thanks" reply (see Message etiquette).

Global flags:
- `--db <path>` — override the database file.
- `--json` — machine-readable output. **Every read command must support this.**
  Agents are the primary consumers; JSON is not an afterthought.

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
   leaves the message `pending` and re-deliverable.
5. **Idempotent register.** Registering an existing name updates its description
   and `last_seen`; it does not error or duplicate.
6. **Send guard (etiquette).** `send` rejects empty and acknowledgement-only
   messages. See Message etiquette — this is a hard rule, not a lint.

## Message lifecycle

```
pending  ── recv (auto-ack) ──▶ acked
```

- `send` → `pending`
- `recv` → `acked` (atomically) and returns the body

`inbox` shows only `pending`. There is no intermediate `delivered` state — read
means acknowledged. Deleting old `acked` rows is manual for now (see Deferred:
retention).

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

Keep the deny-pattern small and boring. Do not build an LLM classifier to detect
politeness; that is exactly the over-engineering this tool exists to avoid.

## Data model

SQLite, WAL mode, `foreign_keys = ON`.

```sql
CREATE TABLE agents (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,   -- unix seconds
    last_seen   INTEGER NOT NULL
);

CREATE TABLE messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,  -- also the FIFO order key
    from_agent TEXT NOT NULL REFERENCES agents(name),
    to_agent   TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',    -- pending|acked
    created_at INTEGER NOT NULL,
    acked_at   INTEGER                             -- set atomically by recv
);

CREATE INDEX idx_inbox ON messages(to_agent, status, id);
```

## Storage location

Resolution order: `--db` flag → `$GARY_DB` env → `$XDG_DATA_HOME/gary/gary.db`
→ `~/.local/share/gary/gary.db`. Create parent dirs on first run.

## Layout

Keep it flat. Resist premature packages.

```
main.go        # flag parsing, command dispatch, output (human + --json)
store.go       # SQLite open/migrate + all queries (Register, List, Send, Recv, Ack)
store_test.go  # queue semantics against a temp-file DB
```

Split a file out only when it genuinely earns its own name. One `store.go` with
plain functions beats a `storage/` package with an interface and one impl.

## Conventions

- **Errors:** wrap with `fmt.Errorf("...: %w", err)`. `main` prints to stderr and
  exits non-zero. No `panic` in library paths.
- **Time:** store unix seconds (`time.Now().Unix()`). Format only at display.
- **DB access:** stdlib `database/sql` with driver `modernc.org/sqlite` (pure Go,
  no cgo — keeps `CGO_ENABLED=0` builds working). Do not add an ORM.
- **Dependencies:** the SQLite driver is the only expected external dep. Add
  nothing else without a concrete reason. Prefer stdlib `flag` over a CLI
  framework unless subcommand ergonomics genuinely demand more.
- **JSON output:** stable field names matching the data model (`from`, `to`,
  `body`, `id`, `status`, timestamps). Agents parse this — don't churn the shape.

## Build & test

```
go build -o gary .
go test ./...
```

Tests run against a temp-file DB (`t.TempDir()`), never the real store. Cover the
invariants above: FIFO order, atomic dequeue + auto-ack under concurrent `recv`,
send-to-unregistered rejection, and the send guard (empty + pleasantry-only
bodies rejected).

## Deferred (YAGNI until asked)

- Message retention / `gary purge` for old `acked` rows.
- Cross-machine transport — **out of scope by design.** `gary` is one machine,
  many repos. If this is ever needed it is a different tool, not a flag.

These are noted so they aren't reinvented ad hoc. Do not build them speculatively.
