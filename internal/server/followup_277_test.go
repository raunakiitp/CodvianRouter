// Sprint-14 follow-ups batch (CHECKLIST 277): pre-body admission
// on the relay + embeddings surfaces (b) and the tier-naming
// refusal on /v1/messages + /v1/responses (c).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ---------------------------------------------------------------------------
// (b) Pre-body admission for the relay + embeddings handlers.
// ---------------------------------------------------------------------------

// TestPreBodyAdmissionRefusesOverBudgetKeyOnMessages proves a key
// already over its budget is refused on /v1/messages BEFORE the
// body is read: the same request with a malformed body answers
// 402 (not 400) — the handler never decoded it — and the engine
// is never called.
func TestPreBodyAdmissionRefusesOverBudgetKeyOnMessages(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.01, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.01 cap
	rec := doReq(t, srv, "POST", "/v1/messages", `{"model":`, s.raw)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("garbage body + over-budget key: status = %d, want 402 (admission before decode): %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", s.eng.chatCalls.Load())
	}
	feed := doReq(t, srv, "GET", "/dashboard/guardrails", "", "")
	var f guardrailsFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode guardrails feed: %v", err)
	}
	if f.EarlyRefusals < 1 {
		t.Fatalf("early_refusals = %d, want >= 1", f.EarlyRefusals)
	}
	// Control: the same garbage body under an uncapped key is a
	// 400 — the body IS read and decoded when admission passes.
	s2 := newKeyBudgetSetup(t)
	srv2 := s2.server(t)
	if err := s2.led.SetKeyBudget(context.Background(), s2.id, 10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, srv2, "POST", "/v1/messages", `{"model":`, s2.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage body + uncapped key: status = %d, want 400 (body read normally): %s", rec.Code, rec.Body.String())
	}
}

// TestPreBodyAdmissionRefusesOverBudgetKeyOnEmbeddings is the
// same proof on /v1/embeddings: a malformed body under an
// over-budget key answers 402, never reaching the decode.
func TestPreBodyAdmissionRefusesOverBudgetKeyOnEmbeddings(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.01, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.01 cap
	rec := doReq(t, srv, "POST", "/v1/embeddings", `{"model":`, s.raw)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("garbage body + over-budget key: status = %d, want 402 (admission before decode): %s", rec.Code, rec.Body.String())
	}
	// Control: uncapped key + garbage body is a 400. The engine
	// must implement Embed for the control: the handler's
	// engine-capability check (503) runs before decode, so a
	// fakeEngine without Embed would answer 503 instead of
	// proving the body was read.
	s2 := newKeyBudgetSetup(t)
	eng2 := &embedFakeEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}}}
	srv2 := newTestServer(t, eng2, WithConfig(s2.cfg), WithLedger(s2.led), WithPriceBook(s2.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}))
	if err := s2.led.SetKeyBudget(context.Background(), s2.id, 10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, srv2, "POST", "/v1/embeddings", `{"model":`, s2.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage body + uncapped key: status = %d, want 400 (body read normally): %s", rec.Code, rec.Body.String())
	}
}

// TestPreBodyAdmissionRefusesOverTrafficKeyOnMessages proves the
// requests-per-minute peek also runs before the body is read on
// /v1/messages: after one admitted request consumes the single
// RPM slot, a second request with a malformed body answers 429.
func TestPreBodyAdmissionRefusesOverTrafficKeyOnMessages(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("rpm-one", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	key.RequestsPerMinute = 1
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}}}
	srv := newTestServer(t, eng, WithConfig(cfg), WithAutoRouter(testIntentRouter()))
	body := `{"model":"model-a","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	rec := doReq(t, srv, "POST", "/v1/messages", body, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", eng.chatCalls.Load())
	}
	rec = doReq(t, srv, "POST", "/v1/messages", `{"model":`, raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("garbage body + over-traffic key: status = %d, want 429 (admission before decode): %s", rec.Code, rec.Body.String())
	}
	if eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1: the refused request must never reach the engine", eng.chatCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// (c) Intent tiers on /v1/messages + /v1/responses name the tier.
// ---------------------------------------------------------------------------

func decodeErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var errBody struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return errBody.Error.Message
}

// TestIntentMessagesUnmappedTierRefused proves an unmapped intent
// tier on /v1/messages answers 400 naming the tier — the same
// refusal chat completions gives — instead of the generic
// empty-chain error the engine returns for the unresolved
// virtual model. Before the fix the request fell through to the
// translate path and the engine answered with its generic
// no-capable-provider error.
func TestIntentMessagesUnmappedTierRefused(t *testing.T) {
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}}}
	srv := newTestServer(t, eng, WithAutoRouter(testIntentRouter()))
	rec := doReq(t, srv, "POST", "/v1/messages",
		`{"model":"intent/best","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unmapped intent messages: status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	msg := decodeErrorMessage(t, rec.Body.Bytes())
	if !strings.Contains(msg, `"best"`) || !strings.Contains(msg, "not mapped") {
		t.Fatalf("body %q must name the tier and say it is not mapped", msg)
	}
	if eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refusal happens before any provider is dialled", eng.chatCalls.Load())
	}
}

// TestIntentMessagesMappedTierServed proves a mapped tier still
// flows through the translate path on /v1/messages and is served
// (200), with the intent headers a chat request would carry.
func TestIntentMessagesMappedTierServed(t *testing.T) {
	router := testIntentRouter()
	dec := autorouter.Decision{ID: "dec-test-1", VirtualModel: "intent/fast", IntentTier: autorouter.IntentFast, Effort: autorouter.EffortLow, Model: "model-a"}
	eng := &intentTestEngine{
		Engine: &fakeEngine{chatResp: provider.ChatResponse{Model: "model-a", Content: "hi"}},
		dec:    dec,
	}
	srv := newTestServer(t, eng, WithAutoRouter(router))
	rec := doReq(t, srv, "POST", "/v1/messages",
		`{"model":"intent/fast","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("mapped intent messages: status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// TestIntentResponsesUnmappedTierRefused is the same proof on
// /v1/responses: an unmapped tier answers 400 naming the tier.
func TestIntentResponsesUnmappedTierRefused(t *testing.T) {
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}}}
	srv := newTestServer(t, eng, WithAutoRouter(testIntentRouter()))
	rec := doReq(t, srv, "POST", "/v1/responses",
		`{"model":"intent/best","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unmapped intent responses: status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	msg := decodeErrorMessage(t, rec.Body.Bytes())
	if !strings.Contains(msg, `"best"`) || !strings.Contains(msg, "not mapped") {
		t.Fatalf("body %q must name the tier and say it is not mapped", msg)
	}
	if eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", eng.chatCalls.Load())
	}
}

// TestIntentResponsesMappedTierServed proves a mapped tier is
// served (200) on /v1/responses.
func TestIntentResponsesMappedTierServed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}}, WithAutoRouter(testIntentRouter()))
	rec := doReq(t, srv, "POST", "/v1/responses",
		`{"model":"intent/fast","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("mapped intent responses: status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}
