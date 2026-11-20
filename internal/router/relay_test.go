// Phase D, builder D5: context-relay engine integration.
//
// Tests drive the public interface (Engine.ChatWithChainStrategy /
// ChatStreamWithChainStrategy) with fake provider.Clients (the
// router_test.go harness) plus the relay.go mirror directly. Every
// test asserts something ONLY the D5 wiring produces — a handoff
// carried to the next attempt, outcomes recorded in order, honest
// degradation with no summariser — so each test FAILS with the wiring
// removed (verified by the toggle check: see the D5 report).
package router

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func relayReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model: "m",
		Messages: []provider.Message{
			{Role: RoleSystem, Content: "be helpful"},
			{Role: RoleUser, Content: "what is 2+2?"},
		},
	}
}

func relayChain(providers ...string) []Attempt {
	chain := make([]Attempt, len(providers))
	for i, p := range providers {
		chain[i] = Attempt{ProviderID: p, Model: "m"}
	}
	return chain
}

func errRelayClient(err error) *fakeClient {
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, err
	}}
}

// firstHandoff returns the leading system message of req when it
// carries a <context_handoff> block, or "" when it does not.
func firstHandoff(req provider.ChatRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}
	m := req.Messages[0]
	if m.Role != RoleSystem || !strings.Contains(m.Content, "<context_handoff>") {
		return ""
	}
	return m.Content
}

func lastChatReq(c *fakeClient) (provider.ChatRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		return provider.ChatRequest{}, false
	}
	return c.reqs[len(c.reqs)-1], true
}

func lastStreamReq(c *fakeClient) (provider.ChatRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.streamReqs) == 0 {
		return provider.ChatRequest{}, false
	}
	return c.streamReqs[len(c.streamReqs)-1], true
}

// ---------------------------------------------------------------------------
// Mirror unit tests: the gating, text and injection of relay.go.
// ---------------------------------------------------------------------------

