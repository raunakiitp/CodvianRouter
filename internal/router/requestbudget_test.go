package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Per-request USD budget (CHECKLIST 155): estimate math, the
// 402 fail-fast with zero provider calls, the strict mid-chain
// stop, and the non-strict pass-through.
//
// Prices in these tests are a stub PriceFunc, not the operator's
// book: "cheap" costs $10/1M in + $30/1M out, "pricey" costs
// $1000/1M in + $3000/1M out. "hello world" counts as 9 local
// tokens (3 message text + 3 per-message framing + 3 reply
// priming, CHECKLIST 229-230), so its cheap prompt estimate is
// 9 × $10/1M = $0.00009 and its pricey prompt estimate is
// 9 × $1000/1M = $0.009.

func budgetTestPriceFunc(providerID, model string, promptTokens, completionTokens int) float64 {
	var in, out float64
	switch model {
	case "cheap":
		in, out = 10, 30
	case "pricey":
		in, out = 1000, 3000
	}
	return float64(promptTokens)*in/1e6 + float64(completionTokens)*out/1e6
}

func budgetTestReq(budget float64, strict bool) provider.ChatRequest {
	b := budget
	return provider.ChatRequest{
		Model:        "cheap",
		Messages:     []provider.Message{{Role: "user", Content: "hello world"}},
		BudgetUSD:    &b,
		BudgetStrict: strict,
	}
}

// Estimate math: the gate prices the prompt-only estimate and
// blocks only in strict mode when spent + estimate exceeds the
// budget.
func TestBudgetGateEstimateMath(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	eng := NewEngine(map[string]provider.Client{}, nil, cfg)
	cheap := Attempt{ProviderID: "cheap-prov", Model: "cheap"}

	// $0.00009 estimate vs a $0.0001 budget: passes, and the
	// estimate is reported back even when not blocked.
	req := budgetTestReq(0.0001, true)
	if blocked, est := eng.budgetGate(req, cheap, 0); blocked || est != 0.00009 {
		t.Errorf("budgetGate(under) = blocked=%v est=%v, want false/0.00009", blocked, est)
	}
	// Same estimate vs a $0.00001 budget, strict: blocked.
	req = budgetTestReq(0.00001, true)
	if blocked, est := eng.budgetGate(req, cheap, 0); !blocked || est != 0.00009 {
		t.Errorf("budgetGate(strict over) = blocked=%v est=%v, want true/0.00009", blocked, est)
	}
	// Same estimate vs a $0.00001 budget, NON-strict: never
	// blocked — the chain continues and the overage is recorded
	// by the server instead.
	req = budgetTestReq(0.00001, false)
	if blocked, est := eng.budgetGate(req, cheap, 0); blocked || est != 0.00009 {
		t.Errorf("budgetGate(non-strict over) = blocked=%v est=%v, want false/0.00009", blocked, est)
	}
	// Spent + estimate: $0.00009 already spent + $0.00009
	// estimate vs a $0.0001 budget, strict: blocked.
	req = budgetTestReq(0.0001, true)
	if blocked, _ := eng.budgetGate(req, cheap, 0.00009); !blocked {
		t.Errorf("budgetGate(spent+estimate over) = not blocked, want blocked")
	}
	// No budget on the request: never blocked.
	req = provider.ChatRequest{Model: "cheap", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	if blocked, est := eng.budgetGate(req, cheap, 0); blocked || est != 0 {
		t.Errorf("budgetGate(no budget) = blocked=%v est=%v, want false/0", blocked, est)
	}
	// No PriceFunc wired: nothing can be priced, so the gate
	// never blocks — unpriced requests proceed.
	noPrice := NewEngine(map[string]provider.Client{}, nil, testConfig())
	if blocked, est := noPrice.budgetGate(budgetTestReq(0.00001, true), cheap, 0); blocked || est != 0 {
		t.Errorf("budgetGate(no PriceFunc) = blocked=%v est=%v, want false/0", blocked, est)
	}
	// Unpriced model (no price in the stub): estimate $0, never
	// blocked even in strict mode.
	req = budgetTestReq(0.00001, true)
	if blocked, est := eng.budgetGate(req, Attempt{ProviderID: "x", Model: "unknown-model"}, 0); blocked || est != 0 {
		t.Errorf("budgetGate(unpriced model) = blocked=%v est=%v, want false/0", blocked, est)
	}
}

// attemptCost prices reported usage and refuses to price what
// cannot be priced honestly: no usage, or a local-estimate
// fallback (streaming — the split is unknown).
func TestAttemptCost(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	eng := NewEngine(map[string]provider.Client{}, nil, cfg)
	att := Attempt{ProviderID: "cheap-prov", Model: "cheap"}
	// 100 prompt + 50 completion on cheap: 100×10/1M + 50×30/1M.
	got := eng.attemptCost(att, &provider.ChatResponse{Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50}})
	if got != 0.0025 {
		t.Errorf("attemptCost = %v, want 0.0025", got)
	}
	if got := eng.attemptCost(att, nil); got != 0 {
		t.Errorf("attemptCost(nil resp) = %v, want 0", got)
	}
	if got := eng.attemptCost(att, &provider.ChatResponse{}); got != 0 {
		t.Errorf("attemptCost(no usage) = %v, want 0", got)
	}
	if got := eng.attemptCost(att, &provider.ChatResponse{Usage: provider.Usage{PromptTokens: 42, TotalTokens: 42, LocalEstimate: true}}); got != 0 {
		t.Errorf("attemptCost(local estimate) = %v, want 0 (split unknown)", got)
	}
}

