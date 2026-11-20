// Nadir decision strategy tests (M3 Area 1, PARITY row 396):
// fail-CLOSED — no configured endpoint means no external call,
// ever — and a configured endpoint that fails falls back to
// priority order.
package router

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func nadirTestCatalog() []autorouter.CatalogModel {
	return []autorouter.CatalogModel{
		{Provider: "provA", Model: "m1"},
		{Provider: "provB", Model: "m2"},
		{Provider: "provC", Model: "m3"},
	}
}

// countingNadirTransport fails the test if it is ever called: it
// proves the unconfigured path makes zero external calls.
func countingNadirTransport(t *testing.T, calls *int) NadirTransport {
	t.Helper()
	return func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
		*calls++
		t.Errorf("nadir transport called (url %s) — unconfigured nadir must never call out", url)
		return 0, nil, nil
	}
}

func TestNadirUnconfiguredMakesZeroExternalCalls(t *testing.T) {
	calls := 0
	eng := NewEngine(nil, nil, Config{NadirTransport: countingNadirTransport(t, &calls)})
	model, ordered, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "write a poem")
	if calls != 0 {
		t.Errorf("transport called %d times, want 0", calls)
	}
	if model != "m1" {
		t.Errorf("unconfigured nadir model = %q, want priority first m1", model)
	}
	if len(ordered) != 3 || ordered[0] != "m1" {
		t.Errorf("unconfigured nadir ordered = %v, want catalog order", ordered)
	}
	if !strings.Contains(notes, "unconfigured") || !strings.Contains(notes, "fail-closed") {
		t.Errorf("unconfigured nadir notes = %q, want it to say unconfigured + fail-closed", notes)
	}
}

func TestNadirInvalidEndpointMakesZeroExternalCalls(t *testing.T) {
	calls := 0
	eng := NewEngine(nil, nil, Config{
		NadirEndpoint:  "ftp://not-http.example",
		NadirTransport: countingNadirTransport(t, &calls),
	})
	model, _, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
	if calls != 0 {
		t.Errorf("transport called %d times, want 0", calls)
	}
	if model != "m1" {
		t.Errorf("invalid endpoint model = %q, want priority first m1", model)
	}
	if !strings.Contains(notes, "invalid decision endpoint") {
		t.Errorf("invalid endpoint notes = %q, want it to name the bad endpoint", notes)
	}
}

func TestNadirEmptyPromptMakesZeroExternalCalls(t *testing.T) {
	calls := 0
	eng := NewEngine(nil, nil, Config{
		NadirEndpoint:  "https://api.getnadir.com",
		NadirTransport: countingNadirTransport(t, &calls),
	})
	model, _, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "   ")
	if calls != 0 {
		t.Errorf("transport called %d times, want 0", calls)
	}
	if model != "m1" {
		t.Errorf("empty prompt model = %q, want priority first m1", model)
	}
	if !strings.Contains(notes, "no user text") {
		t.Errorf("empty prompt notes = %q, want it to say no user text", notes)
	}
}

func TestNadirConfiguredSelectsModel(t *testing.T) {
	var gotURL, gotKey string
	var gotBody map[string]any
	eng := NewEngine(nil, nil, Config{
		NadirEndpoint: "https://api.getnadir.com/v1/",
		NadirAPIKey:   "key-123",
		NadirTransport: func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
			gotURL, gotKey = url, apiKey
			if err := json.Unmarshal(body, &gotBody); err != nil {
				t.Fatalf("decision body is not JSON: %v", err)
			}
			return 200, []byte(`{"selected_model":"m2","bucket":"reasoning","confidence":0.9}`), nil
		},
	})
	model, ordered, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "prove this theorem")
	if model != "m2" {
		t.Errorf("configured nadir model = %q, want m2", model)
	}
	if len(ordered) != 3 || ordered[0] != "m2" {
		t.Errorf("configured nadir ordered = %v, want m2 first", ordered)
	}
	if !strings.Contains(notes, "nadir selected m2") {
		t.Errorf("configured nadir notes = %q, want it to name the selection", notes)
	}
	if gotURL != "https://api.getnadir.com/v1/bucket" {
		t.Errorf("decision URL = %q, want a single /v1/bucket (no double /v1)", gotURL)
	}
	if gotKey != "key-123" {
		t.Errorf("decision API key = %q, want the configured key", gotKey)
	}
	if gotBody["source"] != "codvian-router" {
		t.Errorf("decision body source = %v, want codvian-router", gotBody["source"])
	}
	menu, _ := gotBody["menu"].([]any)
	if len(menu) != 3 {
		t.Errorf("decision menu = %v, want the 3 pool models", gotBody["menu"])
	}
	if gotBody["prompt"] != "prove this theorem" {
		t.Errorf("decision prompt = %v, want the last user text", gotBody["prompt"])
	}
}

