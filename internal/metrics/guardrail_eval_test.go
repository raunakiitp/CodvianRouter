package metrics

import (
	"strings"
	"testing"
)

// TestGuardrailEvaluationsAndUpstreamFailures proves the phase/action
// evaluation counter and the upstream-failure counter (CHECKLIST
// 147): evaluations are counted once per Check call under their
// phase and action, upstream failures under their own series, and
// the two never share a series.
func TestGuardrailEvaluationsAndUpstreamFailures(t *testing.T) {
	r := NewRecorder()
	r.IncGuardrailEval("request", "allowed")
	r.IncGuardrailEval("request", "allowed")
	r.IncGuardrailEval("request", "blocked")
	r.IncGuardrailEval("response", "blocked")
	r.IncGuardrailEval("tool_args", "redacted")
	r.IncUpstreamFailure()
	r.IncUpstreamFailure()
	if got := r.UpstreamFailures(); got != 2 {
		t.Fatalf("UpstreamFailures() = %d, want 2", got)
	}
	out := body(t, r)
	for _, want := range []string{
		`codvian_router_guardrail_evaluations_total{phase="request",action="allowed"} 2`,
		`codvian_router_guardrail_evaluations_total{phase="request",action="blocked"} 1`,
		`codvian_router_guardrail_evaluations_total{phase="response",action="blocked"} 1`,
		`codvian_router_guardrail_evaluations_total{phase="tool_args",action="redacted"} 1`,
		`codvian_router_upstream_failures_total 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition lacks series %q:\n%s", want, out)
		}
	}
	// The zero value is usable, like the rest of the Recorder: a
	// fresh Recorder exposes zero counters, never a missing series
	// line for the totals.
	zero := body(t, &Recorder{})
	if !strings.Contains(zero, "codvian_router_upstream_failures_total 0\n") {
		t.Fatalf("zero Recorder lacks upstream failures 0:\n%s", zero)
	}
}
