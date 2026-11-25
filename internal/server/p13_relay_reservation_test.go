// P13: the /v1/messages relay path takes the SAME atomic budget
// reservation (ledger.ReserveSpend) as the other spend paths (chat,
// Responses, embeddings, images, audio, media-verify). Before this the
// relay relied on a pre-send check that reads only SETTLED spend plus a
// post-hoc usage row, so N concurrent relayed requests could every one
// observe the same headroom and jointly exceed the key's cap. These
// tests prove the reservation closes that window and that settlement /
// refund happens on every exit.
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/relay"
)

// relayReservationSetup builds a ledger-backed /v1/messages server whose
// relayed model is PRICED (so the prompt estimate the reservation uses
// is non-zero), and returns the server, the ledger and the caller key id
// a budget must be set on.
func relayReservationSetup(t *testing.T, mock *anthropicMock, extra ...Option) (*Server, *ledger.Ledger, string) {
	t.Helper()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	book := config.NewPriceBook()
	book.Entries["claude-sonnet-4-5"] = config.ModelPrice{
		InputUSDPer1M: usdPer1M(3), OutputUSDPer1M: usdPer1M(15),
	}
	opts := append([]Option{
		WithLedger(led),
		WithPriceBook(book),
		WithLedgerPrices(ledger.Prices{
			{Model: "claude-sonnet-4-5"}: {PromptPerToken: 3.0 / 1e6, CompletionPerToken: 15.0 / 1e6},
		}),
	}, extra...)
	srv := newMessagesServer(t, &fakeEngine{}, mock, opts...)
	return srv, led, staticKeyID("test-key-123")
}

// relayReservationBody is a relayed request with a long prompt, so the
// prompt estimate is comfortably larger than the tagged reply's actual
// cost — which keeps the cap arithmetic in the concurrency test exact.
func relayReservationBody() string {
	return `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"` +
		strings.Repeat("hello world ", 200) + `"}]}`
}

// relayPromptEstimate is the prompt-cost estimate the server itself will
// commit for this body — computed through the same parse + conversion +
// estimatePromptCost the handler uses, so the test's cap and the
// server's reservation agree by construction.
func relayPromptEstimate(t *testing.T, srv *Server, body string) float64 {
	t.Helper()
	parsed, err := relay.ParseAnthropicDetailed([]byte(body))
	if err != nil {
		t.Fatalf("ParseAnthropicDetailed: %v", err)
	}
	canon := anthropicToChatRequest(parsed.Request)
	est := srv.estimatePromptCost(&canon)
	if est <= 0 {
		t.Fatalf("estimatePromptCost = %v, want > 0 (the model must be priced)", est)
	}
	return est
}

func relayReserved(t *testing.T, led *ledger.Ledger, keyID string) float64 {
	t.Helper()
	reserved, err := led.ReservedSpend(context.Background(), keyID, time.Now())
	if err != nil {
		t.Fatalf("ReservedSpend: %v", err)
	}
	return reserved
}

// TestRelayBudgetReservation_ConcurrentAdmitsAtMostHeadroom is the core
// proof: with a cap that fits exactly two reservations, five concurrent
// relayed requests admit exactly two to the upstream and refuse the
// other three before any provider call.
func TestRelayBudgetReservation_ConcurrentAdmitsAtMostHeadroom(t *testing.T) {
	var admitted int32
	release := make(chan struct{})
	mock := &anthropicMock{
		jsonBody: anthropicJSONReply,
		hook: func(w http.ResponseWriter, _ *http.Request) bool {
			atomic.AddInt32(&admitted, 1)
			<-release // hold the provider call open so reservations overlap
			return false
		},
	}
	srv, led, keyID := relayReservationSetup(t, mock)
	body := relayReservationBody()
	est := relayPromptEstimate(t, srv, body)

	// 2*est <= cap < 3*est: two reservations fit, the third is refused.
	capUSD := est * 2.5
	if err := led.SetKeyBudget(context.Background(), keyID, capUSD, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	const n = 5
	results := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- doReq(t, srv, http.MethodPost, "/v1/messages", body, "test-key-123").Code
		}()
	}

	// Wait until two provider calls have piled up (the admitted set),
	// then release them. If a third ever arrives, the atomicity is
	// broken and the assertion below fails.
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&admitted) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // any (wrong) extra admission lands here
	gotAdmitted := atomic.LoadInt32(&admitted)
	close(release)
	wg.Wait()
	close(results)

	codes := map[int]int{}
	for c := range results {
		codes[c]++
	}

	if gotAdmitted != 2 {
		t.Errorf("concurrent provider calls = %d, want exactly 2 (cap admits two reservations)", gotAdmitted)
	}
	if codes[http.StatusOK] != 2 {
		t.Errorf("status 200 count = %d, want 2 (only the admitted pair is served)", codes[http.StatusOK])
	}
	if codes[http.StatusPaymentRequired] != 3 {
		t.Errorf("status 402 count = %d, want 3 (the rest refused before any provider call)", codes[http.StatusPaymentRequired])
	}
	if r := relayReserved(t, led, keyID); r != 0 {
		t.Errorf("reserved after all requests = %v, want 0 (every reservation settled or refunded)", r)
	}
	spend, err := led.SpendFor(context.Background(), keyID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	if spend > capUSD+1e-12 {
		t.Errorf("accrued spend = %v exceeds cap %v", spend, capUSD)
	}
}

