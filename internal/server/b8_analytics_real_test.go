package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// seedDayEvents imports usage rows at fixed past days, so the
// calendar-day analytics can be checked against known data.
func seedDayEvents(t *testing.T, led *ledger.Ledger, now time.Time) {
	t.Helper()
	ev := func(ts time.Time, cost float64) ledger.UsageEvent {
		return ledger.UsageEvent{TS: ts, GatewayKeyID: "k1", Provider: "mock", Model: "m",
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, SplitKnown: true, OK: true, CostUSD: cost, LatencyMS: 5}
	}
	today := now.UTC().Truncate(24 * time.Hour)
	events := []ledger.UsageEvent{
		ev(today.Add(1*time.Minute), 0.01),
		ev(today.Add(2*time.Minute), 0.02),
		ev(today.AddDate(0, 0, -8).Add(3*time.Hour), 0.04),
	}
	if _, _, err := led.ImportEvents(context.Background(), events); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestAnalyticsHeatmapAndWeeklyUseLedger(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	now := time.Now()
	seedDayEvents(t, led, now)
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))

	rec := doReqPeer(t, srv, "GET", "/api/analytics/heatmap", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("heatmap status %d: %s", rec.Code, rec.Body.String())
	}
	var hm struct {
		Buckets []struct {
			Date     string  `json:"date"`
			Requests int     `json:"requests"`
			CostUSD  float64 `json:"cost_usd"`
			Level    int     `json:"level"`
		} `json:"buckets"`
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hm); err != nil {
		t.Fatal(err)
	}
	if !hm.Available || len(hm.Buckets) != 365 {
		t.Fatalf("available=%v buckets=%d, want true/365", hm.Available, len(hm.Buckets))
	}
	last := hm.Buckets[364]
	if last.Date != now.UTC().Format("2006-01-02") || last.Requests != 2 || last.Level != 4 {
		t.Errorf("today bucket = %+v, want today's date, 2 requests, level 4", last)
	}
	eight := hm.Buckets[364-8]
	if eight.Requests != 1 || eight.Date != now.UTC().AddDate(0, 0, -8).Format("2006-01-02") {
		t.Errorf("8-days-ago bucket = %+v, want 1 request", eight)
	}
	total := 0
	for _, b := range hm.Buckets {
		total += b.Requests
	}
	if total != 3 {
		t.Errorf("heatmap total requests = %d, want 3", total)
	}

	rec = doReqPeer(t, srv, "GET", "/api/analytics/weekly-pattern", "", "test-key-123", loopbackPeer)
	var wp struct {
		Pattern []struct {
			Day      string `json:"day"`
			Requests int    `json:"requests"`
		} `json:"pattern"`
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wp); err != nil {
		t.Fatal(err)
	}
	if !wp.Available || len(wp.Pattern) != 7 {
		t.Fatalf("weekly pattern = %s", rec.Body.String())
	}
	todayName := now.UTC().Weekday().String()
	eightName := now.UTC().AddDate(0, 0, -8).Weekday().String()
	sum := 0
	for _, p := range wp.Pattern {
		sum += p.Requests
		if p.Day == todayName && p.Requests < 2 {
			t.Errorf("%s requests = %d, want >= 2", p.Day, p.Requests)
		}
		if p.Day == eightName && p.Requests < 1 {
			t.Errorf("%s requests = %d, want >= 1", p.Day, p.Requests)
		}
	}
	if sum != 3 {
		t.Errorf("weekly total = %d, want 3", sum)
	}
}

func TestAnalyticsFeedsWithoutLedgerAreHonest(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, p := range []string{"/api/analytics/heatmap", "/api/analytics/weekly-pattern", "/api/analytics/search"} {
		rec := doReqPeer(t, srv, "GET", p, "", "test-key-123", loopbackPeer)
		var res map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if rec.Code != http.StatusOK || res["available"] != false {
			t.Errorf("%s: status %d body %v, want 200 available=false", p, rec.Code, res)
		}
	}
}

func TestScoringInspectorReportsLiveWeightsOnly(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReqPeer(t, srv, "GET", "/api/analytics/scoring-inspector", "", "test-key-123", loopbackPeer)
	var res struct {
		Factors []map[string]any `json:"factors"`
		Values  bool             `json:"values_available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Values {
		t.Error("values_available = true, want false (no live values are read)")
	}
	sum := 0.0
	for _, f := range res.Factors {
		if _, ok := f["value"]; ok {
			t.Errorf("factor %v carries a value; values must not be invented", f)
		}
		sum += f["weight"].(float64)
	}
	if sum < 0.999 || sum > 1.001 {
		t.Errorf("weights sum to %v, want 1", sum)
	}
}