// TestRelayRecordAttemptGates: the mirror's trigger gates match
// strategy.RecordAttempt — allowlist, quota window, already-active,
// on-error opt-in, unknown quota never triggers. A gate that fires
// here and not in the engine (or vice versa) is a divergence, so
// each row pins one gate in isolation.
func TestRelayRecordAttemptGates(t *testing.T) {
	msgs := []RelayMessage{{Role: "user", Content: "hi"}}
	newRelay := func() *relayState {
		return &relayState{providers: []string{"codex"}, relayOnError: true}
	}
	cases := []struct {
		name    string
		outcome relayOutcome
		want    bool
	}{
		{"success in window allowlisted", relayOutcome{Provider: "codex", Model: "m", Success: true, PercentUsed: 0.9, QuotaKnown: true, Messages: msgs}, true},
		{"success below threshold", relayOutcome{Provider: "codex", Model: "m", Success: true, PercentUsed: 0.5, QuotaKnown: true, Messages: msgs}, false},
		{"success at exhaustion", relayOutcome{Provider: "codex", Model: "m", Success: true, PercentUsed: 0.95, QuotaKnown: true, Messages: msgs}, false},
		{"success above exhaustion", relayOutcome{Provider: "codex", Model: "m", Success: true, PercentUsed: 0.99, QuotaKnown: true, Messages: msgs}, false},
		{"success unknown quota", relayOutcome{Provider: "codex", Model: "m", Success: true, QuotaKnown: false, Messages: msgs}, false},
		{"success non-allowlisted", relayOutcome{Provider: "other", Model: "m", Success: true, PercentUsed: 0.9, QuotaKnown: true, Messages: msgs}, false},
		{"failure allowlisted on-error", relayOutcome{Provider: "codex", Model: "m", Success: false, Messages: msgs}, true},
		{"failure non-allowlisted on-error", relayOutcome{Provider: "other", Model: "m", Success: false, Messages: msgs}, false},
	}
	for _, tc := range cases {
		r := newRelay()
		if got := r.recordAttempt(tc.outcome); got != tc.want {
			t.Errorf("%s: recordAttempt = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Failure without RelayOnError never triggers, even allowlisted.
	r := &relayState{providers: []string{"codex"}}
	if got := r.recordAttempt(relayOutcome{Provider: "codex", Model: "m", Success: false, Messages: msgs}); got {
		t.Error("failure without RelayOnError: recordAttempt = true, want false")
	}
	// A handoff already active is never regenerated.
	r2 := newRelay()
	r2.recordAttempt(relayOutcome{Provider: "codex", Model: "m", Success: false, Messages: msgs})
	r2.setHandoff(RelayHandoff{Summary: "s", Summarised: true}, time.Now())
	if got := r2.recordAttempt(relayOutcome{Provider: "codex", Model: "m", Success: false, Messages: msgs}); got {
		t.Error("recordAttempt with active handoff = true, want false (no regeneration)")
	}
	// Default allowlist is ["codex"] when none is configured.
	r3 := &relayState{relayOnError: true}
	if got := r3.recordAttempt(relayOutcome{Provider: "codex", Model: "m", Success: false, Messages: msgs}); !got {
		t.Error("default allowlist: codex failure did not trigger, want true")
	}
	if got := r3.recordAttempt(relayOutcome{Provider: "other", Model: "m", Success: false, Messages: msgs}); got {
		t.Error("default allowlist: non-codex failure triggered, want false")
	}
}

// TestRelayMessageCap: only the last defaultRelayMaxMessages source
// messages are kept for summarisation, mirroring strategy's cap.
func TestRelayMessageCap(t *testing.T) {
	r := &relayState{providers: []string{"codex"}, relayOnError: true}
	msgs := make([]RelayMessage, 0, 35)
	for i := 0; i < 35; i++ {
		msgs = append(msgs, RelayMessage{Role: "user", Content: "m" + strconv.Itoa(i)})
	}
	r.recordAttempt(relayOutcome{Provider: "codex", Model: "m", Success: false, Messages: msgs})
	if got := len(r.messages); got != defaultRelayMaxMessages {
		t.Fatalf("kept messages = %d, want %d", got, defaultRelayMaxMessages)
	}
	if r.messages[0].Content != "m5" || r.messages[len(r.messages)-1].Content != "m34" {
		t.Fatalf("kept window = %q..%q, want m5..m34 (last 30)", r.messages[0].Content, r.messages[len(r.messages)-1].Content)
	}
}

// TestRelayBuildHandoffTextEscapes: the <context_handoff> block
// escapes XML metacharacters (a summary containing markup must not
// break the block), and an unsummarised handoff carries the explicit
// transfer note while a summarised one does not.
func TestRelayBuildHandoffTextEscapes(t *testing.T) {
	r := &relayState{}
	r.setHandoff(RelayHandoff{
		Summary:        `a <b> & "quoted"`,
		KeyDecisions:   []string{`decide <x>`},
		TaskProgress:   "half",
		ActiveEntities: []string{"e1", "e2"},
		MessageCount:   3,
		Summarised:     true,
	}, time.Now())
	text := r.buildHandoffText()
	if strings.Contains(text, "<b>") || strings.Contains(text, `"quoted"`) {
		t.Fatalf("handoff text not escaped: %q", text)
	}
	if !strings.Contains(text, "a &lt;b&gt; &amp; &quot;quoted&quot;") {
		t.Fatalf("escaped summary missing: %q", text)
	}
	if strings.Contains(text, "Unsummarised transcript") {
		t.Fatalf("summarised handoff carries the unsummarised note: %q", text)
	}
	r2 := &relayState{}
	r2.setHandoff(RelayHandoff{Summary: "raw", MessageCount: 2, Summarised: false}, time.Now())
	if got := r2.buildHandoffText(); !strings.Contains(got, "Unsummarised transcript") {
		t.Fatalf("unsummarised handoff lacks the transfer note: %q", got)
	}
}

// TestRelayAttemptMessagesInjection: with an active handoff the next
// attempt's list is the handoff system message first, then the
// original messages untouched; without one the list passes through
// unchanged (same slice contents, applied=false); the caller's slice
// is never mutated.
func TestRelayAttemptMessagesInjection(t *testing.T) {
	orig := []provider.Message{{Role: RoleSystem, Content: "be helpful"}, {Role: RoleUser, Content: "hi"}}
	origCopy := append([]provider.Message(nil), orig...)

	r := &relayState{}
	if got, applied := r.attemptMessages(orig); applied || len(got) != len(orig) {
		t.Fatalf("no handoff: applied=%v len=%d, want false/%d", applied, len(got), len(orig))
	}
	r.setHandoff(RelayHandoff{Summary: "s", MessageCount: 1, Summarised: true}, time.Now())
	got, applied := r.attemptMessages(orig)
	if !applied {
		t.Fatal("active handoff: applied=false, want true")
	}
	if len(got) != len(orig)+1 {
		t.Fatalf("injected len=%d, want %d", len(got), len(orig)+1)
	}
	if got[0].Role != RoleSystem || !strings.Contains(got[0].Content, "<context_handoff>") {
		t.Fatalf("leading message = %+v, want system <context_handoff>", got[0])
	}
	for i, m := range orig {
		if got[i+1].Role != m.Role || got[i+1].Content != m.Content {
			t.Fatalf("original message %d changed: %+v vs %+v", i, got[i+1], m)
		}
	}
	for i, m := range orig {
		if orig[i].Role != origCopy[i].Role || orig[i].Content != origCopy[i].Content || m.Role != origCopy[i].Role || m.Content != origCopy[i].Content {
			t.Fatalf("caller's slice mutated at %d", i)
		}
	}
	// Expired handoff injects nothing.
	r.expiresAt = time.Now().Add(-time.Minute)
	if _, applied := r.attemptMessages(orig); applied {
		t.Fatal("expired handoff: applied=true, want false")
	}
}

// TestRelayDegradedHandoffTruncation: a degraded handoff carries the
// raw transcript verbatim, capped at maxDegradedTranscriptChars, with
// the truncation marker — and is labelled unsummarised.
func TestRelayDegradedHandoffTruncation(t *testing.T) {
	r := &relayState{}
	r.messages = []RelayMessage{{Role: "user", Content: strings.Repeat("x", maxDegradedTranscriptChars+100)}}
	d := r.degradedHandoff(r.messages)
	if d.Summarised {
		t.Fatal("degraded handoff Summarised=true, want false")
	}
	if !strings.Contains(d.Summary, "[transcript truncated]") {
		t.Fatal("over-long transcript lacks the truncation marker")
	}
	if len(d.Summary) > maxDegradedTranscriptChars+len("\n[transcript truncated]")+1 {
		t.Fatalf("degraded summary len=%d, want capped near %d", len(d.Summary), maxDegradedTranscriptChars)
	}
	if !strings.Contains(d.Summary, "user: "+strings.Repeat("x", 10)) {
		t.Fatal("degraded handoff does not carry the raw transcript verbatim")
	}
}

// ---------------------------------------------------------------------------
// Engine tests: the attempt loop feeds outcomes and carries handoffs.
// ---------------------------------------------------------------------------

// TestRelayEngineHandoffCarriedToNextAttempt: context-relay with
// RelayOnError and NO summariser configured — the first (codex)
// attempt fails, the second attempt receives the handoff as a leading
// system message, honestly degraded (unsummarised marker, "no
// summariser configured" reason), and LastRelayStatus records both
// attempts in order. This is the core D5 claim: outcomes in, handoff
// out, no faked summary.
func TestRelayEngineHandoffCarriedToNextAttempt(t *testing.T) {
	bad := errRelayClient(errors.New("boom"))
	good := okClient("from-good")
	cfg := testConfig()
	cfg.RelayOnError = true
	eng := NewEngine(map[string]provider.Client{"codex": bad, "other": good}, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex", "other"), relayReq(), "context-relay")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	if resp.Content != "from-good" {
		t.Fatalf("content = %q, want from-good", resp.Content)
	}
	req, ok := lastChatReq(good)
	if !ok {
		t.Fatal("good client received no chat")
	}
	handoff := firstHandoff(req)
	if handoff == "" {
		t.Fatalf("second attempt got no handoff system message; messages = %+v", req.Messages)
	}
	if !strings.Contains(handoff, "Unsummarised transcript") {
		t.Fatalf("handoff lacks the unsummarised marker: %q", handoff)
	}
	if !strings.Contains(handoff, "what is 2+2?") {
		t.Fatalf("handoff does not carry the raw transcript: %q", handoff)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("second attempt messages = %d, want 3 (handoff + 2 originals)", len(req.Messages))
	}
	if req.Messages[1].Content != "be helpful" || req.Messages[2].Content != "what is 2+2?" {
		t.Fatalf("original messages not preserved after handoff: %+v", req.Messages)
	}
	st, ok := eng.LastRelayStatus()
	if !ok {
		t.Fatal("LastRelayStatus ok=false, want a relay record")
	}
	if st.Attempts != 2 {
		t.Fatalf("relay attempts = %d, want 2 (failed codex + successful other, in order)", st.Attempts)
	}
	if st.FromProvider != "other" {
		t.Fatalf("last relay provider = %q, want other", st.FromProvider)
	}
	if !st.HandoffActive || st.HandoffSummarised {
		t.Fatalf("relay status = active=%v summarised=%v, want active=true summarised=false", st.HandoffActive, st.HandoffSummarised)
	}
	if st.DegradeReason != "no summariser configured" {
		t.Fatalf("degrade reason = %q, want %q", st.DegradeReason, "no summariser configured")
	}
}

// TestRelayEngineSummariserHandoff: with a summariser configured,
// the next attempt receives the summarised handoff (its summary,
// decisions and progress), with no unsummarised marker — and the
// summariser saw exactly the original request messages.
func TestRelayEngineSummariserHandoff(t *testing.T) {
	bad := errRelayClient(errors.New("boom"))
	good := okClient("from-good")
	var saw []RelayMessage
	calls := 0
	cfg := testConfig()
	cfg.RelayOnError = true
	cfg.RelaySummariser = func(ctx context.Context, messages []RelayMessage) (RelayHandoff, error) {
		calls++
		saw = append([]RelayMessage(nil), messages...)
		return RelayHandoff{
			Summary:        "user asks what 2+2 is",
			KeyDecisions:   []string{"use arithmetic"},
			TaskProgress:   "question asked, no answer yet",
			ActiveEntities: []string{"2+2"},
			Model:          "summariser-m",
		}, nil
	}
	eng := NewEngine(map[string]provider.Client{"codex": bad, "other": good}, nil, cfg)
	if _, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex", "other"), relayReq(), "context-relay"); err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	if calls != 1 {
		t.Fatalf("summariser calls = %d, want 1 (built once, lazily, for the next attempt)", calls)
	}
	if len(saw) != 2 || saw[1].Content != "what is 2+2?" {
		t.Fatalf("summariser saw %+v, want the 2 original request messages", saw)
	}
	req, _ := lastChatReq(good)
	handoff := firstHandoff(req)
	if handoff == "" {
		t.Fatal("second attempt got no handoff")
	}
	for _, want := range []string{"user asks what 2+2 is", "use arithmetic", "question asked, no answer yet", "2+2"} {
		if !strings.Contains(handoff, want) {
			t.Fatalf("summarised handoff lacks %q: %q", want, handoff)
		}
	}
	if strings.Contains(handoff, "Unsummarised") {
		t.Fatalf("summarised handoff carries the unsummarised marker: %q", handoff)
	}
	st, _ := eng.LastRelayStatus()
	if !st.HandoffSummarised || st.DegradeReason != "" {
		t.Fatalf("relay status summarised=%v reason=%q, want true/\"\"", st.HandoffSummarised, st.DegradeReason)
	}
}

// TestRelayEngineOtherStrategiesUntouched: under any other strategy
// the relay never runs — no handoff on the next attempt, the
// summariser is never called, and no relay record exists — even with
// RelayOnError set and a summariser configured.
func TestRelayEngineOtherStrategiesUntouched(t *testing.T) {
	bad := errRelayClient(errors.New("boom"))
	good := okClient("from-good")
	calls := 0
	cfg := testConfig()
	cfg.RelayOnError = true
	cfg.RelaySummariser = func(ctx context.Context, messages []RelayMessage) (RelayHandoff, error) {
		calls++
		return RelayHandoff{Summary: "x"}, nil
	}
	eng := NewEngine(map[string]provider.Client{"codex": bad, "other": good}, nil, cfg)
	if _, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex", "other"), relayReq(), "priority"); err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	if calls != 0 {
		t.Fatalf("summariser calls = %d, want 0 under priority", calls)
	}
	req, _ := lastChatReq(good)
	if got := firstHandoff(req); got != "" {
		t.Fatalf("priority second attempt got a handoff: %q", got)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("priority second attempt messages = %d, want 2 (originals only)", len(req.Messages))
	}
	if _, ok := eng.LastRelayStatus(); ok {
		t.Fatal("LastRelayStatus ok=true under priority, want no relay record")
	}
}

// TestRelayEngineNoTriggerWithoutRelayOnError: context-relay with
// RelayOnError off and no quota signal — a plain failure requests no
// handoff, so the next attempt starts cold (the failure alone is not
// a relay trigger; the quota signal is unknown, never invented).
func TestRelayEngineNoTriggerWithoutRelayOnError(t *testing.T) {
	bad := errRelayClient(errors.New("boom"))
	good := okClient("from-good")
	cfg := testConfig()
	eng := NewEngine(map[string]provider.Client{"codex": bad, "other": good}, nil, cfg)
	if _, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex", "other"), relayReq(), "context-relay"); err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	req, _ := lastChatReq(good)
	if got := firstHandoff(req); got != "" {
		t.Fatalf("no-trigger second attempt got a handoff: %q", got)
	}
	st, ok := eng.LastRelayStatus()
	if !ok {
		t.Fatal("LastRelayStatus ok=false, want the recorded outcomes")
	}
	if st.Attempts != 2 || st.HandoffActive {
		t.Fatalf("relay status attempts=%d active=%v, want 2/false", st.Attempts, st.HandoffActive)
	}
}

// TestRelayEngineOutcomesLandInOrder: three attempts — codex fails,
// a second codex key fails, other succeeds. The handoff is built
// ONCE from the first failure (summariser called once), carried to
// the immediate next attempt (not just the last), and NOT rebuilt
// after the second failure; LastRelayStatus ends on the winner.
func TestRelayEngineOutcomesLandInOrder(t *testing.T) {
	bad1 := errRelayClient(errors.New("boom1"))
	bad2 := errRelayClient(errors.New("boom2"))
	good := okClient("from-good")
	calls := 0
	cfg := testConfig()
	cfg.RelayOnError = true
	cfg.RelaySummariser = func(ctx context.Context, messages []RelayMessage) (RelayHandoff, error) {
		calls++
		return RelayHandoff{Summary: "summary-after-first-failure"}, nil
	}
	eng := NewEngine(map[string]provider.Client{"codex": bad1, "other": good}, nil, cfg)
	eng.AddClientForKey("codex", "k2", bad2)
	chain := []Attempt{
		{ProviderID: "codex", Model: "m"},
		{ProviderID: "codex", Model: "m", KeyID: "k2"},
		{ProviderID: "other", Model: "m"},
	}
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), chain, relayReq(), "context-relay")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	if resp.Content != "from-good" {
		t.Fatalf("content = %q, want from-good", resp.Content)
	}
	if calls != 1 {
		t.Fatalf("summariser calls = %d, want 1 (built once from the first failure, never regenerated)", calls)
	}
	// The immediate next attempt (second codex key) already
	// carries the handoff built from the first failure.
	req2, ok := lastChatReq(bad2)
	if !ok {
		t.Fatal("second attempt received no chat")
	}
	if h := firstHandoff(req2); !strings.Contains(h, "summary-after-first-failure") {
		t.Fatalf("second attempt handoff = %q, want the first-failure summary", h)
	}
	// The third attempt carries the same handoff (not a rebuild).
	req3, _ := lastChatReq(good)
	if h := firstHandoff(req3); !strings.Contains(h, "summary-after-first-failure") {
		t.Fatalf("third attempt handoff = %q, want the same first-failure summary", h)
	}
	st, _ := eng.LastRelayStatus()
	if st.Attempts != 3 || st.FromProvider != "other" {
		t.Fatalf("relay status attempts=%d from=%q, want 3/other", st.Attempts, st.FromProvider)
	}
}

