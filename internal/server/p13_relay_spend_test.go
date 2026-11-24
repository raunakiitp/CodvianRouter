// AUD-SEC-002 regression tests at the request layer: a relayed
// /v1/messages request records exactly one honest ledger row.
package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// p13RelaySpend wires a ledger-backed messages server with a price for
// the relayed model, so a recorded row produces a non-zero spend.
func p13RelaySpend(t *testing.T, mock *anthropicMock) (*Server, *ledger.Ledger, string) {
	t.Helper()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	srv := newMessagesServer(t, &fakeEngine{}, mock,
		WithLedger(led),
		WithLedgerPrices(ledger.Prices{
			{Model: "claude-sonnet-4-5"}: {PromptPerToken: 3.0 / 1e6, CompletionPerToken: 15.0 / 1e6},
		}),
	)
	return srv, led, testCallerID(t, srv)
}

func p13RelayRows(t *testing.T, led *ledger.Ledger) []ledger.UsageEvent {
	t.Helper()
	rows, err := led.RecentEvents(context.Background(), 50, ledger.LogAll)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

const p13RelayMsgBody = `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Hi"}]}`

// TestAUD_SEC_002VerbatimRelayRecordsSpend: the gap. Before the fix the
// verbatim relay recorded NO row, so /v1/messages spend never counted
// toward per-key budgets and quota windows.
func TestAUD_SEC_002VerbatimRelayRecordsSpend(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv, led, keyID := p13RelaySpend(t, mock)

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); got != "verbatim" {
		t.Fatalf("X-Codvian-Relay = %q, want verbatim (test must exercise the relay path)", got)
	}

	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1 (no double charge, no gap)", len(rows))
	}
	r := rows[0]
	if r.GatewayKeyID != keyID {
		t.Errorf("row key = %q, want the authenticated caller %q (per-key budgets)", r.GatewayKeyID, keyID)
	}
	if r.Provider != "Anthropic" || r.Model != "claude-sonnet-4-5" {
		t.Errorf("row provider/model = %q/%q, want Anthropic/claude-sonnet-4-5", r.Provider, r.Model)
	}
	if r.Endpoint != "messages" {
		t.Errorf("row endpoint = %q, want messages", r.Endpoint)
	}
	if r.PromptTokens != 12 || r.CompletionTokens != 4 || r.TotalTokens != 16 {
		t.Errorf("row tokens = %d/%d/%d, want 12/4/16 (from the provider's usage)", r.PromptTokens, r.CompletionTokens, r.TotalTokens)
	}
	if !r.OK {
		t.Error("successful relay recorded OK=false")
	}
	// The spend is now visible per key — the property AUD-SEC-002 said
	// was absent.
	spend, err := led.SpendFor(context.Background(), keyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantSpend := 12*3.0/1e6 + 4*15.0/1e6
	if spend < wantSpend*0.99 || spend > wantSpend*1.01 {
		t.Fatalf("per-key spend = %.8f, want ~%.8f for the relayed request", spend, wantSpend)
	}
}

// TestAUD_SEC_002StreamingRelayRecordsSpend: usage arrives inside the
// SSE stream (message_start / message_delta), which is never buffered.
func TestAUD_SEC_002StreamingRelayRecordsSpend(t *testing.T) {
	mock := &anthropicMock{streamBody: anthropicSSEReply}
	srv, led, keyID := p13RelaySpend(t, mock)

	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != anthropicSSEReply {
		t.Fatal("stream must stay byte-identical to the upstream")
	}
	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.GatewayKeyID != keyID || r.PromptTokens != 12 || r.CompletionTokens != 4 || !r.OK {
		t.Fatalf("stream row = %+v, want key %q, 12/4, OK", r, keyID)
	}
}

// TestAUD_SEC_002FailedRelayDoesNotRecordSpend: a provider error is an
// attempt, never a successful spend.
func TestAUD_SEC_002FailedRelayDoesNotRecordSpend(t *testing.T) {
	mock := &anthropicMock{status: http.StatusInternalServerError, jsonBody: `{"type":"error","error":{"type":"api_error","message":"boom"}}`}
	srv, led, keyID := p13RelaySpend(t, mock)

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code == http.StatusOK {
		t.Fatalf("upstream 500 must not surface as 200 (got %d: %s)", rec.Code, rec.Body.String())
	}
	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.OK {
		t.Error("failed relay recorded OK=true")
	}
	if r.PromptTokens != 0 || r.CompletionTokens != 0 {
		t.Errorf("failed relay recorded tokens %d/%d, want 0/0 (never a fabricated success)", r.PromptTokens, r.CompletionTokens)
	}
	spend, err := led.SpendFor(context.Background(), keyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if spend != 0 {
		t.Fatalf("per-key spend after a failed relay = %.8f, want 0", spend)
	}
}

// TestAUD_SEC_002MissingUsageIsNeverRecordedAsZeroCost replaced an
// earlier assertion in this file that pinned exactly the behaviour the
// review rejected: a 2xx with no reported usage was recorded with zero
// tokens and zero cost, which is what let a budgeted key keep spending
// through /v1/messages without its accrued spend moving.
//
// What must still hold is that nothing is FABRICATED: the row must not
// claim the provider reported a usage split. It is explicitly marked as
// a local estimate instead (ledger.LocalEstimate), so the dashboard and
// the estimate-vs-reported drift column can tell the two apart, and the
// charge is a conservative over-estimate rather than an invented exact
// number. The full budget-enforcement proof lives in
// p13_unknown_usage_budget_test.go.
func TestAUD_SEC_002MissingUsageIsNeverRecordedAsZeroCost(t *testing.T) {
	mock := &anthropicMock{jsonBody: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"no usage here"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn"}`}
	srv, led, keyID := p13RelaySpend(t, mock)

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(rows))
	}
	if !rows[0].OK {
		t.Error("2xx relay recorded OK=false")
	}
	if rows[0].LocalEstimatePromptTokens != rows[0].PromptTokens || rows[0].LocalEstimatePromptTokens == 0 {
		t.Error("a row with no provider usage must record that its prompt count was locally measured, not provider-reported")
	}
	if rows[0].PromptTokens <= 0 || rows[0].CompletionTokens <= 0 {
		t.Errorf("tokens = %d/%d, want a positive conservative estimate: unknown usage is not zero usage",
			rows[0].PromptTokens, rows[0].CompletionTokens)
	}
	spend, err := led.SpendFor(context.Background(), keyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if spend <= 0 {
		t.Fatalf("spend = %.8f, want a non-zero conservative charge for a relayed response whose usage was unreported", spend)
	}
}

// TestAUD_SEC_002NoLedgerStillRelays: the accounting is best-effort and
// must never break the relay.
func TestAUD_SEC_002NoLedgerStillRelays(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock)
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