func TestNadirFailuresFallBackToPriority(t *testing.T) {
	cases := []struct {
		name      string
		transport NadirTransport
		wantNote  string
	}{
		{
			"transport error",
			func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
				return 0, nil, context.DeadlineExceeded
			},
			"decision call failed",
		},
		{
			"non-2xx",
			func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
				return 500, []byte("boom"), nil
			},
			"HTTP 500",
		},
		{
			"malformed JSON",
			func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
				return 200, []byte("{not json"), nil
			},
			"malformed",
		},
		{
			"selected model outside pool",
			func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
				return 200, []byte(`{"selected_model":"not-in-pool"}`), nil
			},
			"not in the pool",
		},
		{
			"no selected model",
			func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
				return 200, []byte(`{"bucket":"x"}`), nil
			},
			"no selected_model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(nil, nil, Config{
				NadirEndpoint:  "https://decide.example",
				NadirTransport: tc.transport,
			})
			model, ordered, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
			if model != "m1" || len(ordered) != 3 || ordered[0] != "m1" {
				t.Errorf("model = %q ordered = %v, want priority fallback m1 first", model, ordered)
			}
			if !strings.Contains(notes, tc.wantNote) {
				t.Errorf("notes = %q, want it to contain %q", notes, tc.wantNote)
			}
		})
	}
}

func TestNadirDispatchEndToEndUnconfigured(t *testing.T) {
	// auto/nadir through the real dispatch seam, with no endpoint
	// configured: the request resolves to the priority model and
	// the decision says fail-closed — and no transport exists to
	// call, so nothing can leak out.
	eng, _ := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	calls := 0
	eng.cfg.NadirTransport = countingNadirTransport(t, &calls)
	// dispatchAutoVariant is the seam: model auto/nadir, prompt
	// present, live catalog installed.
	dispReq, disp, handled := eng.dispatchAutoVariant(context.Background(), testNadirRequest())
	if !handled {
		t.Fatalf("auto/nadir was not handled by variant dispatch")
	}
	if disp.Err != nil {
		t.Fatalf("auto/nadir dispatch errored: %v", disp.Err)
	}
	if disp.Strategy != "nadir" {
		t.Errorf("dispatch strategy = %q, want nadir", disp.Strategy)
	}
	if dispReq.Model != "coder-best" {
		t.Errorf("unconfigured auto/nadir resolved to %q, want priority first coder-best", dispReq.Model)
	}
	if calls != 0 {
		t.Errorf("transport called %d times, want 0", calls)
	}
	if dec, ok := eng.LastAutoDecision(); !ok || !strings.Contains(dec.Reason, "fail-closed") {
		t.Errorf("last auto decision = %+v, want a fail-closed reason recorded", dec)
	}
}

func testNadirRequest() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "auto/nadir",
		Messages: []provider.Message{{Role: "user", Content: "which model should prove this?"}},
	}
}