// TestRelayEngineSummariserErrorDegrades: a summariser that errors
// degrades like a missing one — the next attempt still gets the raw
// transcript, labelled unsummarised, and the reason names the error;
// the request itself still succeeds via the fallback.
func TestRelayEngineSummariserErrorDegrades(t *testing.T) {
	bad := errRelayClient(errors.New("boom"))
	good := okClient("from-good")
	cfg := testConfig()
	cfg.RelayOnError = true
	cfg.RelaySummariser = func(ctx context.Context, messages []RelayMessage) (RelayHandoff, error) {
		return RelayHandoff{}, errors.New("summariser down")
	}
	eng := NewEngine(map[string]provider.Client{"codex": bad, "other": good}, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex", "other"), relayReq(), "context-relay")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	if resp.Content != "from-good" {
		t.Fatalf("content = %q, want from-good", resp.Content)
	}
	req, _ := lastChatReq(good)
	handoff := firstHandoff(req)
	if handoff == "" || !strings.Contains(handoff, "Unsummarised transcript") {
		t.Fatalf("error-degraded handoff = %q, want the unsummarised transcript", handoff)
	}
	if !strings.Contains(handoff, "what is 2+2?") {
		t.Fatalf("error-degraded handoff lost the raw transcript: %q", handoff)
	}
	st, _ := eng.LastRelayStatus()
	if st.HandoffSummarised || !strings.HasPrefix(st.DegradeReason, "summariser error: ") {
		t.Fatalf("relay status summarised=%v reason=%q, want false/\"summariser error: ...\"", st.HandoffSummarised, st.DegradeReason)
	}
}

