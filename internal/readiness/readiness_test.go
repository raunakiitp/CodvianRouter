package readiness

import (
	"context"
	"testing"
	"time"
)

func TestGateUnknownIsNotReady(t *testing.T) {
	var g Gate
	st := g.Status(context.Background(), "local:nope")
	if st.Known || st.Ready {
		t.Errorf("unknown capability = %+v, want Known=false Ready=false", st)
	}
	if got := g.Statuses(context.Background()); got == nil || len(got) != 0 {
		t.Errorf("empty gate statuses = %v, want empty non-nil", got)
	}
}

func TestGateProbeAndOverride(t *testing.T) {
	var g Gate
	probes := 0
	g.Register("local:test", 45*time.Second, func(context.Context) bool {
		probes++
		return probes >= 2 // not ready first, ready after
	})
	st := g.Status(context.Background(), "local:test")
	if st.Ready || st.RetryAfterSeconds != 45 || !st.Known {
		t.Errorf("first status = %+v, want not-ready/45/known", st)
	}
	// Second call within the 1 s cache still reports the first probe.
	st = g.Status(context.Background(), "local:test")
	if st.Ready {
		t.Error("cached probe should still report not-ready")
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1 (1 s cache)", probes)
	}
	// An override wins immediately, without waiting for the cache.
	g.SetReady("local:test", true)
	st = g.Status(context.Background(), "local:test")
	if !st.Ready {
		t.Errorf("override ready: status = %+v", st)
	}
	g.SetReady("local:test", false)
	st = g.Status(context.Background(), "local:test")
	if st.Ready {
		t.Errorf("override not-ready: status = %+v", st)
	}
	// SetReady on an unknown name invents nothing.
	g.SetReady("local:ghost", true)
	if st := g.Status(context.Background(), "local:ghost"); st.Known || st.Ready {
		t.Errorf("ghost capability = %+v, want unknown", st)
	}
}

func TestGateNoProbeIsNotReady(t *testing.T) {
	var g Gate
	g.Register("local:assets", 0, nil)
	st := g.Status(context.Background(), "local:assets")
	if st.Ready {
		t.Error("capability with no probe and no override must report not-ready")
	}
	if st.RetryAfterSeconds != 30 {
		t.Errorf("RetryAfterSeconds = %d, want default 30", st.RetryAfterSeconds)
	}
	g.SetReady("local:assets", true)
	if st := g.Status(context.Background(), "local:assets"); !st.Ready {
		t.Errorf("after SetReady(true): %+v", st)
	}
}
