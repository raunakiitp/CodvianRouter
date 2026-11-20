// M6.4 per-request cache controls (server side): header parsing
// (fail-open, never a 400), the chat handler carrying the parsed
// directives to the engine, and the messages-relay cache honouring
// them on its own lookup/store path.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func m64Req(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestM64ParseCacheControls(t *testing.T) {
	f32 := func(v float32) *float32 { return &v }
	cases := []struct {
		name    string
		headers map[string]string
		wantNil bool
		check   func(t *testing.T, cc *provider.CacheControls)
	}{
		{name: "no headers", headers: nil, wantNil: true},
		{name: "unrelated header only", headers: map[string]string{"X-Foo": "bar"}, wantNil: true},
		{
			name:    "X-Codvian-No-Cache true",
			headers: map[string]string{"X-Codvian-No-Cache": "true"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.BypassCache() {
					t.Fatal("BypassCache() = false, want true")
				}
			},
		},
		{
			name:    "X-Codvian-No-Cache 1",
			headers: map[string]string{"X-Codvian-No-Cache": "1"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.Bypass {
					t.Fatal("Bypass = false, want true")
				}
			},
		},
		{
			name:    "X-Codvian-No-Cache false is not bypass",
			headers: map[string]string{"X-Codvian-No-Cache": "false"},
			wantNil: true,
		},
		{
			name:    "OmniRoute alias bypasses",
			headers: map[string]string{"X-OmniRoute-No-Cache": "TRUE"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.Bypass {
					t.Fatal("Bypass = false, want true (alias)")
				}
			},
		},
		{
			name:    "Cache-Control no-cache bypasses",
			headers: map[string]string{"Cache-Control": "max-age=0, no-cache"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.Bypass {
					t.Fatal("Bypass = false, want true")
				}
			},
		},
		{
			name:    "Cache-Control no-cache-foo does not bypass",
			headers: map[string]string{"Cache-Control": "no-cache-foo"},
			wantNil: true,
		},
		{
			name:    "Pragma no-cache bypasses",
			headers: map[string]string{"Pragma": "no-cache"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.Bypass {
					t.Fatal("Bypass = false, want true")
				}
			},
		},
		{
			name:    "Cache-Control no-store is no-store only",
			headers: map[string]string{"Cache-Control": "no-store"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.NoStore || cc.Bypass {
					t.Fatalf("NoStore=%v Bypass=%v, want true/false", cc.NoStore, cc.Bypass)
				}
				if cc.BypassCache() {
					t.Fatal("BypassCache() must be false for no-store")
				}
				if cc.AllowStore() {
					t.Fatal("AllowStore() must be false for no-store")
				}
			},
		},
		{
			name:    "X-Codvian-Cache-No-Store true",
			headers: map[string]string{"X-Codvian-Cache-No-Store": "yes"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.NoStore {
					t.Fatal("NoStore = false, want true")
				}
			},
		},
		{
			name:    "cache-type semantic",
			headers: map[string]string{"X-Codvian-Cache-Type": "semantic"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if cc.Type != "semantic" || !cc.UseSemantic() || cc.UseExact() {
					t.Fatalf("Type=%q UseSemantic=%v UseExact=%v, want semantic/true/false", cc.Type, cc.UseSemantic(), cc.UseExact())
				}
			},
		},
		{
			name:    "cache-type NONE case-insensitive is bypass",
			headers: map[string]string{"X-Codvian-Cache-Type": "NONE"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if !cc.BypassCache() {
					t.Fatal("BypassCache() = false, want true for type=none")
				}
			},
		},
		{
			name:    "cache-type garbage ignored",
			headers: map[string]string{"X-Codvian-Cache-Type": "everything"},
			wantNil: true,
		},
		{
			name:    "cache-key accepted",
			headers: map[string]string{"X-Codvian-Cache-Key": "bucket-42"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if cc.KeyOverride != "bucket-42" {
					t.Fatalf("KeyOverride = %q, want bucket-42", cc.KeyOverride)
				}
				if cc.UseSemantic() {
					t.Fatal("UseSemantic() must be false with a key override")
				}
			},
		},
		{
			name:    "cache-key with space ignored",
			headers: map[string]string{"X-Codvian-Cache-Key": "two words"},
			wantNil: true,
		},
		{
			name:    "cache-key too long ignored",
			headers: map[string]string{"X-Codvian-Cache-Key": strings.Repeat("x", 129)},
			wantNil: true,
		},
		{
			name:    "threshold 0.9 kept",
			headers: map[string]string{"X-Codvian-Cache-Threshold": "0.9"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				v, ok := cc.ThresholdFloor()
				if !ok || v != 0.9 {
					t.Fatalf("ThresholdFloor() = %v,%v, want 0.9,true", v, ok)
				}
			},
		},
		{
			name:    "threshold above 1 clamped",
			headers: map[string]string{"X-Codvian-Cache-Threshold": "1.5"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				v, ok := cc.ThresholdFloor()
				if !ok || v != 1.0 {
					t.Fatalf("ThresholdFloor() = %v,%v, want 1.0,true", v, ok)
				}
			},
		},
		{
			name:    "threshold below 0 clamped",
			headers: map[string]string{"X-Codvian-Cache-Threshold": "-0.2"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				v, ok := cc.ThresholdFloor()
				if !ok || v != 0 {
					t.Fatalf("ThresholdFloor() = %v,%v, want 0,true", v, ok)
				}
			},
		},
		{
			name:    "threshold garbage ignored",
			headers: map[string]string{"X-Codvian-Cache-Threshold": "high"},
			wantNil: true,
		},
		{
			name:    "threshold NaN ignored",
			headers: map[string]string{"X-Codvian-Cache-Threshold": "NaN"},
			wantNil: true,
		},
		{
			name:    "ttl 60 kept",
			headers: map[string]string{"X-Codvian-Cache-TTL": "60"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if cc.EffectiveTTL() != 60*time.Second {
					t.Fatalf("EffectiveTTL() = %v, want 60s", cc.EffectiveTTL())
				}
			},
		},
		{
			name:    "ttl above cap clamps to 1h",
			headers: map[string]string{"X-Codvian-Cache-TTL": "7200"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if cc.EffectiveTTL() != time.Hour {
					t.Fatalf("EffectiveTTL() = %v, want 1h (cap)", cc.EffectiveTTL())
				}
			},
		},
		{
			name:    "ttl huge clamps to 1h",
			headers: map[string]string{"X-Codvian-Cache-TTL": "1e20"},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if cc.EffectiveTTL() != time.Hour {
					t.Fatalf("EffectiveTTL() = %v, want 1h (cap)", cc.EffectiveTTL())
				}
			},
		},
		{
			name:    "ttl zero ignored",
			headers: map[string]string{"X-Codvian-Cache-TTL": "0"},
			wantNil: true,
		},
		{
			name:    "ttl garbage ignored",
			headers: map[string]string{"X-Codvian-Cache-TTL": "soon"},
			wantNil: true,
		},
		{
			name: "invalid threshold dropped, valid key kept",
			headers: map[string]string{
				"X-Codvian-Cache-Threshold": "high",
				"X-Codvian-Cache-Key":       "bucket-7",
			},
			check: func(t *testing.T, cc *provider.CacheControls) {
				if cc.KeyOverride != "bucket-7" {
					t.Fatalf("KeyOverride = %q, want bucket-7", cc.KeyOverride)
				}
				if _, ok := cc.ThresholdFloor(); ok {
					t.Fatal("ThresholdFloor must be unset (invalid value ignored)")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := parseCacheControls(m64Req(tc.headers))
			if tc.wantNil {
				if cc != nil {
					t.Fatalf("parseCacheControls = %+v, want nil", cc)
				}
				return
			}
			if cc == nil {
				t.Fatal("parseCacheControls = nil, want directives")
			}
			if tc.check != nil {
				tc.check(t, cc)
			}
		})
	}
	// Direct-construction backstops on the provider type.
	if got := (&provider.CacheControls{TTL: 2 * time.Hour}).EffectiveTTL(); got != time.Hour {
		t.Fatalf("EffectiveTTL 2h = %v, want 1h cap", got)
	}
	if got := (&provider.CacheControls{TTL: 0}).EffectiveTTL(); got != 0 {
		t.Fatalf("EffectiveTTL 0 = %v, want 0 (default)", got)
	}
	if _, ok := (&provider.CacheControls{Threshold: f32(2)}).ThresholdFloor(); ok {
		t.Fatal("ThresholdFloor 2.0 must be unset")
	}
	if (&provider.CacheControls{}).BypassCache() || !(&provider.CacheControls{}).AllowStore() {
		t.Fatal("zero CacheControls must mean default caching")
	}
	var nilCC *provider.CacheControls
	if nilCC.BypassCache() || !nilCC.AllowStore() || !nilCC.UseExact() || !nilCC.UseSemantic() {
		t.Fatal("nil CacheControls must mean default caching")
	}
}

