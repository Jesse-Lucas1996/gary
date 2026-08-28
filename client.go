package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base  string
	token string
	hc    *http.Client
}

func NewClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		hc:    &http.Client{},
	}
}

func (c *Client) Close() error { return nil }

type errResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func (e errResponse) toError() error {
	switch e.Code {
	case "guard":
		return fmt.Errorf("%w: %s", ErrGuard, strings.TrimPrefix(e.Error, ErrGuard.Error()+": "))
	case "unregistered":
		return fmt.Errorf("%w: %s", ErrUnregistered, strings.TrimPrefix(e.Error, ErrUnregistered.Error()+": "))
	case "unauthorized":
		return fmt.Errorf("%w: %s", ErrUnauthorized, e.Error)
	default:
		return errors.New(e.Error)
	}
}

func (c *Client) do(method, path string, in any, timeout time.Duration) ([]byte, error) {
	return c.doCtx(context.Background(), method, path, in, timeout)
}

func (c *Client) doCtx(ctx context.Context, method, path string, in any, timeout time.Duration) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var e errResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, e.toError()
		}
		return nil, fmt.Errorf("hub %s: %s: %s", c.base, resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func (c *Client) getJSON(method, path string, in, out any, timeout time.Duration) error {
	raw, err := c.do(method, path, in, timeout)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) RegisterOn(name, desc, node string) error {
	return c.getJSON("POST", "/v1/register",
		map[string]string{"name": name, "description": desc, "node": node}, nil, reqTimeout)
}

func (c *Client) Unregister(name string) error {
	return c.getJSON("POST", "/v1/unregister", map[string]string{"name": name}, nil, reqTimeout)
}

func (c *Client) List() ([]Agent, error) {
	var out []Agent
	return out, c.getJSON("GET", "/v1/agents", nil, &out, reqTimeout)
}

func (c *Client) Send(from, to, body string, expectsReply bool) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	in := struct {
		From         string `json:"from"`
		To           string `json:"to"`
		Body         string `json:"body"`
		ExpectsReply bool   `json:"expects_reply"`
	}{from, to, body, expectsReply}
	err := c.getJSON("POST", "/v1/send", in, &out, reqTimeout)
	return out.ID, err
}

func (c *Client) CreateChannel(name, desc string) error {
	return c.getJSON("POST", "/v1/channels/new",
		map[string]string{"name": name, "description": desc}, nil, reqTimeout)
}

func (c *Client) DeleteChannel(name string) error {
	return c.getJSON("POST", "/v1/channels/rm", map[string]string{"name": name}, nil, reqTimeout)
}

func (c *Client) JoinChannel(channel, agent string) error {
	return c.getJSON("POST", "/v1/channels/join",
		map[string]string{"channel": channel, "agent": agent}, nil, reqTimeout)
}

func (c *Client) LeaveChannel(channel, agent string) error {
	return c.getJSON("POST", "/v1/channels/leave",
		map[string]string{"channel": channel, "agent": agent}, nil, reqTimeout)
}

func (c *Client) Channels() ([]Channel, error) {
	var out []Channel
	return out, c.getJSON("GET", "/v1/channels", nil, &out, reqTimeout)
}

func (c *Client) Post(from, channel, body string) ([]int64, error) {
	var out struct {
		IDs []int64 `json:"ids"`
	}
	err := c.getJSON("POST", "/v1/post",
		map[string]string{"from": from, "channel": channel, "body": body}, &out, reqTimeout)
	return out.IDs, err
}

func (c *Client) Inbox(name string) ([]Message, error) {
	var out []Message
	return out, c.getJSON("GET", "/v1/inbox?agent="+url.QueryEscape(name), nil, &out, reqTimeout)
}

func (c *Client) Recv(name string) (*Message, error) {
	return c.RecvWait(context.Background(), name, 0)
}

func (c *Client) RecvWait(ctx context.Context, name string, d time.Duration) (*Message, error) {
	raw, err := c.doCtx(ctx, "POST", "/v1/recv",
		map[string]any{"agent": name, "wait_ms": d.Milliseconds()}, d+reqTimeout)
	if err != nil {
		return nil, err
	}
	return decodeNullable[Message](raw)
}

func (c *Client) Recent(limit int) ([]Message, error) {
	var out []Message
	return out, c.getJSON("GET", "/v1/recent?limit="+strconv.Itoa(limit), nil, &out, reqTimeout)
}

func (c *Client) Heartbeat(name, version string) error {
	return c.getJSON("POST", "/v1/nodes/heartbeat",
		map[string]string{"name": name, "version": version}, nil, reqTimeout)
}

func (c *Client) Nodes() ([]Node, error) {
	var out []Node
	return out, c.getJSON("GET", "/v1/nodes", nil, &out, reqTimeout)
}

func (c *Client) Spawn(spec SpawnSpec) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	return out.ID, c.getJSON("POST", "/v1/spawn", spec, &out, reqTimeout)
}

func (c *Client) ClaimSpawnWait(ctx context.Context, node string, d time.Duration) (*Spawn, error) {
	raw, err := c.doCtx(ctx, "POST", "/v1/nodes/claim",
		map[string]any{"node": node, "wait_ms": d.Milliseconds()}, d+reqTimeout)
	if err != nil {
		return nil, err
	}
	return decodeNullable[Spawn](raw)
}

func (c *Client) UpdateSpawn(id int64, status, sessionID string, pid int, errMsg string) error {
	return c.getJSON("POST", "/v1/spawns/status", map[string]any{
		"id": id, "status": status, "session_id": sessionID, "pid": pid, "error": errMsg,
	}, nil, reqTimeout)
}

func (c *Client) SpawnStatus(id int64) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	return out.Status, c.getJSON("GET", "/v1/spawns/get?id="+strconv.FormatInt(id, 10), nil, &out, reqTimeout)
}

func (c *Client) StopSpawn(agent string) error {
	return c.getJSON("POST", "/v1/spawns/stop", map[string]string{"agent": agent}, nil, reqTimeout)
}

func (c *Client) Spawns(limit int) ([]Spawn, error) {
	var out []Spawn
	return out, c.getJSON("GET", "/v1/spawns?limit="+strconv.Itoa(limit), nil, &out, reqTimeout)
}

const reqTimeout = 30 * time.Second

func decodeNullable[T any](raw []byte) (*T, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
