package router

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// Local token counting + reconciliation (CHECKLIST 145): the
// engine counts the prompt locally (tiktoken, offline), uses it
// as the labelled fallback when the upstream reports no usage,
// keeps the provider's numbers verbatim when both exist, and
// flags material divergences — never silently preferring one.
// The local count is the full count (message text + tool
// definitions + per-message framing, CHECKLIST 229-230), so these
// tests measure the reference with CountPromptFull, the same
// counter the engine uses — not the text-only CountPrompt.
func TestReconcileUsageFallbackEstimate(t *testing.T) {
	led := &fakeLedger{}
	client := &fakeClient{
		chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			// Upstream reports no usage at all.
			return &provider.ChatResponse{Content: "hi", Usage: provider.Usage{}}, nil
		},
	}
	eng := NewEngine(map[string]provider.Client{"p1": client}, led, Config{})
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if led.count() != 1 {
		t.Fatalf("ledger calls = %d, want 1", led.count())
	}
	got := led.calls[0].usage
	want, err := tokencount.CountPromptFull("gpt-4", req)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	if !got.LocalEstimate || got.PromptTokens != want.Tokens || got.TotalTokens != want.Tokens {
		t.Errorf("usage = %+v, want LocalEstimate=true prompt=total=%d", got, want.Tokens)
	}
	if got.CompletionTokens != 0 {
		t.Errorf("usage completion = %d, want 0 (no completion count exists)", got.CompletionTokens)
	}
	stats := eng.TokenCountStats()
	if stats.Compared != 0 || stats.Diverged != 0 {
		t.Errorf("stats = %+v, want 0 compared / 0 diverged (nothing to reconcile against)", stats)
	}
}

func TestReconcileUsageKeepsReported(t *testing.T) {
	led := &fakeLedger{}
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	local, err := tokencount.CountPromptFull("gpt-4", req)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	client := &fakeClient{
		chatFn: func(ctx context.Context, r provider.ChatRequest) (*provider.ChatResponse, error) {
			// Upstream reports the same prompt count the local
			// counter finds, plus a real split.
			return &provider.ChatResponse{Content: "hi", Usage: provider.Usage{
				PromptTokens: local.Tokens, CompletionTokens: 5, TotalTokens: local.Tokens + 5,
				CachedTokens: 2, ReasoningTokens: 3, CacheWriteTokens: 1,
			}}, nil
		},
	}
	eng := NewEngine(map[string]provider.Client{"p1": client}, led, Config{})
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	got := led.calls[0].usage
	if got.LocalEstimate {
		t.Errorf("usage LocalEstimate = true, want false (provider reported a split)")
	}
	if got.PromptTokens != local.Tokens || got.CompletionTokens != 5 || got.CachedTokens != 2 || got.ReasoningTokens != 3 || got.CacheWriteTokens != 1 {
		t.Errorf("usage = %+v, want the provider's split kept verbatim", got)
	}
	stats := eng.TokenCountStats()
	if stats.Compared != 1 || stats.Diverged != 0 {
		t.Errorf("stats = %+v, want 1 compared / 0 diverged", stats)
	}
}

func TestReconcileUsageDivergence(t *testing.T) {
	led := &fakeLedger{}
	var hookCalled *TokenDivergence
	eng := NewEngine(map[string]provider.Client{"p1": &fakeClient{
		chatFn: func(ctx context.Context, r provider.ChatRequest) (*provider.ChatResponse, error) {
			// Upstream claims 1000 prompt tokens for a
			// two-word prompt: a material divergence.
			return &provider.ChatResponse{Content: "hi", Usage: provider.Usage{
				PromptTokens: 1000, CompletionTokens: 5, TotalTokens: 1005,
			}}, nil
		},
	}}, led, Config{OnTokenDivergence: func(d TokenDivergence) {
		d2 := d
		hookCalled = &d2
	}})
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	// The provider's number is kept verbatim — the divergence is
	// a flag, never a correction.
	got := led.calls[0].usage
	if got.PromptTokens != 1000 || got.LocalEstimate {
		t.Errorf("usage = %+v, want provider's 1000 kept, LocalEstimate=false", got)
	}
	stats := eng.TokenCountStats()
	if stats.Compared != 1 || stats.Diverged != 1 {
		t.Fatalf("stats = %+v, want 1 compared / 1 diverged", stats)
	}
	if !stats.HasLast || stats.Last.Provider != "p1" || stats.Last.Model != "gpt-4" || stats.Last.Reported != 1000 {
		t.Errorf("last divergence = %+v, want p1/gpt-4 reported=1000", stats.Last)
	}
	local, _ := tokencount.CountPromptFull("gpt-4", req)
	if stats.Last.Local != local.Tokens {
		t.Errorf("last divergence local = %d, want %d", stats.Last.Local, local.Tokens)
	}
	if hookCalled == nil {
		t.Fatalf("OnTokenDivergence was not called")
	}
	if hookCalled.Reported != 1000 || hookCalled.Local != local.Tokens {
		t.Errorf("hook divergence = %+v, want local=%d reported=1000", *hookCalled, local.Tokens)
	}
}

