// M10-7 dashboard Keys section: single-key disable/re-enable and
// single-key revoke are wired to the real backends, rotate stays
// wired, and create is wired too (critic fix 2026-11-19 —
// POST /admin/keys already existed; only the button was unwired).
//
// Proves, against the real Server + real ledger + counting engine
// (reusing the keystatus fixtures — no mocks of the key lifecycle):
//   - disable round-trip: POST /admin/keys/{id} {"disabled":true}
//     makes the key 401 at once (naming the disable, zero provider
//     calls), GET /dashboard/keys shows disabled:true, and
//     {"disabled":false} restores it immediately;
//   - revoke round-trip: DELETE /admin/keys/{id} with the typed
//     confirmation "DELETE <id>" removes the key for good — the
//     wrong phrase is a 400 with the key untouched, the key 401s
//     at once, GET /dashboard/keys no longer lists it, and a second
//     delete is an honest 404;
//   - rotate round-trip: POST /admin/keys/{id}/rotate returns 201
//     with the new key shown once, the old key is dead at once, the
//     new key works, and GET /dashboard/keys lists the new id only;
//   - create round-trip: POST /admin/keys returns 201 with the
//     raw key shown once, the new key authenticates at once, and
//     GET /dashboard/keys lists the new id;
//   - the served dashboard wires the per-row Disable/Enable and
//     Revoke buttons to those endpoints, keeps the honest
//     "Last used: not tracked" label, and wires the "+ New router
//     key" button to POST /admin/keys (name prompted, raw key shown
//     once in the rotated-key box). The daily/weekly/monthly $ caps
//     ARE wired (M10-7 follow-up): the manage form's Daily/Weekly/
//     Monthly inputs save through POST /api/usage/budget via
//     adminHeaders(), server errors are shown verbatim, and the
//     values are re-read from the live key-budgets feed after
//     save;
//   - the extracted inline script passes node --check.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// m10_7DashboardBody serves GET /dashboard and returns its body.
func m10_7DashboardBody(t *testing.T) string {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// m10_7KeysFeed reads GET /dashboard/keys as the admin key and
// returns the rows by id.
func m10_7KeysFeed(t *testing.T, srv *Server, adminRaw string) map[string]routerKeyView {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/keys", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/keys status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var feed struct {
		Keys []routerKeyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode /dashboard/keys: %v: %s", err, rec.Body.String())
	}
	byID := make(map[string]routerKeyView, len(feed.Keys))
	for _, k := range feed.Keys {
		byID[k.ID] = k
	}
	return byID
}

// m10_7ChatStatus sends one chat request with the given raw key.
func m10_7ChatStatus(t *testing.T, srv *Server, raw string) int {
	t.Helper()
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), raw)
	return rec.Code
}

func TestDashboardM10_7DisableRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	if code := m10_7ChatStatus(t, srv, s.raw); code != http.StatusOK {
		t.Fatalf("chat before disable: status %d, want 200", code)
	}
	// Disable it.
	rec := doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"disabled":true}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var upd keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("decode disable response: %v: %s", err, rec.Body.String())
	}
	// NOTE (M10-7 finding, not a test gap): the update response's
	// key.disabled is always false — handleKeyUpdate's view literal
	// (keystatus.go) omits the Disabled field, so it never reports
	// the flag it just set. The disable itself works and the
	// authoritative GET /dashboard/keys feed (checked below) does
	// carry disabled:true. The note is the response's honest
	// signal, so assert on that; the one-line view fix belongs to
	// the coordinator, not this dashboard slice.
	if !strings.Contains(strings.ToLower(upd.Note), "disabled") {
		t.Errorf("disable note = %q, want it to say the key is disabled", upd.Note)
	}
	// The keys feed shows the disabled flag.
	if row, ok := m10_7KeysFeed(t, srv, adminRaw)[s.id]; !ok {
		t.Errorf("GET /dashboard/keys lost the disabled key %s", s.id)
	} else if !row.Disabled {
		t.Errorf("GET /dashboard/keys disabled = false for %s, want true", s.id)
	}
	// The very next request with the key: 401 naming the disable,
	// zero additional provider calls.
	callsBefore := s.eng.chatCalls.Load()
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions", kbChatBody("unpriced-model", false), s.raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("chat after disable: status %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "disabled") {
		t.Errorf("disabled key 401 body %q must name the disable", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != callsBefore {
		t.Fatalf("engine Chat calls = %d, want %d — the disabled key must be refused before any provider is called", s.eng.chatCalls.Load(), callsBefore)
	}
	// Re-enable: the key works again, immediately.
	rec = doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"disabled":false}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if code := m10_7ChatStatus(t, srv, s.raw); code != http.StatusOK {
		t.Fatalf("chat after re-enable: status %d, want 200", code)
	}
	if row, ok := m10_7KeysFeed(t, srv, adminRaw)[s.id]; !ok {
		t.Errorf("GET /dashboard/keys lost the re-enabled key %s", s.id)
	} else if row.Disabled {
		t.Errorf("GET /dashboard/keys disabled = true for %s after re-enable, want false", s.id)
	}
}

