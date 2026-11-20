// M10-12 dashboard Webhooks page: list / create / edit / delete wired
// to the live GET/POST/PUT/DELETE /api/webhooks endpoints
// (registerWebhooksRoutes, internal/server/webhooks_api.go;
// guard.go rows 213-217 — POST/PUT/DELETE TierAlwaysProtected,
// GETs TierManagement, scope "admin" throughout). The honest
// loopback rule is surfaced: localhost / private targets are
// rejected by default and the server's own rejection text (register
// through the config file) is shown when a create fails.
//
// Edit (B5 follow-up): each row has an Edit button that opens an
// edit form (URL, event checkboxes, optional new signing secret —
// blank keeps the stored secret, which is never displayed) and
// saves through PUT /api/webhooks/{id} via the shared
// adminHeaders() helper. B5 reviewer nits (PASS-WITH-NITS):
// a stale id toasts and reloads the list, Save is disabled while
// the PUT is in flight, and a "Clear secret" checkbox sends
// {"secret":""} (clear wins over a typed secret). The backend PUT handler
// (handleWebhookUpdate, webhooks_api.go:446) is proven by the
// existing PUT tests TestM819UpdatePartial /
// TestM819UpdateValidation / TestM819UpdateSecretNeverEchoed
// (webhooks_api_test.go:365–518); this file proves the page's edit
// path end to end (create → PUT edit → list shows the change,
// secret never echoed) plus the edit form and PUT wiring in the
// served page.
//
// budget.warning delivery is NOT re-proven here: it is proven by
// the existing backend test TestM819BudgetWarningFiresOnThresholdCrossing
// (internal/server/webhooks_api_test.go:252), which drives a real
// threshold crossing into a mock receiver and asserts exactly one
// edge-triggered delivery. This file proves only what the page
// itself does: create → list → edit → delete, and the loopback rejection.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// m10_12Section returns the served HTML of <section id="view-webhooks">.
func m10_12Section(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-webhooks"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-webhooks section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-webhooks section is not closed")
	}
	return rest[:end]
}

// m10_12JSRegion returns this slice's own JS: from its header
// comment to the next section comment, so the no-PUT / no-edit
// assertions below check only this slice's code, never a sibling's.
func m10_12JSRegion(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "M10-12 Webhooks")
	if start < 0 {
		t.Fatalf("dashboard script has no M10-12 Webhooks block")
	}
	rest := script[start:]
	end := strings.Index(rest, "Concurrency: live limiter state")
	if end < 0 {
		t.Fatalf("M10-12 Webhooks JS block has no end marker")
	}
	return rest[:end]
}

