// Settings danger-zone actions (POST /admin/keys/revoke-all and
// POST /admin/config/reset).
//
// Both routes are Tier 2 ALWAYS_PROTECTED (see guard.go): gateway-key
// auth is required unconditionally, even when Tier 3 auth is relaxed,
// and the dashboard must send the key as Authorization: Bearer —
// the dashboard never stores it; the typed-confirmation dialog holds
// it only for the one request and clears the field afterwards. Both
// actions also require the typed confirmation phrase in the JSON body
// ("REVOKE ALL" / "RESET CONFIG"), checked server-side as well as in
// the dialog, so a stray click or a scripted POST without the phrase
// cannot trigger them.
//
// Both actions mutate the live *config.Config in place and then
// persist it through configPersist (wired by main.go to
// cfg.Save(path)). The persist runs before any runtime side effect
// and before bootstrapPending flips, and a persist failure rolls the
// in-memory config back from a Clone snapshot and answers 500 — a
// failed save can never leave runtime and file disagreeing. With no
// config loaded or no persist function wired the handlers answer 503
// (honest "unavailable"), never a pretend success.
//
// Revoke-all takes effect immediately: the verifier reads
// cfg.GatewayKeys live, so the revoked keys — including the key the
// call itself used — stop working on the next request, and bootstrap
// becomes pending so a new first key can be issued loopback-only via
// POST /admin/bootstrap.
//
// Reset restores config defaults (DefaultConfig) while preserving
// the listen address (so the running server keeps its port), the
// data dir, and any unknown top-level fields. Providers and gateway
// keys are removed from the saved config; the in-memory registry is
// cleared too when dangerResetHook is wired (main.go removes every
// provider from the registry), and the response reports
// providers_removed_from_runtime so the dashboard can say honestly
// whether a restart is still needed for provider removal. Usage
// history is deliberately NOT wiped: the ledger is a billing record
// and wiping it needs its own explicit action. Routing/guardrail
// runtime values seeded at startup are also not reloaded by a
// reset — the saved config is the source of truth on the next
// start, and the response says so.
//
// Responses never contain a key, a key hash, a provider key, or a
// base URL — only counts and the preserved listen address.
package server

import (
	"encoding/json"
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// dangerConfirmRevokeAll is the exact phrase the caller must type
// (and send) to revoke every gateway key.
const dangerConfirmRevokeAll = "REVOKE ALL"

// dangerConfirmReset is the exact phrase the caller must type (and
// send) to reset the configuration to defaults.
const dangerConfirmReset = "RESET CONFIG"

// dangerConfirmBody is the JSON body both danger endpoints require.
type dangerConfirmBody struct {
	Confirm string `json:"confirm"`
}

// revokeAllResponse is the POST /admin/keys/revoke-all payload.
type revokeAllResponse struct {
	Revoked              int    `json:"revoked"`
	GatewayKeysRemaining int    `json:"gateway_keys_remaining"`
	BootstrapPending     bool   `json:"bootstrap_pending"`
	Note                 string `json:"note"`
}

// resetConfigResponse is the POST /admin/config/reset payload.
type resetConfigResponse struct {
	ProvidersRemoved            int     `json:"providers_removed"`
	GatewayKeysRevoked          int     `json:"gateway_keys_revoked"`
	ProvidersRemovedFromRuntime bool    `json:"providers_removed_from_runtime"`
	BudgetUSD                   float64 `json:"budget_usd"`
	FailClosed                  bool    `json:"fail_closed"`
	Listen                      string  `json:"listen"`
	BootstrapPending            bool    `json:"bootstrap_pending"`
	Note                        string  `json:"note"`
}

// readDangerConfirm decodes the typed-confirmation body. It returns
// ("", false) with the error already written when the body is
// missing, too large, or not JSON with a confirm string.
func (s *Server) readDangerConfirm(w http.ResponseWriter, r *http.Request, want string) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var body dangerConfirmBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: expected {\"confirm\": \""+want+"\"}")
		return "", false
	}
	if body.Confirm != want {
		writeError(w, http.StatusBadRequest, "confirmation phrase does not match: type "+want+" to confirm")
		return "", false
	}
	return body.Confirm, true
}

