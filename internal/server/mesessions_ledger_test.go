package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

// AK-3: GET /v1/me/sessions serves the ledger-backed agent
// session rollup alongside the Responses chains. The rollup rows
// are written by RecordUsage (same transaction as the usage row),
// so seeding usage through the test ledger is the live write
// path, not a fixture.
func TestMeSessionsLedgerRollup(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	ctx := context.Background()
	prices := ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}
	// One priced chat row: 1000 + 1000 tokens at $10/$30 per 1M = $0.04.
	if _, err := s.led.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: "priced-model",
		PromptTokens: 1000, CompletionTokens: 1000, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	// One Responses session, older than the usage row.
	now := time.Now().Unix()
	srv.responses.Put(responses.Record{
		ID: "resp-root1", Model: "cheap-model", ProviderID: "prov-a",
		Status: responses.StatusCompleted, CreatedAt: now - 3600,
		TurnInputTokens: 10, TurnOutputTokens: 10,
		SessionInputTokens: 10, SessionOutputTokens: 10,
		KeyID: s.id,
	})
	rec := doReq(t, srv, "GET", "/v1/me/sessions", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if out.Total != 2 || len(out.Data) != 2 {
		t.Fatalf("sessions = total %d rows %d, want 2/2 (ledger + responses): %+v", out.Total, len(out.Data), out.Data)
	}
	// Newest first: the ledger row (just recorded) before the older root.
	led := out.Data[0]
	if led.Source != "ledger" || !strings.HasPrefix(led.ID, "as_") {
		t.Errorf("first row = source %q id %q, want ledger/as_*", led.Source, led.ID)
	}
	if led.Requests != 1 || led.Turns != 1 || led.Errors != 0 {
		t.Errorf("ledger row = requests %d turns %d errors %d, want 1/1/0", led.Requests, led.Turns, led.Errors)
	}
	if led.InputTokens != 1000 || led.OutputTokens != 1000 {
		t.Errorf("ledger row tokens = in %d out %d, want 1000/1000", led.InputTokens, led.OutputTokens)
	}
	if led.CostUSD == nil || *led.CostUSD != 0.04 {
		t.Errorf("ledger row cost_usd = %v, want 0.04 (record-time cost)", led.CostUSD)
	}
	if led.ListPriceUSD != nil || led.ListPriceComplete {
		t.Errorf("ledger row list_price_usd = %v complete=%v, want nil/false (no reprice)", led.ListPriceUSD, led.ListPriceComplete)
	}
	if led.Status != "completed" {
		t.Errorf("ledger row status = %q, want completed", led.Status)
	}
	if led.Model != "priced-model" || led.Provider != "p" {
		t.Errorf("ledger row = model %q provider %q, want priced-model/p", led.Model, led.Provider)
	}
	resp := out.Data[1]
	if resp.Source != "responses" || resp.ID != "resp-root1" {
		t.Errorf("second row = source %q id %q, want responses/resp-root1", resp.Source, resp.ID)
	}
}

// A failed attempt counts as a request AND an error; a session
// whose every request failed reports status "failed", a mixed
// session reports "completed" with the error count visible.
func TestMeSessionsLedgerErrorStatus(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	ctx := context.Background()
	prices := ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}
	// One failed row, then one OK row, same key + window: one session.
	for _, ok := range []bool{false, true} {
		if _, err := s.led.RecordUsage(ctx, ledger.Usage{
			GatewayKeyID: s.id, Provider: "p", Model: "priced-model",
			PromptTokens: 10, CompletionTokens: 10, SplitKnown: true, OK: ok,
		}, prices); err != nil {
			t.Fatalf("seed usage ok=%v: %v", ok, err)
		}
	}
	rec := doReq(t, srv, "GET", "/v1/me/sessions", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if out.Total != 1 || len(out.Data) != 1 {
		t.Fatalf("sessions = total %d rows %d, want 1/1 (one idle-window session): %+v", out.Total, len(out.Data), out.Data)
	}
	row := out.Data[0]
	if row.Requests != 2 || row.Errors != 1 {
		t.Errorf("row = requests %d errors %d, want 2/1", row.Requests, row.Errors)
	}
	if row.Status != "completed" {
		t.Errorf("row status = %q, want completed (mixed session)", row.Status)
	}
}

// Another key's ledger sessions are never listed for this
// caller — the rollup read is key-scoped by construction.
func TestMeSessionsLedgerOtherKeyHidden(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	ctx := context.Background()
	prices := ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}
	if _, err := s.led.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID: "other-key", Provider: "p", Model: "priced-model",
		PromptTokens: 10, CompletionTokens: 10, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("seed other-key usage: %v", err)
	}
	rec := doReq(t, srv, "GET", "/v1/me/sessions", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if out.Total != 0 || len(out.Data) != 0 {
		t.Errorf("sessions = total %d rows %d, want 0/0 (other key's rollup hidden)", out.Total, len(out.Data))
	}
}

// The detail endpoint resolves a ledger session id, and 404s
// another key's ledger session (never leaks its existence).
func TestMeSessionsLedgerDetail(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	ctx := context.Background()
	prices := ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}
	if _, err := s.led.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: "priced-model",
		PromptTokens: 1000, CompletionTokens: 1000, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	if _, err := s.led.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID: "other-key", Provider: "p", Model: "priced-model",
		PromptTokens: 10, CompletionTokens: 10, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("seed other-key usage: %v", err)
	}
	other, err := s.led.AgentSessions(ctx, "other-key", 10)
	if err != nil || len(other) != 1 {
		t.Fatalf("other-key sessions = %d, err %v, want 1", len(other), err)
	}
	// List to learn our own ledger id, then fetch its detail.
	rec := doReq(t, srv, "GET", "/v1/me/sessions", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].Source != "ledger" {
		t.Fatalf("list = %+v, want 1 ledger row", out.Data)
	}
	id := out.Data[0].ID
	rec = doReq(t, srv, "GET", "/v1/me/sessions/"+id, "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var det meSessionDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if det.ID != id || det.Source != "ledger" || det.Requests != 1 {
		t.Errorf("detail = id %q source %q requests %d, want %q/ledger/1", det.ID, det.Source, det.Requests, id)
	}
	if det.TurnsDetail == nil || len(det.TurnsDetail) != 0 || det.TurnsDetailTruncated {
		t.Errorf("detail turns = %+v truncated=%v, want empty/untruncated (no per-turn records)", det.TurnsDetail, det.TurnsDetailTruncated)
	}
	rec = doReq(t, srv, "GET", "/v1/me/sessions/"+other[0].ID, "", s.raw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("other key's session detail status = %d, want 404", rec.Code)
	}
	rec = doReq(t, srv, "GET", "/v1/me/sessions/as_nonexistent", "", s.raw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown ledger id detail status = %d, want 404", rec.Code)
	}
}
