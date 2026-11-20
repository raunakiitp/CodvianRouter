package server

// Multi-region failover order (CHECKLIST 257-258): the
// guardrails feed carries the live region-aware failover counts,
// and the decision header names the region step of the serving
// attempt.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/router"
)

// regionFailoverStatsEngine is a fakeEngine that also reports
// live region-aware failover outcomes, for the feed tests.
type regionFailoverStatsEngine struct {
	*fakeEngine
	stats router.Failover257Stats
}

func (e *regionFailoverStatsEngine) Failover257Stats() router.Failover257Stats {
	return e.stats
}

// TestGuardrailsFeedCarriesRegionFailoverCounts proves GET
// /dashboard/guardrails serves the live CHECKLIST 257 counts —
// real counts from the engine, zero until a step happens —
// under failover_257, next to the 243 block.
func TestGuardrailsFeedCarriesRegionFailoverCounts(t *testing.T) {
	eng := &regionFailoverStatsEngine{
		fakeEngine: &fakeEngine{},
		stats: router.Failover257Stats{
			CrossRegion: 3, EquivalentModel: 1, SelfHosted: 2,
			LastStep: router.StepSelfHosted, LastProvider: "prov-local",
			LastModel: "llama-4", LastAt: time.Now(),
		},
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var f guardrailsFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode guardrails feed: %v", err)
	}
	if f.FailoverRegion == nil {
		t.Fatalf("failover_257 absent: the feed must carry the region-aware counts when the engine reports them")
	}
	got := *f.FailoverRegion
	if got.CrossRegion != 3 || got.EquivalentModel != 1 || got.SelfHosted != 2 {
		t.Fatalf("failover_257 = %+v, want 3 cross-region / 1 equivalent / 2 self-hosted", got)
	}
	if got.LastStep != router.StepSelfHosted || got.LastModel != "llama-4" {
		t.Fatalf("failover_257 last = %q / %q, want self-hosted / llama-4", got.LastStep, got.LastModel)
	}
}

// TestGuardrailsFeedOmitsRegionFailoverWithoutEngine proves the
// honest empty state: an engine that does not report region
// failover stats gets no failover_257 block — an absent block,
// never zeroed counts presented as measured.
func TestGuardrailsFeedOmitsRegionFailoverWithoutEngine(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var f guardrailsFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode guardrails feed: %v", err)
	}
	if f.FailoverRegion != nil {
		t.Fatalf("failover_257 = %+v, want nil when the engine reports no region stats", f.FailoverRegion)
	}
}