func TestDashboardM10_12WebhooksRenders(t *testing.T) {
	body := m10_0DashboardBody(t)
	section := m10_12Section(t, body)
	script := m10_0InlineScript(t, body)
	js := m10_12JSRegion(t, script)

	for _, want := range []string{
		"<h1>Webhooks</h1>",
		// Honest empty state + the single create action.
		"No webhooks yet",
		`id="m10_12EmptyCreateBtn"`, "Create your first webhook",
		// Create form: URL + both event kinds + optional secret.
		`id="m10_12UrlInput"`, `id="m10_12EventQuota"`, `id="m10_12EventBudget"`,
		`id="m10_12SecretInput"`, `id="m10_12CreateBtn"`, "Create webhook",
		"quota.exceeded", "budget.warning",
		// List table + refresh + load-with-key row (GET is admin-scoped).
		`id="m10_12TableBody"`, `id="m10_12Count"`, `id="m10_12RefreshBtn"`,
		`id="m10_12KeyRow"`, `id="m10_12LoadKeyBtn"`, "Load with gateway key",
		// The honest loopback rule + the edit path (B5 follow-up).
		"Localhost and private-network targets are rejected by default",
		"register one through the config file instead",
		"Edit a webhook with its Edit button",
		"leave the secret blank to keep the current one",
		// Edit form: URL + both event kinds + optional new secret,
		// Save / Cancel. Hidden until a row's Edit button opens it.
		`id="m10_12EditCard"`, `id="m10_12EditUrlInput"`,
		`id="m10_12EditEventQuota"`, `id="m10_12EditEventBudget"`,
		`id="m10_12EditSecretInput"`, `id="m10_12EditSaveBtn"`,
		`id="m10_12EditCancelBtn"`, "Save changes", "Cancel",
		// Delivery honesty: best-effort, no retries, no delivery log.
		"Delivery is best-effort", "no retries",
		"There is no delivery log, so this page shows registrations only",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Webhooks page missing %q", want)
		}
	}

	// The section reads the live /api feed and mutates through the
	// shared adminHeaders() helper — no second auth path.
	for _, want := range []string{
		`fetch("/api/webhooks"`,
		`fetch("/api/webhooks/"`,
		`method: "POST"`,
		`method: "PUT"`,
		`method: "DELETE"`,
		"adminHeaders()",
		"m10_12LoadWebhooks",
		"m10_12CreateWebhook",
		"m10_12OpenEdit",
		"m10_12SaveEdit",
		"m10_12CloseEdit",
		"m10_12DeleteWebhook",
		"m10_12RenderWebhooks",
		"data-webhook-edit",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dashboard Webhooks JS missing %q", want)
		}
	}

	// Edit (B5 follow-up): the render adds one Edit button per row
	// next to Delete, and the edit form is hidden until opened.
	if !strings.Contains(js, `data-webhook-edit="`) || !strings.Contains(js, `>Edit</button>`) {
		t.Error("Webhooks rows have no Edit button")
	}
	if !strings.Contains(section, `id="m10_12EditCard" style="margin-bottom:14px; display:none"`) {
		t.Error("Webhooks edit form is not hidden by default")
	}
	// The secret is never displayed: the column renders only
	// "Secret set" / "No secret", and the JS never reads a secret
	// value off a registration (only has_secret is rendered).
	if !strings.Contains(js, "Secret set") || !strings.Contains(js, "No secret") {
		t.Error("Webhooks secret column does not render only Secret set / No secret")
	}
	if strings.Contains(js, "w.secret") || strings.Contains(js, ".secret ||") {
		t.Error("Webhooks JS reads a secret value off a registration")
	}
	// No stale delete-and-recreate wording anywhere in the section.
	for _, banned := range []string{"Edit by deleting", "delete it and create a new one"} {
		if strings.Contains(section, banned) || strings.Contains(js, banned) {
			t.Errorf("Webhooks page still says %q", banned)
		}
	}

	// No fake-data markers, no Playground.
	for _, banned := range []string{"Lorem", "Sample data", "sample data", "Playground"} {
		if strings.Contains(section, banned) {
			t.Errorf("Webhooks section contains %q", banned)
		}
	}
	// The delete path is confirmed before it fires.
	if !strings.Contains(js, `window.confirm("Delete this webhook?`) {
		t.Error("Webhooks delete has no confirmation dialog")
	}
	// The loopback rejection text comes from the server verbatim —
	// the create handler shows the error body, it never invents one.
	if !strings.Contains(js, "res.body.error.message") {
		t.Error("Webhooks create does not surface the server error text")
	}
}