// TestRelayBudgetReservation_FailedUpstreamRefunds: a provider failure
// must release the reservation (refund), not leave the key blocked by a
// phantom hold.
func TestRelayBudgetReservation_FailedUpstreamRefunds(t *testing.T) {
	mock := &anthropicMock{
		status:   http.StatusInternalServerError,
		jsonBody: `{"type":"error","error":{"type":"api_error","message":"boom"}}`,
	}
	srv, led, keyID := relayReservationSetup(t, mock)
	if err := led.SetKeyBudget(context.Background(), keyID, 10.0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", relayReservationBody(), "test-key-123")
	if rec.Code == http.StatusOK {
		t.Fatalf("upstream 500 surfaced as 200: %s", rec.Body.String())
	}
	if r := relayReserved(t, led, keyID); r != 0 {
		t.Errorf("reserved after a failed upstream = %v, want 0 (refunded)", r)
	}
}

// TestRelayBudgetReservation_CancelledRequestRefunds: a client that
// disconnects mid-flight must have its reservation refunded.
func TestRelayBudgetReservation_CancelledRequestRefunds(t *testing.T) {
	mock := &anthropicMock{
		jsonBody: anthropicJSONReply,
		hook: func(w http.ResponseWriter, r *http.Request) bool {
			<-r.Context().Done() // block until the client (and the relay) go away
			return true
		},
	}
	srv, led, keyID := relayReservationSetup(t, mock)
	if err := led.SetKeyBudget(context.Background(), keyID, 10.0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpSrv.URL+"/v1/messages", strings.NewReader(relayReservationBody()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// Wait until the reservation is actually committed, then cancel.
	deadline := time.Now().Add(3 * time.Second)
	for relayReserved(t, led, keyID) <= 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if relayReserved(t, led, keyID) <= 0 {
		t.Fatal("reservation was never committed: the test proved nothing")
	}
	cancel()
	<-done

	deadline = time.Now().Add(3 * time.Second)
	for relayReserved(t, led, keyID) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r := relayReserved(t, led, keyID); r != 0 {
		t.Errorf("reserved after cancellation = %v, want 0 (refunded)", r)
	}
}

// TestRelayBudgetReservation_PartialStreamSettles: a stream that ends
// early still settles its reservation and records an honest row.
func TestRelayBudgetReservation_PartialStreamSettles(t *testing.T) {
	// A stream that dies after message_start (no message_stop).
	const partialSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\"}}\n\n"
	mock := &anthropicMock{streamBody: partialSSE}
	srv, led, keyID := relayReservationSetup(t, mock)
	if err := led.SetKeyBudget(context.Background(), keyID, 10.0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != partialSSE {
		t.Fatal("the relay must forward the upstream stream byte-for-byte")
	}
	if r := relayReserved(t, led, keyID); r != 0 {
		t.Errorf("reserved after a partial stream = %v, want 0 (settled)", r)
	}
	if rows := p13RelayRows(t, led); len(rows) != 1 {
		t.Errorf("ledger rows = %d, want exactly 1", len(rows))
	}
}

// TestRelayBudgetReservation_UncappedKeyTakesNoReservation: an unbudgeted
// key gets no reservation (ReserveSpend is a no-op) and still relays.
func TestRelayBudgetReservation_UncappedKeyTakesNoReservation(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv, led, keyID := relayReservationSetup(t, mock)

	rec := doReq(t, srv, http.MethodPost, "/v1/messages", relayReservationBody(), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s (an unbudgeted key must still relay)", rec.Code, rec.Body.String())
	}
	if r := relayReserved(t, led, keyID); r != 0 {
		t.Errorf("reserved for an uncapped key = %v, want 0", r)
	}
}

// TestRelayBudgetReservation_SettleIsIdempotentAndFailureSafe: settlement
// failing (an unknown or already-settled id) must be a no-op — it can
// never fail a request the client already received, and a double settle
// must not double-credit or error.
func TestRelayBudgetReservation_SettleIsIdempotentAndFailureSafe(t *testing.T) {
	srv, led, keyID := relayReservationSetup(t, &anthropicMock{jsonBody: anthropicJSONReply})
	ctx := context.Background()
	if err := led.SetKeyBudget(ctx, keyID, 10.0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	id, err := led.ReserveSpend(ctx, keyID, []string{"claude-sonnet-4-5"}, srv.modelPriced, 0.01, time.Now())
	if err != nil {
		t.Fatalf("ReserveSpend: %v", err)
	}
	if id == "" {
		t.Fatal("ReserveSpend returned no id for a capped key")
	}
	if r := relayReserved(t, led, keyID); r <= 0 {
		t.Fatalf("reserved = %v, want > 0", r)
	}

	// First settle releases it; a second settle and an unknown id are
	// harmless no-ops.
	srv.settleReservation(id)
	if r := relayReserved(t, led, keyID); r != 0 {
		t.Errorf("reserved after settle = %v, want 0", r)
	}
	srv.settleReservation(id)
	srv.settleReservation("resv-does-not-exist")
	srv.settleReservation("")
	if r := relayReserved(t, led, keyID); r != 0 {
		t.Errorf("reserved after repeated/unknown settles = %v, want 0", r)
	}
}
