// G11 Area 2 failover residuals — tests for the 7 PARTIAL rows
// (parity tracker Area 2). Every test uses fake clients (no network)
// and the real Engine.
package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// --- Row 1: 200-OK body validation predicate → failover ---

func TestG11ResponseValidationFailsOverOnErrorPayload(t *testing.T) {
	ctx := context.Background()
	a := okClient(`{"error": {"message": "quota exceeded", "type": "server_error"}}`)
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("error-payload failover = %v, %v, want bee + nil", resp, err)
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2 (a failed validation, b served)", len(trace.Attempts))
	}
}

func TestG11ResponseValidationCleanBodyDoesNotFailOver(t *testing.T) {
	ctx := context.Background()
	a := okClient(`{"choices": [{"message": {"role": "assistant", "content": "hi"}}]}`)
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1})
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil || resp == nil || !strings.Contains(resp.Content, "choices") {
		t.Fatalf("clean body = %v, %v, want the body served", resp, err)
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %d, want 1 (no failover on a clean body)", len(trace.Attempts))
	}
}

func TestG11ResponseValidationEmptyChoicesFailsOver(t *testing.T) {
	ctx := context.Background()
	a := okClient(`{"choices": []}`)
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("empty-choices failover = %v, %v, want bee + nil", resp, err)
	}
}

func TestG11ResponseValidationKnobs(t *testing.T) {
	// Forbidden substring fails the body.
	if err := ValidateResponseBody(&provider.ChatResponse{Content: "sorry, I cannot help with that"}, ResponseValidationConfig{ForbiddenSubstrings: []string{"sorry, I cannot"}}); err == nil {
		t.Fatalf("forbidden substring: want validation error, got nil")
	}
	// Required substring missing fails the body; present passes.
	if err := ValidateResponseBody(&provider.ChatResponse{Content: "hello"}, ResponseValidationConfig{RequiredSubstrings: []string{"goodbye"}}); err == nil {
		t.Fatalf("missing required substring: want validation error, got nil")
	}
	if err := ValidateResponseBody(&provider.ChatResponse{Content: "hello goodbye"}, ResponseValidationConfig{RequiredSubstrings: []string{"goodbye"}}); err != nil {
		t.Fatalf("required substring present: want nil, got %v", err)
	}
	// Minimum content length.
	if err := ValidateResponseBody(&provider.ChatResponse{Content: "hi"}, ResponseValidationConfig{MinContentLength: 10}); err == nil {
		t.Fatalf("short body: want validation error, got nil")
	}
	// Disabled turns the whole predicate off.
	if err := ValidateResponseBody(&provider.ChatResponse{Content: `{"error": "boom"}`}, ResponseValidationConfig{Disabled: true}); err != nil {
		t.Fatalf("disabled predicate: want nil, got %v", err)
	}
	// A validation failure classifies as a retryable
	// invalid-response (taxonomy), never a stop.
	err := ValidateResponseBody(&provider.ChatResponse{Content: `{"error": "boom"}`}, ResponseValidationConfig{})
	cls := provider.ClassifyError(err, provider.EndpointChatCompletions)
	if cls.Decision != provider.DecisionFallback || cls.Reason != provider.ReasonInvalidResponse {
		t.Fatalf("validation error classification = %v/%v, want fallback/invalid_response", cls.Decision, cls.Reason)
	}
}

// --- Row 2: breaker persistence ---

func TestG11BreakerStateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "breakers.json")
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	// Profile defaults (API-key: open at 12, reset 30 s): an
	// explicit BreakerThreshold override would trip the breaker
	// while breakerStateOf still reports CLOSED (its CLOSED
	// check uses the profile degrade threshold of 7 — a
	// pre-existing quirk, not this slice's to fix), so the
	// trip here uses the profile's own numbers.
	cfg := Config{HedgeDelay: -1, BreakerStorePath: path}
	eng1 := NewEngine(map[string]provider.Client{"a": errClient(fail500)}, nil, cfg)
	for i := 0; i < 12; i++ {
		if _, _, err := eng1.ChatWithChain(ctx, chainFor("a"), coolReq("m")); err == nil {
			t.Fatalf("request %d: want failure, got nil", i+1)
		}
	}
	st1 := breakerStatesByID(eng1)["a"]
	if st1.State != BreakerOpen || st1.Trips != 1 || st1.ConsecutiveFailures != 12 {
		t.Fatalf("breaker before restart = %+v, want OPEN, 1 trip, 12 failures", st1)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("breaker store file missing: %v", err)
	}
	// "Restart": a fresh engine on the same store path.
	eng2 := NewEngine(map[string]provider.Client{"a": errClient(fail500)}, nil, cfg)
	st2 := breakerStatesByID(eng2)["a"]
	if st2.State != BreakerOpen || st2.Trips != 1 || st2.ConsecutiveFailures != 12 {
		t.Fatalf("breaker after restart = %+v, want OPEN, 1 trip, 12 failures", st2)
	}
}

