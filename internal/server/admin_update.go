package server

import (
	"context"
	"net/http"
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
	if err != nil {
		writeError(w, http.StatusBadGateway, "update check failed: "+err.Error())
		return
	}
	current := strings.TrimPrefix(s.updateCurrent, "v")
	available := strings.TrimPrefix(latest, "v") != current
	writeJSON(w, http.StatusOK, map[string]any{
		"current":          "v" + current,
		"latest":           latest,
		"update_available": available,
		"release_url":      "https://github.com/" + release.DefaultRepo + "/releases/tag/" + latest,
		"installed":        false,
		"install_command":  "codvian-router update",
	})
}
