package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// intentTestEngine wraps a fakeEngine and reports a fixed last
// auto-routing decision + route trace, like router.Engine does
// after a real request — so the outcome hook can attribute the
// served request to its (task, model).
type intentTestEngine struct {
	Engine
	dec   autorouter.Decision
	trace *routetrace.RouteTrace
}

func (e *intentTestEngine) LastAutoDecision() (autorouter.Decision, bool) {
	if e.dec.ID == "" {
		return autorouter.Decision{}, false
	}
	return e.dec, true
}

func (e *intentTestEngine) LastTrace() *routetrace.RouteTrace {
	return e.trace
}

func testIntentRouter() *autorouter.Router {
	return autorouter.New(autorouter.Config{
		IntentTiers: map[autorouter.IntentTier]autorouter.IntentMapping{
			autorouter.IntentFast: {Model: "model-a", Effort: autorouter.EffortLow},
		},
		TierModels: map[autorouter.Tier][]string{
			autorouter.TierCoding: {"model-a", "model-b"},
		},
		DefaultModel: "model-a",
	})
}

func decodeIntentTiers(t *testing.T, body []byte) intentTiersResponse {
	t.Helper()
	var resp intentTiersResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode intent tiers %s: %v", body, err)
	}
	return resp
}

func decodeSuggestions(t *testing.T, body []byte) suggestionsResponse {
	t.Helper()
	var resp suggestionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode suggestions %s: %v", body, err)
	}
	return resp
}

func TestIntentTiersFeed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(testIntentRouter()))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/intent-tiers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/intent-tiers: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeIntentTiers(t, rec.Body.Bytes())
	if !resp.Configured || resp.Persisted {
		t.Fatalf("feed configured=%v persisted=%v, want true/false", resp.Configured, resp.Persisted)
	}
	if len(resp.Tiers) != 3 {
		t.Fatalf("tiers = %d, want 3", len(resp.Tiers))
	}
	fast := resp.Tiers[0]
	if fast.Tier != autorouter.IntentFast || !fast.Mapped || fast.Model != "model-a" || fast.Effort != autorouter.EffortLow || fast.VirtualModel != "intent/fast" {
		t.Fatalf("fast tier = %+v", fast)
	}
	if resp.Tiers[1].Mapped || resp.Tiers[2].Mapped {
		t.Fatalf("balanced/best must be unmapped, got %+v", resp.Tiers[1:])
	}
	if !strings.Contains(resp.Note, "runtime only") {
		t.Fatalf("feed note %q must say changes are runtime-only", resp.Note)
	}
}

func TestIntentTiersNoRouter(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, tc := range []struct{ method, path string }{
		{"GET", "/dashboard/intent-tiers"},
		{"POST", "/dashboard/intent-tiers"},
		{"GET", "/dashboard/suggestions"},
		{"POST", "/dashboard/suggestions"},
	} {
		if rec := doReq(t, srv, tc.method, tc.path, `{}`, "test-key-123"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", tc.method, tc.path, rec.Code)
		}
	}
}

