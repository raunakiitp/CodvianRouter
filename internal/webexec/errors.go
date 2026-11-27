package webexec

import (
	"errors"
	"net/http"
	"time"
)

// Sentinel errors. Callers classify with errors.Is; the provider-client
// adapter (client.go) maps ErrStaleSession to an upstream 401 and
// ErrQuotaExceeded to provider.RateLimitError so the router's existing
// failover/pacing sees the right signal.
var (
	// ErrNoSession means no session state was ever stored for the
	// provider. The fix is the operator importing/capturing one.
	ErrNoSession = errors.New("webexec: no stored session state for this provider (import one first)")

	// ErrStaleSession means stored state exists but no longer
	// authenticates: the provider answered 401, or the stored blob is
	// corrupt/older than maxSessionAge. Surfaces as upstream 401 —
	// never retried silently against the same dead session.
	ErrStaleSession = errors.New("webexec: stored session state is stale (provider rejected it) — re-capture the session")

	// ErrQuotaExceeded means the provider answered 429. The adapter
	// converts it to provider.RateLimitError for account fallback.
	ErrQuotaExceeded = errors.New("webexec: provider web quota exhausted (429)")

	// ErrExecutorDisabled means the web-exec executor path was turned
	// off (webexecEnabled = false / WithEnabled(false)). Nothing may
	// silently fall back: an off executor must refuse, loudly.
	ErrExecutorDisabled = errors.New("webexec: executor path is disabled")

	// ErrEndpointUndocumented means the provider's web-session chat
	// endpoint is not officially documented AND the executor is not
	// running against a mock surface, so it refuses instead of
	// guessing a URL. The surface's EndpointNote carries the reason.
	ErrEndpointUndocumented = errors.New("webexec: provider web-session endpoint is not officially documented; refusing to guess a live URL (mock-surface contract only)")

	// ErrNoBrowser means session capture was requested but no headless
	// browser factory is registered (documented limitation — the seam
	// exists, the driver does not ship by default).
	ErrNoBrowser = errors.New("webexec: no headless browser factory registered (register one with RegisterBrowserFactory; capture the storage state by hand in the meantime)")

	// ErrUnknownSurface means a catalog ID outside the 37 WebCookie
	// entries was requested.
	ErrUnknownSurface = errors.New("webexec: unknown web surface (not a catalog WebCookie ID)")
)

// webExecError carries an upstream status with one of the sentinels so
// callers can read the concrete HTTP status without re-parsing text.
type webExecError struct {
	err        error
	statusCode int
	retryAfter time.Duration
	hasRetry   bool
}

func (e *webExecError) Error() string { return e.err.Error() }
func (e *webExecError) Unwrap() error { return e.err }

// statusCodeFor returns the upstream HTTP status a sentinel maps to:
// 401 for stale state, 429 for quota, 503 for everything else.
func statusCodeFor(err error) int {
	switch {
	case errors.Is(err, ErrStaleSession):
		return http.StatusUnauthorized
	case errors.Is(err, ErrQuotaExceeded):
		return http.StatusTooManyRequests
	default:
		return http.StatusServiceUnavailable
	}
}
