package router

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// longPrompt builds one user message of at least targetTokens local
// tokens (measured with the same counter the engine uses), by
// repeating a fixed sentence. Tokenization is not linear across
// repetitions — a repeated sentence can merge tokens at the joins
// — so the prompt is measured after every build and extended until
// it really reaches the target, never assumed from the unit count.
func longPrompt(t *testing.T, model string, targetTokens int) provider.ChatRequest {
	t.Helper()
	unit := "The quick brown fox jumps over the lazy dog while the river runs cold and clear. "
	one, err := tokencount.CountPromptFull(model, provider.ChatRequest{Messages: []provider.Message{{Role: RoleUser, Content: unit}}})
	if err != nil {
		t.Fatalf("count unit prompt: %v", err)
	}
	if one.Tokens <= 0 {
		t.Fatalf("unit prompt counted %d tokens", one.Tokens)
	}
	reps := targetTokens/one.Tokens + 1
	for {
		req := provider.ChatRequest{Messages: []provider.Message{{Role: RoleUser, Content: strings.Repeat(unit, reps)}}}
		got, err := tokencount.CountPromptFull(model, req)
		if err != nil {
			t.Fatalf("count long prompt: %v", err)
		}
		if got.Tokens >= targetTokens {
			return req
		}
		// Top up by the measured deficit, not by another full
		// target — the first build already gets close.
		reps += (targetTokens-got.Tokens)/one.Tokens + 1
	}
}

// promptTokens measures a request's local prompt count the way the
// engine does — the full count (message text + tool definitions +
// per-message framing, CHECKLIST 229-230), never the text-only
// count.
func promptTokens(t *testing.T, req provider.ChatRequest) int {
	t.Helper()
	c, err := tokencount.CountPromptFull(req.Model, req)
	if err != nil {
		t.Fatalf("count prompt: %v", err)
	}
	return c.Tokens
}

// windowMap builds a Config.ContextWindow hook from a
// (provider, model) -> window map. A missing entry is an unknown
// window — never a zero window.
func windowMap(windows map[string]int) func(string, string) (int, bool) {
	return func(providerID, model string) (int, bool) {
		w, ok := windows[providerID+"\x00"+model]
		return w, ok
	}
}

// TestContextFitMathBoundaries pins the fit arithmetic: required =
// prompt + reserve, where the reserve is max_tokens when set, the
// configured reserve otherwise, and DefaultContextReserveTokens
// (4096) otherwise. required == window fits; required == window+1
// does not. An unknown window never refuses.
func TestContextFitMathBoundaries(t *testing.T) {
	req := longPrompt(t, "small-model", 100)
	req.Model = "small-model"
	p := promptTokens(t, req)

	cases := []struct {
		name       string
		windows    map[string]int
		maxTokens  int
		reserveCfg int
		hookNil    bool
		wantFit    bool
	}{
		{"exact-fit-default-reserve", map[string]int{"small\x00small-model": p + DefaultContextReserveTokens}, 0, 0, false, true},
		{"one-over-default-reserve", map[string]int{"small\x00small-model": p + DefaultContextReserveTokens - 1}, 0, 0, false, false},
		{"exact-fit-max-tokens-reserve", map[string]int{"small\x00small-model": p + 100}, 100, 0, false, true},
		{"one-over-max-tokens-reserve", map[string]int{"small\x00small-model": p + 99}, 100, 0, false, false},
		{"exact-fit-configured-reserve", map[string]int{"small\x00small-model": p + 50}, 0, 50, false, true},
		{"one-over-configured-reserve", map[string]int{"small\x00small-model": p + 49}, 0, 50, false, false},
		{"unknown-window-fits", map[string]int{}, 0, 0, false, true},
		{"nil-hook-fits", nil, 0, 0, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			small := okClient("small says hi")
			cfg := testConfig()
			if !tc.hookNil {
				cfg.ContextWindow = windowMap(tc.windows)
			}
			cfg.ContextReserveTokens = tc.reserveCfg
			eng := NewEngine(map[string]provider.Client{"small": small}, nil, cfg)
			r := req
			r.MaxTokens = tc.maxTokens
			resp, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, r)
			if tc.wantFit {
				if err != nil {
					t.Fatalf("ChatWithChain: %v, want success", err)
				}
				if resp == nil || resp.Content != "small says hi" {
					t.Fatalf("resp = %+v, want small's reply", resp)
				}
				if small.chatCalls() != 1 {
					t.Fatalf("small calls = %d, want 1", small.chatCalls())
				}
			} else {
				var tooLarge *ErrContextTooLarge
				if !errors.As(err, &tooLarge) {
					t.Fatalf("err = %v, want *ErrContextTooLarge", err)
				}
				if small.chatCalls() != 0 {
					t.Fatalf("small calls = %d, want 0 (too-small model never dialled)", small.chatCalls())
				}
				wantReserve := DefaultContextReserveTokens
				if tc.reserveCfg > 0 {
					wantReserve = tc.reserveCfg
				}
				if tc.maxTokens > 0 {
					wantReserve = tc.maxTokens
				}
				if tooLarge.Required != p+wantReserve {
					t.Fatalf("required = %d, want %d", tooLarge.Required, p+wantReserve)
				}
				if dec, ok := eng.LastContextFit(); !ok || dec.ChainUsed != "none" || dec.Error == "" {
					t.Fatalf("last fit decision = %+v ok=%v, want chain_used none with an error", dec, ok)
				}
			}
		})
	}
}

