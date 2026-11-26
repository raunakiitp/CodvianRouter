package server

// p2_money_gemini_reservation_test.go — audit finding P2-MONEY-001.
//
// The native Gemini surface (:generateContent / :streamGenerateContent)
// used to rely on the advisory checkKeyBudgetForModel gate, which reads
// only SETTLED spend. N concurrent calls could therefore each observe the
// same headroom and jointly exceed the key's cap. It now takes the same
// atomic ledger.ReserveSpend the other spend surfaces take and settles it
// on return.
//
// These tests prove: concurrency cannot overspend; the reservation is
// released on success, on upstream failure and after a stream; and budget
// refusals use the Gemini error envelope, not the OpenAI one.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

const geminiReservationModel = "gem-reserve-model"

// geminiGatedEngine is an Engine that can hold a provider call open (so
// reservations overlap) and can fail on demand.
type geminiGatedEngine struct {
	calls   atomic.Int32
	release chan struct{}
	fail    error
}

func (e *geminiGatedEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.calls.Add(1)
	if e.release != nil {
		<-e.release
	}
	if e.fail != nil {
		return nil, e.fail
	}
	return &provider.ChatResponse{
		Content: "hello", Model: req.Model,
		Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

func (e *geminiGatedEngine) ChatStream(_ context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	e.calls.Add(1)
	if e.release != nil {
		<-e.release
	}
	if e.fail != nil {
		return nil, e.fail
	}
	if err := onDelta("hello"); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{
		Content: "hello", Model: req.Model,
		Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

// geminiReservationBody is a long prompt so the committed estimate is
// comfortably larger than the mock reply's actual cost, which keeps the
// cap arithmetic in the concurrency test exact.
func geminiReservationBody() string {
	return `{"contents":[{"role":"user","parts":[{"text":"` +
		strings.Repeat("hello world ", 200) + `"}]}]}`
}

func geminiReservationServer(t *testing.T, eng Engine) (*Server, *ledger.Ledger, string) {
	t.Helper()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	book := config.NewPriceBook()
	book.Entries[geminiReservationModel] = config.ModelPrice{
		InputUSDPer1M: usdPer1M(3), OutputUSDPer1M: usdPer1M(15),
	}
	srv := newTestServer(t, eng,
		WithLedger(led),
		WithPriceBook(book),
		WithLedgerPrices(ledger.Prices{
			{Model: geminiReservationModel}: {PromptPerToken: 3.0 / 1e6, CompletionPerToken: 15.0 / 1e6},
		}),
	)
	return srv, led, staticKeyID("test-key-123")
}

// geminiPromptEstimate is the estimate the handler itself will commit,
// computed through the same parse + conversion + estimatePromptCost.
func geminiPromptEstimate(t *testing.T, srv *Server, body string) float64 {
	t.Helper()
	in, err := parseGeminiGenerateRequest([]byte(body))
	if err != nil {
		t.Fatalf("parseGeminiGenerateRequest: %v", err)
	}
	req, err := geminiToChatRequest(geminiReservationModel, in)
	if err != nil {
		t.Fatalf("geminiToChatRequest: %v", err)
	}
	est := srv.estimatePromptCost(&req)
	if est <= 0 {
		t.Fatalf("estimatePromptCost = %v, want > 0 (the model must be priced)", est)
	}
	return est
}

func geminiRoute(model string, stream bool) string {
	action := ":generateContent"
	if stream {
		action = ":streamGenerateContent"
	}
	return "/v1beta/models/" + model + action
}

// TestGeminiBudgetReservation_ConcurrentAdmitsAtMostHeadroom is the core
// proof: a cap that fits exactly two reservations admits exactly two
// provider calls out of five concurrent requests.
func TestGeminiBudgetReservation_ConcurrentAdmitsAtMostHeadroom(t *testing.T) {
	release := make(chan struct{})
	eng := &geminiGatedEngine{release: release}
	srv, led, keyID := geminiReservationServer(t, eng)
	body := geminiReservationBody()
	est := geminiPromptEstimate(t, srv, body)

	capUSD := est * 2.5 // two reservations fit, the third is refused
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
			results <- doReq(t, srv, http.MethodPost, geminiRoute(geminiReservationModel, false), body, "test-key-123").Code
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for eng.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a wrongly admitted third lands here
	admitted := eng.calls.Load()
	close(release)
	wg.Wait()
	close(results)

	codes := map[int]int{}
	for c := range results {
		codes[c]++
	}
	if admitted != 2 {
		t.Errorf("concurrent provider calls = %d, want exactly 2 (the cap admits two reservations)", admitted)
	}
	if codes[http.StatusOK] != 2 {
		t.Errorf("status 200 count = %d, want 2", codes[http.StatusOK])
	}
	if codes[http.StatusPaymentRequired] != 3 {
		t.Errorf("status 402 count = %d, want 3 (refused before any provider call)", codes[http.StatusPaymentRequired])
	}
	if r, err := led.ReservedSpend(context.Background(), keyID, time.Now()); err != nil {
		t.Fatalf("ReservedSpend: %v", err)
	} else if r != 0 {
		t.Errorf("reserved after all requests = %v, want 0 (every reservation settled)", r)
	}
	spend, err := led.SpendFor(context.Background(), keyID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	if spend > capUSD+1e-12 {
		t.Errorf("accrued spend %v exceeds cap %v", spend, capUSD)
	}
}

// TestGeminiBudgetReservation_RefusalUsesGeminiEnvelope: a budget refusal
// on /v1beta must be the Gemini error shape, not the OpenAI one.
func TestGeminiBudgetReservation_RefusalUsesGeminiEnvelope(t *testing.T) {
	eng := &geminiGatedEngine{}
	srv, led, keyID := geminiReservationServer(t, eng)
	if err := led.SetKeyBudget(context.Background(), keyID, 1e-9, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}
	rec := doReq(t, srv, http.MethodPost, geminiRoute(geminiReservationModel, false), geminiReservationBody(), "test-key-123")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":402`) {
		t.Fatalf("refusal body is not the Gemini error envelope: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("refusal body looks like the OpenAI envelope: %s", rec.Body.String())
	}
	if eng.calls.Load() != 0 {
		t.Errorf("provider was called %d times despite a refusal before dispatch", eng.calls.Load())
	}
}

// TestGeminiBudgetReservation_SuccessSettles: after a served request the
// reservation is RELEASED, not held. Proven end to end: with a cap that
// fits exactly one reservation, a second sequential request is admitted —
// which it could not be if the first reservation were still committed.
//
// (This harness uses a fake engine, so it cannot assert the spend row:
// usage recording is the engine's contract, asserted at the engine and
// ledger layers. What P2-MONEY-001 changed here is the reservation.)
func TestGeminiBudgetReservation_SuccessSettles(t *testing.T) {
	eng := &geminiGatedEngine{}
	srv, led, keyID := geminiReservationServer(t, eng)
	body := geminiReservationBody()
	est := geminiPromptEstimate(t, srv, body)
	if err := led.SetKeyBudget(context.Background(), keyID, est*1.5, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	first := doReq(t, srv, http.MethodPost, geminiRoute(geminiReservationModel, false), body, "test-key-123")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200: %s", first.Code, first.Body.String())
	}
	if r, _ := led.ReservedSpend(context.Background(), keyID, time.Now()); r != 0 {
		t.Errorf("reserved after a served request = %v, want 0 (settled, not held)", r)
	}

	second := doReq(t, srv, http.MethodPost, geminiRoute(geminiReservationModel, false), body, "test-key-123")
	if second.Code != http.StatusOK {
		t.Fatalf("second sequential status = %d, want 200: the first reservation was not released (phantom hold). Body: %s",
			second.Code, second.Body.String())
	}
	if eng.calls.Load() != 2 {
		t.Errorf("provider calls = %d, want 2", eng.calls.Load())
	}
}

// TestGeminiBudgetReservation_UpstreamFailureRefunds: a provider failure
// must release the reservation rather than leave a phantom hold.
func TestGeminiBudgetReservation_UpstreamFailureRefunds(t *testing.T) {
	eng := &geminiGatedEngine{fail: errors.New("upstream exploded")}
	srv, led, keyID := geminiReservationServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, geminiRoute(geminiReservationModel, false), geminiReservationBody(), "test-key-123")
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200 on a failing upstream: %s", rec.Body.String())
	}
	if r, _ := led.ReservedSpend(context.Background(), keyID, time.Now()); r != 0 {
		t.Errorf("reserved after an upstream failure = %v, want 0 (refunded)", r)
	}
	spend, _ := led.SpendFor(context.Background(), keyID, time.Now().Add(-time.Hour))
	if spend != 0 {
		t.Errorf("accrued spend after a failed call = %v, want 0", spend)
	}
}

// TestGeminiBudgetReservation_StreamSettles: the streaming path settles the
// same reservation once the stream finishes.
func TestGeminiBudgetReservation_StreamSettles(t *testing.T) {
	eng := &geminiGatedEngine{}
	srv, led, keyID := geminiReservationServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, geminiRoute(geminiReservationModel, true), geminiReservationBody(), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if r, _ := led.ReservedSpend(context.Background(), keyID, time.Now()); r != 0 {
		t.Errorf("reserved after a completed stream = %v, want 0", r)
	}
	if eng.calls.Load() != 1 {
		t.Errorf("provider calls = %d, want 1", eng.calls.Load())
	}
}
