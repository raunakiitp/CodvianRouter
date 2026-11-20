// BreakerDegradedForProvider (RT-2 follow-up): the exported
// read of breakerStateOf's DEGRADED state — the signal
// cmd/codvian-router fills into scoring.Signals.BreakerDegraded
// so the LKGP hook can refuse to hoist a pin to a
// slowing/failing provider. Driven through real requests (fake
// clients, no network), mirroring TestResetBreakerDegradedState:
// OAuth degrades at 5 consecutive failures and opens at 8.
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestBreakerDegradedForProvider(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	eng := profileEngine(map[string]AuthProfile{"a": AuthProfileOAuth},
		map[string]provider.Client{"a": errClient(fail500), "b": okClient("bee")})
	chain := chainFor("a", "b")

	// Unknown provider and a fresh provider: neither degraded
	// nor open — false means "not degraded OR unknown", never
	// proof of health.
	if eng.BreakerDegradedForProvider("no-such-provider") {
		t.Fatalf("BreakerDegradedForProvider(unknown) = true, want false")
	}
	if eng.BreakerDegradedForProvider("a") {
		t.Fatalf("BreakerDegradedForProvider(fresh) = true, want false (no failures yet)")
	}
	if eng.BreakerOpenForProvider("a") {
		t.Fatalf("BreakerOpenForProvider(fresh) = true, want false")
	}

	// 5 consecutive 500s: at the OAuth degrade threshold (5),
	// below the open threshold (8) — DEGRADED, not OPEN.
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerDegraded || st.Open {
		t.Fatalf("after 5 failures: %+v, want DEGRADED, not open", st)
	}
	if !eng.BreakerDegradedForProvider("a") {
		t.Fatalf("BreakerDegradedForProvider = false after 5 failures, want true (DEGRADED)")
	}
	if eng.BreakerOpenForProvider("a") {
		t.Fatalf("BreakerOpenForProvider = true after 5 failures, want false (not yet OPEN)")
	}
	// The other provider never failed: still healthy.
	if eng.BreakerDegradedForProvider("b") || eng.BreakerOpenForProvider("b") {
		t.Fatalf("provider b reads degraded/open, want neither (it never failed)")
	}

	// 3 more 500s (8 total): at the open threshold — OPEN,
	// and DEGRADED is now false. OPEN and DEGRADED are
	// mutually exclusive states; the hook checks OPEN first,
	// so the exclusion is carried by the OPEN check alone.
	for i := 0; i < 3; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
			t.Fatalf("request %d: %v", i+6, err)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerOpen || !st.Open {
		t.Fatalf("after 8 failures: %+v, want OPEN", st)
	}
	if eng.BreakerDegradedForProvider("a") {
		t.Fatalf("BreakerDegradedForProvider = true after 8 failures, want false (OPEN, not DEGRADED)")
	}
	if !eng.BreakerOpenForProvider("a") {
		t.Fatalf("BreakerOpenForProvider = false after 8 failures, want true (OPEN)")
	}
}