// Fail-fast: a strict budget the prompt estimate alone exceeds
// refuses BEFORE any provider is dialled — zero calls on every
// target in the chain, and the trace records the skip.
func TestRequestBudgetFailFastZeroCalls(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	cheap := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "cheap says hi"}, nil
	}}
	pricey := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "pricey says hi"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"cheap-prov": cheap, "pricey-prov": pricey}, nil, cfg)
	chain := []Attempt{{ProviderID: "cheap-prov", Model: "cheap"}, {ProviderID: "pricey-prov", Model: "pricey"}}
	// $0.00009 estimate vs a $0.00001 strict budget.
	_, trace, err := eng.ChatWithChain(context.Background(), chain, budgetTestReq(0.00001, true))
	if err == nil || !IsRequestBudgetExceeded(err) {
		t.Fatalf("err = %v, want a RequestBudgetExceededError", err)
	}
	var be *RequestBudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("err is not a RequestBudgetExceededError: %v", err)
	}
	if be.BudgetUSD != 0.00001 || be.NextEstimateUSD != 0.00009 || be.Provider != "cheap-prov" {
		t.Errorf("error = %+v, want budget 0.00001 / estimate 0.00009 / provider cheap-prov", be)
	}
	if cheap.chatCalls() != 0 || pricey.chatCalls() != 0 {
		t.Errorf("provider calls = cheap %d + pricey %d, want 0 + 0 (refused before dialling)", cheap.chatCalls(), pricey.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "skipped_budget" {
		t.Fatalf("trace = %+v, want exactly one skipped_budget attempt", trace)
	}
	if !strings.Contains(be.Error(), "per-request budget exceeded") {
		t.Errorf("error message = %q, want it to name the per-request budget", be.Error())
	}
}

// Strict stop mid-chain: the cheap first target fails (generic
// upstream error — retryable, not terminal), and the pricey
// fallback's prompt estimate ($0.003) exceeds the $0.001 strict
// budget, so the chain stops BEFORE dialling it. The cheap
// target was called exactly once; the pricey target never.
func TestRequestBudgetStrictStopsMidChain(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	cheap := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, errors.New("upstream 500")
	}}
	pricey := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "pricey says hi"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"cheap-prov": cheap, "pricey-prov": pricey}, nil, cfg)
	chain := []Attempt{{ProviderID: "cheap-prov", Model: "cheap"}, {ProviderID: "pricey-prov", Model: "pricey"}}
	// Cheap estimate $0.00009 fits the $0.001 budget; pricey
	// estimate $0.009 does not.
	_, trace, err := eng.ChatWithChain(context.Background(), chain, budgetTestReq(0.001, true))
	if err == nil || !IsRequestBudgetExceeded(err) {
		t.Fatalf("err = %v, want a RequestBudgetExceededError", err)
	}
	if cheap.chatCalls() != 1 {
		t.Errorf("cheap calls = %d, want 1 (it failed, then the chain stopped)", cheap.chatCalls())
	}
	if pricey.chatCalls() != 0 {
		t.Errorf("pricey calls = %d, want 0 (never dialled in strict mode)", pricey.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2 (failed cheap + skipped_budget pricey)", len(trace.Attempts))
	}
	if trace.Attempts[1].Outcome != "skipped_budget" {
		t.Errorf("trace[1].Outcome = %q, want skipped_budget", trace.Attempts[1].Outcome)
	}
	var be *RequestBudgetExceededError
	if errors.As(err, &be) && be.Provider != "pricey-prov" {
		t.Errorf("blocked provider = %q, want pricey-prov", be.Provider)
	}
}

