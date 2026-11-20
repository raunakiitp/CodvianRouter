// Contract + behaviour tests for POST /api/guardrails/test (M8.16).
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// TestM816GuardrailsTestFixture is the apitest contract test with
// blocking off (the default): the email sample is flagged and
// redacted, nothing would block, nothing is recorded or sent.
func TestM816GuardrailsTestFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	f := apitest.LoadFixture(t, "m8.16", "guardrails-test-200")
	apitest.Run(t, http.HandlerFunc(srv.handleGuardrailsTest), f)
}

// TestM816GuardrailsTestBlockedFixture is the apitest contract test
// with BlockPII on: the same sample must report the block the live
// config would apply — still without recording or sending anything.
func TestM816GuardrailsTestBlockedFixture(t *testing.T) {
	store := guardrails.NewStoreWith(guardrails.Config{BlockPII: true})
	srv := newTestServer(t, &fakeEngine{}, WithGuardrails(store))
	f := apitest.LoadFixture(t, "m8.16", "guardrails-test-blocked-200")
	apitest.Run(t, http.HandlerFunc(srv.handleGuardrailsTest), f)
}

// TestM816GuardrailsTestDoesNotRecord proves the probe is not usage:
// the live store's counters are unchanged after a probe, the raw
// sample appears nowhere in the response, and the response carries
// recorded:false / sent_upstream:false.
func TestM816GuardrailsTestDoesNotRecord(t *testing.T) {
	store := guardrails.NewStore()
	srv := newTestServer(t, &fakeEngine{}, WithGuardrails(store))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/guardrails/test", strings.NewReader(`{"text":"my key is sk-live-0123456789abcdef and reach me at alice@example.com"}`))
	srv.handleGuardrailsTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "alice@example.com") || strings.Contains(body, "sk-live-0123456789abcdef") {
		t.Fatalf("response echoes the raw sample: %s", body)
	}
	var resp guardrailsTestResponse
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Recorded || resp.SentUpstream || resp.ExternalRun {
		t.Fatalf("recorded/sent_upstream/external_run = %v/%v/%v, want all false", resp.Recorded, resp.SentUpstream, resp.ExternalRun)
	}
	stats := store.Stats()
	if stats.Checked != 0 || stats.Redacted != 0 || stats.Blocked != 0 || len(stats.Events) != 0 {
		t.Fatalf("live store changed by the probe: checked=%d redacted=%d blocked=%d events=%d, want all 0", stats.Checked, stats.Redacted, stats.Blocked, len(stats.Events))
	}
	kinds := map[string]bool{}
	for _, f := range resp.Findings {
		kinds[f.Kind] = true
	}
	if !kinds["pii"] || !kinds["secret"] {
		t.Fatalf("findings kinds = %v, want pii and secret", kinds)
	}
}

// TestM816GuardrailsTestCleanSample proves a clean sample is
// allowed with no findings and the redacted text equal to the input.
func TestM816GuardrailsTestCleanSample(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/guardrails/test", strings.NewReader(`{"text":"hello, how are you?"}`))
	srv.handleGuardrailsTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp guardrailsTestResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Action != "allowed" || resp.WouldBlock || len(resp.Findings) != 0 {
		t.Fatalf("clean sample response = %+v, want allowed with no findings", resp)
	}
	if resp.RedactedText != "hello, how are you?" {
		t.Fatalf("redacted_text = %q, want the input unchanged", resp.RedactedText)
	}
}

// TestM816GuardrailsTestBadRequests pins the 400s: a missing text
// field and malformed JSON are rejected, and GET is 405.
func TestM816GuardrailsTestBadRequests(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, body := range []string{`{}`, `{"text":123}`, `not json`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/guardrails/test", strings.NewReader(body))
		srv.handleGuardrailsTest(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, want 400", body, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.handleGuardrailsTest(rec, httptest.NewRequest(http.MethodGet, "/api/guardrails/test", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

// TestM816GuardrailsTestToggleNegative proves the contract test is
// toggle-verified: the same fixture asserting a wrong status must
// fail CheckResponse.
func TestM816GuardrailsTestToggleNegative(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.16", "guardrails-test-200")
	f.Assert.Status = http.StatusCreated
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := apitest.CheckResponse(resp, []byte(`{}`), f); err == nil {
		t.Fatalf("CheckResponse with assert 201 against a 200 response passed; toggle check is broken")
	}
}
