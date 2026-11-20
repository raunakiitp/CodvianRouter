// Headless mode + first-run wizard status feed (CHECKLIST 197-198).
//
// serve --headless runs the gateway as a pure API process for
// servers/containers: no dashboard server at all. The dashboard
// routes are not merely hidden — Server.register answers every
// /dashboard* path with 404 while headless (see guard.go), so a
// headless process serves /v1/*, /metrics and /healthz only.
// Headless refuses to start without a gateway key configured
// (auth-on-from-first-boot): that refusal lives in cmd
// runServe, before the stack is built, so no unauthenticated
// headless gateway can ever listen.
//
// GET /dashboard/headless is the Settings feed for the mode: on a
// normal server it reports headless=false (this process HAS a
// dashboard) plus the exact terminal commands to start headless
// and to re-run the first-run wizard. On a headless server the
// feed itself is a /dashboard* route, so it 404s like the rest —
// that is the honest answer there, not a bug: headless serves no
// dashboard, and the docs say so instead of faking one.
package server

import (
	"net/http"
)

// WithHeadless marks the server as a headless (API-only) process:
// every /dashboard* route answers 404. Only serve --headless
// sets it; the default is a dashboard server.
func WithHeadless(headless bool) Option {
	return func(s *Server) { s.headless = headless }
}

// Headless reports whether this server runs headless (no dashboard).
func (s *Server) Headless() bool { return s.headless }

// headlessResponse is the GET /dashboard/headless payload.
type headlessResponse struct {
	// Headless is always false here: a headless server 404s this
	// feed (it is a /dashboard* route), so a 200 answer can only
	// come from a dashboard server.
	Headless bool `json:"headless"`
	// DashboardEnabled is true whenever this feed answers.
	DashboardEnabled bool `json:"dashboard_enabled"`
	// WizardAvailable is true: the wizard is a CLI command
	// (init --wizard), always available in the binary.
	WizardAvailable bool `json:"wizard_available"`
	// Commands are the exact terminal commands to reproduce
	// each mode — the dashboard cannot spawn processes, so it
	// shows the commands instead of pretending to run them.
	HeadlessCommand string `json:"headless_command"`
	WizardCommand   string `json:"wizard_command"`
	RunCommand      string `json:"run_command"`
	// Note states the headless limitation in one line.
	Note string `json:"note"`
}

// handleHeadlessStatus serves GET /dashboard/headless.
func (s *Server) handleHeadlessStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, headlessResponse{
		Headless:         false,
		DashboardEnabled: true,
		WizardAvailable:  true,
		HeadlessCommand:  "codvian-router serve --headless",
		WizardCommand:    "codvian-router init --wizard",
		RunCommand:       "codvian-router run <cli> --model <model>",
		Note:             "Headless mode serves the API only — no dashboard, no browser, no tray. It refuses to start without a gateway key configured.",
	})
}
