// Provider account login (Phase 1): the operator-facing half of the
// provider-documented OAuth flows in internal/providerlogin.
//
// POST /admin/provider/login starts a login flow and returns the
// authorization URL the operator opens in a browser. Today the one
// supported provider is OpenRouter (official PKCE flow; the exchange
// returns a user-controlled API key, not a refresh token). The route
// stays Tier 1 LOCAL_ONLY + admin scope: starting a login stores a
// credential, so it is loopback-only and key-gated like the other
// system-changing routes.
//
// GET /admin/provider/login/callback is the browser redirect target.
// A top-level browser navigation cannot carry the gateway key, so
// this route is loopback-only WITHOUT a gateway key (the Bootstrap
// guard mechanism) and authenticates the request by the one-time
// PKCE state instead: the state was issued to this loopback operator
// by the start route, is returned unchanged by OpenRouter, and is
// consumed exactly once. The authorization code is useless without
// the code_verifier, which never leaves the server. Off-host
// requests are refused 403 before anything else, exactly like every
// other Tier 1 route.
//
// The exchanged key is stored through the SAME registry path as a
// pasted key: live-tested against the upstream, encrypted at rest,
// model list discovered. When an OpenRouter provider is already
// configured, the key joins its encrypted key pool as another
// account (preferred role) instead of creating a duplicate provider
// — the router's per-key failover then rotates across accounts.
// Neither the code, the state, the verifier nor the key is ever
// logged or rendered into a response page.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/providerlogin"
)

// WithProviderLoginFlow overrides the provider-login flow manager
// (tests point it at a local mock of the provider's auth host).
// Production uses the default flow created in New.
func WithProviderLoginFlow(f *providerlogin.OpenRouterFlow) Option {
	return func(s *Server) { s.providerLogin = f }
}

// maxProviderLoginBody caps the start request body (a provider name
// and a few optional parameters — nothing here is large).
const maxProviderLoginBody = 16 << 10

// providerLoginRequest is the POST /admin/provider/login body.
type providerLoginRequest struct {
	Provider   string `json:"provider"`
	Mode       string `json:"mode"` // "" (auto) | "callback" | "headless"
	KeyLabel   string `json:"key_label"`
	Limit      string `json:"limit"`
	Expiration string `json:"expiration"`
}