func TestG11BreakerHalfOpenProbeAfterReload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "breakers.json")
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	cfg := Config{HedgeDelay: -1, BreakerOpenFor: 50 * time.Millisecond, BreakerStorePath: path}
	eng1 := NewEngine(map[string]provider.Client{"a": errClient(fail500)}, nil, cfg)
	for i := 0; i < 12; i++ {
		_, _, _ = eng1.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	}
	if st := breakerStatesByID(eng1)["a"]; st.State != BreakerOpen {
		t.Fatalf("breaker before restart = %v, want OPEN", st.State)
	}
	// Let the open window elapse, then restart: the restored
	// breaker must offer exactly one half-open probe.
	time.Sleep(80 * time.Millisecond)
	eng2 := NewEngine(map[string]provider.Client{"a": errClient(fail500)}, nil, cfg)
	if !eng2.breakerAllow("a") {
		t.Fatalf("reloaded breaker: want the half-open probe admitted")
	}
	if eng2.breakerAllow("a") {
		t.Fatalf("reloaded breaker: want only ONE half-open probe in flight")
	}
}

// --- Row 3: connection cooldown hardening ---

func TestG11CooldownBasesDifferPerAuthProfile(t *testing.T) {
	if got := CooldownBaseFor(AuthProfileOAuth); got != 5*time.Second {
		t.Fatalf("OAuth cooldown base = %v, want 5s", got)
	}
	if got := CooldownBaseFor(AuthProfileAPIKey); got != 3*time.Second {
		t.Fatalf("API-key cooldown base = %v, want 3s", got)
	}
	if got := CooldownBaseFor(AuthProfileLocal); got != 2*time.Second {
		t.Fatalf("Local cooldown base = %v, want 2s", got)
	}
	// Engine level: the soft ladder starts from the profile
	// base, not the uniform 5 s.
	eng := NewEngine(map[string]provider.Client{}, nil, Config{
		CooldownMode: CooldownSoftDemote,
		AuthProfileFor: func(id string) AuthProfile {
			switch id {
			case "oauth-p":
				return AuthProfileOAuth
			case "local-p":
				return AuthProfileLocal
			default:
				return AuthProfileAPIKey
			}
		},
	})
	eng.startSoftCooldown(ClientKey("oauth-p", ""), 0, false)
	eng.startSoftCooldown(ClientKey("apikey-p", ""), 0, false)
	eng.startSoftCooldown(ClientKey("local-p", ""), 0, false)
	for providerID, want := range map[string]time.Duration{
		"oauth-p":  5 * time.Second,
		"apikey-p": 3 * time.Second,
		"local-p":  2 * time.Second,
	} {
		eng.mu.Lock()
		st := eng.softCooldowns[ClientKey(providerID, "")]
		eng.mu.Unlock()
		if st == nil {
			t.Fatalf("no soft cooldown recorded for %q", providerID)
		}
		d := time.Until(st.until)
		if d <= want-500*time.Millisecond || d > want {
			t.Fatalf("soft cooldown for %q = %v until, want ~%v", providerID, d, want)
		}
	}
}

