package routetrace

import (
	"errors"
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
