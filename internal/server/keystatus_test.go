package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// GET /v1/me/status + key expiry/regeneration + per-key traffic
// limits (CHECKLIST 195-196). The tests drive the real Server +
// real ledger + counting engine: "refused pre-call" is proven by
// the engine's call count staying at zero, and "old key dead" is
// proven by a real 401 on the next request with it.

func keyStatusSetup(t *testing.T) (*keyBudgetSetup, string) {
	t.Helper()
	s := newKeyBudgetSetup(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	return s, cfgPath
}

// persistOpt wires a config persist that saves to cfgPath, so
// rotate/update tests exercise the real persist + rollback path.
func (s *keyBudgetSetup) persistOpt(cfgPath string) Option {
	return WithConfigPersist(func() error { return s.cfg.Save(cfgPath) })
}

func TestMeStatusShapeNoLeak(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.10 cap
	// Give the key an expiry + traffic limits in the config.
	exp := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].ExpiresAt = &exp
			s.cfg.GatewayKeys[i].RequestsPerMinute = 60
			s.cfg.GatewayKeys[i].TokensPerMinute = 1000
		}
	}
	rec := doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The raw key and the full stored hash must never appear.
	if strings.Contains(body, s.raw) {
		t.Fatal("me/status response contains the raw gateway key")
	}
	key, _ := s.cfg.FindGatewayKey(s.id)
	if strings.Contains(body, key.KeyHash) {
		t.Fatal("me/status response contains the full stored key hash")
	}
	var st meStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode me/status: %v: %s", err, body)
	}
	if st.KeyID != s.id {
		t.Errorf("key_id = %q, want %q", st.KeyID, s.id)
	}
	if st.Fingerprint != "sha256:"+key.KeyHash[:12] {
		t.Errorf("fingerprint = %q, want sha256:%s", st.Fingerprint, key.KeyHash[:12])
	}
	if st.Name != "test" {
		t.Errorf("name = %q, want test", st.Name)
	}
	if st.ExpiresAt == nil || !st.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at = %v, want %v", st.ExpiresAt, exp)
	}
	if st.Expired {
		t.Error("a key expiring in 24h must not report expired")
	}
	if st.ExpiresInSeconds == nil || *st.ExpiresInSeconds <= 0 || *st.ExpiresInSeconds > 24*3600 {
		t.Errorf("expires_in_seconds = %v, want within (0, 86400]", st.ExpiresInSeconds)
	}
	if st.Budget == nil {
		t.Fatal("budget must be present with a ledger wired")
	}
	if math.Abs(st.Budget.SpentUSD-0.04) > 1e-9 {
		t.Errorf("budget.spent_usd = %v, want 0.04", st.Budget.SpentUSD)
	}
	if st.Budget.LimitUSD == nil || math.Abs(*st.Budget.LimitUSD-0.10) > 1e-9 {
		t.Errorf("budget.limit_usd = %v, want 0.10", st.Budget.LimitUSD)
	}
	if st.Budget.RemainingUSD == nil || math.Abs(*st.Budget.RemainingUSD-0.06) > 1e-9 {
		t.Errorf("budget.remaining_usd = %v, want 0.06", st.Budget.RemainingUSD)
	}
	if st.Traffic.RequestsPerMinute != 60 || st.Traffic.TokensPerMinute != 1000 {
		t.Errorf("traffic limits = %d/%d, want 60/1000", st.Traffic.RequestsPerMinute, st.Traffic.TokensPerMinute)
	}
	if st.Traffic.RequestsLastMinute != 0 || st.Traffic.TokensLastMinute != 0 {
		t.Errorf("traffic usage before any request = %d/%d, want 0/0", st.Traffic.RequestsLastMinute, st.Traffic.TokensLastMinute)
	}
	if st.Traffic.RequestsRemaining == nil || *st.Traffic.RequestsRemaining != 60 {
		t.Errorf("traffic.requests_remaining = %v, want 60", st.Traffic.RequestsRemaining)
	}
}

