package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

//go:embed login.html
var loginHTML []byte

const modulePath = "github.com/Jesse-Lucas1996/gary"

const version = "0.2.0"

const usage = `gary — mailbox/queue for AI agents, across repos and machines

Messaging:
  gary register <name> [--description <text>]   add/update an agent
  gary unregister <name>                        remove an agent and its queue
  gary list                                     list registered agents
  gary send <to> --from <me> [message]          enqueue a message (body from arg or stdin)
      --expect-reply                            ...and have their result sent back to you
  gary inbox <name>                             peek pending messages (no dequeue)
  gary recv <name>                              dequeue oldest pending, auto-ack
  gary watch <name> [--interval 1s]             block, printing messages as they arrive

Channels (one message, every member):
  gary channel new <name> [--description <t>]   create or update a shared channel
  gary channel rm <name>                        delete a channel
  gary channel join <name> --agent <a>          put an agent on a channel
  gary channel leave <name> --agent <a>         take an agent off a channel
  gary channels                                 list channels and their members
  gary post <channel> --from <me> [message]     fan out to every member but you

Cross-machine:
  gary serve [--addr 127.0.0.1:4777]            run the hub: owns the DB, serves the API
  gary token new                                generate a shared token (agents/nodes)
  gary user add|rm|list <name>                  dashboard login accounts
  gary node [--name <machine>]                  run agents on this machine
  gary nodes                                    list machines that have checked in

Claude agents:
  gary spawn <agent> --node <m> --repo <path>   start a claude agent on a node
  gary spawns                                   list spawns and their status
  gary stop <agent>                             wind an agent down after its current turn

Other:
  gary dashboard [--addr localhost:4777]        live HTML view (no API)
  gary update                                   rebuild+install the latest gary

Global flags:
  --db <path>     local database file
  --url <hub>     talk to a hub instead of a local file (or $GARY_URL)
  --token <tok>   hub token (or $GARY_TOKEN, or ~/.config/gary/token)
  --json          machine-readable output`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gary: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage)
		return nil
	}
	cmd, rest := args[0], reorder(args[1:])

	// Shared flags parsed per-subcommand so they can sit anywhere in the line.
	var c conn
	var jsonOut bool
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&c.db, "db", "", "database file path")
	fs.StringVar(&c.url, "url", "", "hub url (default $GARY_URL; empty means local file)")
	fs.StringVar(&c.token, "token", "", "hub token (default $GARY_TOKEN or ~/.config/gary/token)")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON output")

	switch cmd {
	case "register":
		desc := fs.String("description", "", "agent description")
		name, err := parse1(fs, rest, "register <name>")
		if err != nil {
			return err
		}
		return c.with(func(b Backend) error { return b.RegisterOn(name, *desc, "") })

	case "unregister":
		name, err := parse1(fs, rest, "unregister <name>")
		if err != nil {
			return err
		}
		return c.with(func(b Backend) error { return b.Unregister(name) })

	case "list":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			agents, err := b.List()
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(agents)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tNODE\tLAST SEEN\tDESCRIPTION")
			for _, a := range agents {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, dash(a.Node), ago(a.LastSeen), a.Description)
			}
			return tw.Flush()
		})

	case "send":
		from := fs.String("from", "", "sending agent (required)")
		expectReply := fs.Bool("expect-reply", false,
			"have the recipient's result sent back to you as a new message")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: gary send <to> --from <me> [--expect-reply] [message]")
		}
		if *from == "" {
			return fmt.Errorf("--from is required")
		}
		to := fs.Arg(0)
		body := strings.Join(fs.Args()[1:], " ")
		if body == "" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			body = string(b)
		}
		return c.with(func(bk Backend) error {
			id, err := bk.Send(*from, to, body, *expectReply)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(map[string]int64{"id": id})
			}
			fmt.Printf("sent #%d to %s\n", id, to)
			return nil
		})

	case "inbox":
		name, err := parse1(fs, rest, "inbox <name>")
		if err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			msgs, err := b.Inbox(name)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(msgs)
			}
			if len(msgs) == 0 {
				fmt.Println("(empty)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tFROM\tWHEN\tBODY")
			for _, m := range msgs {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", m.ID, m.From, ago(m.CreatedAt), oneline(m.Body))
			}
			return tw.Flush()
		})

	case "recv":
		name, err := parse1(fs, rest, "recv <name>")
		if err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			m, err := b.Recv(name)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(m) // null when empty
			}
			if m == nil {
				fmt.Println("(empty)")
				return nil
			}
			fmt.Printf("from %s (#%d):\n%s\n", m.From, m.ID, m.Body)
			return nil
		})

	case "watch":
		interval := fs.Duration("interval", time.Second, "how long to wait for each message")
		name, err := parse1(fs, rest, "watch <name> [--interval 1s]")
		if err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			for {
				m, err := b.RecvWait(context.Background(), name, *interval)
				if err != nil {
					return err
				}
				if m == nil {
					continue
				}
				if jsonOut {
					if err := writeJSON(m); err != nil {
						return err
					}
				} else {
					fmt.Printf("from %s (#%d):\n%s\n\n", m.From, m.ID, m.Body)
				}
			}
		})

	case "channel":
		desc := fs.String("description", "", "what the channel is for (channel new)")
		agent := fs.String("agent", "", "agent to put on or take off it (channel join|leave)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return channelCmd(c, fs.Args(), *desc, *agent)

	case "channels":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			chans, err := b.Channels()
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(chans)
			}
			if len(chans) == 0 {
				fmt.Println("(no channels — `gary channel new <name>` creates one)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CHANNEL\tMEMBERS\tDESCRIPTION")
			for _, ch := range chans {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", ch.Name, dash(strings.Join(ch.Members, ", ")), ch.Description)
			}
			return tw.Flush()
		})

	case "post":
		from := fs.String("from", "", "posting agent (required)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: gary post <channel> --from <me> [message]")
		}
		if *from == "" {
			return fmt.Errorf("--from is required")
		}
		channel := fs.Arg(0)
		body := strings.Join(fs.Args()[1:], " ")
		if body == "" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			body = string(b)
		}
		return c.with(func(b Backend) error {
			ids, err := b.Post(*from, channel, body)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(map[string]any{"channel": channel, "ids": ids})
			}
			if len(ids) == 0 {
				fmt.Printf("posted to %s — nobody else is on it, so it went nowhere\n", channel)
				return nil
			}
			noun := "recipients"
			if len(ids) == 1 {
				noun = "recipient"
			}
			fmt.Printf("posted to %s (%d %s)\n", channel, len(ids), noun)
			return nil
		})

	case "serve":
		addr := fs.String("addr", "127.0.0.1:4777", "address to listen on")
		insecure := fs.Bool("insecure", false, "allow binding a public interface")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return serveCmd(c, *addr, *insecure, true)

	case "dashboard":
		addr := fs.String("addr", "localhost:4777", "address to listen on")
		insecure := fs.Bool("insecure", false, "allow binding a public interface")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return serveCmd(c, *addr, *insecure, false)

	case "node":
		name := fs.String("name", "", "node name (default: hostname)")
		bin := fs.String("claude-bin", "claude", "claude executable to run")
		limit := fs.Duration("turn-timeout", 30*time.Minute, "max wall time for one claude turn (0 = none)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		ctx, stop := signalContext()
		defer stop()
		return c.with(func(b Backend) error { return runNode(ctx, b, *name, *bin, *limit) })

	case "nodes":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			nodes, err := b.Nodes()
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(nodes)
			}
			if len(nodes) == 0 {
				fmt.Println("(no nodes — run `gary node` on a machine to add one)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NODE\tSTATUS\tLAST SEEN\tVERSION")
			for _, n := range nodes {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n.Name, nodeStatus(n.LastSeen), ago(n.LastSeen), n.Version)
			}
			return tw.Flush()
		})

	case "spawn":
		spec := SpawnSpec{}
		fs.StringVar(&spec.Node, "node", "", "node to run on (required)")
		fs.StringVar(&spec.Repo, "repo", "", "working directory on that node (required)")
		fs.StringVar(&spec.Prompt, "prompt", "", "extra system prompt for the agent")
		fs.StringVar(&spec.Model, "model", "", "model alias (opus, sonnet, ...)")
		fs.StringVar(&spec.PermissionMode, "permission-mode", "acceptEdits", "claude permission mode")
		fs.BoolVar(&spec.Force, "force", false, "skip node/name checks")
		yolo := fs.Bool("yolo", false, "run with --dangerously-skip-permissions")
		agent, err := parse1(fs, rest, "spawn <agent> --node <machine> --repo <path>")
		if err != nil {
			return err
		}
		spec.Agent = agent
		if *yolo {
			spec.PermissionMode = "bypassPermissions"
		}
		if spec.Node == "" || spec.Repo == "" {
			return fmt.Errorf("usage: gary spawn <agent> --node <machine> --repo <path>")
		}
		return c.with(func(b Backend) error {
			id, err := b.Spawn(spec)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(map[string]int64{"id": id})
			}
			fmt.Printf("queued spawn #%d: %s on %s (%s)\n", id, spec.Agent, spec.Node, spec.Repo)
			return nil
		})

	case "spawns":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			spawns, err := b.Spawns(100)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(spawns)
			}
			if len(spawns) == 0 {
				fmt.Println("(no spawns)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAGENT\tNODE\tSTATUS\tREPO\tNOTE")
			for _, s := range spawns {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Agent, s.Node, s.Status, s.Repo, oneline(s.Error))
			}
			return tw.Flush()
		})

	case "stop":
		name, err := parse1(fs, rest, "stop <agent>")
		if err != nil {
			return err
		}
		return c.with(func(b Backend) error {
			if err := b.StopSpawn(name); err != nil {
				return err
			}
			fmt.Printf("%s will stop after its current turn\n", name)
			return nil
		})

	case "token":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.Arg(0) != "new" {
			return fmt.Errorf("usage: gary token new")
		}
		return newToken()

	case "user":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return userCmd(fs.Args())

	case "update":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return runUpdate()

	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func serveCmd(c conn, addr string, insecure, api bool) error {
	if url, _ := HubURL(c.url); url != "" {
		return fmt.Errorf("serve owns the database directly; unset GARY_URL (that points at a hub, and this would be it)")
	}
	path, err := DBPath(c.db)
	if err != nil {
		return err
	}
	s, err := Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	token, err := ResolveToken(c.token)
	if err != nil {
		return err
	}
	users, err := openUsers()
	if err != nil {
		return err
	}
	return serveHub(s, serveOpts{addr: addr, token: token, insecure: insecure, api: api, dbPath: path, users: users})
}