// TestRelayEngineStreamHandoffCarried: the streaming loop feeds
// outcomes too — a failed first stream relays the handoff into the
// second stream's request, and the stream still delivers the
// fallback's content.
func TestRelayEngineStreamHandoffCarried(t *testing.T) {
	bad := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		return nil, errors.New("stream boom")
	}}
	good := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if onDelta != nil {
			if err := onDelta("hello"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "hello"}, nil
	}}
	cfg := testConfig()
	cfg.RelayOnError = true
	eng := NewEngine(map[string]provider.Client{"codex": bad, "other": good}, nil, cfg)
	ch, err := eng.ChatStreamWithChainStrategy(context.Background(), relayChain("codex", "other"), relayReq(), "context-relay")
	if err != nil {
		t.Fatalf("ChatStreamWithChainStrategy = %v", err)
	}
	var sb strings.Builder
	sawDone := false
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		sb.WriteString(ev.Delta)
		if ev.Done {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream ended without a Done event")
	}
	if got := sb.String(); got != "hello" {
		t.Fatalf("stream deltas = %q, want hello", got)
	}
	req, ok := lastStreamReq(good)
	if !ok {
		t.Fatal("good client received no stream")
	}
	if h := firstHandoff(req); h == "" || !strings.Contains(h, "Unsummarised transcript") {
		t.Fatalf("second stream handoff = %q, want the unsummarised transcript", h)
	}
	st, ok := eng.LastRelayStatus()
	// M4.8 early-retry: the failed stream is re-opened 3 times on
	// the bad target before the failover, so the relay counts 5
	// attempts (4 bad + 1 good), not 2.
	if !ok || st.Attempts != 5 {
		t.Fatalf("relay status ok=%v attempts=%d, want true/5", ok, st.Attempts)
	}
}

