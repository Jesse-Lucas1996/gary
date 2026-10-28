package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	Label    string  `json:"label,omitempty"` // Codex windows: "5h", "7d", from the window length
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
		rep.FiveHour = &usageWindow{Percent: w.Utilization, ResetsAt: w.ResetsAt}
	}
	if w := body.SevenDay; w != nil {
		rep.SevenDay = &usageWindow{Percent: w.Utilization, ResetsAt: w.ResetsAt}
	}
	return rep, nil
}

func registerUsage(mux *http.ServeMux) {
	c := &usageCache{http: &http.Client{Timeout: 10 * time.Second}}
	var codexMu sync.Mutex
	var codexAt time.Time
	var codex *codexUsageReport
	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		codexMu.Lock()
		if time.Since(codexAt) >= 30*time.Second {
			codex, codexAt = readCodexUsage(), time.Now()
		}
		cx := codex
		codexMu.Unlock()
		rep, err := c.get()
		w.Header().Set("Content-Type", "application/json")
		// Claude's fields stay top-level (older dashboards); Codex rides along under "codex".
		out := map[string]any{}
		if err != nil {
			// Not an error the dashboard can do anything about; it hides the Claude part.
			out["error"] = err.Error()
		} else {
			raw, _ := json.Marshal(rep)
			json.Unmarshal(raw, &out)
		}
		if cx != nil {
			out["codex"] = cx
		}
		json.NewEncoder(w).Encode(out)
	})
}

// Codex usage: Codex has no usage endpoint the hub can call, but every Codex
// session log (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl) records the
// account's rate-limit snapshot on each token_count event. The newest snapshot
// across recent logs is the current usage. Local files only: no API call.

var codexSessionsDir = func() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "sessions")
}

type codexUsageReport struct {
	Primary   *usageWindow `json:"primary,omitempty"`
	Secondary *usageWindow `json:"secondary,omitempty"`
	At        string       `json:"at,omitempty"` // when Codex last reported it
}

type codexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

func (w *codexWindow) window() *usageWindow {
	if w == nil {
		return nil
	}
	reset := ""
	if w.ResetsAt > 0 {
		reset = time.Unix(w.ResetsAt, 0).UTC().Format(time.RFC3339)
	}
	return &usageWindow{Percent: w.UsedPercent, ResetsAt: reset, Label: windowLabel(w.WindowMinutes)}
}

// windowLabel names a limit window the way the header shows it: 5h, 7d, ...
func windowLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes%1440 == 0:
		return strconv.Itoa(minutes/1440) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	}
	return strconv.Itoa(minutes) + "m"
}

// readCodexUsage returns the newest rate-limit snapshot in the Codex session
// logs written in the last week, or nil when there is none.
func readCodexUsage() *codexUsageReport {
	var files []string
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	filepath.WalkDir(codexSessionsDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		if info, e := d.Info(); e == nil && info.ModTime().After(cutoff) {
			files = append(files, p)
		}
		return nil
	})
	mtime := func(p string) time.Time {
		info, err := os.Stat(p)
		if err != nil {
			return time.Time{}
		}
		return info.ModTime()
	}
	sort.Slice(files, func(i, j int) bool { return mtime(files[i]).After(mtime(files[j])) })
	var best *codexUsageReport
	var bestAt time.Time
	for i, p := range files {
		// the newest few files hold the newest snapshot; stop once older files can't beat it
		if i >= 8 || (best != nil && mtime(p).Before(bestAt)) {
			break
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.Contains(line, []byte(`"rate_limits":{`)) {
				continue
			}
			var ev struct {
				Timestamp string `json:"timestamp"`
				Payload   struct {
					RateLimits *struct {
						Primary   *codexWindow `json:"primary"`
						Secondary *codexWindow `json:"secondary"`
					} `json:"rate_limits"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &ev) != nil || ev.Payload.RateLimits == nil {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, ev.Timestamp)
			if best == nil || at.After(bestAt) {
				rl := ev.Payload.RateLimits
				best = &codexUsageReport{Primary: rl.Primary.window(), Secondary: rl.Secondary.window(), At: ev.Timestamp}
				bestAt = at
			}
		}
		f.Close()
	}
	return best
}
