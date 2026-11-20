// Per-request cache controls (M6.4): bypass headers and
// per-request cache headers on POST /v1/chat/completions and
// POST /v1/messages.
//
// Headers (all request headers; the parsed directives ride
// provider.ChatRequest.CacheControls into the engine's cache
// wrapper and the messages-relay cache):
//
//	Bypass (skip lookup AND store AND the stale fallback — a
//	bypassed request is never served a stale answer and never
//	poisons the cache):
//	  - X-Codvian-No-Cache: true — mirrors the reference router's
//	    X-Router-No-Cache: true
//	    The X-Router-No-Cache spelling is accepted as an
//	    alias, the same way X-Codvian-Budget accepts the
//	    reference-style Budget-Fallback alias.
//	  - Cache-Control containing a no-cache directive — also
//	    verified the reference router behaviour (same two functions check
//	    cache-control for "no-cache").
//	  - Pragma: no-cache — HTTP/1.0 legacy equivalent. NOT
//	    verified in the reference router (their gateway code never reads
//	    Pragma for caching); accepted here as a Codvian
//	    extension because a client sending it plainly means
//	    "do not serve me a cached answer".
//
//	No-store (skip store only; lookup still allowed):
//	  - X-Codvian-Cache-No-Store: true
//	  - Cache-Control containing a no-store directive (the
//	    standard HTTP meaning; the reference router does not document it,
//	    this is a Codvian extension).
//
//	Per-request controls (X-Codvian- prefix; no per-request
//	key/type/threshold/ttl/no-store header was found in
//	the reference router's docs or gateway source — the reference router has only the
//	global semanticCacheThreshold 0.8 and semanticCacheTTL
//	30 min settings in src/app/api/settings/cache-config/
//	route.ts — so these are Codvian extensions in the same
//	naming spirit, not the reference router parity):
//	  - X-Codvian-Cache-Key: caller-chosen exact-cache bucket.
//	    The value is only a token: the stored key is
//	    hash(tenant + model + value), so tenants and models
//	    never share a bucket. It also disables the semantic
//	    layer for the request (text-keyed matching would defeat
//	    the pinned bucket).
//	  - X-Codvian-Cache-Type: exact | semantic | both | none.
//	    "none" behaves as bypass.
//	  - X-Codvian-Cache-Threshold: semantic similarity floor,
//	    clamped to [0, 1]. It can only narrow hits: the cache
//	    itself only returns entries at or above its configured
//	    floor (0.85), so a lower per-request floor changes
//	    nothing.
//	  - X-Codvian-Cache-TTL: store TTL in seconds, capped at
//	    the layer max (1h, provider.MaxCacheControlTTL). A
//	    per-request TTL can only narrow the window.
//
// Fail-open parsing: an invalid value is IGNORED (the directive
// is dropped, default caching applies), never a 400. A client
// that misspells a control gets ordinary caching, not a refused
// request — unlike the per-request budget header, where a wrong
// value must fail closed because the client believes a spend cap
// is in force. Here the worst case of ignoring is one cacheable
// response, which is the safe default.
package server

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Per-request cache-control header names.
const (
	headerCodvianNoCache        = "X-Codvian-No-Cache"
	headerReferenceNoCache      = "X-Router-No-Cache"
	headerCodvianCacheNoStore   = "X-Codvian-Cache-No-Store"
	headerCodvianCacheKey       = "X-Codvian-Cache-Key"
	headerCodvianCacheType      = "X-Codvian-Cache-Type"
	headerCodvianCacheThreshold = "X-Codvian-Cache-Threshold"
	headerCodvianCacheTTL       = "X-Codvian-Cache-TTL"
)

// cacheTruthy reports whether a header value means "yes":
// true, 1 or yes, case-insensitive, surrounding space ignored.
// Anything else (including "false" and garbage) means "no".
func cacheTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// cacheControlHas reports whether a Cache-Control (or Pragma)
// header value carries the named directive as its own
// comma-separated token, case-insensitive. Token matching — not
// substring matching — so "no-cache-foo" does not count.
func cacheControlHas(raw, directive string) bool {
	for _, part := range strings.Split(raw, ",") {
		token := strings.TrimSpace(part)
		if i := strings.Index(token, ";"); i >= 0 {
			token = strings.TrimSpace(token[:i])
		}
		if strings.EqualFold(token, directive) {
			return true
		}
	}
	return false
}

// validCacheKeyOverride reports whether v is a safe cache-key
// override token: 1-128 bytes, no spaces or control characters.
// Bytes >= 0x80 (non-ASCII UTF-8) pass through — the value is
// hashed, never used raw — so "printable ASCII" would understate
// it. Anything else is ignored by the parser (fail-open), so an
// override can never smuggle separators, newlines or absurd
// lengths into a key.
func validCacheKeyOverride(v string) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

// parseCacheControls reads the per-request cache headers from r
// and returns the directives, or nil when no cache header is
// present at all (nil means "default caching" and keeps the
// request bit-identical to before M6.4). Invalid values are
// ignored individually — one bad header never drops the good
// ones, and nothing here ever errors.
func parseCacheControls(r *http.Request) *provider.CacheControls {
	if r == nil {
		return nil
	}
	h := r.Header
	cc := &provider.CacheControls{}
	seen := false

	// Bypass: X-Codvian-No-Cache / X-Router-No-Cache alias /
	// Cache-Control: no-cache / Pragma: no-cache.
	if cacheTruthy(h.Get(headerCodvianNoCache)) ||
		cacheTruthy(h.Get(headerReferenceNoCache)) ||
		cacheControlHas(h.Get("Cache-Control"), "no-cache") ||
		cacheControlHas(h.Get("Pragma"), "no-cache") {
		cc.Bypass = true
		seen = true
	}
	// No-store: X-Codvian-Cache-No-Store / Cache-Control: no-store.
	if cacheTruthy(h.Get(headerCodvianCacheNoStore)) ||
		cacheControlHas(h.Get("Cache-Control"), "no-store") {
		cc.NoStore = true
		seen = true
	}
	// Type: exact | semantic | both | none; anything else ignored.
	if raw := strings.TrimSpace(h.Get(headerCodvianCacheType)); raw != "" {
		switch strings.ToLower(raw) {
		case "exact", "semantic", "both", "none":
			cc.Type = strings.ToLower(raw)
			seen = true
		}
	}
	// Key override: safe token only; anything else ignored.
	if raw := strings.TrimSpace(h.Get(headerCodvianCacheKey)); raw != "" {
		if validCacheKeyOverride(raw) {
			cc.KeyOverride = raw
			seen = true
		}
	}
	// Threshold: a finite number, clamped to [0, 1]; unparseable
	// or non-finite values ignored.
	if raw := strings.TrimSpace(h.Get(headerCodvianCacheThreshold)); raw != "" {
		if v, err := strconv.ParseFloat(raw, 32); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			f := float32(math.Max(0, math.Min(1, v)))
			cc.Threshold = &f
			seen = true
		}
	}
	// TTL: seconds, must be finite and > 0; capped at the layer
	// max by EffectiveTTL. Unparseable / non-positive ignored.
	if raw := strings.TrimSpace(h.Get(headerCodvianCacheTTL)); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 {
			// Cap before converting: a huge value would
			// overflow the int64 duration, so clamp to the
			// layer max in seconds first.
			if max := provider.MaxCacheControlTTL.Seconds(); v > max {
				v = max
			}
			cc.TTL = time.Duration(v * float64(time.Second))
			seen = true
		}
	}
	if !seen {
		return nil
	}
	return cc
}
