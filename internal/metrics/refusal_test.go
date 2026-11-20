// Content-policy refusal chain counters (CHECKLIST 161): the
// refusal counters are separate from the upstream-failure
// counter — a refusal says nothing about provider health, so it
// must never inflate upstream failures.
package metrics

import (
	"strings"
	"testing"
)

func TestRefusalCounters(t *testing.T) {
	r := NewRecorder()
	if got := r.ContentRefusals(); got != 0 {
		t.Fatalf("ContentRefusals() = %d, want 0 before any refusal", got)
	}
	r.IncContentRefusal()
	r.IncContentRefusal()
	r.IncRefusalChainServed()
	r.IncRefusalChainExhausted()
	if got := r.ContentRefusals(); got != 2 {
		t.Fatalf("ContentRefusals() = %d, want 2", got)
	}
	if got := r.RefusalChainServed(); got != 1 {
		t.Fatalf("RefusalChainServed() = %d, want 1", got)
	}
	if got := r.RefusalChainExhausted(); got != 1 {
		t.Fatalf("RefusalChainExhausted() = %d, want 1", got)
	}
	if got := r.UpstreamFailures(); got != 0 {
		t.Fatalf("UpstreamFailures() = %d, want 0 (refusals are not upstream failures)", got)
	}
	out := body(t, r)
	for _, want := range []string{
		"codvian_router_content_refusals_total 2\n",
		"codvian_router_refusal_chain_served_total 1\n",
		"codvian_router_refusal_chain_exhausted_total 1\n",
		"codvian_router_upstream_failures_total 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}
