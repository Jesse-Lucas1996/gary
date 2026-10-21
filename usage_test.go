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
