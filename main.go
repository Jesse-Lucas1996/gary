package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `gary — local mailbox/queue for AI agents across repos

Usage:
  gary register <name> [--description <text>]   add/update an agent
  gary unregister <name>                        remove an agent and its queue
  gary list                                     list registered agents
  gary send <to> --from <me> [message]          enqueue a message (body from arg or stdin)
  gary inbox <name>                             peek pending messages (no dequeue)
  gary recv <name>                              dequeue oldest pending, auto-ack

Global flags: --db <path>, --json`

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

	// Shared flags parsed per-subcommand so --db/--json can sit anywhere.
	var dbFlag string
	var jsonOut bool
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&dbFlag, "db", "", "database file path")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON output")

	switch cmd {
	case "register":
		desc := fs.String("description", "", "agent description")
		name, err := parse1(fs, rest, "register <name>")
		if err != nil {
			return err
		}
		return withStore(dbFlag, func(s *Store) error { return s.Register(name, *desc) })

	case "unregister":
		name, err := parse1(fs, rest, "unregister <name>")
		if err != nil {
			return err
		}
		return withStore(dbFlag, func(s *Store) error { return s.Unregister(name) })

	case "list":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return withStore(dbFlag, func(s *Store) error {
			agents, err := s.List()
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(agents)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tLAST SEEN\tDESCRIPTION")
			for _, a := range agents {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, ago(a.LastSeen), a.Description)
			}
			return tw.Flush()
		})

	case "send":
		from := fs.String("from", "", "sending agent (required)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: gary send <to> --from <me> [message]")
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
		return withStore(dbFlag, func(s *Store) error {
			id, err := s.Send(*from, to, body)
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
		return withStore(dbFlag, func(s *Store) error {
			msgs, err := s.Inbox(name)
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
		return withStore(dbFlag, func(s *Store) error {
			m, err := s.Recv(name)
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

	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

// reorder moves flags ahead of positionals so `gary register <name> --flag` works
// (stdlib flag stops at the first positional). --json is the only boolean flag;
// every other --flag consumes the next token as its value.
// ponytail: a message word starting with "-" must sit after a "--" terminator.
func reorder(args []string) []string {
	boolFlags := map[string]bool{"json": true}
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

func withStore(dbFlag string, fn func(*Store) error) error {
	path, err := DBPath(dbFlag)
	if err != nil {
		return err
	}
	s, err := Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
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

func oneline(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}