func openUsers() (*userStore, error) {
	p, err := UsersPath()
	if err != nil {
		return nil, err
	}
	return loadUsers(p)
}

// channelCmd takes the positionals left after the shared flag set has parsed;
// every subcommand is `<sub> <name>`, so the flags themselves live on that set
// and --db/--url/--json keep working here like everywhere else.
func channelCmd(c conn, args []string, desc, agent string) error {
	const use = "usage: gary channel new <name> [--description <text>] | gary channel rm <name> | " +
		"gary channel join|leave <name> --agent <a>"
	if len(args) != 2 {
		return fmt.Errorf("%s", use)
	}
	sub, name := args[0], args[1]
	switch sub {
	case "new":
		return c.with(func(b Backend) error { return b.CreateChannel(name, desc) })
	case "rm":
		return c.with(func(b Backend) error { return b.DeleteChannel(name) })
	case "join", "leave":
		if agent == "" {
			return fmt.Errorf("usage: gary channel %s <name> --agent <a>", sub)
		}
		return c.with(func(b Backend) error {
			if sub == "join" {
				return b.JoinChannel(name, agent)
			}
			return b.LeaveChannel(name, agent)
		})
	default:
		return fmt.Errorf("unknown channel command %q\n\n%s", sub, use)
	}
}

