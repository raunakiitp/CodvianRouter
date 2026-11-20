// Per-key device tracking (G15 Area 4, the reference router's "{count}
// device(s)" on the key row).
//
// A "device" is a distinct client fingerprint: the SHA-256 of
// (peer IP + "|" + User-Agent), rendered as "dev_" + the first 16
// hex chars. The raw IP and the raw User-Agent are NEVER stored —
// only the one-way fingerprint, first-seen / last-seen times and
// a request count. The store is in-memory only (like the traffic
// limiter and the session tracker): a restart resets it, and
// nothing about devices is ever written to the config file or
// the ledger.
//
// RETENTION RULE (documented, by design): fingerprints live only
// in this process's memory and are forgotten on restart; there is
// no persistence, no export and no cross-key correlation — a
// fingerprint is meaningless without the key it was seen under,
// and two keys used from the same client get the same
// fingerprint value but are stored under different keys.
//
// Coverage: devices are recorded on the spend paths, in
// checkKeyTrafficModel — every admitted /v1 request that passes
// through the traffic gate (chat, embeddings, audio, images,
// responses, relay, A2A). Bare reads that never reach the gate
// (e.g. GET /v1/models) are not recorded; moving the recording
// into the auth funnel itself needs one line in server.go's
// VerifyKey path, which Wave 3 owns — see the report.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// keyDevice is one distinct client fingerprint seen under one
// gateway key: first-seen / last-seen and how many admitted
// requests arrived with it.
type keyDevice struct {
	Fingerprint string
	FirstSeen   time.Time
	LastSeen    time.Time
	Requests    int
}

// keyDeviceView is the wire shape of one device in GET
// /v1/me/status: identifiers and counts only, never a raw IP or
// User-Agent.
type keyDeviceView struct {
	Fingerprint string    `json:"fingerprint"`
	FirstSeen   time.Time `json:"first_seen_at"`
	LastSeen    time.Time `json:"last_seen_at"`
	Requests    int       `json:"requests"`
}

// keyDeviceTracker holds the per-key device maps. It lives inside
// keySessionTracker (keysessions.go) so no Server struct change is
// needed: the tracker is per-server, in-memory, and guarded by its
// own mutex.
type keyDeviceTracker struct {
	mu    sync.Mutex
	byKey map[string]map[string]*keyDevice
}

func newKeyDeviceTracker() *keyDeviceTracker {
	return &keyDeviceTracker{byKey: map[string]map[string]*keyDevice{}}
}

// deviceFingerprint derives the device fingerprint from a peer IP
// and a User-Agent. The inputs are hashed, never stored: the
// fingerprint cannot be reversed into either input, and the same
// client behind a changed IP (or the same IP with a changed
// User-Agent) counts as a different device — which is the honest
// reading, since the router cannot tell a changed client from a
// new one.
func deviceFingerprint(ip, userAgent string) string {
	sum := sha256.Sum256([]byte(ip + "|" + userAgent))
	return "dev_" + hex.EncodeToString(sum[:])[:16]
}

// record notes one admitted request from fingerprint fp under
// keyID: first-seen is set once, last-seen moves, requests counts
// every admitted request with that fingerprint.
func (t *keyDeviceTracker) record(keyID, fp string, now time.Time) {
	if t == nil || keyID == "" || fp == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	byFP, ok := t.byKey[keyID]
	if !ok {
		byFP = map[string]*keyDevice{}
		t.byKey[keyID] = byFP
	}
	dev, ok := byFP[fp]
	if !ok {
		dev = &keyDevice{Fingerprint: fp, FirstSeen: now}
		byFP[fp] = dev
	}
	dev.LastSeen = now
	dev.Requests++
}

// devices returns the devices seen under keyID, newest first.
// The returned views are copies: mutating them cannot change
// the tracker.
func (t *keyDeviceTracker) devices(keyID string) []keyDeviceView {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	byFP := t.byKey[keyID]
	out := make([]keyDeviceView, 0, len(byFP))
	for _, d := range byFP {
		out = append(out, keyDeviceView{
			Fingerprint: d.Fingerprint, FirstSeen: d.FirstSeen,
			LastSeen: d.LastSeen, Requests: d.Requests,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].LastSeen.After(out[b].LastSeen) })
	return out
}

// peerIP extracts the real TCP peer IP from a request: the host
// part of RemoteAddr, falling back to the whole value when it has
// no port. Host / X-Forwarded-For are never consulted — the same
// rule the route guard uses.
func peerIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// keyDeviceTrackerFor returns this server's device tracker,
// lazily creating the session tracker itself for a hand-built
// Server the way trafficLimiter() does for the limiter.
func (s *Server) keyDeviceTrackerFor() *keyDeviceTracker {
	if s.keySessions == nil {
		s.keyMu.Lock()
		if s.keySessions == nil {
			s.keySessions = newKeySessionTracker()
		}
		s.keyMu.Unlock()
	}
	return s.keySessions.deviceStore()
}

// recordKeyDevice notes one admitted request's device under the
// caller's key. It is a no-op without a verifiable caller key.
func (s *Server) recordKeyDevice(r *http.Request) {
	keyID := s.callerKeyID(r)
	if keyID == "" {
		return
	}
	s.keyDeviceTrackerFor().record(keyID, deviceFingerprint(peerIP(r), r.UserAgent()), time.Now())
}

// keyDevicesFor returns the devices seen under keyID, newest
// first, for GET /v1/me/status.
func (s *Server) keyDevicesFor(keyID string) []keyDeviceView {
	return s.keyDeviceTrackerFor().devices(keyID)
}
