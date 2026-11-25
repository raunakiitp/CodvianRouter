// AUD-SEC-002 review blocker: unknown usage must never be treated as
// zero cost.
//
// The defect: a successful relayed /v1/messages response whose provider
// reported NO usage was recorded as a zero-token, zero-cost ledger row.
// Because the per-key budget gate reads accrued spend from that same
// ledger, a budgeted key could keep spending through the Anthropic
// endpoint while its accrued spend never moved — a budget bypass.
package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// p13NoUsageReply is a valid Anthropic message with NO usage object.
const p13NoUsageReply = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"no usage reported here"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn"}`

// TestAUD_SEC_002MissingUsageCannotBypassTheKeyBudget reproduces the
// bypass: a relayed request that reports no usage must still move the
// key's accrued spend, and once the cap is reached the gate must refuse.
func TestAUD_SEC_002MissingUsageCannotBypassTheKeyBudget(t *testing.T) {
	mock := &anthropicMock{jsonBody: p13NoUsageReply}
	srv, led, keyID := p13RelaySpend(t, mock)
	ctx := context.Background()

	// The budget the relay gate actually enforces (ledger.CheckKeyBudget
	// reads the key-budget table, not the older budgets table): a cap
	// generous enough for one estimated call.
	if err := led.SetKeyBudget(ctx, keyID, 1.00, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.LocalEstimate {
		t.Error("a priced row must not be flagged as an estimate-only row: LocalEstimate has its own documented meaning (no split, no cost)")
	}
	if r.LocalEstimatePromptTokens == 0 || r.LocalEstimatePromptTokens != r.PromptTokens {
		t.Errorf("LocalEstimatePromptTokens = %d, want %d: the row must record that its prompt count was LOCALLY measured, not provider-reported", r.LocalEstimatePromptTokens, r.PromptTokens)
	}
	if !r.SplitKnown {
		t.Error("the estimate must be recorded as a known split, or the ledger will not price it (the bypass returns)")
	}
	if r.CostUSD <= 0 {
		t.Errorf("cost = %.8f, want a conservative charge: unknown usage is not zero usage", r.CostUSD)
	}
	if r.PromptTokens <= 0 {
		t.Errorf("prompt tokens = %d, want a conservative estimate > 0 (unknown usage is not zero usage)", r.PromptTokens)
	}
	if r.CompletionTokens <= 0 {
		t.Errorf("completion tokens = %d, want a conservative estimate > 0 for the delivered response", r.CompletionTokens)
	}
	spend, err := led.SpendFor(ctx, keyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if spend <= 0 {
		t.Fatalf("accrued spend = %.8f after a relayed request with no reported usage: the budget bypass is still open", spend)
	}

	// The cap is now BELOW the already-accrued spend: the next relayed
	// request must be refused by the pre-call gate, exactly as it would
	// be for provider-reported spend. This is the assertion that fails
	// against the old behaviour: with a zero-cost row the accrued spend
	// stayed 0, the cap was never reached, and the request returned 200.
	if err := led.SetKeyBudget(ctx, keyID, spend/2, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code == http.StatusOK {
		t.Fatalf("a request over the key budget was allowed: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusPaymentRequired && rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget refusal status = %d, want 402/429 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAUD_SEC_002StreamingWithoutUsageIsChargedForWhatWasDelivered: a
// stream that never reports usage (or is cut short) must still charge
// for the bytes the client received.
func TestAUD_SEC_002StreamingWithoutUsageIsChargedForWhatWasDelivered(t *testing.T) {
	// SSE with content but no usage object anywhere.
	const noUsageSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello there, this is a longer answer to charge for\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	mock := &anthropicMock{streamBody: noUsageSSE}
	srv, led, keyID := p13RelaySpend(t, mock)
	ctx := context.Background()
	if err := led.SetKeyBudget(ctx, keyID, 1.00, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}

	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != noUsageSSE {
		t.Fatal("the stream must stay byte-identical to the upstream")
	}
	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(rows))
	}
	if rows[0].LocalEstimatePromptTokens == 0 || rows[0].CompletionTokens <= 0 || rows[0].CostUSD <= 0 {
		t.Fatalf("stream row = %+v, want a priced, locally-measured estimate for the delivered bytes", rows[0])
	}
	spend, err := led.SpendFor(ctx, keyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if spend <= 0 {
		t.Fatal("a stream with no reported usage accrued zero spend")
	}
}

// TestAUD_SEC_002CompleteUsageIsChargedExactlyOnce: providers that do
// report usage keep the previous, exact behaviour — no estimate, no
// double charge.
func TestAUD_SEC_002CompleteUsageIsChargedExactlyOnce(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv, led, keyID := p13RelaySpend(t, mock)
	ctx := context.Background()

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.LocalEstimatePromptTokens != 0 {
		t.Error("a provider-reported row must not claim a locally measured prompt count")
	}
	if r.PromptTokens != 12 || r.CompletionTokens != 4 {
		t.Errorf("tokens = %d/%d, want the provider's exact 12/4", r.PromptTokens, r.CompletionTokens)
	}
	wantSpend := 12*3.0/1e6 + 4*15.0/1e6
	spend, err := led.SpendFor(ctx, keyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if spend < wantSpend*0.99 || spend > wantSpend*1.01 {
		t.Fatalf("spend = %.8f, want ~%.8f (exact, once)", spend, wantSpend)
	}

	// A second identical call adds exactly one more row and one more
	// charge — never a duplicate of the first.
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("second status %d: %s", rec.Code, rec.Body.String())
	}
	rows = p13RelayRows(t, led)
	if len(rows) != 2 {
		t.Fatalf("rows after two calls = %d, want 2 (one per request, no double charge)", len(rows))
	}
	spend2, err := led.SpendFor(ctx, keyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if spend2 < wantSpend*1.98 || spend2 > wantSpend*2.02 {
		t.Fatalf("spend after two calls = %.8f, want ~%.8f", spend2, 2*wantSpend)
	}
}

// TestAUD_SEC_002EstimateIsConservativeAndVisible documents the estimate
// shape: it over-counts rather than under-counts, and the row says so.
func TestAUD_SEC_002EstimateIsConservativeAndVisible(t *testing.T) {
	mock := &anthropicMock{jsonBody: p13NoUsageReply}
	srv, led, _ := p13RelaySpend(t, mock)

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rows := p13RelayRows(t, led)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	// The body is a few hundred bytes and the reply ~100 bytes; a
	// conservative estimate must be at least the response size / 8 and
	// must not be absurdly large either.
	if r.CompletionTokens < len(p13NoUsageReply)/8 {
		t.Errorf("completion estimate %d is below the response size / 8 (%d): not conservative enough", r.CompletionTokens, len(p13NoUsageReply)/8)
	}
	if r.TotalTokens != r.PromptTokens+r.CompletionTokens {
		t.Errorf("total %d != prompt+completion %d+%d", r.TotalTokens, r.PromptTokens, r.CompletionTokens)
	}
	if !r.OK {
		t.Error("a 2xx relay was recorded as not-OK")
	}
	if r.Endpoint != "messages" {
		t.Errorf("endpoint = %q, want messages", r.Endpoint)
	}
	// The provider is still named, so an operator can see WHO under-reported.
	if !strings.Contains(r.Provider, "Anthropic") {
		t.Errorf("provider = %q, want the resolved provider name", r.Provider)
	}
}

// TestAUD_SEC_002UnbudgetedTrafficStillWorks: an unbudgeted key keeps
// working (the fix charges an estimate, it does not refuse traffic).
func TestAUD_SEC_002UnbudgetedTrafficStillWorks(t *testing.T) {
	mock := &anthropicMock{jsonBody: p13NoUsageReply}
	srv, led, _ := p13RelaySpend(t, mock)
	for i := 0; i < 3; i++ {
		rec := doReq(t, srv, http.MethodPost, "/v1/messages", p13RelayMsgBody, "test-key-123")
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d status %d: %s (no budget is configured, so traffic must not be refused)", i+1, rec.Code, rec.Body.String())
		}
	}
	if rows := p13RelayRows(t, led); len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
}