// Non-strict mid-chain: the same failing-cheap-then-pricey chain
// with the same $0.001 budget proceeds to the pricey fallback
// and serves its response — the overage is the server's to
// record, not the engine's to block.
func TestRequestBudgetNonStrictProceedsMidChain(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	cheap := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, errors.New("upstream 500")
	}}
	pricey := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "pricey says hi"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"cheap-prov": cheap, "pricey-prov": pricey}, nil, cfg)
	chain := []Attempt{{ProviderID: "cheap-prov", Model: "cheap"}, {ProviderID: "pricey-prov", Model: "pricey"}}
	resp, _, err := eng.ChatWithChain(context.Background(), chain, budgetTestReq(0.001, false))
	if err != nil {
		t.Fatalf("err = %v, want nil (non-strict proceeds)", err)
	}
	if resp == nil || resp.Content != "pricey says hi" {
		t.Fatalf("resp = %+v, want the pricey fallback's response", resp)
	}
	if cheap.chatCalls() != 1 || pricey.chatCalls() != 1 {
		t.Errorf("calls = cheap %d + pricey %d, want 1 + 1", cheap.chatCalls(), pricey.chatCalls())
	}
}

// Strict stop on streams: the cheap stream fails before commit
// and the pricey fallback is budget-blocked, so the channel ends
// with the typed budget error and the pricey stream is never
// opened.
func TestRequestBudgetStrictStopsMidChainStreaming(t *testing.T) {
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	cheap := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		return nil, errors.New("upstream 500")
	}}
	pricey := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("pricey"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "pricey"}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"cheap-prov": cheap, "pricey-prov": pricey}, nil, cfg)
	chain := []Attempt{{ProviderID: "cheap-prov", Model: "cheap"}, {ProviderID: "pricey-prov", Model: "pricey"}}
	ch, err := eng.ChatStreamWithChain(context.Background(), chain, budgetTestReq(0.001, true))
	if err != nil {
		t.Fatalf("ChatStreamWithChain err = %v, want nil (errors arrive on the channel)", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
		if ev.Delta != "" {
			t.Errorf("stream delivered delta %q before the budget stop, want none", ev.Delta)
		}
	}
	if streamErr == nil || !IsRequestBudgetExceeded(streamErr) {
		t.Fatalf("stream error = %v, want a RequestBudgetExceededError", streamErr)
	}
	// M4.8 early-retry: the cheap target's untyped stream error
	// is a transport blip, re-opened up to 3 times before the
	// budget gate stops the chain (pricey still never opened).
	if cheap.streamCalls() != 4 {
		t.Errorf("cheap streams = %d, want 4", cheap.streamCalls())
	}
	if pricey.streamCalls() != 0 {
		t.Errorf("pricey streams = %d, want 0 (never opened in strict mode)", pricey.streamCalls())
	}
}

