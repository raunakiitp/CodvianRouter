// Warm HTTP/2 connection pools, sized by Little's Law.
//
// The trap (research-log entry 80): Go's http.DefaultTransport keeps
// at most MaxIdleConnsPerHost = 2 idle connections per host. A router
// serving N concurrent requests to one provider opens N connections
// for the first burst, then keeps only 2 of them — the next burst
// re-dials (TCP + TLS, 2+ RTTs on a real network) for every request
// beyond those 2. The fix is a warm pool per (provider, key): one
// http.Transport per provider+key, created at startup (not lazily on
// the first request), with MaxIdleConnsPerHost sized from the expected
// load — and the connections actually warmed by a real request at
// boot, so the first user request never pays a cold dial.
//
// Sizing — Little's Law: the mean number of requests in the system
// is L = λ × W, where λ is the mean arrival rate (requests/second)
// and W is the mean time a request holds a connection (seconds). A
// pool that holds fewer than L idle connections re-dials under
// steady load; a pool that holds L (rounded up, clamped to a sane
// floor/ceiling) does not. So:
//
//	N = ceil(λ × W)
//
// with λ = pools.expected_rps (operator-tunable, default 20 req/s)
// and W = pools.expected_latency_ms (operator-tunable, default
// 1000 ms — a typical LLM streaming/non-streaming round trip). The
// defaults give N = ceil(20 × 1.0) = 20 idle connections per
// provider+key. An explicit max_idle_per_host overrides the derived
// size (0 = derive). Min 2 (never below Go's own default), max 200
// (an idle connection still costs a socket + TLS state; beyond 200
// the operator is expected to raise the ceiling deliberately, not
// by accident).
//
// Honesty: the size is derived from EXPECTED load, not measured load
// — the router does not yet adapt the pool from live traffic (that
// would need a measured λ and W per provider; the counters here are
// the instrumentation such an adaption would read). The dashboard
// shows the formula, its inputs, and the derived size, plus the live
// counters (requests, dials, reused = requests − dials, idle/total
// connections), so the operator can see whether the sizing holds.
//
// Keys: pools are keyed by (provider ID, key fingerprint). The
// fingerprint is the first 12 hex characters of SHA-256(provider
// API key) — enough to distinguish keys, never the key itself, and
// never logged or served beyond the fingerprint. Two providers that
// happen to share a key still get separate pools (the provider ID
// is part of the key), because their hosts — and therefore their
// connection reuse — are different.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPoolExpectedRPS is the arrival rate the default pool size is
// derived from: 20 requests/second per provider+key. It is an
// operator expectation, not a measurement — see the package doc.
const DefaultPoolExpectedRPS = 20.0

// DefaultPoolExpectedLatencyMs is the mean connection-hold time the
// default pool size is derived from: 1000 ms per request.
const DefaultPoolExpectedLatencyMs = 1000.0

// DefaultPoolMinConns is the floor for the derived pool size: never
// below Go's own MaxIdleConnsPerHost default of 2.
const DefaultPoolMinConns = 2

// DefaultPoolMaxConns is the ceiling for the derived pool size: an
// idle connection costs a socket + TLS session state, and a pool
// larger than this is an operator decision, not an accident.
const DefaultPoolMaxConns = 200

// PoolConfig is the operator-tunable pool sizing, persisted in the
// config file's "pools" block (see internal/config). Every field is
// optional: 0 means "use the default". No field is a magic constant
// — each is an input to the Little's Law derivation below, and the
// derivation is recorded in code, in docs/pool-sizing.md, and on the
// dashboard.
type PoolConfig struct {
	// ExpectedRPS (λ) is the mean request rate the pool is sized
	// for, in requests per second. Default 20.
	ExpectedRPS float64 `json:"expected_rps,omitempty"`
	// ExpectedLatencyMs (W) is the mean time a request holds a
	// connection, in milliseconds. Default 1000.
	ExpectedLatencyMs float64 `json:"expected_latency_ms,omitempty"`
	// MaxIdlePerHost overrides the derived size when > 0. 0 means
	// "derive from λ × W". The override is still clamped to
	// [MinConns, MaxConns].
	MaxIdlePerHost int `json:"max_idle_per_host,omitempty"`
	// MinConns is the floor for the pool size. Default 2.
	MinConns int `json:"min_conns,omitempty"`
	// MaxConns is the ceiling for the pool size. Default 200.
	MaxConns int `json:"max_conns,omitempty"`
}

// Resolved returns the config with every 0 field replaced by its
// default, so callers never have to re-derive the defaults.
func (c PoolConfig) Resolved() PoolConfig {
	if c.ExpectedRPS <= 0 {
		c.ExpectedRPS = DefaultPoolExpectedRPS
	}
	if c.ExpectedLatencyMs <= 0 {
		c.ExpectedLatencyMs = DefaultPoolExpectedLatencyMs
	}
	if c.MinConns <= 0 {
		c.MinConns = DefaultPoolMinConns
	}
	if c.MaxConns <= 0 {
		c.MaxConns = DefaultPoolMaxConns
	}
	return c
}

