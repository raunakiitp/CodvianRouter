// Package health checks whether each registered provider's API key is
// still valid and whether its quota is exhausted, so the router can skip
// dead or quota-exhausted providers.
//
// It never invents a quota value: a successful key test only reports the
// quota hint already stored in the registry (provider.QuotaUnknown, i.e.
// -1, when nothing is known). Only a quota-type failure (429 / quota
// error) ever lowers a hint, via ApplyHints, and only to 0.
//
// API keys are never logged or returned: Check builds its client from the
// registry's decrypted key and only the upstream error string (which
// never contains the key) is stored in Status.LastError.
//
// Limitations: the only probe is provider.Client.TestKey (a tiny
// /v1/models call), so this detects dead keys and quota/rate-limit
// responses, not model-level availability, latency under real chat load,
// or remaining quota as a number — the upstream does not report that.
// QuotaResetsAt is only known when a RateLimitError carries a Retry-After
// value; otherwise it is zero (unknown).
package health

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// DefaultCheckTimeout bounds a single provider key test. A provider that
// does not answer within this time is reported as failed (its error is
// the context deadline error), so one slow upstream cannot stall a check.
const DefaultCheckTimeout = 5 * time.Second

// maxInFlight caps how many providers CheckAll probes concurrently.
const maxInFlight = 5

// Status is the result of one provider health check.
type Status struct {
	// ProviderID is the registry ID of the provider that was checked.
	ProviderID string
	// ProviderName is the provider's display name ("", when unknown).
	ProviderName string
	// OK is true when the key test succeeded.
	OK bool
	// LatencyMs is how long the key test took, in milliseconds.
	LatencyMs int64
	// QuotaHint is the remaining-quota fraction in [0, 1] as stored in
	// the registry, or -1 (provider.QuotaUnknown) when unknown. It is
	// never invented from a successful key test.
	QuotaHint float64
	// QuotaResetsAt is when quota is expected to reset, or the zero
	// time when unknown. It is only populated from a RateLimitError's
	// Retry-After value.
	QuotaResetsAt time.Time
	// LastError is the error string of the last failed check ("" on
	// success). It never contains the provider's API key.
	LastError string
	// CheckedAt is when the check ran.
	CheckedAt time.Time
}

// Checker probes registered providers' keys and tracks the last result
// per provider. It is safe for concurrent use.
type Checker struct {
	reg     *provider.Registry
	factory provider.ClientFactory
	timeout time.Duration

	mu      sync.Mutex
	last    map[string]Status
	lastErr map[string]error
}

// New returns a Checker for reg. factory builds the upstream client for
// a base URL + decrypted API key; a nil factory means the default
// (provider.NewClient, via the registry's ClientFor). The factory is
// primarily a test seam: production callers pass nil and the checker
// builds clients through reg.ClientFor, which decrypts the stored key.
func New(reg *provider.Registry, factory provider.ClientFactory) *Checker {
	return &Checker{
		reg:     reg,
		factory: factory,
		timeout: DefaultCheckTimeout,
		last:    make(map[string]Status),
		lastErr: make(map[string]error),
	}
}

// isQuotaError reports whether err is a quota-type failure: a
// provider.QuotaError (exhausted quota / billing) or a
// provider.RateLimitError (HTTP 429). Wrapped errors count. Everything
// else is a generic failure and must not touch quota hints.
func isQuotaError(err error) bool {
	if err == nil {
		return false
	}
	var quotaErr *provider.QuotaError
	if errors.As(err, &quotaErr) {
		return true
	}
	var rateErr *provider.RateLimitError
	if errors.As(err, &rateErr) {
		return true
	}
	return false
}

// quotaResetAt derives a reset time from a rate-limit error's Retry-After
// value, or the zero time when there is none.
func quotaResetAt(err error, checkedAt time.Time) time.Time {
	var rateErr *provider.RateLimitError
	if errors.As(err, &rateErr) && rateErr.RetryAfter > 0 {
		return checkedAt.Add(rateErr.RetryAfter)
	}
	return time.Time{}
}

// record stores a check result (and its underlying error, for ApplyHints)
// as the checker's last result for that provider.
func (c *Checker) record(st Status, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = make(map[string]Status)
	}
	if c.lastErr == nil {
		c.lastErr = make(map[string]error)
	}
	c.last[st.ProviderID] = st
	c.lastErr[st.ProviderID] = err
}

