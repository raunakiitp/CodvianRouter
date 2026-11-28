// Chaos-mode fault injection (B2-13).
//
// The chaos-mode weight pack is wired as a selectable strategy
// (internal/scoring ModePack "chaos-mode", internal/strategy
// wiring) and its parallel fan-out is live via the combo path
// (internal/router/combo.go). What was missing was the ability to
// deliberately fail a provider on purpose, so the chaos-mode path
// can exercise failover against a dead target. That is what this
// file provides — and nothing more. It is the B2-13 piece, not the
// full failover-testing harness that comes later.
//
// Behaviour studied, not copied: the sector-typical chaos surface
// (src/lib/chaos in the reference clone) is a parallel-panel
// fan-out with per-provider overrides and timeouts — it has no
// fault-injection capability of its own. This mechanism is our own
// design: an armed fault intercepts one attempt before any
// provider is dialled and returns a real provider error type, so
// the whole existing failure machinery (taxonomy, noteFailure,
// breaker, lockout, quota learning, ledger, trace) treats it
// exactly like the genuine failure it stands in for — except the
// error message names the injection, so a trace or ledger row is
// never mistaken for a real upstream failure.
//
// OFF by default, always: with no fault armed the registry is
// empty and the check in callChat is a single read-lock probe
// that changes nothing. A fault becomes active only through an
// explicit ArmFault call (test or operator control). There is no
// config surface, no HTTP endpoint and no request field that can
// arm one — a user request can never switch it on by accident.
// Disarming is explicit (DisarmFault / ClearFaults) or automatic
// when a fault's MaxHits is reached.
//
// Scope: the hook lives in callChat, the choke point for chat
// completions — the sequential chain and the chaos/fusion panel
// fan-out both dial through it. Streaming and embeddings dial
// their own clients and are NOT covered by this piece; extending
// the hook there belongs to the later harness, not B2-13.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// FaultKind is what an armed fault does to a matching attempt.
type FaultKind int

const (
	// FaultFail fails the attempt with an injected upstream
	// error (StatusCode) instead of dialling the provider.
	FaultFail FaultKind = iota + 1
	// FaultDelay stalls the attempt for Delay and then lets the
	// real provider call proceed — a slow provider, not a dead
	// one. It exercises latency / per-target-timeout paths.
	FaultDelay
)

// FaultSpec describes one armed fault.
type FaultSpec struct {
	// ProviderID is the provider the fault applies to
	// (required). Model narrows it to one model of that
	// provider; "" matches every model of the provider.
	ProviderID string
	Model      string
	// Kind is FaultFail or FaultDelay (required).
	Kind FaultKind
	// StatusCode is the upstream HTTP status the injected error
	// carries (FaultFail only; 0 defaults to 503). Retryable
	// statuses (429, 500, 502, 503, 504, 529) exercise failover
	// to the next target; a 401/403 stops the chain by the
	// existing taxonomy, exactly as a real one would.
	StatusCode int
	// Message is an optional operator note carried on the
	// injected error; "" gets a default.
	Message string
	// Delay is how long the attempt stalls before the failure
	// (FaultFail) or before the real call proceeds (FaultDelay).
	// 0 means no stall. It is bounded by the request context:
	// a cancelled request ends the stall with the context
	// error, never with a fake failure.
	Delay time.Duration
	// MaxHits caps how many matching attempts the fault fires
	// on; 0 means every matching attempt until disarmed. The
	// fault auto-disarms when the cap is reached.
	MaxHits int
}

// FaultReport is one armed fault plus how many times it has fired.
type FaultReport struct {
	Spec FaultSpec
	Hits int
}

// faultEntry is an armed spec plus its hit counter. Guarded by
// Engine.faultMu.
type faultEntry struct {
	spec FaultSpec
	hits int
}

// ArmFault arms a fault. An existing fault for the same
// (provider, model) is replaced. It returns an error for an
// invalid spec and arms nothing.
func (e *Engine) ArmFault(spec FaultSpec) error {
	if spec.ProviderID == "" {
		return errors.New("router: fault injection needs a provider")
	}
	if spec.Kind != FaultFail && spec.Kind != FaultDelay {
		return errors.New("router: fault injection kind must be FaultFail or FaultDelay")
	}
	if spec.StatusCode != 0 && (spec.StatusCode < 100 || spec.StatusCode > 599) {
		return fmt.Errorf("router: fault injection status %d is not an HTTP status", spec.StatusCode)
	}
	if spec.Delay < 0 {
		return errors.New("router: fault injection delay must not be negative")
	}
	if spec.MaxHits < 0 {
		return errors.New("router: fault injection MaxHits must not be negative")
	}
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	for _, en := range e.faults {
		if en.spec.ProviderID == spec.ProviderID && en.spec.Model == spec.Model {
			en.spec = spec
			en.hits = 0
			return nil
		}
	}
	e.faults = append(e.faults, &faultEntry{spec: spec})
	return nil
}

// DisarmFault removes the fault for (providerID, model); it
// reports whether one was armed.
func (e *Engine) DisarmFault(providerID, model string) bool {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	for i, en := range e.faults {
		if en.spec.ProviderID == providerID && en.spec.Model == model {
			e.faults = append(e.faults[:i], e.faults[i+1:]...)
			return true
		}
	}
	return false
}

