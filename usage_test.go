package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageReadsOAuthLogin(t *testing.T) {
	var gotAuth, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.Write([]byte(`{"five_hour":{"utilization":4.0,"resets_at":"2026-01-01T10:00:00Z"},
			"seven_day":{"utilization":21.0,"resets_at":"2026-01-03T12:00:00Z"},"seven_day_opus":null}`))
	}))
	defer upstream.Close()

	creds := filepath.Join(t.TempDir(), ".credentials.json")
	os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"fake-token","subscriptionType":"max"}}`), 0o600)
	oldURL, oldPath := usageURL, usageCredsPath
	usageURL, usageCredsPath = upstream.URL, func() string { return creds }
	defer func() { usageURL, usageCredsPath = oldURL, oldPath }()
	tempUsageSave(t)

	mux := http.NewServeMux()
	registerUsage(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage", nil))

	var rep usageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer fake-token" || gotBeta == "" {
		t.Fatalf("upstream headers: auth=%q beta=%q", gotAuth, gotBeta)
	}
	if rep.Plan != "max" || rep.FiveHour == nil || rep.FiveHour.Percent != 4 || rep.SevenDay == nil || rep.SevenDay.Percent != 21 {
		t.Fatalf("report: %s", rec.Body.String())
	}
}

func TestUsageWithoutLoginReportsError(t *testing.T) {
	oldPath := usageCredsPath
	usageCredsPath = func() string { return filepath.Join(t.TempDir(), "missing.json") }
	defer func() { usageCredsPath = oldPath }()
	tempUsageSave(t)

	mux := http.NewServeMux()
	registerUsage(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage", nil))
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["error"] == nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// tempUsageSave keeps a test's saved reading out of the real data dir.
func tempUsageSave(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "usage.json")
	old := usageSavePath
	usageSavePath = func() string { return p }
	t.Cleanup(func() { usageSavePath = old })
	return p
}

// A 429 after a good reading serves that reading marked stale, waits out
// Retry-After before asking again, and a fresh cache (a hub restart) still has
// the saved reading.
func TestUsageRateLimitServesLastReading(t *testing.T) {
	calls, limited := 0, false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if limited {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"five_hour":{"utilization":13,"resets_at":"2026-01-01T10:00:00Z"}}`))
	}))
	defer upstream.Close()
	creds := filepath.Join(t.TempDir(), ".credentials.json")
	os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"t","subscriptionType":"max"}}`), 0o600)
	oldURL, oldPath := usageURL, usageCredsPath
	usageURL, usageCredsPath = upstream.URL, func() string { return creds }
	defer func() { usageURL, usageCredsPath = oldURL, oldPath }()
	tempUsageSave(t)

	c := &usageCache{http: upstream.Client()}
	rep, err := c.get()
	if err != nil || rep.Stale || rep.FiveHour.Percent != 13 || rep.At == "" {
		t.Fatalf("first: %+v %v", rep, err)
	}
	limited = true
	c.at = c.at.Add(-2 * usageTTL) // the minute has passed
	rep, err = c.get()
	if err != nil || !rep.Stale || rep.FiveHour.Percent != 13 {
		t.Fatalf("after 429: %+v %v", rep, err)
	}
	if c.wait != 600*1e9 {
		t.Fatalf("backoff %v, want Retry-After 600s", c.wait)
	}
	c.at = c.at.Add(-2 * usageTTL) // past the minute, not past Retry-After
	c.get()
	if calls != 2 {
		t.Fatalf("asked upstream %d times during Retry-After", calls)
	}
	fresh := &usageCache{http: upstream.Client()}
	rep, err = fresh.get()
	if err != nil || !rep.Stale || rep.FiveHour.Percent != 13 {
		t.Fatalf("restart with saved reading: %+v %v", rep, err)
	}
}

// No test talks to Anthropic or touches the real saved reading: guest_test hits
// /api/usage through the full handler, which used the box's real login (adding
// to the very 429s this cache works around) and, with the cache, wrote
// ~/.local/share/gary/usage.json.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "gary-usage-test")
	usageSavePath = func() string { return filepath.Join(dir, "usage.json") }
	usageCredsPath = func() string { return filepath.Join(dir, "no-credentials.json") }
	usageURL = "http://127.0.0.1:1/usage-disabled-in-tests"
	codexSessionsDir = func() string { return filepath.Join(dir, "codex-sessions") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Codex usage comes from the newest rate_limits snapshot in the session logs.
func TestCodexUsageFromSessionLogs(t *testing.T) {
	dir := t.TempDir()
	old := codexSessionsDir
	codexSessionsDir = func() string { return dir }
	defer func() { codexSessionsDir = old }()
	day := filepath.Join(dir, "2026", "10", "28")
	os.MkdirAll(day, 0o755)
	os.WriteFile(filepath.Join(day, "rollout-a.jsonl"), []byte(
		`{"timestamp":"2026-10-28T01:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":40,"window_minutes":300,"resets_at":1793600000},"secondary":{"used_percent":70,"window_minutes":10080,"resets_at":1793626730}}}}`+"\n"+
			`{"timestamp":"2026-10-28T02:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":87,"window_minutes":10080,"resets_at":1793626730},"secondary":null}}}`+"\n"), 0o644)
	got := readCodexUsage()
	if got == nil || got.Primary == nil || got.Primary.Percent != 87 || got.Primary.Label != "7d" || got.Secondary != nil ||
		got.At != "2026-10-28T02:00:00Z" {
		t.Fatalf("codex usage: %+v", got)
	}
	codexSessionsDir = func() string { return filepath.Join(dir, "missing") }
	if readCodexUsage() != nil {
		t.Fatal("no logs should mean no codex usage")
	}
}