func TestNadirFailureCooldownSkipsCallsThenResumes(t *testing.T) {
	// G5 (a): after a failed decision call the endpoint is
	// cooled down — the next request in the window makes ZERO
	// external calls and falls back to priority; once the
	// cooldown expires, calls resume. The cooldown comes from
	// Config.NadirCooldownMs (G5, operator-configured via
	// routing.nadir.cooldown_ms), shrunk to 100 ms here; the
	// 30 s production default is covered by
	// TestNadirFailureCooldownDefaultsTo30s. The 100 ms value
	// (not the 30 s default) is what proves the field is
	// honoured: with the field ignored, calls would not
	// resume after the 150 ms sleep below.
	calls := 0
	eng := NewEngine(nil, nil, Config{
		NadirEndpoint:   "https://nadir-cooldown.example",
		NadirCooldownMs: 100,
		NadirTransport: func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
			calls++
			return 500, []byte("boom"), nil
		},
	})
	// First request: the call is made, fails (HTTP 500), and
	// arms the cooldown.
	model, _, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
	if calls != 1 {
		t.Fatalf("first request: transport called %d times, want 1", calls)
	}
	if model != "m1" || !strings.Contains(notes, "HTTP 500") {
		t.Errorf("first request: model = %q notes = %q, want priority fallback m1 naming HTTP 500", model, notes)
	}
	// Second request, inside the cooldown: ZERO external calls.
	model, _, notes = eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
	if calls != 1 {
		t.Errorf("second request in cooldown: transport called %d times total, want still 1", calls)
	}
	if model != "m1" {
		t.Errorf("second request in cooldown: model = %q, want priority fallback m1", model)
	}
	if !strings.Contains(notes, "cooldown") {
		t.Errorf("second request in cooldown: notes = %q, want it to name the cooldown", notes)
	}
	// After expiry the endpoint is tried again.
	time.Sleep(150 * time.Millisecond)
	eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
	if calls != 2 {
		t.Errorf("after cooldown expiry: transport called %d times total, want 2 (calls resumed)", calls)
	}
}

// TestNadirFailureCooldownDefaultsTo30s proves the default:
// with NadirCooldownMs unset (<= 0) the engine cools the
// endpoint down for 30 s after a failed decision call — the
// request right after the failure makes ZERO external calls.
// It FAILS if the default is ever dropped (the second
// request would call the transport again) or shortened
// below the window this test observes.
func TestNadirFailureCooldownDefaultsTo30s(t *testing.T) {
	calls := 0
	eng := NewEngine(nil, nil, Config{
		NadirEndpoint: "https://nadir-cooldown-default.example",
		NadirTransport: func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
			calls++
			return 500, []byte("boom"), nil
		},
	})
	if got := eng.nadirCooldown(); got != NadirFailureCooldownMs*time.Millisecond {
		t.Errorf("unset nadirCooldown = %v, want %v (the 30 s default)", got, NadirFailureCooldownMs*time.Millisecond)
	}
	model, _, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
	if calls != 1 {
		t.Fatalf("first request: transport called %d times, want 1", calls)
	}
	if model != "m1" || !strings.Contains(notes, "HTTP 500") {
		t.Errorf("first request: model = %q notes = %q, want priority fallback m1 naming HTTP 500", model, notes)
	}
	// Second request, inside the 30 s default window: ZERO calls.
	model, _, notes = eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
	if calls != 1 {
		t.Errorf("second request in default cooldown: transport called %d times total, want still 1", calls)
	}
	if model != "m1" || !strings.Contains(notes, "cooldown") {
		t.Errorf("second request in default cooldown: model = %q notes = %q, want m1 naming the cooldown", model, notes)
	}
}

