package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/responses"
)

// M5.6: GET /v1/me/sessions — the calling key's Responses
// sessions, with turns, cumulative tokens and list-price cost.
func TestMeSessions(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	now := time.Now().Unix()
	// Session 1: root + one child (cheap-model $0.5/$1.5 per 1M).
	// Turns: 1M+1M and 500+500 -> list (1,000,500*0.5 +
	// 1,000,500*1.5)/1e6 = $2.001.
	srv.responses.Put(responses.Record{
		ID: "resp-root1", Model: "cheap-model", ProviderID: "prov-a",
		Status: responses.StatusCompleted, CreatedAt: now - 3600,
		TurnInputTokens: 1_000_000, TurnOutputTokens: 1_000_000,
		SessionInputTokens: 1_000_000, SessionOutputTokens: 1_000_000,
		KeyID: s.id,
	})
	srv.responses.Put(responses.Record{
		ID: "resp-child1", Model: "cheap-model", ProviderID: "prov-a",
		Status: responses.StatusCompleted, CreatedAt: now - 1800,
		PreviousID:      "resp-root1",
		TurnInputTokens: 500, TurnOutputTokens: 500,
		SessionInputTokens: 1_000_500, SessionOutputTokens: 1_000_500,
		KeyID: s.id,
	})
	// Session 2 (newer root): a model with no catalog price.
	srv.responses.Put(responses.Record{
		ID: "resp-root2", Model: "unknown-price-model", ProviderID: "prov-a",
		Status: responses.StatusCompleted, CreatedAt: now - 60,
		TurnInputTokens: 100, TurnOutputTokens: 100,
		SessionInputTokens: 100, SessionOutputTokens: 100,
		KeyID: s.id,
	})
	// Another key's session and a keyless (pre-M5.6) session:
	// neither may ever appear for this caller.
	srv.responses.Put(responses.Record{
		ID: "resp-other", Model: "cheap-model",
		Status: responses.StatusCompleted, CreatedAt: now - 30,
		TurnInputTokens: 10, TurnOutputTokens: 10, KeyID: "other-key",
	})
	srv.responses.Put(responses.Record{
		ID: "resp-legacy", Model: "cheap-model",
		Status: responses.StatusCompleted, CreatedAt: now - 10,
		TurnInputTokens: 10, TurnOutputTokens: 10,
	})

	rec := doReq(t, srv, "GET", "/v1/me/sessions", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if out.Object != "list" || out.Total != 2 || len(out.Data) != 2 {
		t.Fatalf("sessions = object %q total %d rows %d, want list/2/2: %+v", out.Object, out.Total, len(out.Data), out.Data)
	}
	// Newest first: root2, then root1.
	newest := out.Data[0]
	if newest.ID != "resp-root2" {
		t.Errorf("first row id = %q, want resp-root2 (newest first)", newest.ID)
	}
	if newest.Turns != 1 || newest.InputTokens != 100 || newest.OutputTokens != 100 {
		t.Errorf("root2 row = turns %d in %d out %d, want 1/100/100", newest.Turns, newest.InputTokens, newest.OutputTokens)
	}
	if newest.ListPriceUSD == nil || *newest.ListPriceUSD != 0 {
		t.Errorf("root2 list_price_usd = %v, want 0 (no repricable turn)", newest.ListPriceUSD)
	}
	if newest.ListPriceComplete || newest.UnpricedCount != 1 {
		t.Errorf("root2 complete=%v unpriced=%d, want false/1", newest.ListPriceComplete, newest.UnpricedCount)
	}
	older := out.Data[1]
	if older.ID != "resp-root1" {
		t.Errorf("second row id = %q, want resp-root1", older.ID)
	}
	if older.Turns != 2 || older.InputTokens != 1_000_500 || older.OutputTokens != 1_000_500 {
		t.Errorf("root1 row = turns %d in %d out %d, want 2/1000500/1000500 (whole tree, branches included)", older.Turns, older.InputTokens, older.OutputTokens)
	}
	if older.ListPriceUSD == nil || *older.ListPriceUSD != 2.001 {
		t.Errorf("root1 list_price_usd = %v, want 2.001", older.ListPriceUSD)
	}
	if !older.ListPriceComplete || older.UnpricedCount != 0 {
		t.Errorf("root1 complete=%v unpriced=%d, want true/0", older.ListPriceComplete, older.UnpricedCount)
	}
	if older.Model != "cheap-model" || older.Provider != "prov-a" || older.Status != responses.StatusCompleted {
		t.Errorf("root1 row = model %q provider %q status %q, want cheap-model/prov-a/completed", older.Model, older.Provider, older.Status)
	}
}

func TestMeSessionsRequiresKey(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, "GET", "/v1/me/sessions", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a key", rec.Code)
	}
}

func TestMeSessionsEmpty(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, "GET", "/v1/me/sessions", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if out.Total != 0 || len(out.Data) != 0 {
		t.Errorf("sessions = total %d rows %d, want 0/0", out.Total, len(out.Data))
	}
}