func TestMeStatusAfterTraffic(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	body := `{"model":"unpriced-model","messages":[{"role":"user","content":"hello world"}]}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	count, err := tokencount.CountPromptFull("unpriced-model", provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: "hello world"}}})
	if err != nil {
		t.Fatal(err)
	}
	if count.Tokens == 1 {
		t.Fatal("test setup: the local prompt estimate must differ from the fake upstream's reported 1 prompt token, or the true-up is unproven")
	}
	rec = doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st meStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode me/status: %v", err)
	}
	if st.Traffic.RequestsLastMinute != 1 {
		t.Errorf("requests_last_minute = %d, want 1", st.Traffic.RequestsLastMinute)
	}
	// M5.3 TPM true-up: the slot was admitted on the local
	// estimate (count.Tokens) and corrected post-response to the
	// fake upstream's reported prompt tokens (1) — the axis
	// tracks real consumption, not the estimate.
	if st.Traffic.TokensLastMinute != 1 {
		t.Errorf("tokens_last_minute = %d, want 1 (provider-reported prompt tokens after true-up; local estimate was %d)", st.Traffic.TokensLastMinute, count.Tokens)
	}
}

func TestMeStatusNoKey401(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, "GET", "/v1/me/status", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	rec = doReq(t, srv, "GET", "/v1/me/status", "", "crk_wrongkey")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "expired") {
		t.Error("a wrong key must not be reported as expired")
	}
}

func TestExpiredKey401NamesExpiry(t *testing.T) {
	s, _ := keyStatusSetup(t)
	dead, rawDead, err := config.IssueKey("dead", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	dead.ExpiresAt = &past
	if err := s.cfg.AddGatewayKey(dead); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	// Chat with the expired key: honest 401 naming expiry, zero
	// provider calls.
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), rawDead)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key chat: status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "expired") {
		t.Errorf("expired key chat body %s must name the expiry", rec.Body.String())
	}
	// The server renders expiry in UTC (server.go: "gateway key expired on
	// "+exp.UTC().Format(time.RFC3339)), so the assertion must use the same
	// canonical rendering — past.Format would render in the machine-local
	// zone and fail on any non-UTC host for the same instant.
	if !strings.Contains(rec.Body.String(), past.UTC().Format(time.RFC3339)) {
		t.Errorf("expired key chat body %s must name the expiry time %s", rec.Body.String(), past.UTC().Format(time.RFC3339))
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the expired key must be refused before any provider is called", s.eng.chatCalls.Load())
	}
	// The status endpoint itself answers the same 401.
	rec = doReq(t, srv, "GET", "/v1/me/status", "", rawDead)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("expired key me/status: status = %d, want 401 naming expiry: %s", rec.Code, rec.Body.String())
	}
	// The live key still works.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("live key chat: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyRotateOldDeadNewLive(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	// Give the key limits, so the rotation must inherit them.
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].RequestsPerMinute = 30
			s.cfg.GatewayKeys[i].TokensPerMinute = 500
		}
	}
	rec := doReq(t, srv, "POST", "/admin/keys/"+s.id+"/rotate", `{}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var rot keyRotateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rot); err != nil {
		t.Fatalf("decode rotate response: %v: %s", err, rec.Body.String())
	}
	if rot.RotatedFrom != s.id {
		t.Errorf("rotated_from = %q, want %q", rot.RotatedFrom, s.id)
	}
	if rot.KeyID == s.id || rot.KeyID == "" {
		t.Errorf("new key_id = %q, want a fresh id", rot.KeyID)
	}
	if !strings.HasPrefix(rot.GatewayKey, "crk_") || len(rot.GatewayKey) != 36 {
		t.Errorf("new raw key %q must be a fresh crk_ key", rot.GatewayKey)
	}
	if rot.GatewayKey == s.raw {
		t.Error("rotation must issue a different key value")
	}
	// The old key is dead immediately.
	rec = doReq(t, srv, "GET", "/v1/models", "", s.raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old key after rotation: status = %d, want 401", rec.Code)
	}
	// The new key works (chat scope: the rotated key inherits the
	// old key's ["chat"] scopes, so it proves liveness on chat,
	// not on /v1/models which needs the "models" scope).
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), rot.GatewayKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("new key: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The dashboard feed lists the new key (with inherited limits)
	// and not the old one. The feed also lists the admin key this
	// test added to call the Tier 2 rotate route, so the rotated
	// key is found by id rather than by position.
	rec = doReq(t, srv, "GET", "/dashboard/keys", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("keys feed: status = %d", rec.Code)
	}
	var keys routerKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode keys feed: %v", err)
	}
	var rotated *routerKeyView
	for i := range keys.Keys {
		if keys.Keys[i].ID == rot.KeyID {
			rotated = &keys.Keys[i]
		}
		if keys.Keys[i].ID == s.id {
			t.Fatalf("keys feed still lists the revoked key %s: %+v", s.id, keys.Keys)
		}
	}
	if rotated == nil {
		t.Fatalf("keys feed = %+v, want the rotated key %s", keys.Keys, rot.KeyID)
	}
	if rotated.RequestsPerMinute != 30 || rotated.TokensPerMinute != 500 {
		t.Errorf("rotated key limits = %d/%d, want 30/500 (inherited)", rotated.RequestsPerMinute, rotated.TokensPerMinute)
	}
	if strings.Contains(rec.Body.String(), rot.GatewayKey) {
		t.Fatal("keys feed must never contain the raw rotated key")
	}
	// The rotation survived to the config file: reload and the new
	// key verifies, the old one does not.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if _, ok := reloaded.FindGatewayKey(rot.KeyID); !ok {
		t.Error("reloaded config lost the rotated key")
	}
	if _, ok := reloaded.FindGatewayKey(s.id); ok {
		t.Error("reloaded config still holds the revoked key")
	}
}

