// M8.19: webhooks management API + budget.warning event.
//
// The tests drive the real handlers (called directly, not through
// the mux — the coordinator owns guard.go/server.go registration,
// so the routes are not registered yet) and the real ledger for
// the budget.warning delivery test. Fixture contract tests use
// internal/apitest against tests/fixtures/v1/m8.19/.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

func decodeWebhookBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v (body: %s)", err, rec.Body.String())
	}
}

func callWebhookHandler(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	if strings.HasPrefix(path, "/api/webhooks/") && path != "/api/webhooks/" {
		srv.handleWebhookByID(rec, req)
	} else {
		srv.handleWebhooks(rec, req)
	}
	return rec
}

func TestM819CreateListGetDelete(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["quota.exceeded"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	decodeWebhookBody(t, rec, &created)
	if !strings.HasPrefix(created.ID, "wh_") {
		t.Errorf("created id = %q, want a server-generated wh_ id", created.ID)
	}
	if created.URL != "https://93.184.215.10/hook" || len(created.Events) != 1 || created.Events[0] != "quota.exceeded" {
		t.Errorf("created = %+v, want url + quota.exceeded echoed back", created)
	}
	if created.HasSecret {
		t.Errorf("created has_secret = true, want false (no secret was sent)")
	}
	if got := rec.Body.String(); strings.Contains(got, `"secret"`) {
		t.Errorf("create response contains a secret field: %s", got)
	}

	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list webhooksResponse
	decodeWebhookBody(t, rec, &list)
	if len(list.Webhooks) != 1 || list.Webhooks[0].ID != created.ID {
		t.Fatalf("list = %+v, want exactly the created registration", list)
	}

	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var one webhookView
	decodeWebhookBody(t, rec, &one)
	if one.ID != created.ID {
		t.Errorf("get id = %q, want %q", one.ID, created.ID)
	}

	rec = callWebhookHandler(t, srv, "DELETE", "/api/webhooks/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks/"+created.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks", "")
	var empty webhooksResponse
	decodeWebhookBody(t, rec, &empty)
	if len(empty.Webhooks) != 0 {
		t.Errorf("list after delete = %+v, want empty", empty)
	}
}

func TestM819CreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		note string
	}{
		{"ftp scheme", `{"url":"ftp://93.184.215.10/hook","events":["quota.exceeded"]}`, 400, "scheme"},
		{"loopback ip", `{"url":"http://127.0.0.1:9/hook","events":["quota.exceeded"]}`, 400, "loopback"},
		{"localhost name", `{"url":"https://localhost/hook","events":["quota.exceeded"]}`, 400, "localhost"},
		{"private 10.x", `{"url":"https://10.0.0.5/hook","events":["quota.exceeded"]}`, 400, "private-network"},
		{"private 192.168", `{"url":"https://192.168.1.1/hook","events":["quota.exceeded"]}`, 400, "private-network"},
		{"link-local", `{"url":"https://169.254.1.2/hook","events":["quota.exceeded"]}`, 400, "link-local"},
		{"credentials in url", `{"url":"https://user:pass@203.0.113.10/hook","events":["quota.exceeded"]}`, 400, "credentials"},
		{"no host", `{"url":"https:///hook","events":["quota.exceeded"]}`, 400, "no host"},
		{"missing url", `{"events":["quota.exceeded"]}`, 400, "url is required"},
		{"no events", `{"url":"https://93.184.215.10/hook","events":[]}`, 400, "events is required"},
		{"unknown event", `{"url":"https://93.184.215.10/hook","events":["key.rotated"]}`, 400, "key.rotated"},
		{"unknown event among known", `{"url":"https://93.184.215.10/hook","events":["quota.exceeded","key.deleted"]}`, 400, "key.deleted"},
		{"client-supplied id rejected", `{"id":"wh-mine","url":"https://93.184.215.10/hook","events":["quota.exceeded"]}`, 400, "invalid webhook registration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newKeyBudgetSetup(t)
			srv := s.server(t)
			rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.note) {
				t.Errorf("body = %s, want it to mention %q", rec.Body.String(), tc.note)
			}
		})
	}

	// Duplicate events are deduped, not rejected.
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["quota.exceeded","quota.exceeded","budget.warning"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dedupe create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	decodeWebhookBody(t, rec, &created)
	if len(created.Events) != 2 {
		t.Errorf("created events = %v, want 2 after dedupe", created.Events)
	}
}