// m64RecordingEngine records the CacheControls each Chat call
// arrived with, so the test can prove the chat handler carried
// the parsed headers to the engine.
type m64RecordingEngine struct {
	*fakeEngine
	last  *provider.CacheControls
	calls int
}

func (e *m64RecordingEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.calls++
	e.last = req.CacheControls
	return e.fakeEngine.Chat(ctx, req)
}

func m64ChatBody() string {
	return `{"model":"model-a","messages":[{"role":"user","content":"What is 2+2?"}]}`
}

func m64DoChat(t *testing.T, srv *Server, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(m64ChatBody()))
	req.Header.Set("Authorization", "Bearer test-key-123")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestM64ChatHandlerCarriesCacheControls proves, end to end
// through the real chat handler with a fake engine, that the
// bypass/control headers land on ChatRequest.CacheControls —
// including that an invalid value is ignored without a 400.
func TestM64ChatHandlerCarriesCacheControls(t *testing.T) {
	eng := &m64RecordingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "4", Model: "model-a"}}}
	srv := newTestServer(t, eng, WithModels("model-a"))

	rec := m64DoChat(t, srv, map[string]string{"X-Codvian-No-Cache": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("bypass chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if eng.last == nil || !eng.last.Bypass {
		t.Fatalf("engine saw CacheControls = %+v, want Bypass=true", eng.last)
	}

	rec = m64DoChat(t, srv, map[string]string{
		"X-Codvian-Cache-Type":      "semantic",
		"X-Codvian-Cache-Key":       "bucket-9",
		"X-Codvian-Cache-Threshold": "0.95",
		"X-Codvian-Cache-TTL":       "120",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("controls chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if eng.last == nil {
		t.Fatal("engine saw nil CacheControls, want directives")
	}
	if eng.last.Type != "semantic" || eng.last.KeyOverride != "bucket-9" {
		t.Fatalf("engine saw CacheControls = %+v, want type=semantic key=bucket-9", eng.last)
	}
	if v, ok := eng.last.ThresholdFloor(); !ok || v != 0.95 {
		t.Fatalf("engine saw threshold = %v,%v, want 0.95,true", v, ok)
	}
	if eng.last.EffectiveTTL() != 120*time.Second {
		t.Fatalf("engine saw TTL = %v, want 120s", eng.last.EffectiveTTL())
	}

	// Invalid threshold is ignored, not a 400 — and the valid
	// no-store header on the same request still lands.
	rec = m64DoChat(t, srv, map[string]string{
		"X-Codvian-Cache-Threshold": "high",
		"X-Codvian-Cache-No-Store":  "true",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("invalid-threshold chat status = %d, body = %s (invalid values must never 400)", rec.Code, rec.Body.String())
	}
	if eng.last == nil || !eng.last.NoStore {
		t.Fatalf("engine saw CacheControls = %+v, want NoStore=true", eng.last)
	}
	if _, ok := eng.last.ThresholdFloor(); ok {
		t.Fatal("engine saw a threshold, want none (invalid value ignored)")
	}

	// No cache headers at all: nil controls, default caching.
	rec = m64DoChat(t, srv, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if eng.last != nil {
		t.Fatalf("engine saw CacheControls = %+v, want nil (no headers)", eng.last)
	}
}

// m64RelayReq builds a canonical request for the relay cache
// helpers: model + one user turn, tenant "gk-alice".
func m64RelayReq(cc *provider.CacheControls) provider.ChatRequest {
	return provider.ChatRequest{
		Model:         "test-model",
		Messages:      []provider.Message{{Role: "user", Content: "What is 2+2?"}},
		CallerID:      "gk-alice",
		CacheControls: cc,
	}
}

func m64RelayServer() *Server {
	return &Server{
		exactCache:    cache.NewExactCache(100),
		semanticCache: cache.NewLocalSemanticCache(),
	}
}

// TestM64RelayLookupHonoursBypass seeds the relay exact cache
// directly, then proves a bypass request does not see the entry
// while a normal request does.
func TestM64RelayLookupHonoursBypass(t *testing.T) {
	s := m64RelayServer()
	ctx := context.Background()
	req := m64RelayReq(nil)
	key, err := scopedMessagesKey(req)
	if err != nil {
		t.Fatalf("scopedMessagesKey: %v", err)
	}
	s.exactCache.Set(ctx, key, cache.CachedResponse{Content: "4", Model: "test-model"}, time.Hour)

	if hit, layer, found := s.lookupMessagesCache(ctx, req); !found || layer != "exact" || hit.Content != "4" {
		t.Fatalf("normal relay lookup = %+v,%q,%v, want 4,exact,true", hit, layer, found)
	}
	bypass := m64RelayReq(&provider.CacheControls{Bypass: true})
	if _, _, found := s.lookupMessagesCache(ctx, bypass); found {
		t.Fatal("bypass relay lookup hit, want miss")
	}
	none := m64RelayReq(&provider.CacheControls{Type: "none"})
	if _, _, found := s.lookupMessagesCache(ctx, none); found {
		t.Fatal("type=none relay lookup hit, want miss")
	}
}

// TestM64RelayStoreHonoursNoStoreAndBypass proves the relay's
// independent store path (storeMessagesCache) refuses to store
// for bypass / no-store requests and stores for normal ones.
func TestM64RelayStoreHonoursNoStoreAndBypass(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"4"}],"model":"test-model","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`
	ctx := context.Background()

	seeded := func(t *testing.T, cc *provider.CacheControls) bool {
		t.Helper()
		s := m64RelayServer()
		s.storeMessagesCache(ctx, m64RelayReq(cc), []byte(body))
		_, _, found := s.lookupMessagesCache(ctx, m64RelayReq(nil))
		return found
	}
	if !seeded(t, nil) {
		t.Fatal("normal relay store: entry missing from lookup afterwards")
	}
	if seeded(t, &provider.CacheControls{Bypass: true}) {
		t.Fatal("bypass relay store: entry must not have been stored")
	}
	if seeded(t, &provider.CacheControls{NoStore: true}) {
		t.Fatal("no-store relay store: entry must not have been stored")
	}
}

// TestM64RelayKeyOverrideIsTenantScoped proves the relay's exact
// key honours the override under tenant scoping, identical to the
// chat path: same override + same tenant hits across prompts,
// another tenant misses.
func TestM64RelayKeyOverrideIsTenantScoped(t *testing.T) {
	s := m64RelayServer()
	ctx := context.Background()
	alice := m64RelayReq(&provider.CacheControls{KeyOverride: "shared-bucket"})
	key, err := scopedMessagesKey(alice)
	if err != nil {
		t.Fatalf("scopedMessagesKey: %v", err)
	}
	s.exactCache.Set(ctx, key, cache.CachedResponse{Content: "4", Model: "test-model"}, time.Hour)

	// Same tenant, different prompt, same override: hit.
	aliceOther := provider.ChatRequest{
		Model:         "test-model",
		Messages:      []provider.Message{{Role: "user", Content: "Something else entirely?"}},
		CallerID:      "gk-alice",
		CacheControls: &provider.CacheControls{KeyOverride: "shared-bucket"},
	}
	if _, _, found := s.lookupMessagesCache(ctx, aliceOther); !found {
		t.Fatal("same-tenant same-override other-prompt relay lookup missed, want hit")
	}
	// Other tenant, same override, same prompt: miss.
	bob := m64RelayReq(&provider.CacheControls{KeyOverride: "shared-bucket"})
	bob.CallerID = "gk-bob"
	if _, _, found := s.lookupMessagesCache(ctx, bob); found {
		t.Fatal("other-tenant same-override relay lookup hit, want miss")
	}
	// Same tenant, same prompt, NO override: miss (different key).
	if _, _, found := s.lookupMessagesCache(ctx, m64RelayReq(nil)); found {
		t.Fatal("no-override relay lookup hit the override entry, want miss")
	}
}