func TestG11CooldownStaggerSpreadsReadmission(t *testing.T) {
	// Distinct keys get distinct staggers (deterministic per
	// key, stable across calls).
	if got := CooldownStaggerFor("a"); got != CooldownStaggerFor("a") {
		t.Fatalf("stagger is not deterministic for the same key: %v", got)
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 16; i++ {
		seen[CooldownStaggerFor("key-"+string(rune('a'+i)))] = true
	}
	if len(seen) < 3 {
		t.Fatalf("staggers across 16 keys = %d distinct, want at least 3", len(seen))
	}
	// Behavioural: two keys cooled for the same 100 ms, whose
	// staggers differ by >= 200 ms, are NOT re-admitted
	// together — the earlier one frees while the later one is
	// still gated.
	var k1, k2 string
	var s1, s2 time.Duration
outer:
	for i := 0; i < 100; i++ {
		for j := i + 1; j < 100; j++ {
			a := "stag-a-" + string(rune(i))
			b := "stag-b-" + string(rune(j))
			d := CooldownStaggerFor(b) - CooldownStaggerFor(a)
			if d < 0 {
				a, b = b, a
				d = -d
			}
			if d >= 200*time.Millisecond {
				k1, k2, s1, s2 = a, b, CooldownStaggerFor(a), CooldownStaggerFor(b)
				break outer
			}
		}
	}
	if k1 == "" {
		t.Fatalf("no key pair with >= 200ms stagger difference in the search space")
	}
	eng := NewEngine(map[string]provider.Client{}, nil, Config{})
	eng.setCooldown(k1, 100*time.Millisecond)
	eng.setCooldown(k2, 100*time.Millisecond)
	// Poll until the earlier key frees (deadline 1 s); the
	// later key must still be gated at that moment.
	deadline := time.Now().Add(time.Second)
	for eng.onCooldownStaggered(k1) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if eng.onCooldownStaggered(k1) {
		t.Fatalf("key %q (stagger %v) never freed", k1, s1)
	}
	if !eng.onCooldownStaggered(k2) {
		t.Fatalf("key %q (stagger %v) freed together with %q (stagger %v) — no stagger", k2, s2, k1, s1)
	}
	// ...and it does free, once its own stagger elapses.
	deadline = time.Now().Add(time.Second)
	for eng.onCooldownStaggered(k2) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if eng.onCooldownStaggered(k2) {
		t.Fatalf("key %q never freed after its stagger", k2)
	}
}

// --- Row 4: deadline-failure sub-budget ---

func TestG11DeadlineSubBudgetExhaustsTo504(t *testing.T) {
	ctx := context.Background()
	// One delayClient per provider: a shared fake would share
	// the call counter and the test could not tell who ran.
	mkSlow := func() *fakeClient { return delayClient(200*time.Millisecond, "too slow") }
	clients := map[string]provider.Client{"a": mkSlow(), "b": mkSlow(), "c": mkSlow(), "d": mkSlow()}
	eng := NewEngine(clients, nil, Config{
		HedgeDelay:         -1,
		PerTargetTimeout:   20 * time.Millisecond,
		DeadlineFailureMax: 3,
	})
	_, _, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c", "d"), coolReq("m"))
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("sub-budget exhaustion = %v, want ErrDeadlineExceeded (clean 504)", err)
	}
	// Exactly 3 attempts: the 4th target is never burned.
	total := 0
	for _, id := range []string{"a", "b", "c", "d"} {
		fc := clients[id].(*fakeClient)
		fc.mu.Lock()
		total += fc.chats
		if id == "d" && fc.chats != 0 {
			t.Fatalf("provider d called %d times, want 0 (sub-budget ends the combo first)", fc.chats)
		}
		fc.mu.Unlock()
	}
	if total != 3 {
		t.Fatalf("total attempts = %d, want 3", total)
	}
}

// --- Row 5: 3-attempt cross-provider default ---

func TestG11MaxThreeDistinctProviders(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	clients := map[string]provider.Client{}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		clients[id] = errClient(fail500)
	}
	eng := NewEngine(clients, nil, Config{HedgeDelay: -1})
	_, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c", "d", "e"), coolReq("m"))
	if err == nil {
		t.Fatalf("want an error after the provider budget, got nil")
	}
	if len(trace.Attempts) != 3 {
		t.Fatalf("trace attempts = %d, want 3 (the cross-provider default)", len(trace.Attempts))
	}
	for _, id := range []string{"d", "e"} {
		fc := clients[id].(*fakeClient)
		fc.mu.Lock()
		n := fc.chats
		fc.mu.Unlock()
		if n != 0 {
			t.Fatalf("provider %q called %d times, want 0 (past the 3-provider budget)", id, n)
		}
	}
}

