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
//
// Every probe result is also recorded in a bounded in-memory ring
// (History: last 48 h, at most 2,880 samples per provider), from which
// History computes real uptime percentages and latency percentiles —
// windows with no recorded probes report "no data", never an invented
// value. The ring is persisted to disk (health-history.json in the
// router's data dir) when the checker was built with
// NewWithDataDir, so history survives a restart; a checker built
// with New keeps history in memory only.
package health

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
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

// HistoryRetention bounds how long probe results are kept per provider:
// samples older than this are dropped when a new probe lands. At the
// watcher's 60 s interval this is 2,880 probes per provider, and
// MaxHistorySamples caps it at exactly that many, so a shorter probe
// interval (or a manual Check storm) can never grow memory without
// bound. The ring is persisted to disk by a checker built with
// NewWithDataDir (see history.go); a checker built with New keeps
// it in memory only, so it resets when the router restarts. Windows
// with no recorded probes report "no data" (never an invented 100%).
const HistoryRetention = 48 * time.Hour

// MaxHistorySamples caps the probe samples kept per provider. 48 h at
// the watcher's 60 s interval is 2,880 probes.
const MaxHistorySamples = 2880

// ProbeSample is one recorded health-probe result: when it ran,
// whether the provider was up, and how long the probe took.
// The JSON tags are the on-disk shape in health-history.json.
type ProbeSample struct {
	// At is when the probe ran (UTC).
	At time.Time `json:"at"`
	// OK is true when the probe succeeded.
	OK bool `json:"ok"`
	// LatencyMs is how long the probe took, in milliseconds.
	LatencyMs int64 `json:"latency_ms"`
}

// HistoryBucket is one hour of recorded probes inside the 24 h
// window, for the dashboard's uptime bars and latency line. A bucket
// with no probes has Up == 0 && Down == 0 and P50Ms == nil: the
// dashboard renders it as "no data", never as up or down.
type HistoryBucket struct {
	// Start is the bucket's hour boundary (UTC, oldest first).
	Start time.Time
	// Up is the number of successful probes in the hour.
	Up int
	// Down is the number of failed probes in the hour.
	Down int
	// P50Ms is the median latency of the successful probes in the
	// hour, or nil when the hour had no successful probe.
	P50Ms *int64
}

// HistorySummary is the recorded probe history for one provider,
// computed from real probes only. A window with no recorded probes
// leaves its uptime nil ("no data"); latency percentiles are computed
// over successful probes in the last 24 h only, and are nil when
// there was no successful probe.
type HistorySummary struct {
	// ProviderID is the registry ID of the provider.
	ProviderID string
	// Samples is how many probe results are currently retained.
	Samples int
	// Uptime1h is the share of successful probes in the last hour,
	// in percent [0, 100], or nil when no probe ran in that hour.
	Uptime1h *float64
	// Uptime24h is the share of successful probes in the last
	// 24 hours, in percent [0, 100], or nil when no probe ran in
	// that window.
	Uptime24h *float64
	// P50Ms is the median probe latency over successful probes in
	// the last 24 h, in milliseconds, or nil when there was none.
	P50Ms *int64
	// P95Ms is the 95th-percentile probe latency over successful
	// probes in the last 24 h, in milliseconds, or nil when there
	// was none.
	P95Ms *int64
	// P99Ms is the 99th-percentile probe latency over successful
	// probes in the last 24 h, in milliseconds, or nil when there
	// was none (G10 Area 12 row 11).
	P99Ms *int64
	// Buckets is the last 24 h split into 24 hourly buckets,
	// oldest first, each with its recorded up/down counts.
	Buckets []HistoryBucket
}

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
	// history is the bounded per-provider ring of recorded probe
	// results (see HistoryRetention / MaxHistorySamples). It is
	// appended to by record and pruned there, so it never grows
	// without bound.
	history map[string][]ProbeSample
	// historyPath is the file the ring is persisted to ("" when
	// persistence is off: a checker built with New). Set once by
	// NewWithDataDir, never changed afterwards.
	historyPath string
	// persistMu serializes record + persist, so concurrent probes
	// cannot interleave a snapshot and a write and overwrite a
	// newer ring with an older one. It is held across the file
	// write (milliseconds at probe cadence); record's mu is only
	// ever held for the in-memory update, never for I/O.
	persistMu sync.Mutex
	// journalPath is the append-only probe log next to the
	// snapshot ("" when persistence is off). Set once by
	// NewWithDataDir, never changed afterwards.
	journalPath string
	// nextSeq is the sequence number of the next journal line;
	// compactSeq is the newest seq included in the snapshot on
	// disk; journalLines counts the journal lines written since
	// the last compaction. All three are guarded by persistMu.
	nextSeq      int64
	compactSeq   int64
	journalLines int
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
		history: make(map[string][]ProbeSample),
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
// as the checker's last result for that provider, and appends the
// outcome to the provider's bounded probe history. Old samples (older
// than HistoryRetention) are dropped and the ring is capped at
// MaxHistorySamples, so history never grows without bound. When the
// checker persists history (NewWithDataDir), the outcome is
// appended to the journal before record returns (the snapshot is
// rewritten only at a compaction); a write failure is logged and
// never fails the probe.
func (c *Checker) record(st Status, err error) {
	// Serialize with the persist so a concurrent probe cannot
	// snapshot and write an older ring over a newer one.
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.Lock()
	if c.last == nil {
		c.last = make(map[string]Status)
	}
	if c.lastErr == nil {
		c.lastErr = make(map[string]error)
	}
	c.last[st.ProviderID] = st
	c.lastErr[st.ProviderID] = err
	if c.history == nil {
		c.history = make(map[string][]ProbeSample)
	}
	samples := c.history[st.ProviderID]
	cutoff := st.CheckedAt.Add(-HistoryRetention)
	kept := samples[:0]
	for _, s := range samples {
		if !s.At.Before(cutoff) {
			kept = append(kept, s)
		}
	}
	kept = append(kept, ProbeSample{At: st.CheckedAt, OK: st.OK, LatencyMs: st.LatencyMs})
	if len(kept) > MaxHistorySamples {
		kept = kept[len(kept)-MaxHistorySamples:]
	}
	c.history[st.ProviderID] = kept
	c.mu.Unlock()
	c.persistHistory(st)
}

