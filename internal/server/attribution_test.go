// Outcome attribution under concurrency (CHECKLIST 317-318),
// proven at the server level: the outcome recorded for a request
// (the learning loop's (task, model) cell and the feedback
// decision id on the response header) must come from THAT
// request's own decision/trace, never from the engine-global
// last-decision/last-trace read — even when the globals already
// hold a neighbour's decision/trace at record time.
//
// The engine double below deterministically reproduces the
// interleaving: it publishes this request's own decision+trace
// into the request's attribution sink (exactly what the real
// engine does) while its LastTrace/LastAutoDecision report a
// NEIGHBOUR's decision+trace (a request that finished between
// this request's engine return and the server's outcome
// recording). On the old code — server reads the globals — the
// outcome lands in the neighbour's (quick-qa, model-b) cell and
// the header names the neighbour's decision id, so both
// assertions fail. On the fixed code both pass.
package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// attributionTestEngine stands in for the real engine with a
// neighbour already in the globals: Chat/ChatStream publish this
// request's own decision+trace into the ctx sink, while
// LastTrace/LastAutoDecision report the neighbour's.
type attributionTestEngine struct {
	ownDec       autorouter.Decision
	ownTrace     *routetrace.RouteTrace
	neighbourDec autorouter.Decision
	neighbourTr  *routetrace.RouteTrace
}

func (e *attributionTestEngine) publish(ctx context.Context) {
	if att := router.AttributionFrom(ctx); att != nil {
		dec := e.ownDec
		att.PublishDecision(&dec)
		att.PublishTrace(e.ownTrace)
	}
}

func (e *attributionTestEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.publish(ctx)
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (e *attributionTestEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	e.publish(ctx)
	if onDelta != nil {
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (e *attributionTestEngine) LastTrace() *routetrace.RouteTrace { return e.neighbourTr }

func (e *attributionTestEngine) LastAutoDecision() (autorouter.Decision, bool) {
	return e.neighbourDec, true
}

func newAttributionTestEngine() *attributionTestEngine {
	return &attributionTestEngine{
		ownDec: autorouter.Decision{
			ID: "dec-own", VirtualModel: "auto",
			Tier: autorouter.TierCoding, Confidence: 0.9, Model: "model-a",
		},
		ownTrace: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "prov-a", Model: "model-a", Outcome: "success"},
		}},
		neighbourDec: autorouter.Decision{
			ID: "dec-neighbour", VirtualModel: "auto",
			Tier: autorouter.TierQuickQA, Confidence: 0.9, Model: "model-b",
		},
		neighbourTr: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "prov-b", Model: "model-b", Outcome: "success"},
		}},
	}
}

func assertOwnAttribution(t *testing.T, ar *autorouter.Router) {
	t.Helper()
	stats := ar.OutcomeStats()
	var own, neighbour *autorouter.TaskModelStats
	for i := range stats {
		switch {
		case stats[i].Task == "coding" && stats[i].Model == "model-a":
			own = &stats[i]
		case stats[i].Task == "quick-qa" && stats[i].Model == "model-b":
			neighbour = &stats[i]
		}
	}
	if own == nil || own.Requests != 1 || own.Successes != 1 {
		t.Fatalf("OutcomeStats = %+v, want exactly one success in cell (coding, model-a)", stats)
	}
	if neighbour != nil {
		t.Fatalf("OutcomeStats = %+v, want no cell for the neighbour (quick-qa, model-b) — the outcome was attributed to the neighbour", stats)
	}
}

// assertOwnAttributionCell asserts attribution only (the outcome
// landed in this request's own cell, not the neighbour's) without
// asserting success: the streaming path records every outcome as
// a failure (see the note on
// TestOutcomeAttributionStreamPrefersRequestScopeOverGlobal), so
// a stream test cannot assert Successes — only the cell.
func assertOwnAttributionCell(t *testing.T, ar *autorouter.Router) {
	t.Helper()
	stats := ar.OutcomeStats()
	var own, neighbour *autorouter.TaskModelStats
	for i := range stats {
		switch {
		case stats[i].Task == "coding" && stats[i].Model == "model-a":
			own = &stats[i]
		case stats[i].Task == "quick-qa" && stats[i].Model == "model-b":
			neighbour = &stats[i]
		}
	}
	if own == nil || own.Requests != 1 {
		t.Fatalf("OutcomeStats = %+v, want exactly one request in cell (coding, model-a)", stats)
	}
	if neighbour != nil {
		t.Fatalf("OutcomeStats = %+v, want no cell for the neighbour (quick-qa, model-b) — the outcome was attributed to the neighbour", stats)
	}
}

// TestOutcomeAttributionPrefersRequestScopeOverGlobal: one
// non-streaming auto-routed request whose globals already hold a
// neighbour's decision/trace records its outcome in its OWN
// (coding, model-a) cell, and the response header names its OWN
// decision id.
func TestOutcomeAttributionPrefersRequestScopeOverGlobal(t *testing.T) {
	eng := newAttributionTestEngine()
	ar := autorouter.New(autorouter.Config{DefaultModel: "model-a"})
	srv := newTestServer(t, eng, WithAutoRouter(ar))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"write a function"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Auto-Decision-Id"); got != "dec-own" {
		t.Fatalf("X-Codvian-Auto-Decision-Id = %q, want dec-own (the neighbour's dec-neighbour must never leak onto this response)", got)
	}
	assertOwnAttribution(t, ar)
}

// TestOutcomeAttributionStreamPrefersRequestScopeOverGlobal is
// the same proof on the streaming path (handleChatStream records
// the outcome after the stream ends, from the same sink).
//
// NOTE (pre-existing, out of scope for 317-318): the stream path
// records the outcome as a FAILURE even on success — handleChat
// calls recordMetrics with resp=nil on the stream branch, and
// recordIntentOutcome computes ok = (err == nil && resp != nil),
// so a successful stream lands as a failure with reason
// "upstream_error". Attribution (the cell) is still this
// request's own, which is what this test proves; the success
// classification needs its own slice (thread the stream response
// through handleChatStream's return). This test therefore asserts
// the cell only, not Successes.
func TestOutcomeAttributionStreamPrefersRequestScopeOverGlobal(t *testing.T) {
	eng := newAttributionTestEngine()
	ar := autorouter.New(autorouter.Config{DefaultModel: "model-a"})
	srv := newTestServer(t, eng, WithAutoRouter(ar))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"write a function"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	assertOwnAttributionCell(t, ar)
}