func TestDashboardM10_7RevokeRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	phrase := `{"confirm":"DELETE ` + s.id + `"}`
	// Wrong phrase: 400, key untouched.
	rec := doReq(t, srv, http.MethodDelete, "/admin/keys/"+s.id, `{"confirm":"DELETE wrong"}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong phrase: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if code := m10_7ChatStatus(t, srv, s.raw); code != http.StatusOK {
		t.Fatalf("key must still work after a wrong-phrase revoke: status %d, want 200", code)
	}
	// Correct typed phrase as admin: 200.
	rec = doReq(t, srv, http.MethodDelete, "/admin/keys/"+s.id, phrase, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var del keyDeleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &del); err != nil {
		t.Fatalf("decode revoke response: %v: %s", err, rec.Body.String())
	}
	if !del.Revoked || del.KeyID != s.id {
		t.Errorf("revoke response = %+v, want revoked=true key_id=%s", del, s.id)
	}
	// The key is dead immediately and gone from the keys feed.
	if code := m10_7ChatStatus(t, srv, s.raw); code != http.StatusUnauthorized {
		t.Fatalf("chat after revoke: status %d, want 401", code)
	}
	if _, ok := m10_7KeysFeed(t, srv, adminRaw)[s.id]; ok {
		t.Errorf("GET /dashboard/keys still lists revoked key %s", s.id)
	}
	// A second delete is an honest 404, not a silent success.
	rec = doReq(t, srv, http.MethodDelete, "/admin/keys/"+s.id, phrase, adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestDashboardM10_7RotateRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	if code := m10_7ChatStatus(t, srv, s.raw); code != http.StatusOK {
		t.Fatalf("chat before rotate: status %d, want 200", code)
	}
	rec := doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id+"/rotate", `{}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate: status %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var rot keyRotateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rot); err != nil {
		t.Fatalf("decode rotate response: %v: %s", err, rec.Body.String())
	}
	// The new key is shown once, in the response only.
	if rot.GatewayKey == "" {
		t.Fatal("rotate response carries no gateway_key — the new key must be shown once")
	}
	if rot.KeyID == "" || rot.KeyID == s.id {
		t.Errorf("rotate key_id = %q, want a new id different from %s", rot.KeyID, s.id)
	}
	if rot.RotatedFrom != s.id {
		t.Errorf("rotate rotated_from = %q, want %s", rot.RotatedFrom, s.id)
	}
	// Old key dead at once, new key works.
	if code := m10_7ChatStatus(t, srv, s.raw); code != http.StatusUnauthorized {
		t.Fatalf("chat with old key after rotate: status %d, want 401", code)
	}
	if code := m10_7ChatStatus(t, srv, rot.GatewayKey); code != http.StatusOK {
		t.Fatalf("chat with rotated key: status %d, want 200", code)
	}
	// The keys feed lists the new id and not the old one.
	feed := m10_7KeysFeed(t, srv, adminRaw)
	if _, ok := feed[rot.KeyID]; !ok {
		t.Errorf("GET /dashboard/keys does not list rotated key %s", rot.KeyID)
	}
	if _, ok := feed[s.id]; ok {
		t.Errorf("GET /dashboard/keys still lists rotated-from key %s", s.id)
	}
}

