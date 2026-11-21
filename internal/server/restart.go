package server

import (
	"net/http"
	"os"
)

// handleRestart serves POST /admin/restart (Tier 1 LOCAL_ONLY, the
// guard runs before this handler and before auth). It answers 202
// and then runs the restart hook asynchronously, so the response
// reaches the caller before the HTTP server stops. In production the
// hook stops the server and runServe starts the replacement (see
// relaunchAfterRestart in cmd/codvian-router). Without a restart
// hook it answers 503: it never falls back to the shutdown hook,
// because that would stop the gateway while reporting "restarting".
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	hook := s.restartHook
	if hook == nil {
		writeError(w, http.StatusServiceUnavailable, "restart is not available on this server")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "restarting",
		"pid":    os.Getpid(),
	})
	s.a2aStopPruneLoop()
	go hook()
}