// handleProviderLogin serves POST /admin/provider/login: start a
// provider account login flow. The response carries the
// authorization URL and the one-time state — never the verifier.
func (s *Server) handleProviderLogin(w http.ResponseWriter, r *http.Request) {
	if s.providerLogin == nil {
		writeError(w, http.StatusServiceUnavailable, "provider login is not configured")
		return
	}
	var req providerLoginRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxProviderLoginBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.Provider != "openrouter" {
		writeError(w, http.StatusBadRequest, "unsupported provider for account login: only \"openrouter\" is supported in this build (other providers: POST /api/providers with an API key)")
		return
	}
	if req.Mode != "" && req.Mode != "callback" && req.Mode != "headless" {
		writeError(w, http.StatusBadRequest, "mode must be \"callback\" or \"headless\"")
		return
	}
	base := s.agentCardBaseURL()
	mode := req.Mode
	if mode == "" {
		if base != "" {
			mode = "callback"
		} else {
			mode = "headless"
		}
	}
	callbackURL := ""
	if mode == "callback" {
		if base == "" {
			writeError(w, http.StatusBadRequest, "callback mode needs a configured listen address; use mode \"headless\" instead")
			return
		}
		callbackURL = base + "/admin/provider/login/callback"
	}
	started, err := s.providerLogin.Start(providerlogin.OpenRouterStartOptions{
		CallbackURL: callbackURL,
		KeyLabel:    req.KeyLabel,
		Limit:       req.Limit,
		Expiration:  req.Expiration,
	})
	if err != nil {
		// Validation errors name the bad field, never any secret.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Printf("provider login started: provider=openrouter mode=%s", mode)
	resp := map[string]any{
		"provider":          "openrouter",
		"mode":              mode,
		"authorization_url": started.AuthorizationURL,
		"state":             started.State,
		"expires_at":        started.ExpiresAt,
	}
	if callbackURL != "" {
		resp["callback_url"] = callbackURL
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleProviderLoginCallback serves GET /admin/provider/login/callback:
// the browser redirect from the provider's authorization page. It
// exchanges the code for the user's API key and stores it through
// the registry (live test + encrypted storage + model discovery).
// The answer is a small HTML page for the operator's browser — it
// carries no secret material.
func (s *Server) handleProviderLoginCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.loginCallbackPage(w, http.StatusBadRequest, "Authorization was declined",
			"The provider reported an authorization error ("+html.EscapeString(e)+"). No credential was stored. Start a new login when you are ready.")
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" || len(state) > 128 || len(code) > 4096 {
		s.loginCallbackPage(w, http.StatusBadRequest, "Missing authorization code",
			"This callback needs the code and state parameters from the provider's redirect. Start a new login from POST /admin/provider/login.")
		return
	}
	if s.providerLogin == nil {
		s.loginCallbackPage(w, http.StatusServiceUnavailable, "Provider login is not configured", "")
		return
	}
	key, err := s.providerLogin.Exchange(r.Context(), state, code)
	if err != nil {
		switch {
		case errors.Is(err, providerlogin.ErrUnknownState):
			s.loginCallbackPage(w, http.StatusBadRequest, "Unknown or already-used login state",
				"This login flow was already completed, expired, or never started on this router. Start a new login.")
		case errors.Is(err, providerlogin.ErrLoginExpired):
			s.loginCallbackPage(w, http.StatusBadRequest, "Login flow expired",
				"Authorization codes live 10 minutes. Start a new login and authorize again.")
		default:
			s.logger.Printf("provider login exchange failed: %v", guardrails.RedactLogLine(err.Error()))
			s.loginCallbackPage(w, http.StatusBadGateway, "Key exchange failed",
				"The provider's key-exchange endpoint refused the request. No credential was stored. Start a new login; if it keeps failing, check the router log.")
		}
		return
	}
	if s.providerAdder == nil {
		s.loginCallbackPage(w, http.StatusServiceUnavailable, "Provider registry is not configured",
			"The key was exchanged but this router has no provider registry wired, so nothing was stored.")
		return
	}
	p, asKey, err := s.attachOpenRouterKey(r.Context(), key)
	if err != nil {
		s.logger.Printf("provider login store failed: %v", guardrails.RedactLogLine(err.Error()))
		s.loginCallbackPage(w, http.StatusBadGateway, "Could not store the credential",
			html.EscapeString(guardrails.RedactLogLine(err.Error()))+" — the key was tested against the upstream before storage; nothing was persisted on failure. Start a new login to retry.")
		return
	}
	routingLive, note := false, "restart the router to route through the new credential"
	if s.providerAddedHook != nil {
		if herr := s.providerAddedHook(r.Context(), p); herr == nil {
			routingLive, note = true, ""
		} else {
			note = "the credential is stored, but live routing registration failed: " + guardrails.RedactLogLine(herr.Error()) + " (restart the router)"
		}
	}
	detail := "Provider " + html.EscapeString(p.Name) + " (" + html.EscapeString(p.ID) + ") is configured with " +
		strconv.Itoa(len(p.Models)) + " discovered models."
	if asKey {
		detail = "The key was added as another account in the existing OpenRouter provider's encrypted key pool (" +
			strconv.Itoa(len(p.Keys)) + " accounts); the router fails over across them per key."
	}
	if !routingLive && note != "" {
		detail += " Note: " + html.EscapeString(note) + "."
	}
	s.logger.Printf("provider login completed: provider=openrouter id=%s account_key=%v routing_live=%v", p.ID, asKey, routingLive)
	s.loginCallbackPage(w, http.StatusOK, "OpenRouter connected", detail+" You can close this tab.")
}

// attachOpenRouterKey stores a freshly exchanged OpenRouter key:
// into the existing OpenRouter provider's key pool when one is
// configured (multi-account rotation), otherwise as a new catalog
// provider. Both paths live-test the key against the upstream and
// persist it encrypted; a failure stores nothing.
func (s *Server) attachOpenRouterKey(ctx context.Context, key string) (*provider.Provider, bool, error) {
	type keyPoolAdder interface {
		List() []*provider.Provider
		AddKey(ctx context.Context, providerID, apiKey, role string) (provider.ProviderKey, error)
	}
	if kp, ok := s.providerAdder.(keyPoolAdder); ok {
		for _, existing := range kp.List() {
			if existing.CatalogID == "openrouter" {
				if _, err := kp.AddKey(ctx, existing.ID, key, provider.KeyRolePreferred); err != nil {
					return nil, false, err
				}
				return existing, true, nil
			}
		}
	}
	p, err := s.providerAdder.AddCatalogWithOptions(ctx, "openrouter", key, provider.CatalogAddOptions{})
	return p, false, err
}

// loginCallbackPage renders the operator-facing callback result.
// Every dynamic string is HTML-escaped; the page loads no external
// resource and is never cached (the request URL carried a code).
func (s *Server) loginCallbackPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Codvian Router — `+
		html.EscapeString(title)+`</title></head><body style="font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem">`+
		`<h1 style="font-size:1.3rem">`+html.EscapeString(title)+`</h1>`+
		`<p>`+detail+`</p>`+
		`<p style="color:#666;font-size:.85rem">Codvian Router — provider account login</p></body></html>`)
}
