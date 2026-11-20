// Stream-lifetime feed (CHECKLIST 256).
//
// GET /dashboard/stream-lifetime reports the stream-lifetime cap
// the engine enforces (CHECKLIST 255) and how many streams it has
// terminated: the effective cap (30 minutes by default, the
// operator-configured value when set, 0 when disabled) and the
// live kill count since this process started. It is Tier 3
// MANAGEMENT, auth-exempt like the other dashboard feeds: a cap
// duration and a count only — no keys, no message content, no
// stream bodies.
//
// The values come from the engine via interface probing (the
// same pattern as the groups feed's breaker/stats reporters): an
// engine that does not report a lifetime (a test double, or the
// degrade wrapper before it forwards the method) answers the
// honest disabled state — enabled=false, killed_total=0 — never
// an invented cap. The verbatim-relay bound reads the same cap
// through streamLifetimeCap, so the feed, the dashboard and the
// enforcement can never disagree about it.
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/router"
)

// streamLifetimeReporter is optionally implemented by engines that
// enforce the stream-lifetime cap (router.Engine): the effective
// cap (0 = disabled) and how many streams it has terminated.
type streamLifetimeReporter interface {
	StreamMaxLifetime() time.Duration
	StreamLifetimeExceededCount() uint64
}

// streamLifetimeResponse is the GET /dashboard/stream-lifetime
// payload.
type streamLifetimeResponse struct {
	// Enabled reports whether the cap is on: false when the
	// engine reports no cap (disabled, or an engine that does
	// not report one).
	Enabled bool `json:"enabled"`
	// MaxLifetimeMs is the effective cap in milliseconds (0
	// when disabled).
	MaxLifetimeMs int64 `json:"max_lifetime_ms"`
	// MaxLifetime is the effective cap as a human duration
	// ("30m0s", "0s" when disabled).
	MaxLifetime string `json:"max_lifetime"`
	// KilledTotal is how many streams the cap has terminated
	// since this process started.
	KilledTotal uint64 `json:"killed_total"`
}

// streamLifetimeFeed builds the live stream-lifetime feed from
// the engine's cap and kill count.
func (s *Server) streamLifetimeFeed() streamLifetimeResponse {
	rep, ok := s.engine.(streamLifetimeReporter)
	if !ok || rep == nil {
		return streamLifetimeResponse{Enabled: false, MaxLifetimeMs: 0, MaxLifetime: "0s", KilledTotal: 0}
	}
	cap := rep.StreamMaxLifetime()
	if cap <= 0 {
		return streamLifetimeResponse{Enabled: false, MaxLifetimeMs: 0, MaxLifetime: "0s", KilledTotal: rep.StreamLifetimeExceededCount()}
	}
	return streamLifetimeResponse{Enabled: true, MaxLifetimeMs: cap.Milliseconds(), MaxLifetime: cap.String(), KilledTotal: rep.StreamLifetimeExceededCount()}
}

// streamLifetimeCap returns the effective stream-lifetime cap for
// the verbatim-relay bound (see forwardVerbatim): 0 when the
// engine reports none, so the relay keeps its existing server
// request-timeout bound only.
func (s *Server) streamLifetimeCap() time.Duration {
	rep, ok := s.engine.(streamLifetimeReporter)
	if !ok || rep == nil {
		return 0
	}
	cap := rep.StreamMaxLifetime()
	if cap < 0 {
		return 0
	}
	return cap
}

// streamLifetimeCause reports the lifetime error when ctx was
// cancelled by the stream-lifetime cap firing, and nil otherwise
// — a client disconnect, a guardrail cut, or the request timeout
// cancelling the same context is not a lifetime expiry, and must
// never be reported as one. The relay paths (forwardVerbatim,
// handleMessagesStream) use it to decide whether the terminal
// error event names the cap.
func streamLifetimeCause(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil && errors.Is(cause, router.ErrStreamLifetimeExceeded) {
		return cause
	}
	return nil
}

// handleStreamLifetime serves GET /dashboard/stream-lifetime.
func (s *Server) handleStreamLifetime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.streamLifetimeFeed())
}
