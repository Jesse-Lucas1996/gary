package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Claude subscription usage for the dashboard header: the same numbers Claude
// Code's /usage shows, read with the OAuth login the hub user's claude already
// has. Read-only and best-effort. The upstream endpoint rate-limits hard (429,
// shared with every claude session on the box), so the last good reading is
// kept, also on disk, and served marked stale while the upstream refuses;
// the header only goes blank when there has never been a reading.

var (
	usageURL       = "https://api.anthropic.com/api/oauth/usage"
	usageCredsPath = func() string {
		if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
			return filepath.Join(d, ".credentials.json")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".claude", ".credentials.json")
	}
)

// usageWindow is one rolling limit window: percent used and when it resets.
type usageWindow struct {
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
}

type usageReport struct {
	Plan     string       `json:"plan,omitempty"`
	FiveHour *usageWindow `json:"five_hour,omitempty"`
	SevenDay *usageWindow `json:"seven_day,omitempty"`
	// When this reading was taken, and whether it's an older one served
	// because the latest fetch failed.
	At    string `json:"at,omitempty"`
	Stale bool   `json:"stale,omitempty"`
}

var usageSavePath = func() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "gary", "usage.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "gary", "usage.json")
}

// usageCache keeps the upstream call to one per usageTTL however many
// dashboards are polling, backs off when the upstream says 429, and remembers
// the last good reading.
type usageCache struct {
	mu    sync.Mutex
	at    time.Time     // last fetch attempt
	wait  time.Duration // how long to wait after that attempt
	rep   *usageReport  // last good reading (nil: never had one)
	err   error         // last fetch error, nil after a success
	http  *http.Client
	saved bool // tried loading the on-disk reading
}

const (
	usageTTL     = 60 * time.Second
	usageBackoff = 5 * time.Minute // after a 429 without Retry-After
	usageMaxWait = 30 * time.Minute
)

// errRateLimited carries the upstream's Retry-After.
type errRateLimited struct{ after time.Duration }

func (e errRateLimited) Error() string { return "usage request: 429 Too Many Requests" }

func (c *usageCache) get() (*usageReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.saved {
		c.saved = true
		if raw, err := os.ReadFile(usageSavePath()); err == nil {
			var r usageReport
			if json.Unmarshal(raw, &r) == nil {
				c.rep = &r
			}
		}
	}
	wait := c.wait
	if wait == 0 {
		wait = usageTTL
	}
	if c.at.IsZero() || time.Since(c.at) >= wait {
		rep, err := fetchUsage(c.http)
		c.at = time.Now()
		c.err = err
		c.wait = usageTTL
		if err == nil {
			rep.At = c.at.UTC().Format(time.RFC3339)
			c.rep = rep
			if raw, e := json.Marshal(rep); e == nil {
				_ = os.MkdirAll(filepath.Dir(usageSavePath()), 0o755)
				_ = os.WriteFile(usageSavePath(), raw, 0o644)
			}
		} else if rl, ok := err.(errRateLimited); ok {
			c.wait = usageBackoff
			if rl.after > 0 {
				c.wait = min(max(rl.after, usageTTL), usageMaxWait)
			}
		}
	}
	if c.rep == nil {
		return nil, c.err
	}
	out := *c.rep
	out.Stale = c.err != nil
	return &out, nil
}

func fetchUsage(hc *http.Client) (*usageReport, error) {
	raw, err := os.ReadFile(usageCredsPath())
	if err != nil {
		return nil, fmt.Errorf("read claude credentials: %w", err)
	}
	var creds struct {
		OAuth struct {
			AccessToken      string `json:"accessToken"`
			SubscriptionType string `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil || creds.OAuth.AccessToken == "" {
		return nil, fmt.Errorf("no claude oauth login in credentials")
	}
	req, err := http.NewRequest("GET", usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.OAuth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return nil, errRateLimited{time.Duration(after) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage request: %s", resp.Status)
	}
	var body struct {
		FiveHour *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	rep := &usageReport{Plan: creds.OAuth.SubscriptionType}
	if w := body.FiveHour; w != nil {
		rep.FiveHour = &usageWindow{w.Utilization, w.ResetsAt}
	}
	if w := body.SevenDay; w != nil {
		rep.SevenDay = &usageWindow{w.Utilization, w.ResetsAt}
	}
	return rep, nil
}

func registerUsage(mux *http.ServeMux) {
	c := &usageCache{http: &http.Client{Timeout: 10 * time.Second}}
	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		rep, err := c.get()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			// Not an error the dashboard can do anything about; it hides the widget.
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(rep)
	})
}