// TestContextFitJumpsToFallbackChain is the gate test (CHECKLIST
// 159): an oversize prompt goes straight to the long-context model
// via the dedicated fallback chain — the too-small model is never
// dialled, never retried, and the trace says so.
func TestContextFitJumpsToFallbackChain(t *testing.T) {
	small := okClient("small says hi")
	long := okClient("long says hi")
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{
		"small\x00small-model": 8192,
		"long\x00long-model":   1048576,
	})
	cfg.ContextFallbackChain = func(model string) []Attempt {
		return []Attempt{{ProviderID: "long", Model: "long-model"}}
	}
	eng := NewEngine(map[string]provider.Client{"small": small, "long": long}, nil, cfg)

	// ~6,000 prompt tokens: required ~10,096 > 8,192 (small) but
	// far under 1,048,576 (long).
	req := longPrompt(t, "small-model", 6000)
	req.Model = "small-model"
	resp, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, req)
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp == nil || resp.Content != "long says hi" {
		t.Fatalf("resp = %+v, want the long-context model's reply", resp)
	}
	if small.chatCalls() != 0 {
		t.Fatalf("small calls = %d, want 0 (never dialled)", small.chatCalls())
	}
	if long.chatCalls() != 1 {
		t.Fatalf("long calls = %d, want 1", long.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want [skipped_context_fit, success]", trace)
	}
	if trace.Attempts[0].Outcome != "skipped_context_fit" || trace.Attempts[0].Provider != "small" {
		t.Fatalf("trace[0] = %+v, want small skipped_context_fit", trace.Attempts[0])
	}
	if trace.Attempts[1].Outcome != "success" || trace.Attempts[1].Provider != "long" {
		t.Fatalf("trace[1] = %+v, want long success", trace.Attempts[1])
	}
	dec, ok := eng.LastContextFit()
	if !ok {
		t.Fatal("no fit decision recorded")
	}
	if dec.ChainUsed != "context_fallback" {
		t.Fatalf("chain_used = %q, want context_fallback", dec.ChainUsed)
	}
	if dec.RequiredTokens != dec.PromptTokens+dec.ReserveTokens {
		t.Fatalf("required %d != prompt %d + reserve %d", dec.RequiredTokens, dec.PromptTokens, dec.ReserveTokens)
	}
	if len(dec.Entries) != 2 || dec.Entries[0].Fits || !dec.Entries[1].Fits {
		t.Fatalf("entries = %+v, want [small too-small, long fits]", dec.Entries)
	}
	if dec.LargestWindow != 1048576 {
		t.Fatalf("largest window = %d, want 1048576", dec.LargestWindow)
	}
}