func TestG11SameProviderRetriesDoNotConsumeProviderBudget(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	clients := map[string]provider.Client{
		"a": errClient(fail500), "b": errClient(fail500),
		"c": errClient(fail500), "d": errClient(fail500),
	}
	eng := NewEngine(clients, nil, Config{HedgeDelay: -1})
	chain := []Attempt{
		{ProviderID: "a", KeyID: "k1", Model: "m"},
		{ProviderID: "a", KeyID: "k2", Model: "m"},
		{ProviderID: "b", Model: "m"},
		{ProviderID: "c", Model: "m"},
		{ProviderID: "d", Model: "m"},
	}
	_, trace, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err == nil {
		t.Fatalf("want an error, got nil")
	}
	// Two keys of a + b + c = 4 attempts over 3 providers; d
	// (the 4th provider) is never tried.
	if len(trace.Attempts) != 4 {
		t.Fatalf("trace attempts = %d, want 4 (a×2 keys, b, c)", len(trace.Attempts))
	}
	fc := clients["d"].(*fakeClient)
	fc.mu.Lock()
	n := fc.chats
	fc.mu.Unlock()
	if n != 0 {
		t.Fatalf("provider d called %d times, want 0", n)
	}
}

func TestG11PrefixedProviderNeverRerouted(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	a := errClient(fail500)
	b := okClient("bee")
	c := okClient("cee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, Config{HedgeDelay: -1})
	// A prefixed provider/model arrives as routing.only naming
	// exactly one provider: only that provider is ever tried,
	// however it fails.
	req := coolReq("m")
	req.Routing = &provider.RequestRouting{Only: []string{"a"}}
	_, _, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c"), req)
	if err == nil {
		t.Fatalf("narrowed failure: want the provider's error, got nil")
	}
	for id, fc := range map[string]*fakeClient{"b": b, "c": c} {
		fc.mu.Lock()
		n := fc.chats
		fc.mu.Unlock()
		if n != 0 {
			t.Fatalf("provider %q called %d times, want 0 (routing.only=[a] is never rerouted)", id, n)
		}
	}
	a.mu.Lock()
	na := a.chats
	a.mu.Unlock()
	if na != 1 {
		t.Fatalf("narrowed provider called %d times, want 1", na)
	}
}

func TestG11ProviderAttemptAllowedUnit(t *testing.T) {
	// routing.only narrowing (unit level, the loop guard itself).
	if providerAttemptAllowed([]string{"a"}, map[string]bool{}, "b", 3) {
		t.Fatalf("only=[a] must refuse provider b")
	}
	if !providerAttemptAllowed([]string{"a"}, map[string]bool{}, "a", 3) {
		t.Fatalf("only=[a] must allow provider a")
	}
	if !providerAttemptAllowed([]string{"a", "b"}, map[string]bool{}, "b", 3) {
		t.Fatalf("only=[a b] must allow provider b")
	}
	// Distinct-provider budget (unit level).
	tried := map[string]bool{"a": true, "b": true, "c": true}
	if providerAttemptAllowed(nil, tried, "d", 3) {
		t.Fatalf("3 providers tried must refuse a 4th")
	}
	if !providerAttemptAllowed(nil, tried, "c", 3) {
		t.Fatalf("a retry on an already-tried provider must be allowed")
	}
}

func TestG11ContinuationPinStillFailsOver(t *testing.T) {
	// Boundary guard for the row-5 fix: the Responses
	// continuation pin (PinProviderID) is a PREFERENCE, not a
	// narrowing — a pinned provider that fails DOES fail over
	// to the rest of the chain (TestResponsesContinuationPin).
	// Only routing.only narrows.
	ctx := context.Background()
	a := errClient(errors.New("boom"))
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	req := coolReq("m")
	req.PinProviderID = "a"
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), req)
	if err != nil || resp.Content != "bee" {
		t.Fatalf("pinned failover = %v, %v, want bee + nil", resp, err)
	}
}

// --- Row 6: failover-before-retry knobs ---

func TestG11FailoverMaxRetriesRespected(t *testing.T) {
	ctx := context.Background()
	fail502 := &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"}
	a := errClient(fail502)
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1, FailoverMaxRetries: 1})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("maxRetries=1 = %v, %v, want bee + nil", resp, err)
	}
	a.mu.Lock()
	na := a.chats
	a.mu.Unlock()
	if na != 2 {
		t.Fatalf("provider a called %d times, want 2 (initial + 1 retry)", na)
	}
}