// percentileMs returns the nearest-rank percentile of latencies (in
// milliseconds), or nil when latencies is empty. latencies is not
// modified.
func percentileMs(latencies []int64, pct float64) *int64 {
	if len(latencies) == 0 {
		return nil
	}
	sorted := make([]int64, len(latencies))
	copy(sorted, latencies)
	slices.Sort(sorted)
	rank := int(math.Ceil(pct / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	v := sorted[rank-1]
	return &v
}

// uptimePct returns the share of successful samples in percent
// [0, 100], or nil when there are no samples in the window.
func uptimePct(samples []ProbeSample) *float64 {
	if len(samples) == 0 {
		return nil
	}
	up := 0
	for _, s := range samples {
		if s.OK {
			up++
		}
	}
	v := float64(up) / float64(len(samples)) * 100
	return &v
}

// History returns the recorded probe history for providerID, computed
// from real probes only: uptime over the last 1 h and 24 h, latency
// p50/p95/p99 over successful probes in the last 24 h, and 24 hourly
// buckets over the last 24 h. Windows with no recorded probes report
// nil ("no data"), never an invented value. A provider with no probes
// yet returns an empty summary (Samples 0, all windows nil, 24 empty
// buckets). With a checker built by NewWithDataDir the ring is
// loaded from disk at startup and survives restarts; with New it is
// in-memory only and resets on restart.
func (c *Checker) History(providerID string) HistorySummary {
	return c.historyAt(providerID, time.Now().UTC())
}

func (c *Checker) historyAt(providerID string, now time.Time) HistorySummary {
	sum := HistorySummary{ProviderID: providerID}
	if c == nil {
		return sum
	}
	c.mu.Lock()
	samples := append([]ProbeSample(nil), c.history[providerID]...)
	c.mu.Unlock()
	sum.Samples = len(samples)

	hourAgo := now.Add(-time.Hour)
	dayAgo := now.Add(-24 * time.Hour)
	var lastHour, lastDay []ProbeSample
	var dayLatencies []int64
	for _, s := range samples {
		if !s.At.Before(hourAgo) {
			lastHour = append(lastHour, s)
		}
		if !s.At.Before(dayAgo) {
			lastDay = append(lastDay, s)
			if s.OK {
				dayLatencies = append(dayLatencies, s.LatencyMs)
			}
		}
	}
	sum.Uptime1h = uptimePct(lastHour)
	sum.Uptime24h = uptimePct(lastDay)
	sum.P50Ms = percentileMs(dayLatencies, 50)
	sum.P95Ms = percentileMs(dayLatencies, 95)
	sum.P99Ms = percentileMs(dayLatencies, 99)

	// 24 hourly buckets over the last 24 h, oldest first, aligned to
	// hour boundaries and ending in the current hour, so the most
	// recent probes always land in the last bucket. Buckets with no
	// recorded probes stay empty (Up 0, Down 0, P50Ms nil) so the
	// dashboard can show "no data".
	buckets := make([]HistoryBucket, 24)
	dayStart := now.Truncate(time.Hour).Add(-23 * time.Hour)
	for i := range buckets {
		buckets[i].Start = dayStart.Add(time.Duration(i) * time.Hour)
	}
	bucketLat := make([][]int64, 24)
	for _, s := range lastDay {
		idx := int(s.At.Sub(dayStart) / time.Hour)
		if idx < 0 || idx >= 24 {
			continue
		}
		if s.OK {
			buckets[idx].Up++
			bucketLat[idx] = append(bucketLat[idx], s.LatencyMs)
		} else {
			buckets[idx].Down++
		}
	}
	for i := range buckets {
		buckets[i].P50Ms = percentileMs(bucketLat[i], 50)
	}
	sum.Buckets = buckets
	return sum
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
		// legacy Bearer default (nil headers) and the provider's
		// stored wire format. Production callers pass a nil factory
		// and go through reg.ClientFor, which resolves the catalog
		// auth per (provider, endpoint).
		client, err = c.factory(p.BaseURL, key, nil, p.WireFormat)
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
