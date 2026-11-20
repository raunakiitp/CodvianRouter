package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// poolsFeed is the decoded GET /dashboard/pools payload.
type poolsFeed struct {
	Pools []struct {
		ProviderID     string `json:"provider_id"`
		Name           string `json:"name"`
		KeyFingerprint string `json:"key_fingerprint"`
		MaxIdle        int    `json:"max_idle"`
		Idle           int    `json:"idle"`
		Total          int    `json:"total"`
		Requests       uint64 `json:"requests"`
		Dials          uint64 `json:"dials"`
		Reused         uint64 `json:"reused"`
		Warmed         bool   `json:"warmed"`
	} `json:"pools"`
	Sizing struct {
		Formula           string  `json:"formula"`
		ExpectedRPS       float64 `json:"expected_rps"`
		ExpectedLatencyMs float64 `json:"expected_latency_ms"`
		DerivedSize       int     `json:"derived_size"`
		MinConns          int     `json:"min_conns"`
		MaxConns          int     `json:"max_conns"`
		PerHostOverride   int     `json:"per_host_override"`
	} `json:"sizing"`
}

func getPools(t *testing.T, srv *Server) poolsFeed {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/pools", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/pools: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var feed poolsFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("GET /dashboard/pools: not JSON: %v (body %q)", err, rec.Body.String())
	}
	return feed
}

// TestDashboardPools proves the pools feed reports real pool state:
// one live pool per (provider, key) with the registry name, the key
// fingerprint (never the key), live counters from a real chat, and
// the Little's Law sizing in force.
func TestDashboardPools(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	reg := provider.NewRegistry(nil)
	p, err := reg.Add(mock.URL, "pool-feed-key-123")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "mock-model-a",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg))
	feed := getPools(t, srv)

	if feed.Sizing.Formula != "N = ceil(expected_rps * expected_latency_ms / 1000)" {
		t.Errorf("sizing formula = %q, want the Little's Law formula", feed.Sizing.Formula)
	}
	if feed.Sizing.ExpectedRPS != 20 || feed.Sizing.ExpectedLatencyMs != 1000 || feed.Sizing.DerivedSize != 20 {
		t.Errorf("sizing = %.0f rps / %.0f ms / derived %d, want 20 / 1000 / 20 (defaults)",
			feed.Sizing.ExpectedRPS, feed.Sizing.ExpectedLatencyMs, feed.Sizing.DerivedSize)
	}
	if feed.Sizing.MinConns != 2 || feed.Sizing.MaxConns != 200 || feed.Sizing.PerHostOverride != 0 {
		t.Errorf("sizing bounds = min %d max %d override %d, want 2/200/0",
			feed.Sizing.MinConns, feed.Sizing.MaxConns, feed.Sizing.PerHostOverride)
	}
	if len(feed.Pools) != 1 {
		t.Fatalf("pools = %d rows, want 1", len(feed.Pools))
	}
	row := feed.Pools[0]
	if row.ProviderID != p.ID {
		t.Errorf("pool provider_id = %q, want %q", row.ProviderID, p.ID)
	}
	if row.Name == "" || row.Name == p.ID {
		// The registry derives a display name from the base URL
		// host; it must be a name, not the raw ID.
		t.Errorf("pool name = %q, want the provider display name", row.Name)
	}
	if want := provider.KeyFingerprint("pool-feed-key-123"); row.KeyFingerprint != want {
		t.Errorf("key fingerprint = %q, want %q", row.KeyFingerprint, want)
	}
	if row.MaxIdle != 20 {
		t.Errorf("max_idle = %d, want 20 (default derived size)", row.MaxIdle)
	}
	if row.Requests != 1 || row.Dials != 1 || row.Reused != 0 {
		t.Errorf("counters = requests %d dials %d reused %d, want 1/1/0 after one chat", row.Requests, row.Dials, row.Reused)
	}
	if !row.Warmed || row.Idle == 0 || row.Total == 0 {
		t.Errorf("pool not warm after a chat: warmed=%v idle=%d total=%d", row.Warmed, row.Idle, row.Total)
	}
	// The key itself must never appear in the feed.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/pools", "", "")
	if strings.Contains(rec.Body.String(), "pool-feed-key-123") {
		t.Error("pools feed contains the raw provider key")
	}
}

// TestDashboardPoolsEmpty proves the honest empty state: a registry
// with no providers answers an empty pool list with the default
// sizing, never invented pools.
func TestDashboardPoolsEmpty(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(provider.NewRegistry(nil)))
	feed := getPools(t, srv)
	if len(feed.Pools) != 0 {
		t.Errorf("pools = %d rows, want 0 (no providers)", len(feed.Pools))
	}
	if feed.Sizing.DerivedSize != 20 {
		t.Errorf("sizing derived = %d, want 20 (defaults)", feed.Sizing.DerivedSize)
	}
}

// TestDashboardPoolsNoRegistry proves the feed without any registry
// wired still answers 200 with an empty list and default sizing.
func TestDashboardPoolsNoRegistry(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	feed := getPools(t, srv)
	if len(feed.Pools) != 0 {
		t.Errorf("pools = %d rows, want 0 (no registry)", len(feed.Pools))
	}
	if feed.Sizing.DerivedSize != 20 {
		t.Errorf("sizing derived = %d, want 20 (defaults)", feed.Sizing.DerivedSize)
	}
}

// TestDashboardPoolsServesSection proves the served dashboard
// carries the Connection pools section (nav, view, loader) and no
// Playground anywhere.
func TestDashboardPoolsServesSection(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(provider.NewRegistry(nil)))
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-view="pools"`, `id="view-pools"`, "loadPools",
		`id="poolsBody"`, `id="poolsSizing"`, "Connection pools",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served dashboard is missing %q", want)
		}
	}
	if strings.Contains(body, "Playground") {
		t.Error("served dashboard contains a Playground section")
	}
}