// TestDashboardM10_7CreateRoundTrip (critic fix 2026-11-19): POST
// /admin/keys exists (handleKeyCreate, registered in server.go) —
// the old "still missing" premise was wrong (the endpoint was
// already there; only the dashboard button was unwired) — so the
// "+ New router key" button is wired to it. Creating a key returns
// 201 with the raw key shown once, the new key authenticates at
// once, and GET /dashboard/keys lists the new id.
func TestDashboardM10_7CreateRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t, s.persistOpt(cfgPath))
	rec := doReq(t, srv, http.MethodPost, "/admin/keys", `{"name":"from-dashboard","scopes":["chat"]}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /admin/keys status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v: %s", err, rec.Body.String())
	}
	if created.KeyID == "" || created.GatewayKey == "" {
		t.Fatalf("create response key_id/gateway_key = %q/%q, want both set", created.KeyID, created.GatewayKey)
	}
	if created.Name != "from-dashboard" {
		t.Errorf("create response name = %q, want %q", created.Name, "from-dashboard")
	}
	// The new key works at once.
	if code := m10_7ChatStatus(t, srv, created.GatewayKey); code != http.StatusOK {
		t.Fatalf("chat with created key: status %d, want 200", code)
	}
	// The keys feed lists the new id.
	if _, ok := m10_7KeysFeed(t, srv, adminRaw)[created.KeyID]; !ok {
		t.Errorf("GET /dashboard/keys does not list created key %s", created.KeyID)
	}
}

// m10_7KeyBudgetsFeed reads GET /dashboard/key-budgets and
// returns its rows by key id — the same live feed the Keys
// view's D/W/M prefill reads the stored caps from.
func m10_7KeyBudgetsFeed(t *testing.T, srv *Server) map[string]keyBudgetView {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/key-budgets", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/key-budgets status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var feed keyBudgetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode /dashboard/key-budgets: %v: %s", err, rec.Body.String())
	}
	byID := make(map[string]keyBudgetView, len(feed.Budgets))
	for _, b := range feed.Budgets {
		byID[b.KeyID] = b
	}
	return byID
}

// TestDashboardM10_7DwmLimitsFreshKeyRequiresLimitUsd (nit 2):
// a D/W/M-only POST on a key with no budget row yet is a 400
// naming limitUsd, and nothing is persisted — no budget row is
// created and the usage summary carries no D/W/M caps.
func TestDashboardM10_7DwmLimitsFreshKeyRequiresLimitUsd(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	rec := doReq(t, srv, http.MethodPost, "/api/usage/budget", `{"apiKeyId":"`+s.id+`","dailyLimitUsd":2.5,"weeklyLimitUsd":10,"monthlyLimitUsd":50}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("D/W/M-only POST on a fresh key: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "limitUsd is required when the key has no budget yet") {
		t.Errorf("fresh-key 400 body = %s, want it to name limitUsd", rec.Body.String())
	}
	// Nothing persisted: the key-budgets feed still shows no
	// budget row for the key, and the usage summary carries no
	// D/W/M caps.
	if row, ok := m10_7KeyBudgetsFeed(t, srv)[s.id]; !ok {
		t.Errorf("GET /dashboard/key-budgets lost key %s", s.id)
	} else if row.HasBudget {
		t.Errorf("GET /dashboard/key-budgets has_budget = true for %s after a rejected D/W/M-only POST, want false (no row created)", s.id)
	}
	sum := m10_7UsageBudgetFeed(t, srv, s.id, s.raw)
	if sum.DailyLimitUSD != nil || sum.WeeklyLimitUSD != nil || sum.MonthlyLimitUSD != nil {
		t.Errorf("usage summary after rejected POST daily/weekly/monthly = %v/%v/%v, want all nil", sum.DailyLimitUSD, sum.WeeklyLimitUSD, sum.MonthlyLimitUSD)
	}
}

// m10_7UsageBudgetFeed reads GET /api/usage/budget for one key
// and returns its live summary — the same feed the dashboard
// re-reads after a D/W/M save.
func m10_7UsageBudgetFeed(t *testing.T, srv *Server, keyID, adminRaw string) usageBudgetSummary {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/api/usage/budget?apiKeyId="+keyID, "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/usage/budget status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var sum usageBudgetSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode /api/usage/budget: %v: %s", err, rec.Body.String())
	}
	return sum
}