// TestRelayEngineHedgeDoesNotRecordCancelledLoser: with hedging on,
// the slow first attempt loses the race to the fast hedge target —
// the cancelled loser is a synthetic context.Canceled result, not an
// attempt outcome, so it must not land in the relay (Attempts stays
// 1 and no handoff is ever wanted for a finished request).
func TestRelayEngineHedgeDoesNotRecordCancelledLoser(t *testing.T) {
	slow := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return &provider.ChatResponse{Content: "slow"}, nil
		}
	}}
	fast := okClient("fast wins")
	cfg := testConfig()
	cfg.HedgeDelay = time.Millisecond // hedging on: second attempt races
	cfg.RelayOnError = true
	eng := NewEngine(map[string]provider.Client{"other": slow, "codex": fast}, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("other", "codex"), relayReq(), "context-relay")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	if resp.Content != "fast wins" {
		t.Fatalf("content = %q, want fast wins", resp.Content)
	}
	st, ok := eng.LastRelayStatus()
	if !ok {
		t.Fatal("LastRelayStatus ok=false, want the recorded outcome")
	}
	if st.Attempts != 1 {
		t.Fatalf("relay attempts = %d, want 1 (winner only — the cancelled hedge loser is not an outcome)", st.Attempts)
	}
	if st.HandoffActive {
		t.Fatal("relay handoff active after a hedged success, want none")
	}
}