func TestKeyRotateUnknownKeyAndNoPersist(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doReq(t, srv, "POST", "/admin/keys/gk_doesnotexist/rotate", `{}`, adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key rotate: status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// Without a persist function the endpoint refuses rather than
	// minting a key that would vanish on restart.
	noPersist := s.server(t)
	rec = doReq(t, noPersist, "POST", "/admin/keys/"+s.id+"/rotate", `{}`, adminRaw)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rotate without persist: status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyUpdateSetsExpiryAndLimits(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	exp := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	body := `{"expires_at":"` + exp.Format(time.RFC3339) + `","requests_per_minute":10,"tokens_per_minute":2000}`
	rec := doReq(t, srv, "POST", "/admin/keys/"+s.id, body, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var upd keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if !upd.PersistedToConfig {
		t.Error("persisted_to_config must be true with a persist function wired")
	}
	if upd.Key.ExpiresAt == nil || !upd.Key.ExpiresAt.Equal(exp) {
		t.Errorf("updated expires_at = %v, want %v", upd.Key.ExpiresAt, exp)
	}
	if upd.Key.RequestsPerMinute != 10 || upd.Key.TokensPerMinute != 2000 {
		t.Errorf("updated limits = %d/%d, want 10/2000", upd.Key.RequestsPerMinute, upd.Key.TokensPerMinute)
	}
	// The dashboard feed shows the same values (found by id: the
	// feed also lists the admin key this test added).
	rec = doReq(t, srv, "GET", "/dashboard/keys", "", "")
	var keys routerKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode keys feed: %v", err)
	}
	var updatedKey *routerKeyView
	for i := range keys.Keys {
		if keys.Keys[i].ID == s.id {
			updatedKey = &keys.Keys[i]
		}
	}
	if updatedKey == nil {
		t.Fatalf("keys feed after update = %+v, want key %s", keys.Keys, s.id)
	}
	if updatedKey.RequestsPerMinute != 10 || updatedKey.TokensPerMinute != 2000 {
		t.Fatalf("keys feed limits = %d/%d, want 10/2000", updatedKey.RequestsPerMinute, updatedKey.TokensPerMinute)
	}
	// Clear the expiry again.
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"clear_expiry":true}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear expiry: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var cleared keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cleared); err != nil {
		t.Fatalf("decode clear response: %v", err)
	}
	if cleared.Key.ExpiresAt != nil {
		t.Errorf("expires_at after clear = %v, want nil", cleared.Key.ExpiresAt)
	}
	// Validation: negative limits and unknown keys.
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"requests_per_minute":-1}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative rpm: status = %d, want 400", rec.Code)
	}
	rec = doReq(t, srv, "POST", "/admin/keys/gk_doesnotexist", `{"requests_per_minute":5}`, adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key update: status = %d, want 404", rec.Code)
	}
	// A past expiry takes effect immediately and says so.
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"expires_at":"`+past+`"}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("past expiry update: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var pastUpd keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pastUpd); err != nil {
		t.Fatalf("decode past-expiry response: %v", err)
	}
	if !pastUpd.Key.Expired || !strings.Contains(pastUpd.Note, "expired") {
		t.Errorf("past-expiry update = expired=%v note=%q, want expired=true naming expiry", pastUpd.Key.Expired, pastUpd.Note)
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("key with a past expiry: chat status = %d, want 401 naming expiry: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyTrafficLimitRPM429(t *testing.T) {
	s, _ := keyStatusSetup(t)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].RequestsPerMinute = 2
		}
	}
	srv := s.server(t)
	body := kbChatBody("unpriced-model", false)
	for i := 0; i < 2; i++ {
		rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request over a 2/min limit: status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requests per minute") {
		t.Errorf("429 body %s must name the requests-per-minute limit", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_rate_limit") {
		t.Errorf("429 body %s must carry the key_rate_limit type (distinct from an upstream 429)", rec.Body.String())
	}
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("429 must carry a Retry-After header")
	}
	secs, err := strconv.Atoi(retryAfter)
	if err != nil || secs < 1 || secs > 60 {
		t.Errorf("Retry-After = %q, want an integer within (0, 60]", retryAfter)
	}
	if s.eng.chatCalls.Load() != 2 {
		t.Fatalf("engine Chat calls = %d, want 2 — the limited request must be refused before any provider is called", s.eng.chatCalls.Load())
	}
}