func TestG11FailoverRetryDelayHonoured(t *testing.T) {
	ctx := context.Background()
	fail502 := &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"}
	a := errClient(fail502)
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1, FailoverMaxRetries: 1, FailoverRetryDelay: 300 * time.Millisecond})
	start := time.Now()
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	elapsed := time.Since(start)
	if err != nil || resp.Content != "bee" {
		t.Fatalf("retryDelay test = %v, %v, want bee + nil", resp, err)
	}
	// First transient retry waits exactly the base (the jitter
	// ladder's first draw is [base, base]); allow 50 ms of
	// scheduling slack below it.
	if elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want ~300ms (the configured retry delay)", elapsed)
	}
}

func TestG11FailoverMaxSetRetriesCapsSet(t *testing.T) {
	ctx := context.Background()
	fail502 := &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"}
	a := errClient(fail502)
	b := errClient(fail502)
	c := okClient("cee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, Config{
		HedgeDelay: -1, FailoverMaxRetries: 2, FailoverMaxSetRetries: 1,
	})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c"), coolReq("m"))
	if err != nil || resp.Content != "cee" {
		t.Fatalf("maxSetRetries=1 = %v, %v, want cee + nil", resp, err)
	}
	counts := map[string]int{}
	for id, fc := range map[string]*fakeClient{"a": a, "b": b, "c": c} {
		fc.mu.Lock()
		counts[id] = fc.chats
		fc.mu.Unlock()
	}
	// a: initial + the single set-wide retry; b: initial only
	// (the set budget is spent); c: the failover.
	if counts["a"] != 2 || counts["b"] != 1 || counts["c"] != 1 {
		t.Fatalf("call counts = %v, want a=2 b=1 c=1", counts)
	}
}

// --- Row 7 (engine store): fallback chains ---

func TestG11FallbackChainStoreRoundTrip(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{})
	if err := eng.SetFallbackChain("gpt-4o", []FallbackEntry{
		{Provider: "b", Priority: 1, Enabled: true},
		{Provider: "a", Priority: 0, Enabled: true},
		{Provider: "c", Priority: 2, Enabled: false},
	}); err != nil {
		t.Fatalf("SetFallbackChain: %v", err)
	}
	atts := eng.FallbackChainFor("gpt-4o")
	if len(atts) != 2 || atts[0].ProviderID != "a" || atts[1].ProviderID != "b" {
		t.Fatalf("FallbackChainFor = %+v, want [a b] in priority order (disabled c skipped)", atts)
	}
	if atts[0].Model != "gpt-4o" {
		t.Fatalf("FallbackChainFor model = %q, want the request model when unpinned", atts[0].Model)
	}
	all := eng.ListFallbackChains()
	if len(all) != 1 || len(all["gpt-4o"]) != 3 {
		t.Fatalf("ListFallbackChains = %v, want one chain of 3 entries (disabled kept)", all)
	}
	if err := eng.SetFallbackChain("gpt-4o", []FallbackEntry{{Provider: "a", Priority: 0, Enabled: true}}); err != nil {
		t.Fatalf("update SetFallbackChain: %v", err)
	}
	if !eng.DeleteFallbackChain("gpt-4o") {
		t.Fatalf("DeleteFallbackChain: want true for an existing chain")
	}
	if eng.DeleteFallbackChain("gpt-4o") {
		t.Fatalf("DeleteFallbackChain: want false for a missing chain")
	}
	if got := eng.FallbackChainFor("gpt-4o"); len(got) != 0 {
		t.Fatalf("FallbackChainFor after delete = %+v, want empty", got)
	}
	if err := eng.SetFallbackChain("", []FallbackEntry{{Provider: "a", Enabled: true}}); err == nil {
		t.Fatalf("empty model: want a validation error")
	}
	if err := eng.SetFallbackChain("m", nil); err == nil {
		t.Fatalf("empty chain: want a validation error")
	}
	if err := eng.SetFallbackChain("m", []FallbackEntry{{Provider: "a"}, {Provider: "a"}}); err == nil {
		t.Fatalf("duplicate provider: want a validation error")
	}
}