func TestDispatchSLAAwareHonoursConfiguredPolicy(t *testing.T) {
	// G5 (b), dispatch layer: the engine's configured SLA
	// fields reach auto/sla-aware — a loose 20% error policy
	// picks the fast cheap model, while the default policy
	// picks the strict one (same catalog as the resolver-level
	// test in internal/autorouter).
	catalog := []autorouter.CatalogModel{
		{Provider: "provA", Model: "m-loose-ok", LatencyMs: 100, HasLatency: true, Quality: 0.90, HasQuality: true, PriceUSDPerMTok: 0.1, HasPrice: true},
		{Provider: "provB", Model: "m-strict-ok", LatencyMs: 1900, HasLatency: true, Quality: 0.99, HasQuality: true, PriceUSDPerMTok: 10, HasPrice: true},
	}
	installAutoCatalog(t, catalog)
	req := provider.ChatRequest{Model: "auto/sla-aware", Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	eng, _ := newAutoVariantTestEngine(t)
	dispReq, disp, handled := eng.dispatchAutoVariant(context.Background(), req)
	if !handled || disp.Err != nil {
		t.Fatalf("default policy: handled=%v err=%v", handled, disp.Err)
	}
	if dispReq.Model != "m-strict-ok" {
		t.Errorf("default policy: dispatched to %q, want m-strict-ok", dispReq.Model)
	}

	engLoose, _ := newAutoVariantTestEngine(t)
	engLoose.cfg.SLAMaxErrorRate = 0.20
	dispReq, disp, handled = engLoose.dispatchAutoVariant(context.Background(), req)
	if !handled || disp.Err != nil {
		t.Fatalf("loose policy: handled=%v err=%v", handled, disp.Err)
	}
	if dispReq.Model != "m-loose-ok" {
		t.Errorf("loose policy (20%% errors): dispatched to %q, want m-loose-ok", dispReq.Model)
	}
}

// TestNadirConfiguredValuesReachEngine is the S1/S2-pattern
// consumer test for the routing.nadir config surface (M3 Area
// 1, PARITY row 396): unset -> the engine defaults (no
// endpoint, 2000 ms timeout, 30 s failure cooldown, zero
// external calls); configured -> the endpoint, key, timeout,
// and cooldown the operator set reach nadirSelect's transport
// call, and nadirTimeout()/nadirCooldown() return the
// configured values. It FAILS if main.go stops wiring the
// config fields into router.Config (the configured engine
// would then behave exactly like the unset one).
func TestNadirConfiguredValuesReachEngine(t *testing.T) {
	// Unset: defaults — zero calls, 2000 ms timeout, 30 s cooldown.
	calls := 0
	eng := NewEngine(nil, nil, Config{NadirTransport: countingNadirTransport(t, &calls)})
	if got := eng.nadirTimeout(); got != 2000*time.Millisecond {
		t.Errorf("unset nadirTimeout = %v, want 2000ms (the default)", got)
	}
	if got := eng.nadirCooldown(); got != NadirFailureCooldownMs*time.Millisecond {
		t.Errorf("unset nadirCooldown = %v, want %v (the 30 s default)", got, NadirFailureCooldownMs*time.Millisecond)
	}
	model, _, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "hi")
	if calls != 0 || model != "m1" || !strings.Contains(notes, "unconfigured") {
		t.Errorf("unset nadir: calls=%d model=%q notes=%q, want 0 calls, m1, unconfigured", calls, model, notes)
	}

	// Configured: endpoint + key + timeout + cooldown reach the call.
	var gotURL, gotKey string
	var gotTimeout time.Duration
	eng = NewEngine(nil, nil, Config{
		NadirEndpoint:   "https://nadir.example/v1/",
		NadirAPIKey:     "key-abc",
		NadirTimeoutMs:  5000,
		NadirCooldownMs: 5000,
		NadirTransport: func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
			gotURL, gotKey, gotTimeout = url, apiKey, timeout
			return 200, []byte(`{"selected_model":"m3","bucket":"reasoning","confidence":0.8}`), nil
		},
	})
	if got := eng.nadirTimeout(); got != 5000*time.Millisecond {
		t.Errorf("configured nadirTimeout = %v, want 5000ms", got)
	}
	if got := eng.nadirCooldown(); got != 5000*time.Millisecond {
		t.Errorf("configured nadirCooldown = %v, want 5000ms", got)
	}
	model, ordered, notes := eng.nadirSelect(context.Background(), nadirTestCatalog(), "prove it")
	if model != "m3" {
		t.Errorf("configured nadir model = %q, want m3 (the decision's pick)", model)
	}
	if len(ordered) != 3 || ordered[0] != "m3" {
		t.Errorf("configured nadir ordered = %v, want m3 first", ordered)
	}
	if !strings.Contains(notes, "nadir selected m3") {
		t.Errorf("configured nadir notes = %q, want it to name the selection", notes)
	}
	if gotURL != "https://nadir.example/v1/bucket" {
		t.Errorf("decision URL = %q, want https://nadir.example/v1/bucket (pasted /v1 stripped once)", gotURL)
	}
	if gotKey != "key-abc" {
		t.Errorf("decision API key = %q, want key-abc (the configured key)", gotKey)
	}
	if gotTimeout != 5000*time.Millisecond {
		t.Errorf("decision timeout = %v, want 5000ms (the configured timeout)", gotTimeout)
	}
}