// TestRelayEngineUnknownProviderRecordsNothing: a never-dialled
// attempt is not an outcome. With RelayOnError on, a chain whose
// first member ("codex", allowlisted) has no registered client
// must not trigger a handoff want for the provider never dialled:
// the next attempt gets no handoff and LastRelayStatus counts
// only the attempt that actually ran.
func TestRelayEngineUnknownProviderRecordsNothing(t *testing.T) {
	good := okClient("from-good")
	cfg := testConfig()
	cfg.RelayOnError = true
	// NOTE: no "codex" client is registered — codex is unknown.
	eng := NewEngine(map[string]provider.Client{"other": good}, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex", "other"), relayReq(), "context-relay")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v", err)
	}
	if resp.Content != "from-good" {
		t.Fatalf("content = %q, want from-good", resp.Content)
	}
	req, ok := lastChatReq(good)
	if !ok {
		t.Fatal("good client received no chat")
	}
	if handoff := firstHandoff(req); handoff != "" {
		t.Fatalf("second attempt got a handoff for a provider never dialled: %q", handoff)
	}
	st, ok := eng.LastRelayStatus()
	if !ok {
		t.Fatal("LastRelayStatus ok=false, want a relay record")
	}
	if st.Attempts != 1 {
		t.Fatalf("relay attempts = %d, want 1 (only the dialled attempt)", st.Attempts)
	}
	if st.FromProvider != "other" {
		t.Fatalf("last relay provider = %q, want other", st.FromProvider)
	}
}