// TestDashboardM10_7DwmLimitsRoundTrip (M10-7 follow-up): the
// D/W/M $ caps the Keys view saves through POST /api/usage/budget
// round-trip to the live feed — set all three, the feed shows
// them; zero/negative in any window is a 400 naming that window
// and persists nothing; the 5h limitUsd: 0 is still accepted
// (0 means $0, unlike a zero D/W/M cap); the live GET
// /dashboard/key-budgets feed — the feed the Keys view's D/W/M
// prefill reads — carries the stored caps too.
func TestDashboardM10_7DwmLimitsRoundTrip(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	// The 5h cap keeps its older rule: limitUsd: 0 means $0
	// (any spend is over it), so the POST succeeds and the row
	// is created with a 0 cap.
	rec := doReq(t, srv, http.MethodPost, "/api/usage/budget", `{"apiKeyId":"`+s.id+`","limitUsd":0}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST limitUsd 0: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if sum := m10_7UsageBudgetFeed(t, srv, s.id, s.raw); sum.LimitUSD != 0 {
		t.Errorf("feed limit_usd = %v, want 0 (the 5h $0 cap)", sum.LimitUSD)
	}
	// Set all three D/W/M caps — the row exists now, so no
	// limitUsd is needed; absent fields keep stored values.
	rec = doReq(t, srv, http.MethodPost, "/api/usage/budget", `{"apiKeyId":"`+s.id+`","dailyLimitUsd":2.5,"weeklyLimitUsd":10,"monthlyLimitUsd":50}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST D/W/M caps: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The live feed shows the stored caps.
	sum := m10_7UsageBudgetFeed(t, srv, s.id, s.raw)
	if sum.DailyLimitUSD == nil || *sum.DailyLimitUSD != 2.5 {
		t.Errorf("feed daily_limit_usd = %v, want 2.5", sum.DailyLimitUSD)
	}
	if sum.WeeklyLimitUSD == nil || *sum.WeeklyLimitUSD != 10 {
		t.Errorf("feed weekly_limit_usd = %v, want 10", sum.WeeklyLimitUSD)
	}
	if sum.MonthlyLimitUSD == nil || *sum.MonthlyLimitUSD != 50 {
		t.Errorf("feed monthly_limit_usd = %v, want 50", sum.MonthlyLimitUSD)
	}
	if sum.LimitUSD != 0 {
		t.Errorf("feed limit_usd = %v, want 0 (the 5h cap is untouched by a D/W/M-only POST)", sum.LimitUSD)
	}
	// The live key-budgets feed — the feed the Keys view's
	// D/W/M prefill reads — carries the same stored caps for
	// this key (nit 4: the feed the prefill consumes, not just
	// the usage summary).
	if row, ok := m10_7KeyBudgetsFeed(t, srv)[s.id]; !ok {
		t.Errorf("GET /dashboard/key-budgets lost key %s", s.id)
	} else {
		if !row.HasBudget {
			t.Errorf("key-budgets feed has_budget = false for %s, want true", s.id)
		}
		if row.Daily.LimitUSD == nil || *row.Daily.LimitUSD != 2.5 {
			t.Errorf("key-budgets feed daily.limit_usd = %v, want 2.5", row.Daily.LimitUSD)
		}
		if row.Weekly.LimitUSD == nil || *row.Weekly.LimitUSD != 10 {
			t.Errorf("key-budgets feed weekly.limit_usd = %v, want 10", row.Weekly.LimitUSD)
		}
		if row.Monthly.LimitUSD == nil || *row.Monthly.LimitUSD != 50 {
			t.Errorf("key-budgets feed monthly.limit_usd = %v, want 50", row.Monthly.LimitUSD)
		}
	}
	// Zero or negative in any window: a 400 naming that window.
	for _, tc := range []struct {
		name, body, want string
	}{
		{"zero daily", `{"apiKeyId":"` + s.id + `","dailyLimitUsd":0}`, "dailyLimitUsd must be positive"},
		{"negative daily", `{"apiKeyId":"` + s.id + `","dailyLimitUsd":-1}`, "dailyLimitUsd must be positive"},
		{"zero weekly", `{"apiKeyId":"` + s.id + `","weeklyLimitUsd":0}`, "weeklyLimitUsd must be positive"},
		{"negative weekly", `{"apiKeyId":"` + s.id + `","weeklyLimitUsd":-1}`, "weeklyLimitUsd must be positive"},
		{"zero monthly", `{"apiKeyId":"` + s.id + `","monthlyLimitUsd":0}`, "monthlyLimitUsd must be positive"},
		{"negative monthly", `{"apiKeyId":"` + s.id + `","monthlyLimitUsd":-1}`, "monthlyLimitUsd must be positive"},
	} {
		rec := doReq(t, srv, http.MethodPost, "/api/usage/budget", tc.body, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: body = %s, want it to say %q", tc.name, rec.Body.String(), tc.want)
		}
	}
	// The feed still shows the stored caps — no rejected update
	// persisted anything.
	sum = m10_7UsageBudgetFeed(t, srv, s.id, s.raw)
	if sum.DailyLimitUSD == nil || *sum.DailyLimitUSD != 2.5 || sum.WeeklyLimitUSD == nil || *sum.WeeklyLimitUSD != 10 || sum.MonthlyLimitUSD == nil || *sum.MonthlyLimitUSD != 50 {
		t.Errorf("feed after rejected updates daily/weekly/monthly = %v/%v/%v, want 2.5/10/50", sum.DailyLimitUSD, sum.WeeklyLimitUSD, sum.MonthlyLimitUSD)
	}
}

// m10_7ExtractFunction returns the source of the named
// top-level function from the served dashboard script,
// brace-matched from its "function name(){" opening.
func m10_7ExtractFunction(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, "function "+name+"(){")
	if start < 0 {
		t.Fatalf("function %s not found in served dashboard", name)
	}
	depth := 0
	for i := start; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start : i+1]
			}
		}
	}
	t.Fatalf("function %s has unbalanced braces", name)
	return ""
}

