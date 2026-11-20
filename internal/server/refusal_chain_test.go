// Content-policy refusal chain, server half (CHECKLIST 161-162):
// an exhausted refusal chain answers HTTP 403 with the
// policy-refusal code -32001 and reason
// "upstream_content_refusal" — never a 502, never a fake
// success, never counted as an upstream failure — and the
// guardrails feed carries the live refusal-chain outcomes.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// refusalStatsEngine is a fakeEngine that also exposes live
// refusal-chain outcomes, the way the real engine does.
type refusalStatsEngine struct {
	*fakeEngine
	stats router.RefusalChainStats
}

func (e *refusalStatsEngine) RefusalChainStats() router.RefusalChainStats { return e.stats }

func decodeRefusalBody(t *testing.T, recBody string) (code *int, typ, reason string) {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    *int   `json:"code"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(recBody), &body); err != nil {
		t.Fatalf("refusal body is not JSON: %v (%s)", err, recBody)
	}
	return body.Error.Code, body.Error.Type, body.Error.Reason
}

// TestUpstreamContentRefusalShape proves an exhausted refusal
// chain is an honest refusal, not an upstream failure (CHECKLIST
// 161): HTTP 403 with the Codvian policy-refusal code -32001,
// type "policy_refusal" and reason "upstream_content_refusal" —
// never the upstream -32603, never a 502 — and it is NOT counted
// as an upstream failure.
func TestUpstreamContentRefusalShape(t *testing.T) {
	recorder := metrics.NewRecorder()
	eng := &fakeEngine{chatErr: fmt.Errorf("%w: %w", router.ErrContentRefused,
		&provider.ContentRefusalError{StatusCode: 400, Message: "content filtered due to policy"})}
	srv := newTestServer(t, eng, WithMetrics(recorder))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	code, typ, reason := decodeRefusalBody(t, rec.Body.String())
	if code == nil || *code != guardrails.PolicyRefusalCode {
		t.Fatalf("error.code = %v, want %d (body %s)", code, guardrails.PolicyRefusalCode, rec.Body.String())
	}
	if typ != "policy_refusal" {
		t.Fatalf("error.type = %q, want policy_refusal", typ)
	}
	if reason != "upstream_content_refusal" {
		t.Fatalf("error.reason = %q, want upstream_content_refusal", reason)
	}
	if recorder.UpstreamFailures() != 0 {
		t.Fatalf("upstream_failures = %d, want 0 (a refusal is not an upstream failure)", recorder.UpstreamFailures())
	}
	if recorder.ContentRefusals() != 0 {
		t.Fatalf("content_refusals = %d, want 0 (the recorder counts refusals only when the engine reports them)", recorder.ContentRefusals())
	}
}

// TestBareContentRefusalErrorIsRefusal proves a bare content
// refusal error — one that stopped the request mid-chain, e.g. a
// replay-exempt request — is answered the same way: 403 /
// -32001 / upstream_content_refusal, not the upstream's own 400
// and not a 502.
func TestBareContentRefusalErrorIsRefusal(t *testing.T) {
	recorder := metrics.NewRecorder()
	eng := &fakeEngine{chatErr: &provider.UpstreamError{StatusCode: 400, Body: `{"error":{"message":"content filtered due to policy","code":"content_filter"}}`}}
	srv := newTestServer(t, eng, WithMetrics(recorder))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "hello", false), "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	code, typ, reason := decodeRefusalBody(t, rec.Body.String())
	if code == nil || *code != guardrails.PolicyRefusalCode || typ != "policy_refusal" || reason != "upstream_content_refusal" {
		t.Fatalf("error = code %v type %q reason %q, want -32001 / policy_refusal / upstream_content_refusal", code, typ, reason)
	}
	if recorder.UpstreamFailures() != 0 {
		t.Fatalf("upstream_failures = %d, want 0", recorder.UpstreamFailures())
	}
}

// TestGuardrailsFeedRefusalChain proves the guardrails feed
// carries the live refusal-chain outcomes (CHECKLIST 162): the
// engine's counts and most recent refusing target, served as
// refusal_chain — and that an engine without the stats keeps
// the field absent, so the dashboard keeps its honest empty
// state instead of inventing zeros as refusals.
func TestGuardrailsFeedRefusalChain(t *testing.T) {
	when := time.Date(2026, 11, 18, 3, 30, 0, 0, time.UTC)
	eng := &refusalStatsEngine{
		fakeEngine: &fakeEngine{},
		stats: router.RefusalChainStats{
			Refusals:         3,
			ServedViaRefusal: 2,
			Exhausted:        1,
			LastProvider:     "prov-a",
			LastModel:        "model-m",
			LastAt:           when,
		},
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed status = %d", rec.Code)
	}
	var feed struct {
		guardrails.Stats
		RefusalChain *router.RefusalChainStats `json:"refusal_chain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if feed.RefusalChain == nil {
		t.Fatalf("refusal_chain absent, want the live stats (body %s)", rec.Body.String())
	}
	rc := feed.RefusalChain
	if rc.Refusals != 3 || rc.ServedViaRefusal != 2 || rc.Exhausted != 1 {
		t.Fatalf("refusal_chain = %+v, want refusals=3 served=2 exhausted=1", rc)
	}
	if rc.LastProvider != "prov-a" || rc.LastModel != "model-m" {
		t.Fatalf("refusal_chain last = %q/%q, want prov-a/model-m", rc.LastProvider, rc.LastModel)
	}
	if !rc.LastAt.Equal(when) {
		t.Fatalf("refusal_chain last_at = %v, want %v", rc.LastAt, when)
	}
}

func TestGuardrailsFeedRefusalChainAbsentWithoutStats(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "refusal_chain") {
		t.Fatalf("feed carries refusal_chain without an engine that exposes it (body %s)", rec.Body.String())
	}
}
