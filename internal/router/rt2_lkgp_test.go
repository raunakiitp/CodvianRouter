// RT-2 tests: the engine calls Config.LKGPInvalidator with the
// chain key + failed provider after a hard failure (5xx, timeout,
// connection error) — in both cooldown modes — and never for
// 4xx, 429s, quota errors, model-not-found, or cancellations.
// The pin store itself lives behind the hook (the router never
// imports internal/strategy); the hook's provider-gated clear is
// pinned in internal/strategy/rt2_lkgp_test.go.
package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

type rt2Invalidator struct {
	mu    sync.Mutex
	calls [][2]string // (chainKey, provider), in call order
}

func (r *rt2Invalidator) hook(chainKey, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [2]string{chainKey, provider})
}

func (r *rt2Invalidator) called() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]string(nil), r.calls...)
}

func TestRT2NoteFailureInvalidatesPinOnHardFailuresOnly(t *testing.T) {
	err500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	err502 := &provider.UpstreamError{StatusCode: 502, Retryable: true, Body: "bad gateway"}
	err529 := &provider.UpstreamError{StatusCode: 529, Retryable: true, Body: "overloaded"}
	errTimeout := &perTargetTimeoutError{Timeout: time.Second}
	errTransport := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	err400 := &provider.UpstreamError{StatusCode: 400, Retryable: false, Body: "bad request"}
	err401 := &provider.UpstreamError{StatusCode: 401, Retryable: false, Body: "invalid api key"}
	err404Model := &provider.UpstreamError{StatusCode: 404, Retryable: false, Body: "model not found"}
	err402 := &provider.QuotaError{StatusCode: 402, Message: "quota exhausted"}
	err429 := &provider.RateLimitError{RetryAfter: time.Second, RetryAfterPresent: true, Body: "slow down"}

	cases := []struct {
		name string
		mode string
		err  error
		want bool // invalidator called
	}{
		{"500 hard mode", CooldownHard, err500, true},
		{"502 hard mode", CooldownHard, err502, true},
		{"529 hard mode", CooldownHard, err529, true},
		{"per-target timeout hard mode", CooldownHard, errTimeout, true},
		{"connection error hard mode", CooldownHard, errTransport, true},
		{"400 hard mode", CooldownHard, err400, false},
		{"401 hard mode", CooldownHard, err401, false},
		{"404 model-not-found hard mode", CooldownHard, err404Model, false},
		{"402 quota hard mode", CooldownHard, err402, false},
		{"429 hard mode", CooldownHard, err429, false},
		{"cancelled hard mode", CooldownHard, context.Canceled, false},
		{"deadline hard mode", CooldownHard, context.DeadlineExceeded, false},
		{"500 soft-demote mode", CooldownSoftDemote, err500, true},
		{"529 soft-demote mode", CooldownSoftDemote, err529, true},
		{"per-target timeout soft-demote mode", CooldownSoftDemote, errTimeout, true},
		{"connection error soft-demote mode", CooldownSoftDemote, errTransport, true},
		{"429 soft-demote mode", CooldownSoftDemote, err429, false},
		{"402 quota soft-demote mode", CooldownSoftDemote, err402, false},
		{"404 model-not-found soft-demote mode", CooldownSoftDemote, err404Model, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &rt2Invalidator{}
			cfg := testConfig()
			cfg.LKGPInvalidator = inv.hook
			eng := NewEngine(map[string]provider.Client{"prov-a": okClient("ok")}, nil, cfg)
			res := attemptResult{
				att:    Attempt{ProviderID: "prov-a", Model: "m"},
				err:    tc.err,
				called: true,
			}
			c429 := 0
			eng.noteFailure(&routetrace.RouteTrace{}, res, &c429, tc.mode, "key", "chain-m")
			got := inv.called()
			if tc.want {
				if len(got) != 1 || got[0] != [2]string{"chain-m", "prov-a"} {
					t.Fatalf("invalidator calls = %v, want exactly [{chain-m prov-a}]", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("invalidator calls = %v, want none", got)
			}
		})
	}
}

func TestRT2NoteFailureNoChainKeyNoClear(t *testing.T) {
	// An empty chain key (e.g. embeddings, which carry no LKGP
	// chain) disables the clear even on a 500.
	inv := &rt2Invalidator{}
	cfg := testConfig()
	cfg.LKGPInvalidator = inv.hook
	eng := NewEngine(map[string]provider.Client{"prov-a": okClient("ok")}, nil, cfg)
	res := attemptResult{
		att:    Attempt{ProviderID: "prov-a", Model: "m"},
		err:    &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"},
		called: true,
	}
	eng.noteFailure(&routetrace.RouteTrace{}, res, new(int), CooldownHard, "key", "")
	if got := inv.called(); len(got) != 0 {
		t.Fatalf("invalidator calls = %v, want none for empty chain key", got)
	}
}

func TestRT2NoteFailureNilInvalidatorIsSafe(t *testing.T) {
	// No hook wired (the zero Config): a 500 must not panic
	// and must still fail over normally.
	eng := NewEngine(map[string]provider.Client{"prov-a": okClient("ok")}, nil, testConfig())
	res := attemptResult{
		att:    Attempt{ProviderID: "prov-a", Model: "m"},
		err:    &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"},
		called: true,
	}
	eng.noteFailure(&routetrace.RouteTrace{}, res, new(int), CooldownHard, "key", "chain-m")
}

func TestRT2NoteStreamStartFailureInvalidatesPin(t *testing.T) {
	// The stream-start path classifies the same way: a 500
	// clears the pin, a 400 does not.
	inv := &rt2Invalidator{}
	cfg := testConfig()
	cfg.LKGPInvalidator = inv.hook
	eng := NewEngine(map[string]provider.Client{"prov-a": okClient("ok")}, nil, cfg)
	start := time.Now()
	eng.noteStreamStartFailure(&routetrace.RouteTrace{},
		attemptResult{att: Attempt{ProviderID: "prov-a", Model: "m"},
			err: &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}, called: true},
		start, CooldownHard, "key", "chain-m")
	if got := inv.called(); len(got) != 1 || got[0] != [2]string{"chain-m", "prov-a"} {
		t.Fatalf("stream 500 invalidator calls = %v, want exactly [{chain-m prov-a}]", got)
	}
	inv2 := &rt2Invalidator{}
	cfg.LKGPInvalidator = inv2.hook
	eng2 := NewEngine(map[string]provider.Client{"prov-a": okClient("ok")}, nil, cfg)
	eng2.noteStreamStartFailure(&routetrace.RouteTrace{},
		attemptResult{att: Attempt{ProviderID: "prov-a", Model: "m"},
			err: &provider.UpstreamError{StatusCode: 400, Retryable: false, Body: "bad request"}, called: true},
		start, CooldownHard, "key", "chain-m")
	if got := inv2.called(); len(got) != 0 {
		t.Fatalf("stream 400 invalidator calls = %v, want none", got)
	}
}