// Validate rejects values the router cannot safely run with, naming
// the field. A negative rate/latency is nonsense; a ceiling below
// the floor is contradictory; an explicit size above 1000 is almost
// certainly a typo (each idle connection is a socket + TLS state).
func (c PoolConfig) Validate() error {
	if c.ExpectedRPS < 0 {
		return fmt.Errorf("config: pools.expected_rps must not be negative (got %v)", c.ExpectedRPS)
	}
	if c.ExpectedLatencyMs < 0 {
		return fmt.Errorf("config: pools.expected_latency_ms must not be negative (got %v)", c.ExpectedLatencyMs)
	}
	if c.MaxIdlePerHost < 0 || c.MaxIdlePerHost > 1000 {
		return fmt.Errorf("config: pools.max_idle_per_host must be in [0, 1000] (0 = derive, got %d)", c.MaxIdlePerHost)
	}
	if c.MinConns < 0 {
		return fmt.Errorf("config: pools.min_conns must not be negative (got %d)", c.MinConns)
	}
	if c.MaxConns < 0 {
		return fmt.Errorf("config: pools.max_conns must not be negative (got %d)", c.MaxConns)
	}
	if c.MinConns > 0 && c.MaxConns > 0 && c.MaxConns < c.MinConns {
		return fmt.Errorf("config: pools.max_conns (%d) must not be below pools.min_conns (%d)", c.MaxConns, c.MinConns)
	}
	return nil
}

// PoolSizeForRate is the Little's Law derivation as a pure function:
// N = ceil(λ × W), clamped to [min, max]. λ is requests/second, W
// the mean connection-hold time, min/max the floor/ceiling. A
// non-positive λ or W means "no load expected", which still gets the
// floor (a pool of 0 would re-dial every request).
func PoolSizeForRate(rps float64, latency time.Duration, min, max int) int {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	n := min
	if rps > 0 && latency > 0 {
		n = int(math.Ceil(rps * latency.Seconds()))
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

// Size derives this config's pool size: the Little's Law size from
// the resolved λ and W, or the explicit override when set — either
// way clamped to [MinConns, MaxConns].
func (c PoolConfig) Size() int {
	r := c.Resolved()
	if r.MaxIdlePerHost > 0 {
		n := r.MaxIdlePerHost
		if n < r.MinConns {
			n = r.MinConns
		}
		if n > r.MaxConns {
			n = r.MaxConns
		}
		return n
	}
	return PoolSizeForRate(r.ExpectedRPS, time.Duration(r.ExpectedLatencyMs)*time.Millisecond, r.MinConns, r.MaxConns)
}

// KeyFingerprint returns the pool-key fingerprint for an API key:
// the first 12 hex characters of SHA-256(key). It distinguishes
// keys without ever carrying key material — the dashboard serves
// this, never the key.
func KeyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])[:12]
}

// PoolStat is one row of live pool state, served by
// GET /dashboard/pools. Reused = Requests − Dials (floored at 0):
// the requests that did NOT need a new dial because a warm idle
// connection was waiting. Warmed is true while at least one idle
// connection is held — the pool is warm for the next request.
type PoolStat struct {
	ProviderID     string `json:"provider_id"`
	KeyFingerprint string `json:"key_fingerprint"`
	MaxIdle        int    `json:"max_idle"`
	Idle           int    `json:"idle"`
	Total          int    `json:"total"`
	Requests       uint64 `json:"requests"`
	Dials          uint64 `json:"dials"`
	Reused         uint64 `json:"reused"`
	Warmed         bool   `json:"warmed"`
}

// countingConn is a net.Conn that reports its Close to the pool, so
// the pool can count live connections (dials − closes) without
// reaching into http.Transport internals.
type countingConn struct {
	net.Conn
	onClose func()
	once    sync.Once
}

func (c *countingConn) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}

// pooledTransport is one warm HTTP/2 transport for one
// (provider, key) pair: a cloned DefaultTransport (so proxy, TLS,
// HTTP/2 and idle settings stay the Go defaults) with
// MaxIdleConnsPerHost sized by Little's Law, the TFO dialer, and
// live counters for dials, closes, requests and in-flight requests.
// It embeds *http.Transport, so it is an http.RoundTripper.
type pooledTransport struct {
	*http.Transport
	providerID string
	keyFP      string
	maxIdle    int
	createdAt  time.Time

	dials    atomic.Uint64
	closes   atomic.Uint64
	requests atomic.Uint64
	inFlight atomic.Int64
}

func (p *pooledTransport) stat() PoolStat {
	open := int64(p.dials.Load()) - int64(p.closes.Load())
	if open < 0 {
		open = 0
	}
	inFlight := p.inFlight.Load()
	if inFlight < 0 {
		inFlight = 0
	}
	idle := open - inFlight
	if idle < 0 {
		idle = 0
	}
	reqs := p.requests.Load()
	dials := p.dials.Load()
	var reused uint64
	if reqs > dials {
		reused = reqs - dials
	}
	return PoolStat{
		ProviderID:     p.providerID,
		KeyFingerprint: p.keyFP,
		MaxIdle:        p.maxIdle,
		Idle:           int(idle),
		Total:          int(open),
		Requests:       reqs,
		Dials:          dials,
		Reused:         reused,
		Warmed:         idle > 0,
	}
}

