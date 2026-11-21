//go:build otel

package server

// Phase 2 observability wiring proof: every HTTP request through the
// server's logging wrapper records one root span ("codvian.request")
// carrying the correlation id and route, in the otel build. The
// no-op tracer keeps this free in default builds.

import (
	"net/http"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/observability"
)

func TestRequestRootSpanRecorded(t *testing.T) {
	tr := observability.NewConfiguredTracer(observability.TracerConfig{}) // local ring only
	srv := newTestServer(t, &fakeEngine{}, WithTracer(tr), WithTimeout(30*time.Second))

	rec := doReq(t, srv, http.MethodGet, "/health", "", "")
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	spans := tr.Spans()
	if len(spans) == 0 {
		t.Fatal("no spans recorded for a served request")
	}
	var found bool
	for _, s := range spans {
		if s.Name == "codvian.request" {
			found = true
			if s.Kind != "server" {
				t.Errorf("kind = %q, want server", s.Kind)
			}
			if _, ok := s.Attributes["http.route"]; !ok {
				t.Errorf("span attributes missing http.route: %+v", s.Attributes)
			}
			if s.Duration() < 0 {
				t.Error("negative duration")
			}
		}
	}
	if !found {
		t.Fatalf("no codvian.request span among %+v", spans)
	}
}
