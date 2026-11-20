// Chaos harness (M4.1): fault injection + outage/hedge measurement.
//
// The harness answers the M4 exit gate with measured numbers, not
// assertions: a single-provider outage with >= 3 healthy fallbacks
// must show a 0% client-visible error rate; the added failover
// latency must be <= the detection time + 2 ms; hedging must cut
// p99 by >= 50% on a slow-provider scenario.
//
// Fault injection is a client wrapper, not engine behaviour: an
// Injector wraps any provider.Client and applies deterministic
// faults (kill, slow, fail-N-times) to its Chat/ChatStream calls.
// Tests and the gate register the wrapped client with the Engine
// exactly like a real one, so the harness exercises the real
// request path — ordering, breakers, cooldowns, hedging, preread —
// with no test-only branch inside the engine. Faults are always
// deterministic: a chaos run that depends on a coin flip cannot
// prove a 0% error rate. Chaos-mode (the auto/chaos panel) uses
// the same injector in tests; the engine itself never injects
// faults into production traffic.
package chaos

import (
	"context"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Fault is one deterministic fault applied to a wrapped client's
// calls. Exactly one of Kill / Slow / FailTimes applies per call
// evaluation, in this precedence: Kill, then FailTimes, then Slow.
// A Fault with no field set is a no-op (the call passes through).
type Fault struct {
	// Kill makes every call fail with Err (or a generic error
	// when Err is nil): the provider is down.
	Kill bool
	// Err is the error Kill / FailTimes returns. Nil means a
	// generic 500 UpstreamError.
	Err error
	// FailTimes fails the first N calls, then passes through:
	// a provider that dies and recovers. 0 means "never".
	FailTimes int
	// Slow delays every call by D before it runs: a provider
	// that answers, but late. 0 means "no delay".
	Slow time.Duration
}

// Injector wraps a provider.Client and applies a Fault to its calls.
// It is safe for concurrent use; Calls/Failures are the live
// counters (read via Stats). SetFault replaces the fault atomically
// mid-run, so a scenario can kill a provider halfway through and
// revive it later — the chaos script, not the engine, owns the
// timeline.
type Injector struct {
	mu       sync.Mutex
	client   provider.Client
	fault    Fault
	calls    int
	failures int
	failed   int // calls failed by the fault so far (drives FailTimes)
}

// NewInjector wraps client with an initial fault (Fault{} = pass-through).
func NewInjector(client provider.Client, fault Fault) *Injector {
	return &Injector{client: client, fault: fault}
}

// SetFault replaces the fault atomically.
func (inj *Injector) SetFault(f Fault) {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	inj.fault = f
	inj.failed = 0
}

// Stats returns (calls, failures) served by this injector so far.
func (inj *Injector) Stats() (calls, failures int) {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	return inj.calls, inj.failures
}

// faultErr is the error a Kill / FailTimes fault returns.
func (inj *Injector) faultErr() error {
	if inj.fault.Err != nil {
		return inj.fault.Err
	}
	return &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "chaos: injected failure"}
}

// apply decides, under the lock, whether this call fails and how
// long it must wait first. It returns (fail, delay, err).
func (inj *Injector) apply(ctx context.Context) (bool, time.Duration, error) {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	inj.calls++
	f := inj.fault
	if f.Kill {
		inj.failures++
		return true, 0, inj.faultErr()
	}
	if f.FailTimes > 0 && inj.failed < f.FailTimes {
		inj.failed++
		inj.failures++
		return true, 0, inj.faultErr()
	}
	return false, f.Slow, nil
}

// sleep waits d or until ctx ends, reporting whether ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

// Chat applies the fault, then delegates to the wrapped client.
func (inj *Injector) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	fail, delay, err := inj.apply(ctx)
	if fail {
		return nil, err
	}
	if sleep(ctx, delay) {
		return nil, ctx.Err()
	}
	return inj.client.Chat(ctx, req)
}

// ChatStream applies the fault, then delegates to the wrapped client.
func (inj *Injector) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	fail, delay, err := inj.apply(ctx)
	if fail {
		return nil, err
	}
	if sleep(ctx, delay) {
		return nil, ctx.Err()
	}
	return inj.client.ChatStream(ctx, req, onDelta)
}

// ListModels and TestKey delegate untouched: fault injection is a
// request-path tool, never a catalog tool.
func (inj *Injector) ListModels(ctx context.Context) ([]string, error) {
	return inj.client.ListModels(ctx)
}

func (inj *Injector) TestKey(ctx context.Context) error {
	return inj.client.TestKey(ctx)
}

// WireFormat delegates when the wrapped client reports one.
func (inj *Injector) WireFormat() string {
	type wireFormer interface{ WireFormat() string }
	if wf, ok := inj.client.(wireFormer); ok {
		return wf.WireFormat()
	}
	return ""
}

var _ provider.Client = (*Injector)(nil)