// RoundTrip counts the request and its in-flight time, then
// delegates to the embedded transport.
func (p *pooledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.requests.Add(1)
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	return p.Transport.RoundTrip(req)
}

// poolKey identifies one pool: a provider plus one key.
type poolKey struct {
	providerID string
	keyFP      string
}

// PoolManager owns the warm transports, one per (provider, key).
// Transports are created on first use for a pair — which, in serve
// mode, is at startup, when main builds one client per provider and
// warms each pool with a real request — and then shared by every
// client built for that pair afterwards (engine clients, health
// probes, live-added providers). A pool is dropped when its provider
// is removed, so a deleted provider's idle connections are closed
// instead of lingering.
type PoolManager struct {
	mu         sync.Mutex
	pools      map[poolKey]*pooledTransport
	cfg        PoolConfig
	tfoEnabled bool
}

// NewPoolManager returns a manager sizing pools from cfg (0 fields
// mean defaults) and dialing with TFO when tfoEnabled.
func NewPoolManager(cfg PoolConfig, tfoEnabled bool) *PoolManager {
	return &PoolManager{
		pools:      make(map[poolKey]*pooledTransport),
		cfg:        cfg.Resolved(),
		tfoEnabled: tfoEnabled,
	}
}

// SetConfig replaces the sizing config for pools created afterwards.
// Pools already created keep the size they were built with — a pool
// size is a property of its transport, and rebuilding a live
// transport mid-flight would drop its warm connections. (The config
// is only read at startup in serve mode, so this is a test hook in
// practice.)
func (m *PoolManager) SetConfig(cfg PoolConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg.Resolved()
}

// SetTFOEnabled replaces the TFO flag for pools created afterwards.
func (m *PoolManager) SetTFOEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tfoEnabled = enabled
}

// Config returns the resolved sizing config in force.
func (m *PoolManager) Config() PoolConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Size returns the pool size new pools are built with.
func (m *PoolManager) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Size()
}

// TransportFor returns the warm transport for (providerID, apiKey),
// creating it on first use. The apiKey is fingerprinted, never
// stored: the map key carries only the fingerprint.
func (m *PoolManager) TransportFor(providerID, apiKey string) *pooledTransport {
	key := poolKey{providerID: providerID, keyFP: KeyFingerprint(apiKey)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pools[key]; ok {
		return p
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	size := m.cfg.Size()
	tr.MaxIdleConnsPerHost = size
	tr.MaxIdleConns = size * 4
	if tr.MaxIdleConns < 100 {
		tr.MaxIdleConns = 100
	}
	tr.IdleConnTimeout = 90 * time.Second
	// IdleConnTimeout stays the Go default (90 s): a pool warmed at
	// boot stays warm across the health checker's 60 s probe
	// cadence, so probes re-warm it before it can go cold.
	dialer := tfoDialer(m.tfoEnabled)
	baseDial := dialer.DialContext
	// The counting wrapper must be installed on the pooled
	// transport itself (not the shared dialer) so each pool counts
	// only its own dials.
	p := &pooledTransport{
		Transport:  tr,
		providerID: providerID,
		keyFP:      key.keyFP,
		maxIdle:    size,
		createdAt:  time.Now(),
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := baseDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		p.dials.Add(1)
		return &countingConn{Conn: conn, onClose: func() { p.closes.Add(1) }}, nil
	}
	m.pools[key] = p
	return p
}

// Stats returns one live row per pool, sorted by provider ID then
// key fingerprint for a stable dashboard order.
func (m *PoolManager) Stats() []PoolStat {
	m.mu.Lock()
	stats := make([]PoolStat, 0, len(m.pools))
	for _, p := range m.pools {
		stats = append(stats, p.stat())
	}
	m.mu.Unlock()
	// Stable order: provider, then fingerprint.
	for i := 1; i < len(stats); i++ {
		for j := i; j > 0 && (stats[j].ProviderID < stats[j-1].ProviderID ||
			(stats[j].ProviderID == stats[j-1].ProviderID && stats[j].KeyFingerprint < stats[j-1].KeyFingerprint)); j-- {
			stats[j], stats[j-1] = stats[j-1], stats[j]
		}
	}
	return stats
}

// Drop closes and forgets every pool for providerID (provider
// removal). Idle connections are closed, so a deleted provider
// leaves no warm socket behind.
func (m *PoolManager) Drop(providerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, p := range m.pools {
		if k.providerID == providerID {
			p.Transport.CloseIdleConnections()
			delete(m.pools, k)
		}
	}
}

// PoolCount returns the number of live pools (test/observability
// hook).
func (m *PoolManager) PoolCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pools)
}