func TestIntentTiersUpdate(t *testing.T) {
	router := testIntentRouter()
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router))
	// Map balanced at runtime.
	rec := doReq(t, srv, http.MethodPost, "/dashboard/intent-tiers",
		`{"tier":"balanced","model":"model-b","effort":"medium"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeIntentTiers(t, rec.Body.Bytes())
	if !resp.Tiers[1].Mapped || resp.Tiers[1].Model != "model-b" || resp.Tiers[1].Effort != autorouter.EffortMedium {
		t.Fatalf("balanced after update = %+v", resp.Tiers[1])
	}
	if m, ok := router.ResolveIntent(autorouter.IntentBalanced); !ok || m.Model != "model-b" {
		t.Fatalf("router mapping after update = %+v, %v", m, ok)
	}
	// And the new mapping resolves through the preview endpoint.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=intent/balanced", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview intent/balanced: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Validation errors name the problem, never store a bad mapping.
	for _, tc := range []struct{ body, want string }{
		{`{"tier":"turbo","model":"model-b"}`, "unknown intent tier"},
		{`{"tier":"fast","model":""}`, "needs a model"},
		{`{"tier":"fast","model":"model-b","effort":"huge"}`, "unknown effort"},
		{`{not json`, "invalid intent-tier update"},
	} {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/intent-tiers", tc.body, "test-key-123")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("POST %s: status = %d, body = %s, want 400 containing %q", tc.body, rec.Code, rec.Body.String(), tc.want)
		}
	}
	if m, _ := router.ResolveIntent(autorouter.IntentFast); m.Model != "model-a" {
		t.Fatalf("a rejected update must not change fast: %+v", m)
	}
}

func TestIntentPreviewUnmapped(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(testIntentRouter()))
	// No prompt needed for an intent: the tier IS the instruction.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=intent/fast", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview intent/fast: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if dec.Model != "model-a" || dec.IntentTier != autorouter.IntentFast || dec.Effort != autorouter.EffortLow {
		t.Fatalf("preview decision = %+v", dec)
	}
	// Unmapped tier: 502 with the decision attached, naming the tier.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=intent/best", "", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("preview intent/best: status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode unmapped preview: %v", err)
	}
	if dec.Model != "" || !strings.Contains(dec.Reason, `"best"`) {
		t.Fatalf("unmapped preview decision = %+v, want empty model + reason naming \"best\"", dec)
	}
	// An unknown tier name is an unknown virtual model: 400.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=intent/turbo&prompt=hi", "", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown virtual model") {
		t.Fatalf("preview intent/turbo: status = %d, body = %s, want 400 unknown virtual model", rec.Code, rec.Body.String())
	}
}

func TestIntentChatUnmappedTierRefused(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(testIntentRouter()))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"intent/best","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unmapped intent chat: status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	// The tier name is JSON-escaped in the raw body (\"best\"), so
	// decode the envelope and assert on the message itself.
	var errBody struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(errBody.Error.Message, `"best"`) || !strings.Contains(errBody.Error.Message, "not mapped") {
		t.Fatalf("body %q must name the tier and say it is not mapped", rec.Body.String())
	}
}

func TestIntentChatServedCarriesIntentHeaders(t *testing.T) {
	router := testIntentRouter()
	dec := autorouter.Decision{ID: "dec-test-1", VirtualModel: "intent/fast", IntentTier: autorouter.IntentFast, Effort: autorouter.EffortLow, Model: "model-a"}
	eng := &intentTestEngine{
		Engine: &fakeEngine{chatResp: provider.ChatResponse{Model: "model-a", Content: "hi"}},
		dec:    dec,
		trace: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "prov-a", Model: "model-a", LatencyMs: 42, Outcome: "success"},
		}},
	}
	srv := newTestServer(t, eng, WithAutoRouter(router))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"intent/fast","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("mapped intent chat: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Intent-Tier"); got != "fast" {
		t.Fatalf("X-Codvian-Intent-Tier = %q, want fast", got)
	}
	if got := rec.Header().Get("X-Codvian-Intent-Effort"); got != "low" {
		t.Fatalf("X-Codvian-Intent-Effort = %q, want low", got)
	}
	// The served request fed the learning loop: one outcome for
	// (intent/fast, model-a), success, with the resolved effort.
	stats := router.OutcomeStats()
	if len(stats) != 1 {
		t.Fatalf("outcome stats = %+v, want 1 row", stats)
	}
	row := stats[0]
	if row.Task != "intent/fast" || row.Model != "model-a" || row.Requests != 1 || row.Successes != 1 || row.Effort != autorouter.EffortLow {
		t.Fatalf("outcome row = %+v", row)
	}
}

func TestSuggestionsApproveRevertRoundTrip(t *testing.T) {
	router := testIntentRouter()
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(router))
	// Seed real recorded outcomes: model-a fails 40% on
	// intent/fast, model-b is perfect on the same task.
	for i := 0; i < 6; i++ {
		router.RecordOutcome(autorouter.Outcome{Task: "intent/fast", Model: "model-a", Effort: autorouter.EffortLow, LatencyMs: 200, OK: true})
	}
	for i := 0; i < 4; i++ {
		router.RecordOutcome(autorouter.Outcome{Task: "intent/fast", Model: "model-a", Effort: autorouter.EffortLow, LatencyMs: 200, OK: false, Reason: "upstream_error"})
	}
	for i := 0; i < 12; i++ {
		router.RecordOutcome(autorouter.Outcome{Task: "intent/fast", Model: "model-b", LatencyMs: 100, OK: true})
	}
	rec := doReq(t, srv, http.MethodGet, "/dashboard/suggestions", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/suggestions: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	feed := decodeSuggestions(t, rec.Body.Bytes())
	if len(feed.Suggestions) != 1 || feed.Suggestions[0].Kind != autorouter.SuggestionIntentRemap {
		t.Fatalf("suggestions = %+v, want 1 intent_remap", feed.Suggestions)
	}
	if len(feed.Stats) != 2 {
		t.Fatalf("stats rows = %d, want 2", len(feed.Stats))
	}
	if feed.MinSamples != autorouter.SuggestionMinSamples {
		t.Fatalf("min_samples = %d", feed.MinSamples)
	}
	id := feed.Suggestions[0].ID
	// Approve: the tier remaps and the change is logged.
	rec = doReq(t, srv, http.MethodPost, "/dashboard/suggestions",
		`{"action":"approve","suggestion_id":"`+id+`"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	feed = decodeSuggestions(t, rec.Body.Bytes())
	if len(feed.Suggestions) != 0 {
		t.Fatalf("after approve, pending = %+v, want none", feed.Suggestions)
	}
	if len(feed.Log) != 1 || feed.Log[0].SuggestionID != id || feed.Log[0].Reverted {
		t.Fatalf("log after approve = %+v", feed.Log)
	}
	if m, _ := router.ResolveIntent(autorouter.IntentFast); m.Model != "model-b" {
		t.Fatalf("after approve, fast maps to %+v", m)
	}
	changeID := feed.Log[0].ID
	// Approving the same suggestion again is a 409, not a silent success.
	rec = doReq(t, srv, http.MethodPost, "/dashboard/suggestions",
		`{"action":"approve","suggestion_id":"`+id+`"}`, "test-key-123")
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-approve: status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
	// Revert: the previous mapping is restored and logged.
	rec = doReq(t, srv, http.MethodPost, "/dashboard/suggestions",
		`{"action":"revert","change_id":"`+changeID+`"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("revert: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	feed = decodeSuggestions(t, rec.Body.Bytes())
	if !feed.Log[0].Reverted {
		t.Fatalf("log after revert = %+v", feed.Log)
	}
	if m, _ := router.ResolveIntent(autorouter.IntentFast); m.Model != "model-a" {
		t.Fatalf("after revert, fast maps to %+v", m)
	}
	if len(feed.Suggestions) != 1 {
		t.Fatalf("after revert, pending = %+v, want the suggestion back", feed.Suggestions)
	}
	// Unknown ids and bad actions error honestly.
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"action":"approve","suggestion_id":"intent_remap:intent/fast:nope:nope"}`, http.StatusConflict},
		{`{"action":"revert","change_id":"chg-999"}`, http.StatusNotFound},
		{`{"action":"revert","change_id":"` + changeID + `"}`, http.StatusConflict},
		{`{"action":"explode"}`, http.StatusBadRequest},
		{`{"action":"approve"}`, http.StatusBadRequest},
		{`{not json`, http.StatusBadRequest},
	} {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/suggestions", tc.body, "test-key-123")
		if rec.Code != tc.code {
			t.Errorf("POST %s: status = %d, want %d, body = %s", tc.body, rec.Code, tc.code, rec.Body.String())
		}
	}
}

func TestSuggestionsEmptyState(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(testIntentRouter()))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/suggestions", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	feed := decodeSuggestions(t, rec.Body.Bytes())
	if len(feed.Suggestions) != 0 || len(feed.Stats) != 0 || len(feed.Log) != 0 {
		t.Fatalf("empty feed = %+v, want empty suggestions/stats/log", feed)
	}
	if !strings.Contains(feed.Rules, "10 recorded requests") {
		t.Fatalf("rules %q must state the evidence floor", feed.Rules)
	}
}

func TestOutcomeHookIgnoresPlainModels(t *testing.T) {
	router := testIntentRouter()
	eng := &intentTestEngine{
		Engine: &fakeEngine{chatResp: provider.ChatResponse{Model: "model-a", Content: "hi"}},
		trace: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "prov-a", Model: "model-a", LatencyMs: 42, Outcome: "success"},
		}},
	}
	srv := newTestServer(t, eng, WithAutoRouter(router))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("plain model chat: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// No routing decision, no task: nothing is recorded.
	if got := router.OutcomeStats(); len(got) != 0 {
		t.Fatalf("plain-model request recorded outcomes: %+v", got)
	}
}