func TestKeyTrafficLimitTPM429(t *testing.T) {
	s, _ := keyStatusSetup(t)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].TokensPerMinute = 5
		}
	}
	srv := s.server(t)
	// "hello world, this is a longer prompt with several words in it"
	// counts well over 5 local tokens, so the first request is
	// already over the token budget.
	body := `{"model":"unpriced-model","messages":[{"role":"user","content":"hello world, this is a longer prompt with several words in it"}]}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-TPM request: status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tokens per minute") {
		t.Errorf("429 body %s must name the tokens-per-minute limit", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry a Retry-After header")
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the limited request must be refused before any provider is called", s.eng.chatCalls.Load())
	}
}

func TestKeyTrafficLimitUnlimitedKeyRoutes(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	body := kbChatBody("unpriced-model", false)
	for i := 0; i < 5; i++ {
		rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d with no limits: status = %d, want 200", i+1, rec.Code)
		}
	}
	if s.eng.chatCalls.Load() != 5 {
		t.Fatalf("engine Chat calls = %d, want 5", s.eng.chatCalls.Load())
	}
}

// adminKeyFor adds a second gateway key carrying the admin scope to
// the setup's config and returns its raw value. Key-mutation
// endpoints are Tier 2 admin-scoped (M1.1), so tests that exercise
// them must authenticate as an admin key, not as the chat-only
// fixture key.
func adminKeyFor(t *testing.T, s *keyBudgetSetup) string {
	t.Helper()
	key, raw, err := config.IssueKey("admin", []string{"chat", "models", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestKeyDisableImmediate401 (M1.3 acceptance): disabling a key via
// POST /admin/keys/{id} {"disabled":true} takes effect on the very
// next request — no restart — with a 401 naming the disable, and
// an audit line is written. Re-enabling restores the key, and the
// disabled flag survives a config reload.
func TestKeyDisableImmediate401(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	var audit bytes.Buffer
	srv := s.server(t, s.persistOpt(cfgPath), WithLogger(log.New(&audit, "", 0)))
	// The fixture key works before the disable.
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat before disable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Disable it.
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"disabled":true}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var upd keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("decode disable response: %v: %s", err, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(upd.Note), "disabled") {
		t.Errorf("disable note = %q, want it to say the key is disabled", upd.Note)
	}
	if !strings.Contains(audit.String(), "disabled") || !strings.Contains(audit.String(), s.id) {
		t.Errorf("audit log = %q, want a disable entry naming key %s", audit.String(), s.id)
	}
	// The very next request with the key: 401 naming the disable,
	// zero additional provider calls.
	callsBefore := s.eng.chatCalls.Load()
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("chat after disable: status %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "disabled") {
		t.Errorf("disabled key 401 body %q must name the disable", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != callsBefore {
		t.Fatalf("engine Chat calls = %d, want %d — the disabled key must be refused before any provider is called", s.eng.chatCalls.Load(), callsBefore)
	}
	rec = doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(strings.ToLower(rec.Body.String()), "disabled") {
		t.Fatalf("me/status after disable: status = %d, want 401 naming disabled: %s", rec.Code, rec.Body.String())
	}
	// The disabled flag survived to the config file.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	reloadedKey, ok := reloaded.FindGatewayKey(s.id)
	if !ok || !reloadedKey.Disabled {
		t.Errorf("reloaded key disabled = %v (found=%v), want true", reloadedKey.Disabled, ok)
	}
	// The admin key still works throughout.
	rec = doReq(t, srv, "GET", "/admin/status", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status while a key is disabled: status %d, want 200", rec.Code)
	}
	// Re-enable: the key works again, immediately.
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"disabled":false}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat after re-enable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestKeyDeleteRevokesPermanently (M1.3 acceptance): DELETE
// /admin/keys/{id} with the typed phrase "DELETE <id>" removes the
// key for good — it stops authenticating at once, is gone from the
// reloaded config file, and a second delete is an honest 404. The
// wrong phrase is a 400, no key is a 401, and a non-admin key is
// a 403.
func TestKeyDeleteRevokesPermanently(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	var audit bytes.Buffer
	srv := s.server(t, s.persistOpt(cfgPath), WithLogger(log.New(&audit, "", 0)))
	phrase := `{"confirm":"DELETE ` + s.id + `"}`
	// Wrong phrase: 400, key untouched.
	rec := doReq(t, srv, "DELETE", "/admin/keys/"+s.id, `{"confirm":"DELETE wrong"}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong phrase: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// No key: 401.
	rec = doReq(t, srv, "DELETE", "/admin/keys/"+s.id, phrase, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401", rec.Code)
	}
	// Chat-only (non-admin) key: 403 — auth passed, scope refused.
	rec = doReq(t, srv, "DELETE", "/admin/keys/"+s.id, phrase, s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin key: status %d, want 403", rec.Code)
	}
	// Correct phrase as admin: 200, audit entry written.
	rec = doReq(t, srv, "DELETE", "/admin/keys/"+s.id, phrase, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(audit.String(), "revoked") || !strings.Contains(audit.String(), s.id) {
		t.Errorf("audit log = %q, want a revoke entry naming key %s", audit.String(), s.id)
	}
	// The key is dead immediately, and gone from the file.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("chat after delete: status %d, want 401: %s", rec.Code, rec.Body.String())
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if _, ok := reloaded.FindGatewayKey(s.id); ok {
		t.Error("reloaded config still holds the revoked key")
	}
	// A second delete is an honest 404, not a silent success.
	rec = doReq(t, srv, "DELETE", "/admin/keys/"+s.id, phrase, adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestKeyDeleteWithoutPersist (M1.3): without a persist function the
// delete refuses (503) rather than removing a key that would come
// back on restart while the caller believes it revoked.
func TestKeyDeleteWithoutPersist(t *testing.T) {
	s, _ := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t) // no persistOpt
	rec := doReq(t, srv, "DELETE", "/admin/keys/"+s.id, `{"confirm":"DELETE `+s.id+`"}`, adminRaw)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete without persist: status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("key must still work after a refused delete: status %d", rec.Code)
	}
}

