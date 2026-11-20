// Package readiness is the honest-unreadiness gate (research-log
// entry 179): a gateway-provided local capability that is not ready
// answers 503 with Retry-After until its assets are ready — it
// degrades honestly instead of failing silently.
//
// A capability is registered with a probe (does the local runtime /
// asset set answer yet?) and a Retry-After in seconds (how long the
// client should wait before retrying). Operators and tests can also
// force a capability ready / not-ready (SetReady) — e.g. while
// background asset downloads run. A capability with no probe and no
// override is unknown, never assumed ready: Status reports
// ready=false with the registered Retry-After, so an unprobed local
// capability degrades honestly instead of being served half-built.
package readiness

import (
	"context"
	"sync"
	"time"
)

// Capability is one gateway-provided local capability's live state.
type Capability struct {
	// Name is the capability id, e.g. "local:ollama-local".
	Name string `json:"name"`
	// Ready is whether the capability can serve requests right now.
	Ready bool `json:"ready"`
	// RetryAfterSeconds is how long a client should wait before
	// retrying when Ready is false. 0 means "no guidance" — the
	// server still sends the header with the registered value, or
	// omits it only when nothing was registered.
	RetryAfterSeconds int `json:"retry_after_seconds"`
	// Known is false when the capability was never registered: the
	// gate knows nothing about it, and the server must not treat
	// that as ready.
	Known bool `json:"known"`
}

type capability struct {
	name       string
	retryAfter time.Duration
	probe      func(context.Context) bool
	override   *bool
	mu         sync.Mutex
	lastProbe  time.Time
	lastReady  bool
}

// Gate holds the registered local capabilities. The zero value is
// usable but empty (no capabilities registered).
type Gate struct {
	mu   sync.RWMutex
	caps map[string]*capability
}

// Register adds (or replaces) a capability. probe may be nil — then
// readiness is decided only by SetReady, and an un-set capability
// reports not-ready. retryAfter is the Retry-After the server sends
// while the capability is not ready; 0 means the server sends its
// own default (30 s) instead of inventing a per-capability value.
func (g *Gate) Register(name string, retryAfter time.Duration, probe func(context.Context) bool) {
	if name == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.caps == nil {
		g.caps = make(map[string]*capability)
	}
	g.caps[name] = &capability{name: name, retryAfter: retryAfter, probe: probe}
}

// Unregister removes a capability (e.g. its provider was deleted):
// the readiness feed stops listing it, and its models are no longer
// gated. Unknown names are a no-op.
func (g *Gate) Unregister(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.caps, name)
}

// SetReady forces a capability's readiness (background asset
// downloads call SetReady(name, true) when they finish; tests use it
// to flip a capability without a network probe). It is a no-op for
// an unregistered name — forcing an unknown capability ready would
// be inventing readiness.
func (g *Gate) SetReady(name string, ready bool) {
	g.mu.RLock()
	c, ok := g.caps[name]
	g.mu.RUnlock()
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.override = &ready
}

// Status probes one capability (cached for 1 s so a hot request path
// does not re-probe a local runtime on every request; a forced
// override via SetReady always wins over the probe) and reports its
// live state. Unknown names report Known=false, Ready=false.
func (g *Gate) Status(ctx context.Context, name string) Capability {
	g.mu.RLock()
	c, ok := g.caps[name]
	g.mu.RUnlock()
	if !ok {
		return Capability{Name: name, Ready: false, Known: false}
	}
	ra := int(c.retryAfter / time.Second)
	if ra <= 0 {
		ra = 30
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.override != nil {
		return Capability{Name: name, Ready: *c.override, RetryAfterSeconds: ra, Known: true}
	}
	if c.probe == nil {
		return Capability{Name: name, Ready: false, RetryAfterSeconds: ra, Known: true}
	}
	if time.Since(c.lastProbe) < time.Second {
		return Capability{Name: name, Ready: c.lastReady, RetryAfterSeconds: ra, Known: true}
	}
	ready := c.probe(ctx)
	c.lastProbe = time.Now()
	c.lastReady = ready
	return Capability{Name: name, Ready: ready, RetryAfterSeconds: ra, Known: true}
}

// Statuses reports every registered capability's live state (each
// probed under the same 1 s cache as Status). An empty gate reports
// an empty, non-nil list — never nil, never invented capabilities.
func (g *Gate) Statuses(ctx context.Context) []Capability {
	g.mu.RLock()
	names := make([]string, 0, len(g.caps))
	for n := range g.caps {
		names = append(names, n)
	}
	g.mu.RUnlock()
	out := make([]Capability, 0, len(names))
	for _, n := range names {
		out = append(out, g.Status(ctx, n))
	}
	return out
}