// A small disagreement is rounding noise, not a divergence: a
// 5-token difference on a ~100-token prompt stays under both the
// 20-token floor and the 10% bar.
func TestReconcileUsageSmallDifferenceNotMaterial(t *testing.T) {
	led := &fakeLedger{}
	long := "The quick brown fox jumps over the lazy dog. The quick brown fox jumps over the lazy dog. The quick brown fox jumps over the lazy dog. The quick brown fox jumps over the lazy dog."
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: long}}}
	local, err := tokencount.CountPromptFull("gpt-4", req)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	eng := NewEngine(map[string]provider.Client{"p1": &fakeClient{
		chatFn: func(ctx context.Context, r provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "hi", Usage: provider.Usage{
				PromptTokens: local.Tokens + 5, CompletionTokens: 5, TotalTokens: local.Tokens + 10,
			}}, nil
		},
	}}, led, Config{})
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	stats := eng.TokenCountStats()
	if stats.Compared != 1 || stats.Diverged != 0 {
		t.Errorf("stats = %+v, want 1 compared / 0 diverged (5-token gap is noise)", stats)
	}
}

// A successful stream records the local-estimate fallback: the
// engine never sees the upstream's streaming usage, so the prompt
// count is the local count, labelled — never a guessed split.
func TestStreamSuccessRecordsLocalEstimate(t *testing.T) {
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{"p1": &fakeClient{}}, led, Config{})
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4"}}, req)
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	for range ch {
	}
	if led.count() != 1 {
		t.Fatalf("ledger calls = %d, want 1", led.count())
	}
	got := led.calls[0].usage
	want, err := tokencount.CountPromptFull("gpt-4", req)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	if !got.LocalEstimate || got.PromptTokens != want.Tokens || got.TotalTokens != want.Tokens {
		t.Errorf("stream usage = %+v, want LocalEstimate=true prompt=total=%d", got, want.Tokens)
	}
}

// wireFormatFakeClient is a fakeClient that reports a wire format,
// the way provider.NewClient's native clients do (they inherit
// WireFormat from the embedded *openAIClient).
type wireFormatFakeClient struct {
	*fakeClient
	wire string
}

func (w *wireFormatFakeClient) WireFormat() string { return w.wire }

// wireFor must find a native-wire client registered under a keyed
// identity (CHECKLIST 229-230 live-smoke finding): the engine's
// clients map is keyed by ClientKey(provider, key), so a bare-ID
// lookup misses every provider with a key pool and the estimator
// silently falls back to the OpenAI-reference framing — +44% on a
// two-message Anthropic request in the live smoke. A keyed
// Anthropic client must count with the native rule (one role token
// per message), not the OpenAI rule (3/message + 3 priming).
func TestWireForFindsKeyedNativeClient(t *testing.T) {
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{}, led, Config{})
	eng.AddClientForKey("p1", "key-1", &wireFormatFakeClient{
		fakeClient: &fakeClient{
			chatFn: func(ctx context.Context, r provider.ChatRequest) (*provider.ChatResponse, error) {
				return &provider.ChatResponse{Content: "hi", Usage: provider.Usage{
					PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105,
				}}, nil
			},
		},
		wire: provider.WireFormatAnthropic,
	})
	req := provider.ChatRequest{Model: "claude-x", Messages: []provider.Message{{Role: "user", Content: "hello world"}}}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", KeyID: "key-1", Model: "claude-x"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	got := led.calls[0].usage
	// "hello world" is 3 message-text tokens; the native rule adds
	// 1 role token for the single message (4 total) while the
	// OpenAI rule would add 3 + 3 priming (9 total).
	want, err := tokencount.CountPromptWire("claude-x", provider.WireFormatAnthropic, req)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	if got.LocalEstimatePrompt != want.Tokens {
		t.Errorf("local estimate = %d, want %d (native wire rule)", got.LocalEstimatePrompt, want.Tokens)
	}
	if got.PromptTokens != 100 {
		t.Errorf("reported prompt = %d, want 100 (kept verbatim)", got.PromptTokens)
	}
}

