package router

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestDisableAdaptiveLimiter (E1, CHECKLIST 233) is the plumbing
// gate for the limiter-off measurement switch: with
// Config.DisableAdaptiveLimiter the engine carries a nil limiter
// (the adaptive package's fail-open stub — TryAcquire always
// admits, Record no-ops, Snapshot is empty), and by default it
// carries a real Gradient2 limiter. The behavioural proof is the
// E1 bench pair itself: the limiter-off run must show ~0% 502s
// (nothing shed) on the same tree.
func TestDisableAdaptiveLimiter(t *testing.T) {
	off := NewEngine(map[string]provider.Client{}, nil, Config{DisableAdaptiveLimiter: true})
	if off.limiter != nil {
		t.Fatal("DisableAdaptiveLimiter: engine limiter must be nil (fail-open), got a real limiter")
	}
	if got := off.ConcurrencyStats(); len(got) != 0 {
		t.Fatalf("DisableAdaptiveLimiter: ConcurrencyStats = %v, want empty (nil limiter snapshot)", got)
	}
	if !off.limiter.HasHeadroom("x") {
		t.Fatal("nil limiter must always have headroom (fail open)")
	}

	on := NewEngine(map[string]provider.Client{}, nil, Config{})
	if on.limiter == nil {
		t.Fatal("default Config: engine limiter must be a real Gradient2 limiter, got nil")
	}
	if got := on.limiter.Limit("never-used"); got != 20 {
		t.Fatalf("default limiter Limit(never-used) = %v, want initial cap 20", got)
	}
}
