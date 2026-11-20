package server

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Slice 5 (CHECKLIST 101-102; research-log entries 150/152):
// redaction default-on. Portkey 1.15.2 leaked prompts AND provider
// keys to an open log stream unless NODE_ENV=production; LiteLLM's
// spend-log redaction is OFF by default. These tests prove the
// opposite for Codvian Router in default mode: a real-shaped key
// and a real prompt never appear in log output, in error responses,
// or on the dashboard. All keys below are synthetic fixtures shaped
// like real keys, not real credentials.
const (
	redactTestKey    = "sk-proj-TESTKEY0123456789abcdefghij"
	redactTestPrompt = "PROMPTMARKER7f3a9 tell me about the secret meeting in Pune"
)

func TestRedactingLoggerDropsRealKey(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServer(t, &fakeEngine{}, WithLogger(log.New(&buf, "", 0)))
	// Even a log call that forgets RedactLogLine must not leak: New
	// wraps every server logger in a redactWriter.
	srv.logger.Printf("boom: provider add failed: %s", redactTestKey)
	srv.logger.Printf("plain line with email raunak@example.com and key " + redactTestKey)
	got := buf.String()
	if strings.Contains(got, redactTestKey) || strings.Contains(got, "TESTKEY0123456789") {
		t.Fatalf("log contains the provider key: %q", got)
	}
	if strings.Contains(got, "raunak@example.com") {
		t.Fatalf("log contains PII: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:SECRET]") || !strings.Contains(got, "[REDACTED:EMAIL]") {
		t.Fatalf("redaction placeholders missing in %q", got)
	}
}

func TestRedactWriterCatchesSplitSecret(t *testing.T) {
	var buf bytes.Buffer
	w := newRedactWriter(&buf)
	// A secret split across two Write calls (no newline in between)
	// must still be caught when the line completes.
	if _, err := w.Write([]byte("prefix " + redactTestKey[:10])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(redactTestKey[10:] + " suffix\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.Contains(got, redactTestKey) {
		t.Fatalf("split secret survived the redacting writer: %q", got)
	} else if !strings.Contains(got, "[REDACTED:SECRET]") {
		t.Fatalf("no placeholder in %q", got)
	}
}

func TestLogSafeErrorDropsUpstreamBody(t *testing.T) {
	// An upstream error body may echo the prompt verbatim, so the
	// log line must carry the status only — never the body.
	upErr := &provider.UpstreamError{StatusCode: 502, Body: "upstream echoed prompt: " + redactTestPrompt + " key " + redactTestKey}
	if got := logSafeError(upErr); got != "upstream error: HTTP 502" {
		t.Fatalf("logSafeError(upstream) = %q, want status only", got)
	}
	if got := logSafeError(&provider.QuotaError{Message: "quota " + redactTestKey, StatusCode: 402}); strings.Contains(got, redactTestKey) {
		t.Fatalf("logSafeError(quota) leaked the key: %q", got)
	}
	if got := logSafeError(&provider.RateLimitError{Body: redactTestKey}); strings.Contains(got, redactTestKey) {
		t.Fatalf("logSafeError(rate-limit) leaked the key: %q", got)
	}
	// A plain error carrying a key is redacted, not dropped.
	if got := logSafeError(errors.New("dial failed with " + redactTestKey)); strings.Contains(got, redactTestKey) {
		t.Fatalf("logSafeError(plain) leaked the key: %q", got)
	}
}

// TestChatCompletionsNeverLogsPromptOrKey is the end-to-end proof
// for default mode: a real chat request whose prompt contains a
// unique marker AND a key-shaped string goes through the real
// handler (guardrails on, blocking off by default), and neither the
// prompt text nor the key appears anywhere in the server log.
func TestChatCompletionsNeverLogsPromptOrKey(t *testing.T) {
	var buf bytes.Buffer
	eng := &fakeEngine{chatResp: provider.ChatResponse{Content: "Hello there"}}
	srv := newTestServer(t, eng, WithLogger(log.New(&buf, "", 0)))
	body := `{"model":"model-a","messages":[{"role":"user","content":"` + redactTestPrompt + ` my key is ` + redactTestKey + `"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Guardrail"); got != "flagged" {
		t.Fatalf("X-Codvian-Guardrail = %q, want flagged (the key in the prompt must be detected)", got)
	}
	logged := buf.String()
	if strings.Contains(logged, redactTestPrompt) {
		t.Fatalf("server log contains the prompt text: %q", logged)
	}
	if strings.Contains(logged, redactTestKey) {
		t.Fatalf("server log contains the provider key: %q", logged)
	}
	// The guardrail hit itself must still be visible — kind and
	// pattern only, never prompt text.
	if !strings.Contains(logged, "guardrail: kind=secret pattern=openai-key blocked=false") {
		t.Fatalf("guardrail log line missing or wrong: %q", logged)
	}
	// And the dashboard guardrail feed must not render the key
	// either (its previews are redacted by construction).
	feed := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if strings.Contains(feed.Body.String(), redactTestKey) {
		t.Fatalf("dashboard guardrail feed contains the provider key: %s", feed.Body.String())
	}
}

// TestStreamFailureLogDropsUpstreamBody proves the mid-stream
// failure path: the stream fails after content was sent, the resume
// fails with an upstream error whose body echoes the prompt and a
// key, and the log line carries the status only.
func TestStreamFailureLogDropsUpstreamBody(t *testing.T) {
	var buf bytes.Buffer
	upErr := &provider.UpstreamError{
		StatusCode: 502,
		Body:       "upstream echoed prompt: " + redactTestPrompt + " key " + redactTestKey,
	}
	eng := &fakeEngine{
		deltas:    []string{"hello "},
		failAfter: 1,
		streamErr: upErr,
	}
	srv := newTestServer(t, eng, WithLogger(log.New(&buf, "", 0)))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream started, then failed mid-way)", rec.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "stream failed after") {
		t.Fatalf("stream-failure log line missing (path not hit?): %q", logged)
	}
	if strings.Contains(logged, redactTestPrompt) {
		t.Fatalf("stream-failure log contains the prompt text: %q", logged)
	}
	if strings.Contains(logged, redactTestKey) {
		t.Fatalf("stream-failure log contains the provider key: %q", logged)
	}
	if !strings.Contains(logged, "upstream error: HTTP 502") {
		t.Fatalf("stream-failure log lacks the status-only error: %q", logged)
	}
}

// TestAddProviderInvalidBodyDoesNotEchoKey proves the provider-add
// endpoint never bounces the request body (which carries the API
// key) back in an error response.
func TestAddProviderInvalidBodyDoesNotEchoKey(t *testing.T) {
	srv, _ := newAddTestServer(t)
	rec := postAdd(t, srv, `{"base_url":"http://127.0.0.1:9/v1","api_key":"`+redactTestKey+`"`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), redactTestKey) {
		t.Fatalf("error response echoes the provider key: %s", rec.Body.String())
	}
}

// TestAddProviderUnknownCatalogDoesNotEchoKey proves an
// operator-typed catalog id that happens to be a pasted key is not
// bounced back in the 404 message.
func TestAddProviderUnknownCatalogDoesNotEchoKey(t *testing.T) {
	srv, _ := newAddTestServer(t)
	rec := postAdd(t, srv, `{"catalog_id":"`+redactTestKey+`","api_key":"k"}`, "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), redactTestKey) {
		t.Fatalf("error response echoes the pasted key: %s", rec.Body.String())
	}
}

// TestDashboardNeverRendersFullProviderKey proves the dashboard
// provider feed — fed by the real registry holding a real-shaped
// key — never renders that key.
func TestDashboardNeverRendersFullProviderKey(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, _ := newAddTestServer(t)
	rec := postAdd(t, srv, `{"base_url":"`+mock.URL+`","api_key":"`+redactTestKey+`"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("add provider: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	feed := doReq(t, srv, http.MethodGet, "/dashboard/providers", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("dashboard providers: status = %d", feed.Code)
	}
	if strings.Contains(feed.Body.String(), redactTestKey) {
		t.Fatalf("dashboard provider feed contains the full provider key: %s", feed.Body.String())
	}
}

// TestSettingsExportNeverRendersKeys proves the settings export —
// built from the live config holding real gateway-key hashes and a
// provider base URL — never renders a full key, a key hash, or a
// base URL (base URLs can carry embedded credentials).
func TestSettingsExportNeverRendersKeys(t *testing.T) {
	path := t.TempDir() + "/config.json"
	cfg, raw1, raw2 := dangerTestConfig(t, path)
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	rec := doReq(t, srv, http.MethodPost, "/admin/settings/export", `{"confirm":"EXPORT SETTINGS"}`, raw1)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings export: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{raw1, raw2, "base_url", "127.0.0.1:9"} {
		if strings.Contains(body, secret) {
			t.Fatalf("settings export contains %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"gateway_key_count":2`) {
		t.Fatalf("settings export lacks the honest key count: %s", body)
	}
}