// The material-divergence threshold itself: >20 tokens AND >10%
// of the reported count.
func TestTokenDivergenceMaterial(t *testing.T) {
	cases := []struct {
		local, reported int
		want            bool
	}{
		{100, 100, false},
		{105, 100, false},   // 5 <= 20 floor
		{119, 100, false},   // 19 <= 20 floor
		{121, 100, true},    // 21 > 20 floor
		{110, 100, false},   // 10 = 10% exactly, not more
		{111, 100, false},   // 11 > 10% but <= 20 floor
		{1100, 1000, false}, // 100 = 10% exactly
		{1101, 1000, true},  // 101 > 100 (10%) and > 20
		{0, 100, false},     // no local count: nothing to compare
		{100, 0, false},     // no reported count: nothing to compare
		{50, 1000, true},    // far below
	}
	for _, tc := range cases {
		if got := tokenDivergenceMaterial(tc.local, tc.reported); got != tc.want {
			t.Errorf("tokenDivergenceMaterial(%d, %d) = %v, want %v", tc.local, tc.reported, got, tc.want)
		}
	}
}

// TestReconcileUsageFeedsCalibration (M7 merge wave): a compared
// request feeds one observation into the process-wide calibrator —
// the UNCALIBRATED local against the reported count. Three
// identical observations at ratio 1.2 move the factor to 1.2; three
// more identical observations must leave it at 1.2. If the feed
// recorded the CALIBRATED local, the ratio would collapse toward
// 1.0 and the factor would decay (1.15, 1.11, 1.08) — the
// calibrator learning its own output.
func TestReconcileUsageFeedsCalibration(t *testing.T) {
	tokencount.ResetCalibrationForTests()
	defer tokencount.ResetCalibrationForTests()
	eng := NewEngine(nil, &fakeLedger{}, Config{})
	long := strings.Repeat("The quick brown fox jumps over the lazy dog near the river bank. ", 12)
	req := provider.ChatRequest{Model: "gpt-4", Messages: []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: long},
	}}
	// Measure the engine's own local via the no-report branch.
	probe := eng.reconcileUsage("pcal", "gpt-4", req, provider.Usage{})
	local := probe.LocalEstimatePrompt
	if local < 100 {
		t.Fatalf("fixture local=%d, want >= 100 (calibration floor)", local)
	}
	reported := int(math.Round(float64(local) * 1.2))
	if reported <= local {
		t.Fatalf("reported=%d must exceed local=%d", reported, local)
	}
	compared := provider.Usage{PromptTokens: reported, CompletionTokens: 10, TotalTokens: reported + 10}
	for i := 0; i < 3; i++ {
		eng.reconcileUsage("pcal", "gpt-4", req, compared)
	}
	if f := tokencount.CalibrationFactor("pcal", "gpt-4", false); math.Abs(f-1.2) > 0.01 {
		t.Fatalf("factor after 3 observations = %v, want 1.2", f)
	}
	for i := 0; i < 3; i++ {
		eng.reconcileUsage("pcal", "gpt-4", req, compared)
	}
	if f := tokencount.CalibrationFactor("pcal", "gpt-4", false); math.Abs(f-1.2) > 0.01 {
		t.Fatalf("factor after 6 observations = %v, want 1.2 (feed must use the uncalibrated local)", f)
	}
	// The calibrated estimate site now scales the local by the factor.
	want := int(math.Round(float64(local) * 1.2))
	if got := eng.localPromptCountCalibrated("pcal", "gpt-4", eng.wireFor("pcal"), req); math.Abs(float64(got)-float64(want)) > 1 {
		t.Fatalf("calibrated local = %d, want ~%d", got, want)
	}
}