// ClearFaults disarms every fault.
func (e *Engine) ClearFaults() {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	e.faults = nil
}

// ListFaults snapshots the armed faults and how many times each
// has fired.
func (e *Engine) ListFaults() []FaultReport {
	e.faultMu.RLock()
	defer e.faultMu.RUnlock()
	out := make([]FaultReport, 0, len(e.faults))
	for _, en := range e.faults {
		out = append(out, FaultReport{Spec: en.spec, Hits: en.hits})
	}
	return out
}

// faultInjectedError marks an error the fault injector produced
// instead of a real upstream call. It unwraps to the real provider
// error type (RateLimitError / QuotaError / UpstreamError) so the
// taxonomy, noteFailure, breaker, lockout and quota-learning
// machinery classify it exactly like the genuine failure it stands
// in for — while the message names the injection.
type faultInjectedError struct {
	providerID string
	model      string
	inner      error
}

func (e *faultInjectedError) Error() string {
	if e == nil || e.inner == nil {
		return "chaos fault injection"
	}
	if e.model != "" {
		return fmt.Sprintf("chaos fault injection (%s/%s): %s", e.providerID, e.model, e.inner.Error())
	}
	return fmt.Sprintf("chaos fault injection (%s): %s", e.providerID, e.inner.Error())
}

func (e *faultInjectedError) Unwrap() error { return e.inner }

// injectedError builds the provider error a FaultFail stands in
// for: a 429 becomes a RateLimitError (so the 429 path —
// consecutive-429 counting, retry-after, quota learning — fires
// exactly as for a real 429), a 402 a QuotaError, anything else an
// UpstreamError with the injected status.
func (s FaultSpec) injectedError() error {
	msg := s.Message
	if msg == "" {
		msg = "injected by chaos-mode fault injection"
	}
	var inner error
	switch s.StatusCode {
	case 429:
		inner = &provider.RateLimitError{Body: msg}
	case 402:
		inner = &provider.QuotaError{Message: msg, StatusCode: 402, Body: msg}
	default:
		code := s.StatusCode
		if code == 0 {
			code = 503
		}
		inner = &provider.UpstreamError{StatusCode: code, Retryable: true, Body: msg}
	}
	return &faultInjectedError{providerID: s.ProviderID, model: s.Model, inner: inner}
}

// applyFault checks the armed faults for one attempt. It returns
// (res, true) when the attempt is replaced by an injected failure
// — the caller returns res as the attempt result — and
// (attemptResult{}, false) when the attempt proceeds normally
// (including after a FaultDelay stall). With no fault armed this
// is a single read-lock probe.
//
// A FaultFail result carries called=true so the failure flows
// through the normal path (noteFailure, breaker, lockout, ledger,
// trace) exactly like a real failed attempt; the error names the
// injection, so the footprint is honest about what happened.
func (e *Engine) applyFault(ctx context.Context, att Attempt, label string) (attemptResult, bool) {
	e.faultMu.RLock()
	armed := false
	for _, en := range e.faults {
		if en.spec.ProviderID == att.ProviderID && (en.spec.Model == "" || en.spec.Model == att.Model) {
			armed = true
			break
		}
	}
	e.faultMu.RUnlock()
	if !armed {
		return attemptResult{}, false
	}
	// Consume one hit under the write lock, re-finding the entry:
	// it may have been disarmed or replaced since the read-lock
	// scan, and a disarmed fault must never fire again.
	e.faultMu.Lock()
	var spec *FaultSpec
	for _, en := range e.faults {
		if en.spec.ProviderID == att.ProviderID && (en.spec.Model == "" || en.spec.Model == att.Model) {
			if en.spec.MaxHits > 0 && en.hits >= en.spec.MaxHits {
				e.disarmLocked(en.spec.ProviderID, en.spec.Model)
				break
			}
			en.hits++
			if en.spec.MaxHits > 0 && en.hits >= en.spec.MaxHits {
				e.disarmLocked(en.spec.ProviderID, en.spec.Model)
			}
			s := en.spec
			spec = &s
			break
		}
	}
	e.faultMu.Unlock()
	if spec == nil {
		return attemptResult{}, false
	}

	if spec.Delay > 0 {
		timer := time.NewTimer(spec.Delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return attemptResult{att: att, err: ctx.Err(), reasoning: label}, true
		case <-timer.C:
		}
	}
	if spec.Kind == FaultDelay {
		return attemptResult{}, false
	}
	return attemptResult{
		att:       att,
		err:       spec.injectedError(),
		latencyMs: spec.Delay.Milliseconds(),
		called:    true,
		reasoning: label,
	}, true
}

// disarmLocked removes one fault; the caller holds e.faultMu.
func (e *Engine) disarmLocked(providerID, model string) {
	for i, en := range e.faults {
		if en.spec.ProviderID == providerID && en.spec.Model == model {
			e.faults = append(e.faults[:i], e.faults[i+1:]...)
			return
		}
	}
}