// Check probes one provider's key with a short, context-aware timeout
// and returns its Status. It never logs or returns the provider's API
// key. On success OK is true and QuotaHint is the hint already stored
// in the registry (or -1 when unknown); on failure OK is false,
// LastError is the error string, and QuotaHint is the stored hint
// unchanged. An unknown provider ID returns OK=false with a clear
// "provider not found" error.
func (c *Checker) Check(ctx context.Context, providerID string) Status {
	if ctx == nil {
		ctx = context.Background()
	}
	checkedAt := time.Now().UTC()
	fail := func(providerName string, hint float64, err error, latencyMs int64) Status {
		st := Status{
			ProviderID:    providerID,
			ProviderName:  providerName,
			OK:            false,
			LatencyMs:     latencyMs,
			QuotaHint:     hint,
			QuotaResetsAt: quotaResetAt(err, checkedAt),
			LastError:     err.Error(),
			CheckedAt:     checkedAt,
		}
		c.record(st, err)
		return st
	}

	if c == nil || c.reg == nil {
		err := errors.New("health: no provider registry configured")
		return fail("", provider.QuotaUnknown, err, 0)
	}

	p, ok := c.reg.Get(providerID)
	if !ok {
		err := fmt.Errorf("provider not found: %s", providerID)
		return fail("", provider.QuotaUnknown, err, 0)
	}
	hint := p.QuotaHint

	timeout := c.timeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the client from the registry's decrypted key. With an
	// injected factory the key comes from reg.DecryptedKey and the
	// factory builds the client; with a nil factory the registry's own
	// ClientFor does both (its default is provider.NewClient).
	var client provider.Client
	var err error
	if c.factory != nil {
		var key string
		key, err = c.reg.DecryptedKey(providerID)
		if err != nil {
			return fail(p.Name, hint, fmt.Errorf("decrypting provider API key: %w", err), 0)
		}
		// The injected factory is a test seam: it builds with the
		// legacy Bearer default (nil headers). Production callers
		// pass a nil factory and go through reg.ClientFor, which
		// resolves the catalog auth per (provider, endpoint).
		client, err = c.factory(p.BaseURL, key, nil)
	} else {
		client, err = c.reg.ClientFor(providerID)
	}
	if err != nil {
		return fail(p.Name, hint, fmt.Errorf("building client for provider %s: %w", providerID, err), 0)
	}
	if client == nil {
		return fail(p.Name, hint, errors.New("client factory returned a nil client"), 0)
	}

	start := time.Now()
	testErr := client.TestKey(checkCtx)
	latencyMs := time.Since(start).Milliseconds()

	if testErr != nil {
		return fail(p.Name, hint, testErr, latencyMs)
	}

	st := Status{
		ProviderID:   providerID,
		ProviderName: p.Name,
		OK:           true,
		LatencyMs:    latencyMs,
		QuotaHint:    hint,
		CheckedAt:    checkedAt,
	}
	c.record(st, nil)
	return st
}

// CheckAll checks every enabled provider in registry order,
// concurrently (at most maxInFlight in flight), each with its own
// timeout, and returns the results in registry order. A slow provider
// cannot block the others: each check is bounded by DefaultCheckTimeout
// and runs in its own goroutine.
func (c *Checker) CheckAll(ctx context.Context) []Status {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.reg == nil {
		return nil
	}
	enabled := make([]*provider.Provider, 0)
	for _, p := range c.reg.List() {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}
	results := make([]Status, len(enabled))
	if len(enabled) == 0 {
		return results
	}

	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i, p := range enabled {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = c.Check(ctx, id)
		}(i, p.ID)
	}
	wg.Wait()
	return results
}

// Last returns a copy of the checker's last recorded results, in
// registry order (providers never checked are omitted). It is empty
// until at least one Check or CheckAll has run.
func (c *Checker) Last() []Status {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reg == nil {
		out := make([]Status, 0, len(c.last))
		for _, st := range c.last {
			out = append(out, st)
		}
		return out
	}
	out := make([]Status, 0, len(c.last))
	for _, p := range c.reg.List() {
		if st, ok := c.last[p.ID]; ok {
			out = append(out, st)
		}
	}
	return out
}

// ApplyHints lowers the registry quota hint to 0 for every provider
// whose last check failed with a quota-type error (429 / quota error),
// via reg.SetQuotaHint, so the routing engine skips it. A successful
// check never overwrites an operator-set hint, and a generic (non-quota)
// failure leaves the hint untouched. A nil reg falls back to the
// checker's own registry; with no registry at all it is a no-op.
// SetQuotaHint errors (e.g. provider removed since the check) are
// ignored: there is no return value to report them through.
func (c *Checker) ApplyHints(reg *provider.Registry) {
	if c == nil {
		return
	}
	if reg == nil {
		reg = c.reg
	}
	if reg == nil {
		return
	}
	c.mu.Lock()
	errs := make(map[string]error, len(c.lastErr))
	for id, err := range c.lastErr {
		errs[id] = err
	}
	c.mu.Unlock()
	for id, err := range errs {
		if isQuotaError(err) {
			_ = reg.SetQuotaHint(id, 0)
		}
	}
}

// StartWatcher runs CheckAll followed by ApplyHints immediately and
// then on every interval tick, until ctx ends. It blocks until ctx is
// done; callers should run it in a goroutine. An interval <= 0 is a
// no-op (returns immediately) and a nil ctx is treated as
// context.Background (which never ends, so prefer a cancellable ctx).
func (c *Checker) StartWatcher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		<-ctx.Done()
		return
	}
	select {
	case <-ctx.Done():
		return
	default:
	}
	c.CheckAll(ctx)
	c.ApplyHints(c.reg)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckAll(ctx)
			c.ApplyHints(c.reg)
		}
	}
}