// m10_12WebhooksList drives GET /api/webhooks against the live
// test server and decodes the list.
func m10_12WebhooksList(t *testing.T, srv *Server, key string) (int, webhooksResponse) {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/api/webhooks", "", key)
	var out webhooksResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode webhooks list: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// m10_12Setup returns a server whose config is wired (webhook
// mutations need s.cfg) plus an admin-scoped raw key.
func m10_12Setup(t *testing.T) (*Server, string) {
	t.Helper()
	s := newKeyBudgetSetup(t)
	adminRaw := adminKeyFor(t, s)
	return s.server(t), adminRaw
}

// Create → list shows it → get by id → delete → list empty again,
// all through the mux (proving registerWebhooksRoutes), with the
// admin key. Without a key both the list and the create refuse.
func TestDashboardM10_12CreateListDeleteRoundTrip(t *testing.T) {
	srv, adminRaw := m10_12Setup(t)

	// No key → 401 on both the list and the create (admin scope).
	if code, _ := m10_12WebhooksList(t, srv, ""); code != http.StatusUnauthorized {
		t.Errorf("GET /api/webhooks without a key status = %d, want 401", code)
	}
	rec := doReq(t, srv, http.MethodPost, "/api/webhooks", `{"url":"https://203.0.113.10/hook","events":["quota.exceeded"]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/webhooks without a key status = %d, want 401", rec.Code)
	}

	// Empty list to start (honest zero, not a fabricated row).
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200: %s", code, rec.Body.String())
	} else if len(list.Webhooks) != 0 {
		t.Fatalf("list before create = %+v, want empty", list)
	}

	// Create with both event kinds + a secret.
	rec = doReq(t, srv, http.MethodPost, "/api/webhooks",
		`{"url":"https://203.0.113.10/hook","events":["quota.exceeded","budget.warning"],"secret":"hook-secret"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created webhook: %v: %s", err, rec.Body.String())
	}
	if !strings.HasPrefix(created.ID, "wh_") {
		t.Errorf("created id = %q, want a server-generated wh_ id", created.ID)
	}
	if created.URL != "https://203.0.113.10/hook" || len(created.Events) != 2 {
		t.Errorf("created = %+v, want url + both events echoed back", created)
	}
	if !created.HasSecret {
		t.Errorf("created has_secret = false, want true (secret was set)")
	}
	if strings.Contains(rec.Body.String(), "hook-secret") {
		t.Error("create response echoes the secret value")
	}

	// List shows it; get-by-id returns it.
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200", code)
	} else if len(list.Webhooks) != 1 || list.Webhooks[0].ID != created.ID {
		t.Fatalf("list after create = %+v, want exactly the created registration", list)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/webhooks/"+created.ID, "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/webhooks/{id} status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Delete without a key is refused; with the key it succeeds.
	rec = doReq(t, srv, http.MethodDelete, "/api/webhooks/"+created.ID, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE without a key status = %d, want 401", rec.Code)
	}
	rec = doReq(t, srv, http.MethodDelete, "/api/webhooks/"+created.ID, "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// List is empty again; get-by-id is 404.
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200", code)
	} else if len(list.Webhooks) != 0 {
		t.Errorf("list after delete = %+v, want empty", list)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/webhooks/"+created.ID, "", adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// Loopback / localhost creates fail with the honest error: the
// 400 names the loopback rule and points at the config file —
// exactly the text the dashboard shows verbatim in the form.
func TestDashboardM10_12LoopbackRejected(t *testing.T) {
	srv, adminRaw := m10_12Setup(t)

	// The localhost-name rejection names the config-file path
	// (validateWebhookURL's localhost branch); the literal-IP
	// rejections name the address class instead (checkWebhookIP).
	// Both are the server's own honest text, shown verbatim by
	// the page — the test asserts each message says what the
	// backend actually says, not a uniform wording it does not.
	for _, tc := range []struct {
		name, url, want string
		configFile      bool
	}{
		{"localhost name", "https://localhost/hook", "localhost", true},
		{"loopback ip", "http://127.0.0.1:9/hook", "loopback", false},
		{"private ip", "https://192.168.1.1/hook", "private-network", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, srv, http.MethodPost, "/api/webhooks",
				`{"url":`+strconv.Quote(tc.url)+`,"events":["quota.exceeded"]}`, adminRaw)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST %s status = %d, want 400: %s", tc.url, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("rejection body = %s, want it to mention %q", body, tc.want)
			}
			if tc.configFile && !strings.Contains(body, "config file") {
				t.Errorf("rejection body = %s, want it to point at the config file", body)
			}
		})
	}

	// Nothing was registered by the rejected creates.
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200", code)
	} else if len(list.Webhooks) != 0 {
		t.Errorf("list after rejected creates = %+v, want empty", list)
	}
}

