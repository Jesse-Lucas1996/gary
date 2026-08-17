package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Backend interface {
	RegisterOn(name, desc, node string) error
	Unregister(name string) error
	List() ([]Agent, error)
	Send(from, to, body string) (int64, error)
	Inbox(name string) ([]Message, error)
	Recv(name string) (*Message, error)
	RecvWait(ctx context.Context, name string, d time.Duration) (*Message, error)
	Recent(limit int) ([]Message, error)

	Heartbeat(name, version string) error
	Nodes() ([]Node, error)
	Spawn(spec SpawnSpec) (int64, error)
	ClaimSpawnWait(ctx context.Context, node string, d time.Duration) (*Spawn, error)
	UpdateSpawn(id int64, status, sessionID string, pid int, errMsg string) error
	SpawnStatus(id int64) (string, error)
	StopSpawn(agent string) error
	Spawns(limit int) ([]Spawn, error)

	Close() error
}

var (
	_ Backend = (*Store)(nil)
	_ Backend = (*Client)(nil)
)

type conn struct {
	db    string
	url   string
	token string
}

func (c conn) open() (Backend, error) {
	url, err := HubURL(c.url)
	if err != nil {
		return nil, err
	}
	if url == "" {
		path, err := DBPath(c.db)
		if err != nil {
			return nil, err
		}
		return Open(path)
	}
	token, err := ResolveToken(c.token)
	if err != nil {
		return nil, err
	}
	return NewClient(url, token), nil
}

func (c conn) with(fn func(Backend) error) error {
	b, err := c.open()
	if err != nil {
		return err
	}
	defer b.Close()
	return fn(b)
}

func HubURL(flag string) (string, error) {
	raw := flag
	if raw == "" {
		raw = os.Getenv("GARY_URL")
	}
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "/"))
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("hub url must be http or https, got %q", raw)
	}
	return raw, nil
}

func ResolveToken(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv("GARY_TOKEN"); env != "" {
		return env, nil
	}
	path, err := TokenPath()
	if err != nil {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "gary: warning: %s is mode %04o; run `chmod 600 %s`\n",
			path, info.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func TokenPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gary", "token"), nil
}