func TestM819SecretNeverEchoed(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["budget.warning"],"secret":"mock-secret-value"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "mock-secret-value") {
		t.Fatalf("create response echoes the secret: %s", rec.Body.String())
	}
	var created webhookView
	decodeWebhookBody(t, rec, &created)
	if !created.HasSecret {
		t.Errorf("has_secret = false, want true (a secret was sent)")
	}
	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks/"+created.ID, "")
	if strings.Contains(rec.Body.String(), "mock-secret-value") {
		t.Fatalf("get response echoes the secret: %s", rec.Body.String())
	}
	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks", "")
	if strings.Contains(rec.Body.String(), "mock-secret-value") {
		t.Fatalf("list response echoes the secret: %s", rec.Body.String())
	}
}

func TestM819Fixtures(t *testing.T) {
	// Each fixture runs against a fresh server; the create
	// fixtures share the collection handler, the id fixtures the
	// per-id handler.
	collection := []string{
		"webhooks-create-201",
		"webhooks-create-budget-warning-201",
		"webhooks-create-400-invalid-url",
		"webhooks-create-400-unknown-event",
		"webhooks-list-200",
	}
	for _, name := range collection {
		t.Run(name, func(t *testing.T) {
			s := newKeyBudgetSetup(t)
			srv := s.server(t)
			apitest.Run(t, http.HandlerFunc(srv.handleWebhooks), apitest.LoadFixture(t, "m8.19", name))
		})
	}
	byID := []string{"webhooks-get-404", "webhooks-delete-404"}
	for _, name := range byID {
		t.Run(name, func(t *testing.T) {
			s := newKeyBudgetSetup(t)
			srv := s.server(t)
			apitest.Run(t, http.HandlerFunc(srv.handleWebhookByID), apitest.LoadFixture(t, "m8.19", name))
		})
	}
}

func TestM819FixtureWrongStatusFails(t *testing.T) {
	// Toggle proof for the harness path: the list fixture
	// asserts 200; demanding 201 against the same 200 response
	// must fail the check.
	f := apitest.LoadFixture(t, "m8.19", "webhooks-list-200")
	f.Assert.Status = http.StatusCreated
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := apitest.CheckResponse(resp, []byte(`{"webhooks":[]}`), f); err == nil {
		t.Fatal("CheckResponse with a wrong expected status did not fail — the contract assertion is not toggle-proven")
	}
}

// mockWebhookReceiver records deliveries for the budget.warning test.
type mockWebhookReceiver struct {
	mu     sync.Mutex
	bodies [][]byte
	sigs   []string
	ch     chan []byte
}

func (m *mockWebhookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.bodies = append(m.bodies, body)
	m.sigs = append(m.sigs, r.Header.Get("X-Codvian-Signature-256"))
	m.mu.Unlock()
	select {
	case m.ch <- body:
	default:
	}
	w.WriteHeader(http.StatusOK)
}

