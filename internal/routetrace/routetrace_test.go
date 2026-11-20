package routetrace

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRouteTraceAddPreservesOrder(t *testing.T) {
	tr := &RouteTrace{}
	tr.Add("prov-a", "model-a", 12, "error", errors.New("boom"))
	tr.Add("prov-b", "model-a", 50, "success", nil)
	if len(tr.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(tr.Attempts))
	}
	first, second := tr.Attempts[0], tr.Attempts[1]
	if first.Provider != "prov-a" || first.Outcome != "error" || first.Error != "boom" || first.LatencyMs != 12 {
		t.Fatalf("first attempt = %+v", first)
	}
	if second.Provider != "prov-b" || second.Outcome != "success" || second.Error != "" {
		t.Fatalf("second attempt = %+v", second)
	}
}

func TestRouteTraceAddNilErrorLeavesErrorEmpty(t *testing.T) {
	tr := &RouteTrace{}
	tr.Add("prov-a", "model-a", 0, "skipped_zero_quota", nil)
	if tr.Attempts[0].Error != "" {
		t.Fatalf("error = %q, want empty", tr.Attempts[0].Error)
	}
}

// TestRouteTraceAddRedactsKeyInError (slice 5, entries 150/152)
// proves a trace attempt never stores a provider key, even in
// memory: an upstream error quoting a key is redacted on Add. The
// fixture key is synthetic.
func TestRouteTraceAddRedactsKeyInError(t *testing.T) {
	tr := &RouteTrace{}
	tr.Add("prov-a", "model-a", 12, "error", errors.New("upstream error: HTTP 502: bad key sk-proj-TESTKEY0123456789abcdefghij"))
	if got := tr.Attempts[0].Error; strings.Contains(got, "sk-proj-") {
		t.Fatalf("trace stores the provider key: %q", got)
	}
}

// TestRouteTraceAddWithReason pins the fallback-reason field the
// dashboard's Live Routing view renders (CHECKLIST 121): a failed
// attempt carries its taxonomy slug, a success carries none.
func TestRouteTraceAddWithReason(t *testing.T) {
	tr := &RouteTrace{}
	tr.AddWithReason("dead", "gate-model", 12, "error", errors.New("upstream error: HTTP 404"), "model_not_found")
	tr.AddWithReason("fallback", "gate-model", 50, "success", nil, "")
	if tr.Attempts[0].Reason != "model_not_found" {
		t.Fatalf("attempt 0 reason = %q, want model_not_found", tr.Attempts[0].Reason)
	}
	if tr.Attempts[1].Reason != "" {
		t.Fatalf("attempt 1 reason = %q, want empty", tr.Attempts[1].Reason)
	}
	// Add (no reason) still leaves the reason empty.
	tr.Add("dead", "gate-model", 3, "skipped_breaker", nil)
	if tr.Attempts[2].Reason != "" {
		t.Fatalf("attempt 2 reason = %q, want empty", tr.Attempts[2].Reason)
	}
}

// TestRouteTraceAddWithPrereadRetry pins the Retry-After record
// (CHECKLIST 167-168): a rate-limited attempt carries the hint seen
// plus the abandon/floor decision, and an attempt with no hint
// carries no record at all — an absent hint is never recorded as 0s.
func TestRouteTraceAddWithPrereadRetry(t *testing.T) {
	tr := &RouteTrace{}
	tr.AddWithPrereadRetry("dead", "gate-model", 12, "rate_limited", errors.New("upstream rate limited, retry after 2m0s"), "rate_limited", "", 0, 0,
		&RetryAfterInfo{Seconds: 120, Decision: "abandoned", WaitSeconds: 0})
	tr.AddWithPreread("fallback", "gate-model", 50, "success", nil, "", "", 0, 0)
	if got := tr.Attempts[0].RetryAfter; got == nil || got.Seconds != 120 || got.Decision != "abandoned" || got.WaitSeconds != 0 {
		t.Fatalf("attempt 0 retry_after = %+v, want {120 abandoned 0}", got)
	}
	if tr.Attempts[1].RetryAfter != nil {
		t.Fatalf("attempt 1 retry_after = %+v, want nil (no hint on a success)", tr.Attempts[1].RetryAfter)
	}
	// The JSON the dashboard feed serves carries the record under
	// "retry_after" on the rate-limited attempt only.
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal trace: %v", err)
	}
	// RouteTrace marshals its Attempts field directly.
	var decodedTrace struct {
		Attempts []AttemptTrace `json:"Attempts"`
	}
	if err := json.Unmarshal(raw, &decodedTrace); err != nil {
		t.Fatalf("decode trace: %v (body %s)", err, raw)
	}
	if len(decodedTrace.Attempts) != 2 {
		t.Fatalf("decoded attempts = %d, want 2", len(decodedTrace.Attempts))
	}
	if decodedTrace.Attempts[0].RetryAfter == nil || decodedTrace.Attempts[0].RetryAfter.Decision != "abandoned" {
		t.Fatalf("decoded attempt 0 retry_after = %+v, want abandoned", decodedTrace.Attempts[0].RetryAfter)
	}
	if decodedTrace.Attempts[1].RetryAfter != nil {
		t.Fatalf("decoded attempt 1 retry_after = %+v, want nil", decodedTrace.Attempts[1].RetryAfter)
	}
}