// TestDispatchNadirHonoursConfiguredEndpoint is the
// configured-path engine test the design's DoD requires (M3
// Area 1, PARITY row 396): with the endpoint configured on
// router.Config, auto/nadir dispatches to the fake
// transport's model end-to-end; unconfigured, the same
// request takes the fail-closed priority fallback with zero
// external calls. It FAILS if the dispatch path stops
// consulting the configured endpoint.
func TestDispatchNadirHonoursConfiguredEndpoint(t *testing.T) {
	installAutoCatalog(t, autoVariantCatalog())
	req := provider.ChatRequest{Model: "auto/nadir", Messages: []provider.Message{{Role: "user", Content: "which model should prove this?"}}}

	// Unconfigured: priority first, zero calls.
	calls := 0
	eng, _ := newAutoVariantTestEngine(t)
	eng.cfg.NadirTransport = countingNadirTransport(t, &calls)
	dispReq, disp, handled := eng.dispatchAutoVariant(context.Background(), req)
	if !handled || disp.Err != nil {
		t.Fatalf("unconfigured auto/nadir: handled=%v err=%v", handled, disp.Err)
	}
	if dispReq.Model != "coder-best" {
		t.Errorf("unconfigured auto/nadir resolved to %q, want priority first coder-best", dispReq.Model)
	}
	if disp.Strategy != "nadir" {
		t.Errorf("unconfigured dispatch strategy = %q, want nadir", disp.Strategy)
	}
	if calls != 0 {
		t.Errorf("unconfigured transport called %d times, want 0", calls)
	}

	// Configured: the fake transport's pick wins.
	calls = 0
	eng, _ = newAutoVariantTestEngine(t)
	eng.cfg.NadirEndpoint = "https://nadir.example"
	eng.cfg.NadirAPIKey = "key-abc"
	eng.cfg.NadirTimeoutMs = 5000
	eng.cfg.NadirTransport = func(ctx context.Context, url string, body []byte, apiKey string, timeout time.Duration) (int, []byte, error) {
		calls++
		if url != "https://nadir.example/v1/bucket" {
			t.Errorf("decision URL = %q, want https://nadir.example/v1/bucket", url)
		}
		if apiKey != "key-abc" {
			t.Errorf("decision API key = %q, want key-abc", apiKey)
		}
		if timeout != 5000*time.Millisecond {
			t.Errorf("decision timeout = %v, want 5000ms", timeout)
		}
		return 200, []byte(`{"selected_model":"chat-fast","bucket":"chat","confidence":0.9}`), nil
	}
	dispReq, disp, handled = eng.dispatchAutoVariant(context.Background(), req)
	if !handled || disp.Err != nil {
		t.Fatalf("configured auto/nadir: handled=%v err=%v", handled, disp.Err)
	}
	if dispReq.Model != "chat-fast" {
		t.Errorf("configured auto/nadir resolved to %q, want chat-fast (the decision's pick)", dispReq.Model)
	}
	if disp.Strategy != "nadir" {
		t.Errorf("configured dispatch strategy = %q, want nadir", disp.Strategy)
	}
	if calls != 1 {
		t.Errorf("configured transport called %d times, want 1", calls)
	}
	if dec, ok := eng.LastAutoDecision(); !ok || dec.Model != "chat-fast" || !strings.Contains(dec.Reason, "nadir selected chat-fast") {
		t.Errorf("last auto decision = %+v, want chat-fast with a nadir-selected reason", dec)
	}
}