// TestKeyRotatePreservesDisabled (M1.3): rotating a disabled key
// must not silently re-enable it — the replacement inherits the
// disabled flag, so neither the old nor the new raw key works.
func TestKeyRotatePreservesDisabled(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"disabled":true}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id+"/rotate", `{}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate disabled key: status %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var rot keyRotateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rot); err != nil {
		t.Fatalf("decode rotate response: %v: %s", err, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), rot.GatewayKey)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(strings.ToLower(rec.Body.String()), "disabled") {
		t.Fatalf("rotated key from a disabled key: status = %d, want 401 naming disabled: %s", rec.Code, rec.Body.String())
	}
}

// M5.6: me/status usage windows — metered spend + tokens and the
// list-price repricing for the active window, the day and the week.
func TestMeStatusUsageWindows(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	// cheap-model: metered $10/$30 per 1M, list $0.5/$1.5 per 1M.
	// 1M + 1M tokens -> metered $40.00, list $2.00.
	seedModelSpend(t, s, "cheap-model", 1_000_000, 1_000_000, 10.0/1e6, 30.0/1e6)
	// unknown-price-model: metered $0.004, no catalog price ->
	// the window's list price is incomplete.
	seedModelSpend(t, s, "unknown-price-model", 100, 100, 10.0/1e6, 30.0/1e6)
	// Unpriced row: no metered price at all.
	seedModelSpend(t, s, "unpriced-model", 50, 50, 0, 0)
	rec := doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st meStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode me/status: %v", err)
	}
	if st.Usage == nil {
		t.Fatal("usage section missing")
	}
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// Monday of this week.
	weekStart := dayStart.AddDate(0, 0, -int(now.Weekday()-time.Monday+7)%7)
	for name, w := range map[string]meStatusUsageWindow{"cost": st.Usage.Cost, "daily": st.Usage.Daily, "weekly": st.Usage.Weekly} {
		if w.SpentUSD != 40.004 {
			t.Errorf("usage.%s spent = %v, want 40.004", name, w.SpentUSD)
		}
		if w.Tokens != 2_000_300 {
			t.Errorf("usage.%s tokens = %d, want 2000300", name, w.Tokens)
		}
		if w.UnpricedRequests != 1 {
			t.Errorf("usage.%s unpriced_requests = %d, want 1", name, w.UnpricedRequests)
		}
		if w.ListPriceUSD == nil || *w.ListPriceUSD != 2.00 {
			t.Errorf("usage.%s list_price_usd = %v, want 2.00", name, w.ListPriceUSD)
		}
		if w.ListPriceComplete {
			t.Errorf("usage.%s list_price_complete = true, want false (unknown-price-model has no catalog price)", name)
		}
	}
	if !st.Usage.Daily.PeriodStart.Equal(dayStart) {
		t.Errorf("usage.daily period_start = %v, want %v", st.Usage.Daily.PeriodStart, dayStart)
	}
	if !st.Usage.Weekly.PeriodStart.Equal(weekStart) {
		t.Errorf("usage.weekly period_start = %v, want %v (Monday)", st.Usage.Weekly.PeriodStart, weekStart)
	}
}

func TestMeStatusUsageEmpty(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st meStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode me/status: %v", err)
	}
	if st.Usage == nil {
		t.Fatal("usage section missing on an empty key")
	}
	for name, w := range map[string]meStatusUsageWindow{"cost": st.Usage.Cost, "daily": st.Usage.Daily, "weekly": st.Usage.Weekly} {
		if w.SpentUSD != 0 || w.Tokens != 0 || w.UnpricedRequests != 0 {
			t.Errorf("usage.%s = %+v, want all zeros", name, w)
		}
		if w.ListPriceUSD != nil {
			t.Errorf("usage.%s list_price_usd = %v, want absent on an empty window", name, *w.ListPriceUSD)
		}
		if !w.ListPriceComplete {
			t.Errorf("usage.%s list_price_complete = false, want true (nothing unknown in an empty window)", name)
		}
	}
}

// seedModelSpend records one priced usage row for the fixture key
// at the given per-token metered rates (0/0 = unpriced).
func seedModelSpend(t *testing.T, s *keyBudgetSetup, model string, prompt, completion int, promptRate, completionRate float64) {
	t.Helper()
	var prices ledger.Prices
	if promptRate > 0 || completionRate > 0 {
		prices = ledger.Prices{ledger.PriceKey{Model: model}: ledger.ModelPrice{PromptPerToken: promptRate, CompletionPerToken: completionRate}}
	}
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: model,
		PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("seed spend %s: %v", model, err)
	}
}

// ---------------------------------------------------------------------------
// POST /admin/keys — create one gateway key (B1, M10-7 enabler).
// ---------------------------------------------------------------------------

// doKeyCreate calls handleKeyCreate directly. B1 scope is handler +
// tests only: the guard row and the server.go registration land
// with the coordinator, so the mux cannot route POST /admin/keys
// until then and these tests drive the handler itself. Auth and
// scope enforcement live in the guard wrappers, not the handler;
// TestKeyCreateNonAdminForbidden exercises those wrappers via
// requireScope — the exact helper register() applies.
func doKeyCreate(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleKeyCreate(rec, req)
	return rec
}

// TestKeyCreateRoundTrip (B1): create returns 201 with the raw key
// exactly once, the key record, and the fingerprint; the new key
// authenticates afterwards; GET /dashboard/keys never exposes the
// raw key or the full hash; the key survives a config reload with
// its scopes, budget and limits; and the raw key is never logged.
func TestKeyCreateRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	_ = adminRaw // the handler does not enforce auth — the guard does (see TestKeyCreateNonAdminForbidden).
	var audit bytes.Buffer
	srv := s.server(t, s.persistOpt(cfgPath), WithLogger(log.New(&audit, "", 0)))
	body := `{"name":"api-client","scopes":["chat","models"],"budget_usd":5,"limits":{"requests_per_minute":60,"tokens_per_minute":1000,"requests_per_day":500}}`
	rec := doKeyCreate(t, srv, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v: %s", err, rec.Body.String())
	}
	raw := created.GatewayKey
	if !strings.HasPrefix(raw, "crk_") || len(raw) != 36 {
		t.Fatalf("gateway_key %q must be a fresh crk_ key", raw)
	}
	if raw == s.raw {
		t.Fatal("created key must differ from the fixture key")
	}
	// The raw key appears exactly once in the whole response.
	if n := strings.Count(rec.Body.String(), raw); n != 1 {
		t.Fatalf("raw key appears %d times in the create response, want exactly 1", n)
	}
	if created.KeyID == "" || created.KeyID == s.id {
		t.Errorf("key_id = %q, want a fresh id", created.KeyID)
	}
	if created.Name != "api-client" {
		t.Errorf("name = %q, want api-client", created.Name)
	}
	if created.Key.ID != created.KeyID || created.Key.Name != "api-client" {
		t.Errorf("key record = %+v, want id %s name api-client", created.Key, created.KeyID)
	}
	if len(created.Key.Scopes) != 2 || created.Key.Scopes[0] != "chat" || created.Key.Scopes[1] != "models" {
		t.Errorf("key scopes = %v, want [chat models]", created.Key.Scopes)
	}
	if created.Key.BudgetUSD != 5 {
		t.Errorf("key budget_usd = %v, want 5", created.Key.BudgetUSD)
	}
	if created.Key.RequestsPerMinute != 60 || created.Key.TokensPerMinute != 1000 {
		t.Errorf("key limits = %d/%d, want 60/1000", created.Key.RequestsPerMinute, created.Key.TokensPerMinute)
	}
	if created.Key.Masked == "" || strings.Contains(created.Key.Masked, raw) {
		t.Errorf("key masked = %q, want a fixed mask, never the raw key", created.Key.Masked)
	}
	if created.Fingerprint == "" || !strings.HasPrefix(created.Fingerprint, "sha256:") {
		t.Errorf("fingerprint = %q, want a non-empty sha256: value", created.Fingerprint)
	}
	if created.Fingerprint != created.Key.Fingerprint {
		t.Errorf("fingerprint = %q, key fingerprint = %q, want the same fingerprint in both places", created.Fingerprint, created.Key.Fingerprint)
	}
	if !created.PersistedToConfig {
		t.Error("persisted_to_config must be true with a persist function wired")
	}
	if !strings.Contains(created.Note, "shown once") {
		t.Errorf("note = %q, want it to say the key is shown once", created.Note)
	}
	// Never logged: the audit log names the id, never the raw key.
	if strings.Contains(audit.String(), raw) {
		t.Fatal("log contains the raw created key")
	}
	if !strings.Contains(audit.String(), created.KeyID) {
		t.Errorf("audit log = %q, want a create entry naming key %s", audit.String(), created.KeyID)
	}
	// The new key authenticates (chat scope) on a real route.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("unpriced-model", false), raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("new key chat: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The read endpoints never expose the raw key or its full hash.
	rec = doReq(t, srv, "GET", "/dashboard/keys", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("keys feed: status = %d", rec.Code)
	}
	feedBody := rec.Body.String()
	if strings.Contains(feedBody, raw) {
		t.Fatal("keys feed contains the raw created key")
	}
	stored, ok := s.cfg.FindGatewayKey(created.KeyID)
	if !ok {
		t.Fatalf("created key %s not in the live config", created.KeyID)
	}
	if strings.Contains(feedBody, stored.KeyHash) {
		t.Fatal("keys feed contains the full stored key hash")
	}
	var keys routerKeysResponse
	if err := json.Unmarshal([]byte(feedBody), &keys); err != nil {
		t.Fatalf("decode keys feed: %v", err)
	}
	found := false
	for _, k := range keys.Keys {
		if k.ID == created.KeyID {
			found = true
			if k.Masked == "" || k.Name != "api-client" || k.BudgetUSD != 5 {
				t.Errorf("feed row = %+v, want api-client with budget 5", k)
			}
		}
	}
	if !found {
		t.Fatalf("keys feed = %+v, want the created key %s", keys.Keys, created.KeyID)
	}
	// GET /v1/me/status for the new key: fingerprint only, no raw key.
	rec = doReq(t, srv, "GET", "/v1/me/status", "", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("me/status for new key: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), raw) {
		t.Fatal("me/status contains the raw created key")
	}
	// The creation survived to the config file, with every field.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	reloadedKey, ok := reloaded.FindGatewayKey(created.KeyID)
	if !ok {
		t.Fatalf("reloaded config lost the created key %s", created.KeyID)
	}
	if !reloadedKey.Verify(raw) {
		t.Error("reloaded key does not verify against the issued raw key")
	}
	if reloadedKey.Name != "api-client" || reloadedKey.BudgetUSD != 5 {
		t.Errorf("reloaded key = %+v, want api-client with budget 5", reloadedKey)
	}
	if len(reloadedKey.Scopes) != 2 || reloadedKey.Scopes[0] != "chat" || reloadedKey.Scopes[1] != "models" {
		t.Errorf("reloaded scopes = %v, want [chat models]", reloadedKey.Scopes)
	}
	if reloadedKey.RequestsPerMinute != 60 || reloadedKey.TokensPerMinute != 1000 || reloadedKey.RequestsPerDay != 500 {
		t.Errorf("reloaded limits = %d/%d/%d, want 60/1000/500", reloadedKey.RequestsPerMinute, reloadedKey.TokensPerMinute, reloadedKey.RequestsPerDay)
	}
	if reloadedKey.KeyHash != stored.KeyHash {
		t.Error("reloaded key hash differs from the live config hash")
	}
}

// TestKeyCreateDefaultsAndBudgetAlias (B1): an absent scopes list
// defaults to ["chat", "models"] (the headless-wizard default for
// a working key — a scopeless key would authenticate yet call
// nothing), and "budget" is accepted as the alias of "budget_usd".
func TestKeyCreateDefaultsAndBudgetAlias(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	_ = adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doKeyCreate(t, srv, `{"name":"plain"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create without scopes: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if len(created.Key.Scopes) != 2 || created.Key.Scopes[0] != "chat" || created.Key.Scopes[1] != "models" {
		t.Errorf("default scopes = %v, want [chat models]", created.Key.Scopes)
	}
	if created.Key.BudgetUSD != 0 {
		t.Errorf("default budget = %v, want 0 (no per-key cap)", created.Key.BudgetUSD)
	}
	rec = doKeyCreate(t, srv, `{"name":"alias","scopes":[],"budget":2.5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with budget alias: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var aliased keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &aliased); err != nil {
		t.Fatalf("decode alias response: %v", err)
	}
	if aliased.Key.BudgetUSD != 2.5 {
		t.Errorf("budget alias = %v, want 2.5", aliased.Key.BudgetUSD)
	}
	if len(aliased.Key.Scopes) != 2 || aliased.Key.Scopes[0] != "chat" || aliased.Key.Scopes[1] != "models" {
		t.Errorf("empty-list scopes = %v, want the [chat models] default", aliased.Key.Scopes)
	}
	// An mcp: scope is a valid scope for a new key.
	rec = doKeyCreate(t, srv, `{"name":"mcp-reader","scopes":["mcp:read:cost"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with an mcp: scope: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var mcpKey keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mcpKey); err != nil {
		t.Fatalf("decode mcp response: %v", err)
	}
	if len(mcpKey.Key.Scopes) != 1 || mcpKey.Key.Scopes[0] != "mcp:read:cost" {
		t.Errorf("mcp scopes = %v, want [mcp:read:cost]", mcpKey.Key.Scopes)
	}
}

// TestKeyCreateValidation (B1): every invalid body is a 400 naming
// the problem, and no key is created by any of them.
func TestKeyCreateValidation(t *testing.T) {
	s, _ := keyStatusSetup(t)
	_ = adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(filepath.Join(t.TempDir(), "config.json")))
	before := len(s.cfg.GatewayKeys)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing name", `{"scopes":["chat"]}`, "name is required"},
		{"blank name", `{"name":"  "}`, "name is required"},
		{"unknown scope", `{"name":"x","scopes":["chat","teleport"]}`, "unknown scope teleport"},
		{"empty scope", `{"name":"x","scopes":["chat",""]}`, "empty scope"},
		{"bare star", `{"name":"x","scopes":["*"]}`, "unknown scope *"},
		{"duplicate scope", `{"name":"x","scopes":["chat","chat"]}`, "duplicate scope chat"},
		{"negative budget_usd", `{"name":"x","budget_usd":-1}`, "budget must not be negative"},
		{"negative budget alias", `{"name":"x","budget":-0.5}`, "budget must not be negative"},
		{"budget alias disagree", `{"name":"x","budget":1,"budget_usd":2}`, "disagree"},
		{"negative rpm", `{"name":"x","limits":{"requests_per_minute":-1}}`, "requests_per_minute must not be negative"},
		{"negative tpm", `{"name":"x","limits":{"tokens_per_minute":-5}}`, "tokens_per_minute must not be negative"},
		{"negative rpd", `{"name":"x","limits":{"requests_per_day":-1}}`, "requests_per_day must not be negative"},
		{"unknown top-level field", `{"name":"x","colour":"red"}`, "invalid key creation"},
		{"unknown limits field", `{"name":"x","limits":{"token_limits":[]}}`, "invalid key creation"},
		{"malformed json", `{"name":}`, "invalid key creation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doKeyCreate(t, srv, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("400 body %q must name %q", rec.Body.String(), tc.want)
			}
			if got := len(s.cfg.GatewayKeys); got != before {
				t.Fatalf("key count = %d, want %d — a rejected create must not mint a key", got, before)
			}
		})
	}
}

