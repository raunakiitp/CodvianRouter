package server

import (
	"net/http"
	"os"
)

// handleRestart serves POST /admin/restart (Tier 1): it answers 202
// and invokes the restart hook (or shutdown hook if restart hook is nil).
// The hook runs asynchronously so the 202 response reliably reaches the caller.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	hook := s.restartHook
	if hook == nil {
		hook = s.shutdownHook
	}
	if hook == nil {
		writeError(w, http.StatusServiceUnavailable, "restart hook is not configured")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "restarting",
		"pid":    os.Getpid(),
	})
	s.a2aStopPruneLoop()
	go hook()
}