// TestDashboardM10_7PrefillKeyManageForm (nits 4 + 5) drives the
// real prefillKeyManageForm from the served dashboard under node
// against a stub DOM — no substring check stands in for it:
//   - a key with a key-budgets feed row prefills its Daily/
//     Weekly/Monthly fields from the row's limit_usd values
//     (the feed the Go round-trip test just proved carries
//     them);
//   - with the feed loaded, switching to a row-less key clears
//     the previous key's caps instead of leaving them displayed;
//   - while the feed is still pending, a row-less key leaves
//     typed values alone (the by-design case).
func TestDashboardM10_7PrefillKeyManageForm(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not installed — cannot drive the prefill path (install node or set NODE_PATH)")
	}
	body := m10_7DashboardBody(t)
	fn := m10_7ExtractFunction(t, body, "prefillKeyManageForm")
	harness := `
var keyBudgetRows = [
  {key_id: "k1", daily: {limit_usd: 2.5}, weekly: {limit_usd: 10}, monthly: {limit_usd: 50}}
];
var keyBudgetsLoaded = false;
var routerKeys = [
  {id: "k1", expires_at: null, requests_per_minute: 0, tokens_per_minute: 0, guardrails: []},
  {id: "k2", expires_at: null, requests_per_minute: 0, tokens_per_minute: 0, guardrails: []}
];
var els = {};
["keyManageSelect", "keyManageExpiry", "keyManageRPM", "keyManageTPM", "keyManageGuardrails", "keyManageClearExpiry", "keyManageDaily", "keyManageWeekly", "keyManageMonthly"].forEach(function(id){ els[id] = {value: "", checked: false}; });
function $(s){ return els[s.slice(1)] || null; }
function toLocalInput(iso){ return ""; }
`
	scenarios := `
function dwm(){ return {daily: String(els.keyManageDaily.value), weekly: String(els.keyManageWeekly.value), monthly: String(els.keyManageMonthly.value)}; }
var out = {};
/* Feed loaded, key with a row: prefill from the row. */
keyBudgetsLoaded = true;
els.keyManageSelect.value = "k1";
prefillKeyManageForm();
out.rowPrefill = dwm();
/* Feed loaded, switch to a row-less key: stale caps cleared. */
els.keyManageSelect.value = "k2";
prefillKeyManageForm();
out.rowlessClears = dwm();
/* Feed still pending, row-less key: typed values left alone. */
keyBudgetsLoaded = false;
els.keyManageDaily.value = "2.5"; els.keyManageWeekly.value = "10"; els.keyManageMonthly.value = "50";
els.keyManageSelect.value = "k2";
prefillKeyManageForm();
out.pendingKeeps = dwm();
console.log(JSON.stringify(out));
`
	path := filepath.Join(t.TempDir(), "prefill-dwm.js")
	if err := os.WriteFile(path, []byte(harness+"\n"+fn+"\n"+scenarios), 0o600); err != nil {
		t.Fatalf("write prefill harness: %v", err)
	}
	out, err := exec.Command("node", path).Output()
	if err != nil {
		t.Fatalf("node prefill harness failed: %v: %s", err, out)
	}
	var res struct {
		RowPrefill    map[string]string `json:"rowPrefill"`
		RowlessClears map[string]string `json:"rowlessClears"`
		PendingKeeps  map[string]string `json:"pendingKeeps"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode prefill harness output: %v: %s", err, out)
	}
	want := map[string]string{"daily": "2.5", "weekly": "10", "monthly": "50"}
	for k, v := range want {
		if res.RowPrefill[k] != v {
			t.Errorf("row prefill %s = %q, want %q", k, res.RowPrefill[k], v)
		}
		if res.RowlessClears[k] != "" {
			t.Errorf("row-less key (feed loaded) %s = %q, want empty — the previous key's cap must not stay displayed", k, res.RowlessClears[k])
		}
		if res.PendingKeeps[k] != v {
			t.Errorf("row-less key (feed pending) %s = %q, want %q — typed values must be left alone until the feed arrives", k, res.PendingKeeps[k], v)
		}
	}
}

func TestDashboardM10_7ServedHTML(t *testing.T) {
	body := m10_7DashboardBody(t)
	secStart := strings.Index(body, `id="view-keys"`)
	secEnd := strings.Index(body, `id="view-logs"`)
	if secStart < 0 || secEnd <= secStart {
		t.Fatalf("dashboard Keys section bounds not found (view-keys .. view-logs)")
	}
	section := body[secStart:secEnd]
	for _, want := range []string{
		`id="view-keys"`,
		`id="routerKeys"`,
		`id="routerKeyCount"`,
		`id="newKeyBtn"`,
		`id="keyManageSelect"`,
		`id="keyManageGwKey"`,
		`id="keyManageSaveBtn"`,
		`id="keyManageRotateBtn"`,
		// Per-key device tracking: honest "not tracked" label, kept.
		"Last used: not tracked",
	} {
		if !strings.Contains(section, want) && !strings.Contains(body, want) {
			t.Errorf("dashboard Keys section missing %q", want)
		}
	}
	// The per-row Disable/Enable + Revoke wiring (in the script):
	// the row buttons, the disabled toggle, the DELETE with the
	// typed "DELETE <id>" phrase, all with Authorization.
	for _, want := range []string{
		"data-toggle-disabled",
		"data-revoke",
		"data-key-id",
		`"/admin/keys/" + encodeURIComponent`,
		`{disabled: disable}`,
		`method: "DELETE"`,
		`"DELETE " + keyId`,
		`confirm: phrase`,
		`"Authorization": "Bearer " + gwKey`,
		`setKeyDisabled`,
		`revokeKey`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard Keys JS missing %q", want)
		}
	}
	// The old unwired revoke toast must be gone — revoke is wired.
	if strings.Contains(body, "Revoking keys from the dashboard isn't wired yet") {
		t.Error("dashboard still carries the unwired revoke toast — revoke is wired now")
	}
	// The new-key button is wired now (critic fix 2026-11-19): it
	// POSTs the typed name to /admin/keys (the route exists —
	// server.go registers handleKeyCreate) and shows the raw key
	// once in the rotated-key box, like rotate does. The old
	// unwired toast must be gone.
	if strings.Contains(body, "Issuing keys from the dashboard isn't wired yet") {
		t.Error("dashboard still carries the unwired new-key toast — key creation is wired now")
	}
	for _, want := range []string{
		`fetch("/admin/keys", {`,
		`JSON.stringify({name: name})`,
		`res.body.gateway_key`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard new-key wiring missing %q", want)
		}
	}
	// M10-7 follow-up: the daily/weekly/monthly $ caps ARE wired
	// now — the manage form's inputs save through POST
	// /api/usage/budget (adminHeaders() auth) and the values are
	// re-read from the live key-budgets feed after save.
	for _, want := range []string{
		`id="keyManageDaily"`,
		`id="keyManageWeekly"`,
		`id="keyManageMonthly"`,
		`id="keyManageDwmSaveBtn"`,
		`"/api/usage/budget"`,
		"dailyLimitUsd",
		"weeklyLimitUsd",
		"monthlyLimitUsd",
		"apiKeyId",
		`saveKeyDwmLimits`,
		`refreshKeyDwmFromFeed`,
		// Prefill reads the stored caps from the key-budgets
		// feed rows (daily/weekly/monthly limit_usd).
		`dwmRow.daily`,
		`daily.limit_usd`,
		`weekly.limit_usd`,
		`monthly.limit_usd`,
		// Fresh-key caveat (nit 3): the hint naming where the
		// rolling 5 h budget is set first.
		`Costs → Per-key rolling budgets`,
		// Key-switch prefill (nit 5): only clear a row-less
		// key's fields once the feed has loaded.
		`keyBudgetsLoaded`,
	} {
		if !strings.Contains(section, want) && !strings.Contains(body, want) {
			t.Errorf("dashboard Keys section missing D/W/M wiring %q", want)
		}
	}
	// The D/W/M save goes through the shared adminHeaders()
	// helper — the sidebar operator key, cleared after the call
	// — never a second key field of its own.
	if !strings.Contains(body, "var headers = adminHeaders();") {
		t.Error("dashboard Keys D/W/M save must go through adminHeaders()")
	}
	// The old "not wired" note must be gone — D/W/M is wired.
	if strings.Contains(body, "Daily/weekly/monthly per-key limits are also not") {
		t.Error("dashboard still carries the unwired D/W/M note — the caps are wired now")
	}
	// No Playground anywhere in the dashboard.
	if strings.Contains(body, "Playground") {
		t.Error("dashboard must not contain a Playground")
	}
}

func TestDashboardM10_7InlineScriptSyntax(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not installed — cannot run node --check (install node or set NODE_PATH)")
	}
	body := m10_7DashboardBody(t)
	start := strings.Index(body, "<script>")
	end := strings.Index(body, "</script>")
	if start < 0 || end <= start {
		t.Fatal("served dashboard has no inline <script> block")
	}
	script := body[start+len("<script>") : end]
	path := filepath.Join(t.TempDir(), "dashboard-inline.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write inline script: %v", err)
	}
	cmd := exec.Command("node", "--check", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check on the extracted inline script failed: %v: %s", err, out)
	}
}

// TestDashboardProvidersKeyLinkServedHTML proves the served dashboard
// renders the key-creation link (parity tracker Area 3, "Key-creation
// links"): the provider feed's key_creation_url is mapped into the
// card model, and the card shows a "Get API key →" link — opening
// in a new tab — only when the catalog records a key page for that
// provider. No link is rendered for providers without one.
func TestDashboardProvidersKeyLinkServedHTML(t *testing.T) {
	body := m10_7DashboardBody(t)
	if !strings.Contains(body, "keyCreationUrl: p.key_creation_url") {
		t.Error("dashboard provider feed mapping missing key_creation_url")
	}
	if !strings.Contains(body, "p.keyCreationUrl") {
		t.Error("dashboard provider card never reads keyCreationUrl")
	}
	if !strings.Contains(body, "Get API key") {
		t.Error("dashboard provider card missing the 'Get API key' link")
	}
	if !strings.Contains(body, `target="_blank" rel="noopener"`) {
		t.Error("dashboard key-creation link must open in a new tab with rel=noopener")
	}
}