// TestKeyCreateWithoutPersist (B1): without a persist function the
// endpoint refuses (503) rather than minting a key that would
// vanish on restart — and nothing is added to the live config.
func TestKeyCreateWithoutPersist(t *testing.T) {
	s, _ := keyStatusSetup(t)
	_ = adminKeyFor(t, s)
	srv := s.server(t) // no persistOpt
	before := len(s.cfg.GatewayKeys)
	rec := doKeyCreate(t, srv, `{"name":"nope","scopes":["chat"]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create without persist: status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if got := len(s.cfg.GatewayKeys); got != before {
		t.Fatalf("key count = %d, want %d — a refused create must not mint a key", got, before)
	}
}

// TestKeyCreateNonAdminForbidden (B1): the scope gate is the guard
// wrapper, not the handler — so this test drives handleKeyCreate
// through requireScope, the exact helper register() applies with
// the coordinator's guard row (Scopes ["admin"]). A chat-only key
// is refused 403 naming the scope; an admin key passes through
// and the create succeeds. (No-key 401 is the auth wrapper's job —
// requireAuthAlways, applied by register() alongside requireScope
// — not requireScope's, which passes keyless requests through to
// the auth layer.)
func TestKeyCreateNonAdminForbidden(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	wrapped := srv.requireScope(srv.handleKeyCreate, []string{"admin"})
	call := func(raw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/keys", strings.NewReader(`{"name":"scoped","scopes":["chat"]}`))
		if raw != "" {
			req.Header.Set("Authorization", "Bearer "+raw)
		}
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		return rec
	}
	rec := call(s.raw) // chat-only fixture key
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin key: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "scope") {
		t.Errorf("403 body %q must name the missing scope", rec.Body.String())
	}
	rec = call(adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin key: status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}