// handleRevokeAllKeys serves POST /admin/keys/revoke-all (Tier 2):
// it revokes every gateway key, persists the empty key list, and
// marks bootstrap pending. The caller's own key is revoked too.
func (s *Server) handleRevokeAllKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.readDangerConfirm(w, r, dangerConfirmRevokeAll); !ok {
		return
	}
	if s.cfg == nil || s.configPersist == nil {
		writeError(w, http.StatusServiceUnavailable, "revoke-all unavailable: config persistence is not configured")
		return
	}
	s.dangerMu.Lock()
	defer s.dangerMu.Unlock()
	snapshot := s.cfg.Clone()
	revoked := len(s.cfg.GatewayKeys)
	s.cfg.GatewayKeys = []config.GatewayKey{}
	if err := s.configPersist(); err != nil {
		// M8.14 race fix, round 3: restore via RestoreFrom,
		// not `*s.cfg = *snapshot` — the plain assignment
		// swapped the Routing pointer (and the live mutexes)
		// without holding routingMu. Lock order on this path:
		// dangerMu -> pricesMu -> routingMu; no path takes
		// routingMu/pricesMu and then dangerMu, so no cycle
		// (mirrors release routingMu before any persist runs).
		s.cfg.RestoreFrom(snapshot)
		s.logger.Printf("danger: revoke-all persist failed, config rolled back: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not save config: no keys were revoked")
		return
	}
	s.bootstrapPending.Store(true)
	// AK-2: a fresh one-shot token — the old one is dead, so the
	// first key can be claimed again after the wipe.
	s.resetBootstrapToken()
	s.logger.Printf("danger: revoked %d gateway key(s); bootstrap pending", revoked)
	writeJSON(w, http.StatusOK, revokeAllResponse{
		Revoked:              revoked,
		GatewayKeysRemaining: 0,
		BootstrapPending:     true,
		Note:                 "Every gateway key is revoked, including the key used for this request. Issue a new first key with POST /admin/bootstrap (loopback-only).",
	})
}

// handleResetConfig serves POST /admin/config/reset (Tier 2): it
// resets the config to defaults (preserving listen address, data
// dir and unknown fields), persists it, and marks bootstrap pending.
func (s *Server) handleResetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.readDangerConfirm(w, r, dangerConfirmReset); !ok {
		return
	}
	if s.cfg == nil || s.configPersist == nil {
		writeError(w, http.StatusServiceUnavailable, "reset unavailable: config persistence is not configured")
		return
	}
	s.dangerMu.Lock()
	defer s.dangerMu.Unlock()
	snapshot := s.cfg.Clone()
	providersRemoved := len(snapshot.Providers)
	keysRevoked := len(snapshot.GatewayKeys)
	// Defaults, then restore the pieces a reset must not move: the
	// listen address (the running server keeps its port), the data
	// dir, and unknown top-level fields from newer versions.
	defs := config.DefaultConfig(snapshot.DataDir)
	defs.Listen = snapshot.Listen
	defs.Extensions = snapshot.Extensions
	// M8.14 race fix, round 3: restore via RestoreFrom, not
	// `*s.cfg = *defs` — the plain assignment copied
	// DefaultConfig's FRESH routingMu/pricesMu over the live
	// mutexes (breaking mutual exclusion across the swap)
	// and swapped the Routing pointer unlocked. RestoreFrom
	// keeps the live mutexes and writes Routing under the
	// routing write lock. Same dangerMu -> pricesMu ->
	// routingMu order as the revoke-all rollback above.
	s.cfg.RestoreFrom(defs)
	if err := s.configPersist(); err != nil {
		s.cfg.RestoreFrom(snapshot)
		s.logger.Printf("danger: config reset persist failed, config rolled back: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not save config: nothing was reset")
		return
	}
	s.bootstrapPending.Store(true)
	// AK-2: a fresh one-shot token — the old one is dead, so the
	// first key can be claimed again after the reset.
	s.resetBootstrapToken()
	// Best-effort in-memory provider removal: with no hook the
	// registry keeps serving its loaded providers until restart,
	// and the response says so instead of claiming otherwise.
	runtimeRemoved := false
	runtimeNote := "Providers were removed from the saved config but are still loaded in the running router — restart it for provider removal to take effect."
	if s.dangerResetHook != nil {
		if err := s.dangerResetHook(); err != nil {
			s.logger.Printf("danger: config reset saved but registry removal failed: %v", guardrails.RedactLogLine(err.Error()))
			runtimeNote = "Config reset is saved, but removing providers from the running router failed — restart it for provider removal to take effect."
		} else {
			runtimeRemoved = true
			runtimeNote = "Providers were removed from the saved config and from the running router."
		}
	}
	s.logger.Printf("danger: config reset to defaults (%d provider(s), %d gateway key(s) removed; runtime providers removed=%v)", providersRemoved, keysRevoked, runtimeRemoved)
	writeJSON(w, http.StatusOK, resetConfigResponse{
		ProvidersRemoved:            providersRemoved,
		GatewayKeysRevoked:          keysRevoked,
		ProvidersRemovedFromRuntime: runtimeRemoved,
		BudgetUSD:                   s.cfg.BudgetUSD,
		FailClosed:                  s.cfg.FailClosed,
		Listen:                      s.cfg.Listen,
		BootstrapPending:            true,
		Note:                        runtimeNote + " Usage history was not wiped, and routing/guardrail runtime values apply from the saved config on the next start. Issue a new first key with POST /admin/bootstrap (loopback-only).",
	})
}