func userCmd(args []string) error {
	users, err := openUsers()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: gary user add <name> | gary user rm <name> | gary user list")
	}
	switch args[0] {
	case "add":
		if len(args) != 2 {
			return fmt.Errorf("usage: gary user add <name>")
		}
		pw, err := readPassword("password: ")
		if err != nil {
			return err
		}
		if confirm, err := readPassword("confirm: "); err != nil {
			return err
		} else if confirm != pw {
			return fmt.Errorf("passwords do not match")
		}
		if err := users.set(args[1], pw); err != nil {
			return err
		}
		fmt.Printf("user %q set in %s\n", args[1], users.path)
		fmt.Println("existing browser sessions were invalidated; sign in again")
		return nil
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: gary user rm <name>")
		}
		if err := users.remove(args[1]); err != nil {
			return err
		}
		fmt.Printf("removed %q\n", args[1])
		return nil
	case "list":
		names := users.names()
		if len(names) == 0 {
			fmt.Println("(no users — run `gary user add <name>`; the dashboard falls back to the token)")
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	default:
		return fmt.Errorf("unknown: gary user %s", args[0])
	}
}

func newToken() error {
	path, err := TokenPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; delete it first if you mean to rotate (every machine needs the new value)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n\ncopy this to every machine (same path, or $GARY_TOKEN):\n  %s\n", path, tok)
	return nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// reorder moves flags ahead of positionals so `gary register <name> --flag` works
// (stdlib flag stops at the first positional). Boolean flags don't consume the
// next token; every other --flag does.
// a message word starting with "-" must sit after a "--" terminator.
func reorder(args []string) []string {
	boolFlags := map[string]bool{
		"json": true, "insecure": true, "force": true, "yolo": true, "expect-reply": true,
	}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(a, "=") && !boolFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

// parse1 parses flags then requires exactly one positional arg.
func parse1(fs *flag.FlagSet, args []string, use string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 1 {
		return "", fmt.Errorf("usage: gary %s", use)
	}
	return fs.Arg(0), nil
}

// runUpdate shells out to `go install <module>@latest` — the Go toolchain
// already resolves versions, verifies checksums, and rebuilds; no need to
// reinvent binary download/replace. Only updates the go-install location
// (GOPATH/bin); if you copied the binary elsewhere, re-copy it after.
func runUpdate() error {
	fmt.Printf("go install %s@latest\n", modulePath)
	cmd := exec.Command("go", "install", modulePath+"@latest")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// ponytail: no version tags on this repo, so @latest is a pseudo-version
	// lookup — proxy.golang.org caches that and can lag behind a fresh push.
	// GOPROXY=direct skips the proxy and resolves straight from git.
	cmd.Env = append(os.Environ(), "GOPROXY=direct")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	fmt.Println("updated. if `gary` isn't on your PATH from $(go env GOPATH)/bin, re-copy it there.")
	return nil
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func ago(unix int64) string {
	d := time.Since(time.Unix(unix, 0)).Round(time.Second)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func nodeStatus(lastSeen int64) string {
	if time.Since(time.Unix(lastSeen, 0)) > 3*heartbeatEvery {
		return "stale"
	}
	return "up"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneline(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}