// Edit round trip (B5 follow-up): create → PUT edit (URL + events,
// no secret field: the stored secret is kept) → list shows the
// change with has_secret still true and no secret value anywhere;
// then a PUT that rotates the secret, again with no echo. PUT
// without the admin key is refused, and PUT to an unknown id 404s.
func TestDashboardM10_12EditRoundTrip(t *testing.T) {
	srv, adminRaw := m10_12Setup(t)

	rec := doReq(t, srv, http.MethodPost, "/api/webhooks",
		`{"url":"https://203.0.113.10/hook","events":["quota.exceeded"],"secret":"hook-secret"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created webhook: %v: %s", err, rec.Body.String())
	}

	// PUT without a key is refused (TierAlwaysProtected, admin).
	rec = doReq(t, srv, http.MethodPut, "/api/webhooks/"+created.ID,
		`{"url":"https://203.0.113.20/other","events":["budget.warning"]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("PUT without a key status = %d, want 401", rec.Code)
	}
	// PUT to an unknown id 404s.
	rec = doReq(t, srv, http.MethodPut, "/api/webhooks/wh_nope",
		`{"events":["budget.warning"]}`, adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown id status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	// Edit URL + events only — no secret field, so the stored
	// secret is kept (this is what the page sends when the edit
	// secret box is left blank).
	rec = doReq(t, srv, http.MethodPut, "/api/webhooks/"+created.ID,
		`{"url":"https://203.0.113.20/other","events":["budget.warning"]}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT edit status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "hook-secret") || strings.Contains(body, `"secret"`) {
		t.Fatalf("PUT response echoes a secret: %s", body)
	}
	var updated webhookView
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated webhook: %v: %s", err, rec.Body.String())
	}
	if updated.URL != "https://203.0.113.20/other" || len(updated.Events) != 1 || updated.Events[0] != "budget.warning" || !updated.HasSecret {
		t.Fatalf("PUT response = %+v, want new URL, budget.warning, secret kept", updated)
	}

	// List shows the edit; GET by id agrees, still no secret value.
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200", code)
	} else if len(list.Webhooks) != 1 || list.Webhooks[0].URL != "https://203.0.113.20/other" || len(list.Webhooks[0].Events) != 1 || !list.Webhooks[0].HasSecret {
		t.Fatalf("list after PUT = %+v, want the edited registration", list)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/webhooks/"+created.ID, "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "hook-secret") || strings.Contains(body, `"secret"`) {
		t.Fatalf("GET after PUT echoes a secret: %s", body)
	}

	// Rotate the secret through PUT: neither the old nor the new
	// value is ever echoed back.
	rec = doReq(t, srv, http.MethodPut, "/api/webhooks/"+created.ID,
		`{"secret":"rotated-secret"}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT rotate-secret status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "hook-secret") || strings.Contains(body, "rotated-secret") || strings.Contains(body, `"secret"`) {
		t.Fatalf("PUT rotate-secret response echoes a secret: %s", body)
	}
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200", code)
	} else if len(list.Webhooks) != 1 || !list.Webhooks[0].HasSecret {
		t.Fatalf("list after secret rotation = %+v, want has_secret true", list)
	}
}

// TestDashboardM10_12EditClearSecretRoundTrip proves the
// payload the "Clear secret" checkbox sends: create with a
// secret → a PUT with no secret field keeps it (blank-means-keep,
// checkbox unchecked) → a PUT with {"secret":""} clears it
// (checkbox checked) → has_secret is false everywhere and the
// secret value is never echoed. This is the backend behaviour
// the checkbox relies on (also proven by TestM819UpdatePartial);
// here it is driven through the same mux the page calls.
func TestDashboardM10_12EditClearSecretRoundTrip(t *testing.T) {
	srv, adminRaw := m10_12Setup(t)

	rec := doReq(t, srv, http.MethodPost, "/api/webhooks",
		`{"url":"https://203.0.113.10/hook","events":["quota.exceeded"],"secret":"hook-secret"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created webhookView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created webhook: %v: %s", err, rec.Body.String())
	}
	if !created.HasSecret {
		t.Fatalf("created = %+v, want has_secret true", created)
	}

	// No secret field (checkbox unchecked, field blank): kept.
	rec = doReq(t, srv, http.MethodPut, "/api/webhooks/"+created.ID,
		`{"url":"https://203.0.113.10/hook","events":["quota.exceeded"]}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT keep-secret status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200", code)
	} else if len(list.Webhooks) != 1 || !list.Webhooks[0].HasSecret {
		t.Fatalf("list after keep PUT = %+v, want has_secret true", list)
	}

	// {"secret":""} (checkbox checked): cleared, never echoed.
	rec = doReq(t, srv, http.MethodPut, "/api/webhooks/"+created.ID,
		`{"secret":""}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT clear-secret status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "hook-secret") || strings.Contains(body, `"secret"`) {
		t.Fatalf("PUT clear-secret response echoes a secret: %s", body)
	}
	if code, list := m10_12WebhooksList(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /api/webhooks status = %d, want 200", code)
	} else if len(list.Webhooks) != 1 || list.Webhooks[0].HasSecret {
		t.Fatalf("list after clear PUT = %+v, want has_secret false", list)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/webhooks/"+created.ID, "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after clear status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "hook-secret") || strings.Contains(body, `"secret"`) {
		t.Fatalf("GET after clear echoes a secret: %s", body)
	}
}

// TestDashboardM10_12EditFormWiring asserts the edit form and its
// PUT wiring exist in the served page: the form pre-fills URL and
// events from the loaded list but always starts the secret box
// blank, Save sends a partial PUT through adminHeaders() with the
// secret included only when typed, and Cancel closes the form.
func TestDashboardM10_12EditFormWiring(t *testing.T) {
	body := m10_0DashboardBody(t)
	section := m10_12Section(t, body)
	script := m10_0InlineScript(t, body)
	js := m10_12JSRegion(t, script)

	// The edit secret box is a password field whose placeholder
	// says blank keeps the current secret; it is never pre-filled.
	for _, want := range []string{
		`id="m10_12EditSecretInput"`,
		`type="password"`,
		"Leave blank to keep the current secret",
		`id="m10_12EditFormError"`,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Webhooks edit form missing %q", want)
		}
	}
	// Open: pre-fill from the loaded list, secret box blank.
	for _, want := range []string{
		"function m10_12OpenEdit",
		"m10_12CurrentWebhooks",
		`if(s) s.value = "";`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard Webhooks edit open missing %q", want)
		}
	}
	// Save: partial PUT through the shared adminHeaders() helper,
	// secret sent only when a new one was typed.
	for _, want := range []string{
		"function m10_12SaveEdit",
		`fetch("/api/webhooks/" + encodeURIComponent(id)`,
		`method: "PUT"`,
		"adminHeaders()",
		`else if(secret) payload.secret = secret;`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard Webhooks edit save missing %q", want)
		}
	}
	// B5 reviewer nits (reviewer ccd697ee, PASS-WITH-NITS):
	// (1) a stale id toasts and reloads the list instead of
	// returning silently; (2) Save is disabled while the PUT is
	// in flight and re-enabled when it settles; (3) a "Clear
	// secret" checkbox sends {"secret":""} and wins over a typed
	// secret, while blank-means-keep is unchanged when unchecked.
	for _, want := range []string{
		`id="m10_12ClearSecret"`,
		"Clear secret",
		"wins over any secret typed above",
		`tick "Clear secret" in the edit form to remove it entirely`,
		`toast("That webhook is no longer in the list`,
		`m10_12LoadWebhooks(m10_12Keyed); return;`,
		`if(clearEl) clearEl.checked = false;`,
		`if(clearSecret) payload.secret = "";`,
		`saveBtn.disabled = true;`,
		`saveBtn.disabled = false;`,
		`.finally(function(){`,
	} {
		if !strings.Contains(js, want) && !strings.Contains(section, want) {
			t.Errorf("dashboard Webhooks edit missing B5 nit fix %q", want)
		}
	}
	// The stale-id path is not a silent return: the old
	// `if(!w) return;` must be gone.
	if strings.Contains(js, "if(!w) return;") {
		t.Error("dashboard Webhooks edit still silently returns on a stale id")
	}
	// Clear wins over a typed secret: the clear branch must come
	// before the typed-secret branch in the payload logic.
	if clearAt, typedAt := strings.Index(js, `payload.secret = "";`), strings.Index(js, `else if(secret) payload.secret = secret;`); clearAt < 0 || typedAt < 0 || clearAt > typedAt {
		t.Error("dashboard Webhooks edit clear-secret branch does not precede the typed-secret branch")
	}
	// Save / Cancel buttons are wired; Cancel closes the form.
	for _, want := range []string{
		`m10_12EditSaveBtn`,
		`m10_12EditCancelBtn`,
		"function m10_12CloseEdit",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard Webhooks edit wiring missing %q", want)
		}
	}
	// Row clicks reach the edit opener; the delete path is untouched.
	if !strings.Contains(js, `ev.target.closest("[data-webhook-edit]")`) || !strings.Contains(js, "m10_12OpenEdit(editBtn.getAttribute") {
		t.Error("dashboard Webhooks rows do not open the edit form")
	}
}

// TestDashboardM10_12InlineScriptSyntax runs node --check over the
// extracted inline script of the served dashboard (same pattern as
// TestDashboardM10_4InlineScriptSyntax). It skips when node is not
// installed.
func TestDashboardM10_12InlineScriptSyntax(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed: cannot run node --check")
	}
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write extracted script: %v", err)
	}
	out, err := exec.Command("node", "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
}