// TestRequestBudgetStrictHedgeCountsFirstAttempt (M5.14): a hedged
// pair is TWO attempts in flight at once, so the strict gate must
// count the first attempt's estimate when deciding whether the
// second may launch. "hello world" is 9 tokens; each "cheap"
// attempt estimates $0.00009. A $0.00013 budget fits one attempt
// (0 + 0.00009 <= 0.00013) but not the pair (0.00009 + 0.00009 =
// $0.00018 > 0.00013): the hedge must never fire, the slow first
// attempt serves alone, and the second provider is never called.
// Before the fix the hedge gate checked the second attempt against
// spentUSD (0 at idx 0) alone, so the pair launched and jointly
// spent $0.00018 against a $0.00013 budget.
func TestRequestBudgetStrictHedgeCountsFirstAttempt(t *testing.T) {
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "cheap"}, {ProviderID: "b", Model: "cheap"}}
	start := time.Now()
	resp, _, err := eng.ChatWithChain(context.Background(), chain, budgetTestReq(0.00013, true))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "slow" {
		t.Fatalf("content = %q, want slow (hedge must not fire)", resp.Content)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 250ms (the slow first attempt ran alone)", elapsed)
	}
	if b.chatCalls() != 0 {
		t.Fatalf("b calls = %d, want 0 (hedge never launched)", b.chatCalls())
	}
}

// TestRequestBudgetStrictHedgeAllowedWhenJointEstimateFits: the
// control — a $0.0002 budget fits the joint $0.00018 estimate, so
// the hedge fires and the fast second attempt wins.
func TestRequestBudgetStrictHedgeAllowedWhenJointEstimateFits(t *testing.T) {
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "cheap"}, {ProviderID: "b", Model: "cheap"}}
	start := time.Now()
	resp, _, err := eng.ChatWithChain(context.Background(), chain, budgetTestReq(0.0002, true))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "fast" {
		t.Fatalf("content = %q, want fast (hedge winner)", resp.Content)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("elapsed = %v, hedge should have returned in well under 250ms", elapsed)
	}
	if b.chatCalls() != 1 {
		t.Fatalf("b calls = %d, want 1", b.chatCalls())
	}
}

// TestRequestBudgetStrictHedgeCountsFirstAttemptStreaming: the
// same M5.14 fix in the stream loop — a $0.00013 strict budget
// must keep the hedged second stream from ever opening.
func TestRequestBudgetStrictHedgeCountsFirstAttemptStreaming(t *testing.T) {
	slow := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		time.Sleep(300 * time.Millisecond)
		if err := onDelta("slow"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "slow"}, nil
	}}
	fast := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("fast"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "fast"}, nil
	}}
	cfg := testConfig()
	cfg.PriceFunc = budgetTestPriceFunc
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": slow, "b": fast}, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "cheap"}, {ProviderID: "b", Model: "cheap"}}
	ch, err := eng.ChatStreamWithChain(context.Background(), chain, budgetTestReq(0.00013, true))
	if err != nil {
		t.Fatalf("ChatStreamWithChain err = %v", err)
	}
	var deltas []string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream error = %v, want none (slow serves alone)", ev.Err)
		}
		if ev.Delta != "" {
			deltas = append(deltas, ev.Delta)
		}
	}
	if len(deltas) != 1 || deltas[0] != "slow" {
		t.Fatalf("deltas = %v, want [slow] only (hedge must not fire)", deltas)
	}
	if fast.streamCalls() != 0 {
		t.Fatalf("b streams = %d, want 0 (hedge never launched)", fast.streamCalls())
	}
}