func TestM819BudgetWarningFiresOnThresholdCrossing(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	recv := &mockWebhookReceiver{ch: make(chan []byte, 10)}
	mock := httptest.NewServer(recv)
	defer mock.Close()

	// Seed the endpoint the way the config file does (a
	// localhost receiver cannot be registered through the API —
	// the SSRF default rejects it — so this seeds cfg directly
	// and rebuilds the dispatcher, exactly as main.go does from
	// the config file at startup).
	s.cfg.Webhooks = []config.WebhookConfig{{
		ID: "wh-test", URL: mock.URL, Events: []string{"budget.warning"}, Secret: "test-secret",
	}}
	// The production dispatcher refuses loopback targets at delivery
	// (egress policy); this test exercises the threshold logic, so it
	// delivers to its loopback receiver through the receiver's client.
	srv.webhookClient = mock.Client()
	srv.rebuildWebhookDispatcher()

	if err := s.led.SetInternalBudget(context.Background(), ledger.InternalBudget{
		ID: "warn-budget-m819", Scope: ledger.InternalBudgetScopeGlobal,
		Period: "daily", LimitType: "currency", LimitValue: 1.0,
		WarningThreshold: 0.8, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Below the threshold: a chat is served with the warning
	// header absent and no delivery.
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("below-threshold chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if h := rec.Header().Get("X-Codvian-Budget-Warning"); h != "" {
		t.Fatalf("below-threshold warning header = %q, want empty", h)
	}
	select {
	case body := <-recv.ch:
		t.Fatalf("below-threshold chat delivered a webhook unexpectedly: %s", body)
	case <-time.After(500 * time.Millisecond):
	}

	// Cross the threshold: $0.80 of a $1.00 budget at 0.8.
	s.seedPricedSpend(t, 20000, 20000)
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("at-threshold chat status = %d, want 200 (warn serves): %s", rec.Code, rec.Body.String())
	}
	if h := rec.Header().Get("X-Codvian-Budget-Warning"); h != "warn-budget-m819" {
		t.Fatalf("warning header = %q, want warn-budget-m819", h)
	}
	var body []byte
	select {
	case body = <-recv.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no budget.warning delivery arrived within 5s of the threshold crossing")
	}
	var delivered map[string]any
	if err := json.Unmarshal(body, &delivered); err != nil {
		t.Fatalf("delivered body is not JSON: %v (%s)", err, body)
	}
	if delivered["event"] != "budget.warning" {
		t.Errorf("delivered event = %v, want budget.warning", delivered["event"])
	}
	data, ok := delivered["data"].(map[string]any)
	if !ok {
		t.Fatalf("delivered data = %v, want an object", delivered["data"])
	}
	if data["budget_id"] != "warn-budget-m819" {
		t.Errorf("data.budget_id = %v, want warn-budget-m819", data["budget_id"])
	}
	pct, _ := data["pct"].(float64)
	if pct < 80 || pct >= 81 {
		t.Errorf("data.pct = %v, want ~80", data["pct"])
	}
	if data["threshold"] != 0.8 {
		t.Errorf("data.threshold = %v, want 0.8", data["threshold"])
	}
	// HMAC-SHA256 signature over the exact delivered body.
	recv.mu.Lock()
	sig := ""
	if len(recv.sigs) > 0 {
		sig = recv.sigs[0]
	}
	recv.mu.Unlock()
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write(body)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig != wantSig {
		t.Errorf("signature = %q, want %q", sig, wantSig)
	}

	// Edge-trigger: a second request while still above the
	// threshold serves with the header but must NOT deliver again.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	select {
	case body := <-recv.ch:
		t.Fatalf("repeat warn delivered a second budget.warning unexpectedly: %s", body)
	case <-time.After(500 * time.Millisecond):
	}
	recv.mu.Lock()
	n := len(recv.bodies)
	recv.mu.Unlock()
	if n != 1 {
		t.Errorf("deliveries = %d, want exactly 1 (edge-triggered, not per-request)", n)
	}
}

