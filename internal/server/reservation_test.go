package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M5.8: committed-vs-settled spend at the HTTP layer — parallel
// chat requests against one small cap must not all pass. The
// reservation commits each admitted request's prompt estimate
// before the engine is called, so the (K+1)th parallel request
// is refused with 402 and never reaches a provider.

// blockingEngine holds every admitted Chat call until the test
// releases it, so the parallel requests genuinely overlap —
// without the hold, a fast fake engine would settle each
// reservation before the next request reserved, and the race
// would never happen.
type blockingEngine struct {
	*fakeEngine
	entered chan struct{}
	release chan struct{}
	calls   int
	mu      sync.Mutex
}

func (b *blockingEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	return b.fakeEngine.Chat(ctx, req)
}

func (b *blockingEngine) chatCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func TestReserveSpendParallelChat(t *testing.T) {
	s, _ := keyStatusSetup(t)
	// A long prompt, so the estimate dominates any fixed
	// additions the handler makes to the request.
	body := `{"model":"priced-model","messages":[{"role":"user","content":"` + strings.Repeat("hello world ", 20000) + `"}]}`
	// The estimate the reserve will compute, measured with the
	// same function on the same request shape.
	var decoded provider.ChatRequest
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode test body: %v", err)
	}
	estimateTokens := estimatePromptTokens(&decoded)
	if estimateTokens <= 0 {
		t.Fatalf("prompt estimate = %d, want > 0", estimateTokens)
	}
	estimateUSD := float64(estimateTokens) * (10.0 / 1e6) // priced-model input price
	// A cap of 2.5 estimates: exactly 2 parallel requests fit.
	if err := s.led.SetKeyBudget(context.Background(), s.id, estimateUSD*2.5, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	eng := &blockingEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}},
		entered:    make(chan struct{}, 5),
		release:    make(chan struct{}),
	}
	srv := newTestServer(t, eng,
		WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}))
	// Fire 5 parallel requests.
	type result struct {
		code int
	}
	results := make(chan result, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
			results <- result{code: rec.Code}
		}()
	}
	// Collect the refusals FIRST: a refused request finishes
	// without ever reaching the engine, while an admitted one
	// stays blocked inside it. Waiting for 2 engine entries
	// and releasing immediately would be wrong — the remaining
	// requests may not have reserved yet, and the release would
	// settle the first reservations before they check. So the
	// release happens only once the gate has refused 3 (or a
	// 5s bound expires, in which case the counts below fail
	// the test with the real numbers instead of hanging).
	refusedBeforeRelease := 0
	timeout := time.After(5 * time.Second)
collect:
	for refusedBeforeRelease < 3 {
		select {
		case r := <-results:
			if r.code == http.StatusPaymentRequired {
				refusedBeforeRelease++
			} else {
				t.Errorf("a request finished before the release with status %d (want only 402s before release)", r.code)
			}
		case <-timeout:
			break collect
		}
	}
	// The admitted requests are still in flight (their
	// reservations still committed): release them now.
	close(eng.release)
	wg.Wait()
	close(results)
	ok, refused := 0, refusedBeforeRelease
	for r := range results {
		switch r.code {
		case http.StatusOK:
			ok++
		case http.StatusPaymentRequired:
			refused++
		default:
			t.Errorf("unexpected status %d (want 200 or 402)", r.code)
		}
	}
	if ok != 2 || refused != 3 {
		t.Errorf("parallel chat: ok=%d refused=%d, want 2/3 (cap 2.5 estimates)", ok, refused)
	}
	if got := eng.chatCalls(); got != 2 {
		t.Errorf("engine chat calls = %d, want 2 (refused requests must never reach a provider)", got)
	}
	// Every reservation settled once the handlers returned.
	if reserved, _ := s.led.ReservedSpend(context.Background(), s.id, time.Now()); reserved != 0 {
		t.Errorf("reserved after all requests finished = %v, want 0", reserved)
	}
}

// TestCumulativeBudgetRaceParallelChat (M5.14): the cumulative
// (legacy) per-key cap, with NO rolling key-budget row at all, is
// enforced against parallel requests. Same shape as
// TestReserveSpendParallelChat, but the cap is SetBudget (the
// budgets table) instead of SetKeyBudget: exactly 2 of 5 parallel
// requests fit a 2.5-estimate cap, the other 3 get 402, and the
// engine sees exactly 2 calls. Before M5.14 the reservation was a
// no-op without a key-budget row, so all 5 passed the settled-only
// pre-send check and the engine ran 5 times.
func TestCumulativeBudgetRaceParallelChat(t *testing.T) {
	s, _ := keyStatusSetup(t)
	s.led.FailClosed = true
	body := `{"model":"priced-model","messages":[{"role":"user","content":"` + strings.Repeat("hello world ", 20000) + `"}]}`
	var decoded provider.ChatRequest
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode test body: %v", err)
	}
	estimateTokens := estimatePromptTokens(&decoded)
	if estimateTokens <= 0 {
		t.Fatalf("prompt estimate = %d, want > 0", estimateTokens)
	}
	estimateUSD := float64(estimateTokens) * (10.0 / 1e6) // priced-model input price
	// Cumulative-only cap: 2.5 estimates, no key_budgets row.
	if err := s.led.SetBudget(context.Background(), s.id, estimateUSD*2.5, "daily"); err != nil {
		t.Fatal(err)
	}
	eng := &blockingEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}},
		entered:    make(chan struct{}, 5),
		release:    make(chan struct{}),
	}
	srv := newTestServer(t, eng,
		WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}))
	type result struct {
		code int
	}
	results := make(chan result, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
			results <- result{code: rec.Code}
		}()
	}
	// Collect the refusals FIRST (same ordering argument as
	// TestReserveSpendParallelChat: a refused request finishes
	// without the engine, an admitted one stays blocked inside
	// it, and releasing early would settle reservations before
	// the late requests have checked).
	refusedBeforeRelease := 0
	timeout := time.After(5 * time.Second)
collect:
	for refusedBeforeRelease < 3 {
		select {
		case r := <-results:
			if r.code == http.StatusPaymentRequired {
				refusedBeforeRelease++
			} else {
				t.Errorf("a request finished before the release with status %d (want only 402s before release)", r.code)
			}
		case <-timeout:
			break collect
		}
	}
	close(eng.release)
	wg.Wait()
	close(results)
	ok, refused := 0, refusedBeforeRelease
	for r := range results {
		switch r.code {
		case http.StatusOK:
			ok++
		case http.StatusPaymentRequired:
			refused++
		default:
			t.Errorf("unexpected status %d (want 200 or 402)", r.code)
		}
	}
	if ok != 2 || refused != 3 {
		t.Errorf("parallel chat (cumulative cap): ok=%d refused=%d, want 2/3 (cap 2.5 estimates)", ok, refused)
	}
	if got := eng.chatCalls(); got != 2 {
		t.Errorf("engine chat calls = %d, want 2 (refused requests must never reach a provider)", got)
	}
	if reserved, _ := s.led.ReservedSpend(context.Background(), s.id, time.Now()); reserved != 0 {
		t.Errorf("reserved after all requests finished = %v, want 0", reserved)
	}
}
