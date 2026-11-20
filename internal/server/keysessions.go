package server

import (
	"fmt"
	"net/http"
	"sync"
)

// Concurrent in-flight sessions per gateway key (M5.10,
// OmniRoute's maxSessions — stored but unenforced for gateway
// keys in the current checkout; see keyaccess.go). The cap
// counts requests that have passed every refusal gate and are
// inside the engine call: a request past the cap is refused
// with a 429 and never reaches a provider. The count is held in
// memory only — a restart resets it, the same way the traffic
// limiter's windows reset.
type keySessionTracker struct {
	mu       sync.Mutex
	inFlight map[string]int
	// devices is the per-key device tracker (G15 Area 4): the
	// distinct client fingerprints seen under each key, held
	// in memory only. It lives here so no Server struct change
	// is needed; see keydevices.go for the retention rule.
	devices *keyDeviceTracker
}

func newKeySessionTracker() *keySessionTracker {
	return &keySessionTracker{inFlight: map[string]int{}, devices: newKeyDeviceTracker()}
}

// devices returns the tracker's device store, creating it lazily
// for a hand-built tracker.
func (t *keySessionTracker) deviceStore() *keyDeviceTracker {
	if t == nil {
		return newKeyDeviceTracker()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.devices == nil {
		t.devices = newKeyDeviceTracker()
	}
	return t.devices
}

// tryEnter admits one more in-flight request for keyID under a
// cap of max. False means the cap is already reached. A max <= 0
// means no cap.
func (t *keySessionTracker) tryEnter(keyID string, max int) bool {
	if max <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inFlight[keyID] >= max {
		return false
	}
	t.inFlight[keyID]++
	return true
}

// exit releases one in-flight slot for keyID.
func (t *keySessionTracker) exit(keyID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inFlight[keyID] <= 1 {
		delete(t.inFlight, keyID)
		return
	}
	t.inFlight[keyID]--
}

// tryEnterKeySession admits the caller's request under its
// max-sessions cap. It writes the 429 and returns false when
// the cap is reached; true when admitted (or when the key has
// no cap / no verifiable key). The caller must defer
// exitKeySession with the returned keyID when admitted.
func (s *Server) tryEnterKeySession(w http.ResponseWriter, r *http.Request) (string, bool) {
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return "", true
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok || key.MaxSessions <= 0 {
		return "", true
	}
	if !s.keySessions.tryEnter(keyID, key.MaxSessions) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{
			Message: fmt.Sprintf("Maximum %d concurrent sessions reached for this API key", key.MaxSessions),
			Type:    "key_max_sessions",
		}})
		return "", false
	}
	return keyID, true
}

// exitKeySession releases one in-flight slot. A "" keyID is a
// no-op (the request was never counted).
func (s *Server) exitKeySession(keyID string) {
	if keyID == "" {
		return
	}
	s.keySessions.exit(keyID)
}