// TestContextFitNoFitFailsHonestly proves the no-fit path: a prompt
// bigger than every configured window fails pre-call with the
// required-vs-largest numbers, zero provider calls, and no ledger
// row.
func TestContextFitNoFitFailsHonestly(t *testing.T) {
	small := okClient("small says hi")
	long := okClient("long says hi")
	led := &fakeLedger{}
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{
		"small\x00small-model": 8192,
		"long\x00long-model":   16384,
	})
	cfg.ContextFallbackChain = func(model string) []Attempt {
		return []Attempt{{ProviderID: "long", Model: "long-model"}}
	}
	eng := NewEngine(map[string]provider.Client{"small": small, "long": long}, led, cfg)

	// ~20,000 prompt tokens: required ~24,096 > 16,384 (largest).
	req := longPrompt(t, "small-model", 20000)
	req.Model = "small-model"
	p := promptTokens(t, req)
	_, trace, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, req)
	var tooLarge *ErrContextTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *ErrContextTooLarge", err)
	}
	if tooLarge.Required != p+DefaultContextReserveTokens {
		t.Fatalf("required = %d, want %d", tooLarge.Required, p+DefaultContextReserveTokens)
	}
	if tooLarge.LargestWindow != 16384 {
		t.Fatalf("largest window = %d, want 16384", tooLarge.LargestWindow)
	}
	if !strings.Contains(err.Error(), "largest available context window is 16384") || !strings.Contains(err.Error(), strconv.Itoa(tooLarge.Required)) {
		t.Fatalf("error message %q does not name required (%d) vs largest window (16384)", err.Error(), tooLarge.Required)
	}
	if small.chatCalls() != 0 || long.chatCalls() != 0 {
		t.Fatalf("provider calls = small %d, long %d, want 0/0", small.chatCalls(), long.chatCalls())
	}
	if led.count() != 0 {
		t.Fatalf("ledger rows = %d, want 0 (no provider was called)", led.count())
	}
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want 2 skipped_context_fit rows", trace)
	}
	for i, at := range trace.Attempts {
		if at.Outcome != "skipped_context_fit" {
			t.Fatalf("trace[%d] = %+v, want skipped_context_fit", i, at)
		}
	}
	dec, ok := eng.LastContextFit()
	if !ok || dec.ChainUsed != "none" || dec.Error == "" {
		t.Fatalf("last fit decision = %+v ok=%v, want chain_used none with an error", dec, ok)
	}
}

// TestContextFitStreamingJumpsToFallbackChain is the streaming half
// of the gate: an oversize streamed prompt goes straight to the
// long-context model — the small model's stream is never opened.
func TestContextFitStreamingJumpsToFallbackChain(t *testing.T) {
	small := streamClient([]string{"small says hi"}, nil)
	long := streamClient([]string{"long says hi"}, nil)
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{
		"small\x00small-model": 8192,
		"long\x00long-model":   1048576,
	})
	cfg.ContextFallbackChain = func(model string) []Attempt {
		return []Attempt{{ProviderID: "long", Model: "long-model"}}
	}
	eng := NewEngine(map[string]provider.Client{"small": small, "long": long}, nil, cfg)

	req := longPrompt(t, "small-model", 6000)
	req.Model = "small-model"
	var got strings.Builder
	// Explicit chain (like the non-streaming gate test): the
	// server-facing ChatStreamChunks would resolve the chain via
	// chainFor — every registered client — so the long provider
	// would enter the PRIMARY chain with an unknown window and
	// serve it there. The explicit chain keeps the test on the
	// fallback jump itself.
	resp, err := eng.chatStreamDrainChunks(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, req, 0, func(chunk provider.StreamChunk) error {
		got.WriteString(chunk.Content)
		return nil
	}, "", false)
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if got.String() != "long says hi" || resp.Content != "long says hi" {
		t.Fatalf("streamed %q resp %q, want the long model's reply", got.String(), resp.Content)
	}
	if small.streamCalls() != 0 {
		t.Fatalf("small streams = %d, want 0 (never dialled)", small.streamCalls())
	}
	if long.streamCalls() != 1 {
		t.Fatalf("long streams = %d, want 1", long.streamCalls())
	}
	dec, ok := eng.LastContextFit()
	if !ok || dec.ChainUsed != "context_fallback" {
		t.Fatalf("last fit decision = %+v ok=%v, want chain_used context_fallback", dec, ok)
	}
}