// TestM819UpdatePartial: PUT is a partial update (the sector baseline
// semantics) — only provided fields change, the rest are kept,
// and the secret is never echoed (has_secret is the observable).
func TestM819UpdatePartial(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["quota.exceeded"],"secret":"s3cr3t"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	decodeWebhookBody(t, rec, &created)
	get := func() webhookView {
		t.Helper()
		rec := callWebhookHandler(t, srv, "GET", "/api/webhooks/"+created.ID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var one webhookView
		decodeWebhookBody(t, rec, &one)
		return one
	}

	// Events only: URL and secret survive untouched.
	rec = callWebhookHandler(t, srv, "PUT", "/api/webhooks/"+created.ID, `{"events":["budget.warning"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put events status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := get(); got.URL != "https://93.184.215.10/hook" || len(got.Events) != 1 || got.Events[0] != "budget.warning" || !got.HasSecret {
		t.Fatalf("after events-only PUT: %+v, want same URL, budget.warning, secret kept", got)
	}

	// URL only: events survive untouched.
	rec = callWebhookHandler(t, srv, "PUT", "/api/webhooks/"+created.ID, `{"url":"https://93.184.215.20/other"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put url status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := get(); got.URL != "https://93.184.215.20/other" || len(got.Events) != 1 || got.Events[0] != "budget.warning" {
		t.Fatalf("after url-only PUT: %+v, want new URL, budget.warning kept", got)
	}

	// Empty object: a no-op update, still 200, nothing changes.
	rec = callWebhookHandler(t, srv, "PUT", "/api/webhooks/"+created.ID, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put empty status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := get(); got.URL != "https://93.184.215.20/other" || !got.HasSecret {
		t.Fatalf("after empty PUT: %+v, want unchanged", got)
	}

	// Clearing the secret: an explicitly empty secret clears it.
	rec = callWebhookHandler(t, srv, "PUT", "/api/webhooks/"+created.ID, `{"secret":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put clear-secret status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := get(); got.HasSecret {
		t.Fatalf("after clear-secret PUT: %+v, want has_secret false", got)
	}
}

// TestM819UpdateValidation: unknown id 404s; a bad URL, an
// unknown event, empty events and unknown fields are all 400s;
// nothing is stored on a refused update.
func TestM819UpdateValidation(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["quota.exceeded"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	decodeWebhookBody(t, rec, &created)

	cases := []struct {
		name string
		path string
		body string
		note string
	}{
		{"unknown id", "/api/webhooks/wh_nope", `{"events":["budget.warning"]}`, ""},
		{"loopback url", "/api/webhooks/" + created.ID, `{"url":"http://127.0.0.1:9/hook"}`, "loopback"},
		{"localhost name", "/api/webhooks/" + created.ID, `{"url":"https://localhost/hook"}`, "localhost"},
		{"ftp scheme", "/api/webhooks/" + created.ID, `{"url":"ftp://93.184.215.10/hook"}`, "http or https"},
		{"private url", "/api/webhooks/" + created.ID, `{"url":"https://10.0.0.5/hook"}`, "private-network"},
		{"empty url", "/api/webhooks/" + created.ID, `{"url":"  "}`, ""},
		{"unknown event", "/api/webhooks/" + created.ID, `{"events":["key.rotated"]}`, "key.rotated"},
		{"empty events", "/api/webhooks/" + created.ID, `{"events":[]}`, "events is required"},
		{"unknown field", "/api/webhooks/" + created.ID, `{"definitely_not_a_field":true}`, ""},
		{"client-supplied id", "/api/webhooks/" + created.ID, `{"id":"wh-mine"}`, ""},
		{"enabled field", "/api/webhooks/" + created.ID, `{"enabled":false}`, ""},
	}
	for _, tc := range cases {
		rec := callWebhookHandler(t, srv, "PUT", tc.path, tc.body)
		want := http.StatusBadRequest
		if tc.name == "unknown id" {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Errorf("%s: status = %d, want %d: %s", tc.name, rec.Code, want, rec.Body.String())
		}
		if tc.note != "" && !strings.Contains(rec.Body.String(), tc.note) {
			t.Errorf("%s: body %q does not name %q", tc.name, rec.Body.String(), tc.note)
		}
	}
	// The stored registration is untouched by every refused PUT.
	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks/"+created.ID, "")
	var one webhookView
	decodeWebhookBody(t, rec, &one)
	if one.URL != "https://93.184.215.10/hook" || len(one.Events) != 1 || one.Events[0] != "quota.exceeded" {
		t.Fatalf("after refused PUTs: %+v, want the original registration", one)
	}
}

// TestM819UpdateSecretNeverEchoed (B5): a PUT that rotates the
// secret — and a PUT that replaces url+events in one full
// payload — must never echo either the old or the new secret
// value, and the response carries no "secret" field at all.
// GET after the PUT shows the edit (new URL + events) with
// has_secret true and no secret value.
func TestM819UpdateSecretNeverEchoed(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	rec := callWebhookHandler(t, srv, "POST", "/api/webhooks", `{"url":"https://93.184.215.10/hook","events":["quota.exceeded"],"secret":"original-secret-value"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	decodeWebhookBody(t, rec, &created)

	// Full-payload PUT: new URL + new events + rotated secret.
	rec = callWebhookHandler(t, srv, "PUT", "/api/webhooks/"+created.ID, `{"url":"https://93.184.215.20/other","events":["budget.warning"],"secret":"rotated-secret-value"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "original-secret-value") || strings.Contains(body, "rotated-secret-value") || strings.Contains(body, `"secret"`) {
		t.Fatalf("put response echoes a secret: %s", body)
	}
	var updated webhookView
	decodeWebhookBody(t, rec, &updated)
	if updated.URL != "https://93.184.215.20/other" || len(updated.Events) != 1 || updated.Events[0] != "budget.warning" || !updated.HasSecret {
		t.Fatalf("put response = %+v, want new URL, budget.warning, has_secret true", updated)
	}

	// GET shows the edit, still with no secret value.
	rec = callWebhookHandler(t, srv, "GET", "/api/webhooks/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "original-secret-value") || strings.Contains(body, "rotated-secret-value") || strings.Contains(body, `"secret"`) {
		t.Fatalf("get-after-put response echoes a secret: %s", body)
	}
	var one webhookView
	decodeWebhookBody(t, rec, &one)
	if one.URL != "https://93.184.215.20/other" || len(one.Events) != 1 || one.Events[0] != "budget.warning" || !one.HasSecret {
		t.Fatalf("after PUT: %+v, want the edited registration with the secret kept", one)
	}
}
