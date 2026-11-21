package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/release"
)

// UpdateCheckFunc returns the latest published release tag
// (v-prefixed). Production wires the same GitHub releases lookup
// `codvian-router update --check` uses (cmd/codvian-router/update.go).
type UpdateCheckFunc func(ctx context.Context) (latestTag string, err error)

// WithUpdateCheck wires POST /admin/update. Without it the route
// answers 503: there is no checker, so it cannot claim anything.
func WithUpdateCheck(currentVersion string, check UpdateCheckFunc) Option {
	return func(s *Server) {
		s.updateCheck = check
		s.updateCurrent = currentVersion
	}
}

// handleAdminUpdate serves POST /admin/update (Tier 1 LOCAL_ONLY):
// a real update CHECK against the release API. It never downloads,
// swaps or restarts anything: installation stays in the CLI
// (`codvian-router update`), which verifies the cosign signature and
// the SHA-256 checksum before replacing the binary.
func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.updateCheck == nil {
		writeError(w, http.StatusServiceUnavailable, "update check is not available on this server")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	latest, err := s.updateCheck(ctx)
	current := strings.TrimPrefix(s.updateCurrent, "v")
	if err != nil {
		var le *release.LookupError
		switch {
		case release.IsNoRelease(err):
			// Confirmed: the repository exists and has no published
			// release. No tag or URL is invented; nothing to update to.
			writeJSON(w, http.StatusOK, map[string]any{
				"current":           "v" + current,
				"latest":            nil,
				"release_available": false,
				"update_available":  false,
				"installed":         false,
				"status":            string(release.KindNoRelease),
				"message":           "no published release is available yet",
			})
		case errors.As(err, &le) && le.Kind == release.KindRateLimited:
			if le.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter/time.Second)))
			}
			writeUpdateCheckError(w, http.StatusServiceUnavailable, le.Kind, err)
		case errors.As(err, &le) && le.Kind == release.KindTimeout,
			errors.Is(err, context.DeadlineExceeded):
			writeUpdateCheckError(w, http.StatusGatewayTimeout, release.KindTimeout, err)
		case errors.As(err, &le):
			writeUpdateCheckError(w, http.StatusBadGateway, le.Kind, err)
		default:
			writeUpdateCheckError(w, http.StatusBadGateway, release.KindUpstream, err)
		}
		return
	}
	available := strings.TrimPrefix(latest, "v") != current
	writeJSON(w, http.StatusOK, map[string]any{
		"current":           "v" + current,
		"latest":            latest,
		"update_available":  available,
		"release_available": true,
		"release_url":       "https://github.com/" + release.DefaultRepo + "/releases/tag/" + latest,
		"installed":         false,
		"install_command":   "codvian-router update",
		"status":            "ok",
	})
}

// writeUpdateCheckError answers a failed check with its kind, so a
// caller can tell a rate limit or timeout from a broken release API.
// It never reports "no release" for these.
func writeUpdateCheckError(w http.ResponseWriter, status int, kind release.LookupKind, err error) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": "update check failed: " + err.Error(),
			"type":    "update_check_failed",
			"code":    string(kind),
		},
		"update_available": false,
	})
}