// TestContextFitPrimaryFitsNoFallbackConsulted proves a fitting
// primary chain never touches the fallback chain: the fallback
// client is not even consulted.
func TestContextFitPrimaryFitsNoFallbackConsulted(t *testing.T) {
	small := okClient("small says hi")
	long := okClient("long says hi")
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{
		"small\x00small-model": 8192,
		"long\x00long-model":   1048576,
	})
	cfg.ContextFallbackChain = func(model string) []Attempt {
		return []Attempt{{ProviderID: "long", Model: "long-model"}}
	}
	eng := NewEngine(map[string]provider.Client{"small": small, "long": long}, nil, cfg)

	req := testReq()
	req.Model = "small-model"
	resp, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, req)
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp == nil || resp.Content != "small says hi" {
		t.Fatalf("resp = %+v, want the small model's reply", resp)
	}
	if long.chatCalls() != 0 {
		t.Fatalf("long calls = %d, want 0 (fallback never consulted)", long.chatCalls())
	}
	dec, ok := eng.LastContextFit()
	if !ok || dec.ChainUsed != "primary" {
		t.Fatalf("last fit decision = %+v ok=%v, want chain_used primary", dec, ok)
	}
}

// TestContextFitUnknownWindowIsTried pins the unknown-window rule:
// a model whose window is not known is tried, not skipped —
// unknown is not proof the model is too small.
func TestContextFitUnknownWindowIsTried(t *testing.T) {
	small := okClient("small says hi")
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{}) // every window unknown
	eng := NewEngine(map[string]provider.Client{"small": small}, nil, cfg)

	req := longPrompt(t, "small-model", 20000)
	req.Model = "small-model"
	resp, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, req)
	if err != nil {
		t.Fatalf("ChatWithChain: %v, want success (unknown window fits)", err)
	}
	if resp == nil || resp.Content != "small says hi" {
		t.Fatalf("resp = %+v, want the small model's reply", resp)
	}
	dec, ok := eng.LastContextFit()
	if !ok || len(dec.Entries) != 1 || dec.Entries[0].Known || !dec.Entries[0].Fits {
		t.Fatalf("last fit decision = %+v ok=%v, want one unknown-but-fitting entry", dec, ok)
	}
}

// TestContextFitNoFallbackChainFailsHonestly covers the
// no-dedicated-chain configuration: a prompt that fits nothing in
// the primary chain fails pre-call with zero provider calls.
func TestContextFitNoFallbackChainFailsHonestly(t *testing.T) {
	small := okClient("small says hi")
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{"small\x00small-model": 8192})
	// No ContextFallbackChain: nil means no dedicated chain.
	eng := NewEngine(map[string]provider.Client{"small": small}, nil, cfg)

	req := longPrompt(t, "small-model", 6000)
	req.Model = "small-model"
	_, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, req)
	var tooLarge *ErrContextTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *ErrContextTooLarge", err)
	}
	if small.chatCalls() != 0 {
		t.Fatalf("small calls = %d, want 0", small.chatCalls())
	}
}

// TestLastContextFitReturnsCopy proves the recorded decision is a
// copy: mutating the returned entries does not affect the engine.
func TestLastContextFitReturnsCopy(t *testing.T) {
	small := okClient("small says hi")
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{"small\x00small-model": 8192})
	eng := NewEngine(map[string]provider.Client{"small": small}, nil, cfg)
	req := testReq()
	req.Model = "small-model"
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "small", Model: "small-model"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	dec, ok := eng.LastContextFit()
	if !ok || len(dec.Entries) != 1 {
		t.Fatalf("last fit decision = %+v ok=%v", dec, ok)
	}
	dec.Entries[0].Fits = false
	dec2, _ := eng.LastContextFit()
	if !dec2.Entries[0].Fits {
		t.Fatal("mutating the returned decision changed the engine's copy")
	}
}
