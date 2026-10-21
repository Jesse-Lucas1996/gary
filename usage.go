package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Claude subscription usage for the dashboard header: the same numbers Claude
// Code's /usage shows, read with the OAuth login the hub user's claude already
// has. Read-only and best-effort — no credentials file, or an API hiccup, just
// means the header shows nothing.

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
}

// usageCache keeps the upstream call to one per usageTTL however many
// dashboards are polling.
type usageCache struct {
	mu   sync.Mutex
	at   time.Time
	rep  *usageReport
	err  error
	http *http.Client
}

const usageTTL = 60 * time.Second

func (c *usageCache) get() (*usageReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < usageTTL {
		return c.rep, c.err
	}
	c.rep, c.err = fetchUsage(c.http)
	c.at = time.Now()
	return c.rep, c.err
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
