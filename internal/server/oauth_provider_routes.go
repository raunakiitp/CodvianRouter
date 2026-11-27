package server

// Provider OAuth connect routes (A3 W1): HTTP surfaces for the five
// bridge methods that connect a catalog OAuth provider —
//
//	POST /dashboard/oauth/providers/{provider}/start            → StartProviderOAuth
//	POST /dashboard/oauth/providers/{provider}/complete         → CompleteProviderOAuth
//	POST /dashboard/oauth/providers/{provider}/device/start     → StartProviderDevice
//	POST /dashboard/oauth/providers/{provider}/device/complete  → CompleteProviderDevice
//	POST /dashboard/oauth/providers/{provider}/import           → ImportProviderBlob
//
// plus one read-only listing route the dashboard connect flow needs:
//
//	GET  /dashboard/oauth/providers                             → providers.All()
//
// The routes are provider-agnostic: the provider ID lives in the path,
// so every registered handler is automatically reachable. Auth follows
// the PR #52 dashboard OAuth routes exactly (TierManagement, admin
// scope, session-or-gateway-key enforced in the handler). Responses
// NEVER carry a credential: the access/refresh tokens go straight into
// the Vault-sealed connection store and stay there.
//
// Error mapping is typed, not guessed: unknown provider → 404;
// honestly-unimplemented → 501 with the registry's documented reason;
// wrong-flow for the route → 409; upstream token endpoint refused the
// exchange → 502; everything else → 500. The device-complete route
// answers 200 with status "pending" while approval is outstanding
// (ErrAuthorizationPending is not an error state — it means poll again
// after DeviceCode.Interval).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/oauth/providers"
)

// providerOAuthRequestLimit bounds every connect-route body: none of
// these payloads is more than a redirect URI, a code, a device-code
// round trip, or a pasted credential blob.
const providerOAuthRequestLimit = 1 << 20 // 1 MiB

// decodeProviderOAuthBody reads and decodes a JSON body; an empty body
// decodes to the zero struct (the start route's redirect_uri is
// optional).
func decodeProviderOAuthBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, providerOAuthRequestLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, "oauth: request body too large or unreadable")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "oauth: invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// checkProviderOAuthAuth is the shared credential check: a dashboard
// session or a gateway key, exactly like the PR #52 OAuth routes.
func (s *Server) checkProviderOAuthAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return false
	}
	return true
}

// providerOAuthRefusal maps a providers-registry refusal to its HTTP
// status and message. The registry's own error text is preserved — the
// reason strings are the public, documented explanations.
func providerOAuthRefusal(err error) (int, string) {
	var status int
	switch {
	case errors.Is(err, providers.ErrNotImplemented):
		status = http.StatusNotImplemented
	case errors.Is(err, providers.ErrImportOnly):
		status = http.StatusConflict
	case errors.Is(err, providers.ErrNotCodeFlow), errors.Is(err, providers.ErrNotDeviceFlow):
		status = http.StatusConflict
	case errors.Is(err, providers.ErrImportRefused):
		status = http.StatusBadRequest
	default:
		if strings.Contains(err.Error(), "is not a catalog OAuth provider") {
			return http.StatusNotFound, err.Error()
		}
		status = http.StatusBadGateway
	}
	return status, err.Error()
}

// refuseProviderOAuth validates the route provider against the
// providers registry up front so the typed refusals answer with the
// right status before any flow state is built.
func refuseProviderOAuth(w http.ResponseWriter, providerID string) bool {
	if _, err := providers.Get(providerID); err != nil {
		status, msg := providerOAuthRefusal(err)
		writeError(w, status, msg)
		return false
	}
	return true
}

// handleProviderOAuthList serves GET /dashboard/oauth/providers: every
// catalog OAuth provider's registry entry — implemented flows and the
// honestly-unimplemented ones with their reason — so the dashboard
// renders real connect options instead of a hard-coded list.
func (s *Server) handleProviderOAuthList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.dashSessionValid(r) && !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	type entry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Alias       string `json:"alias,omitempty"`
		Flow        string `json:"flow"`
		DocsURL     string `json:"docs_url,omitempty"`
		Implemented bool   `json:"implemented"`
		ImportOnly  bool   `json:"import_only"`
		Reason      string `json:"reason,omitempty"`
	}
	infos := providers.All()
	out := make([]entry, 0, len(infos))
	for _, in := range infos {
		out = append(out, entry{
			ID:          in.ID,
			Name:        in.Name,
			Alias:       in.Alias,
			Flow:        string(in.Flow),
			DocsURL:     in.DocsURL,
			Implemented: in.Implemented,
			ImportOnly:  in.ImportOnly,
			Reason:      in.Reason,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out, "total": len(out)})
}

// handleProviderOAuthStart serves POST
// /dashboard/oauth/providers/{provider}/start: builds the provider's
// authorization URL (code / browser-sign-in flows) and returns state,
// authorize_url and the PKCE verifier. The verifier stays in the
// operator's session until the complete call; it is never stored
// server-side beyond this response.
func (s *Server) handleProviderOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.checkProviderOAuthAuth(w, r) {
		return
	}
	providerID := r.PathValue("provider")
	if !refuseProviderOAuth(w, providerID) {
		return
	}
	var body struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if !decodeProviderOAuthBody(w, r, &body) {
		return
	}
	state, authURL, verifier, err := s.StartProviderOAuth(providerID, body.RedirectURI)
	if err != nil {
		status, msg := providerOAuthRefusal(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":      providerID,
		"flow":          "code",
		"state":         state,
		"authorize_url": authURL,
		"verifier":      verifier,
	})
}

// handleProviderOAuthComplete serves POST
// /dashboard/oauth/providers/{provider}/complete: exchanges the
// authorization code (same redirect URI + verifier the start call
// returned) for tokens and stores the connection. The response never
// includes the tokens.
func (s *Server) handleProviderOAuthComplete(w http.ResponseWriter, r *http.Request) {
	if !s.checkProviderOAuthAuth(w, r) {
		return
	}
	providerID := r.PathValue("provider")
	if !refuseProviderOAuth(w, providerID) {
		return
	}
	var body struct {
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
		Verifier    string `json:"verifier"`
	}
	if !decodeProviderOAuthBody(w, r, &body) {
		return
	}
	if body.Code == "" {
		writeError(w, http.StatusBadRequest, "oauth: code is required")
		return
	}
	if err := s.CompleteProviderOAuth(providerID, body.Code, body.RedirectURI, body.Verifier); err != nil {
		status, msg := providerOAuthRefusal(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": providerID, "status": "connected"})
}

// providerDeviceFlowTTL bounds a cached in-flight device flow: the
// RFC 8628 default verification window is minutes, not hours.
const providerDeviceFlowTTL = 20 * time.Minute

// providerDeviceFlowEntry is one cached in-flight device flow.
type providerDeviceFlowEntry struct {
	dc       *providers.DeviceCode
	deadline time.Time
}

// rememberProviderDeviceFlow caches a just-started device flow keyed by
// provider + device code, pruning expired entries as it goes.
func (s *Server) rememberProviderDeviceFlow(providerID string, dc *providers.DeviceCode) {
	s.providerDeviceFlowsMu.Lock()
	defer s.providerDeviceFlowsMu.Unlock()
	if s.providerDeviceFlows == nil {
		s.providerDeviceFlows = map[string]providerDeviceFlowEntry{}
	}
	now := time.Now()
	for k, e := range s.providerDeviceFlows {
		if now.After(e.deadline) {
			delete(s.providerDeviceFlows, k)
		}
	}
	ttl := providerDeviceFlowTTL
	if dc.ExpiresIn > 0 {
		ttl = time.Duration(dc.ExpiresIn) * time.Second
	}
	s.providerDeviceFlows[providerID+"/"+dc.DeviceCode] = providerDeviceFlowEntry{dc: dc, deadline: now.Add(ttl)}
}

// takeProviderDeviceFlow returns the cached device flow for a provider
// + device code (removing nothing — the same flow may be polled several
// times before approval); ok=false means unknown or expired.
func (s *Server) takeProviderDeviceFlow(providerID, deviceCode string) (*providers.DeviceCode, bool) {
	s.providerDeviceFlowsMu.Lock()
	defer s.providerDeviceFlowsMu.Unlock()
	e, ok := s.providerDeviceFlows[providerID+"/"+deviceCode]
	if !ok || time.Now().After(e.deadline) {
		if ok {
			delete(s.providerDeviceFlows, providerID+"/"+deviceCode)
		}
		return nil, false
	}
	return e.dc, true
}

// handleProviderDeviceStart serves POST
// /dashboard/oauth/providers/{provider}/device/start: begins the
// provider's device authorization grant and returns the device code,
// user code and verification URI for the operator to approve. The
// flow's poll state is cached server-side — the complete call only
// needs the device code back.
func (s *Server) handleProviderDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !s.checkProviderOAuthAuth(w, r) {
		return
	}
	providerID := r.PathValue("provider")
	if !refuseProviderOAuth(w, providerID) {
		return
	}
	dc, err := s.StartProviderDevice(providerID)
	if err != nil {
		status, msg := providerOAuthRefusal(err)
		writeError(w, status, msg)
		return
	}
	s.rememberProviderDeviceFlow(providerID, dc)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":                  providerID,
		"flow":                      "device",
		"device_code":               dc.DeviceCode,
		"user_code":                 dc.UserCode,
		"verification_uri":          dc.VerificationURI,
		"verification_uri_complete": dc.VerificationURIComplete,
		"expires_in":                dc.ExpiresIn,
		"interval":                  dc.Interval,
	})
}

// handleProviderDeviceComplete serves POST
// /dashboard/oauth/providers/{provider}/device/complete: polls the
// device flow ONCE with the device code from the start call (the flow's
// poll state is the server-side cache — the operator only echoes the
// device code). Approval outstanding answers 200 with status "pending"
// (poll again after the reported interval); approval stores the tokens
// — the response never carries them.
func (s *Server) handleProviderDeviceComplete(w http.ResponseWriter, r *http.Request) {
	if !s.checkProviderOAuthAuth(w, r) {
		return
	}
	providerID := r.PathValue("provider")
	if !refuseProviderOAuth(w, providerID) {
		return
	}
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeProviderOAuthBody(w, r, &body) {
		return
	}
	if body.DeviceCode == "" {
		writeError(w, http.StatusBadRequest, "oauth: device_code is required")
		return
	}
	dc, ok := s.takeProviderDeviceFlow(providerID, body.DeviceCode)
	if !ok {
		writeError(w, http.StatusBadRequest, "oauth: unknown or expired device flow — start the device flow again")
		return
	}
	tr, err := s.CompleteProviderDevice(providerID, dc)
	if err != nil {
		if errors.Is(err, providers.ErrAuthorizationPending) {
			writeJSON(w, http.StatusOK, map[string]any{"provider": providerID, "status": "pending"})
			return
		}
		status, msg := providerOAuthRefusal(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":   providerID,
		"status":     "connected",
		"scope":      tr.Scope,
		"token_type": tr.TokenType,
		"expires_in": tr.ExpiresIn,
	})
}

// handleProviderImport serves POST
// /dashboard/oauth/providers/{provider}/import: stores an allowlisted
// paste-token credential blob (antigravity, agy only; the blob's
// embedded provider must match the route provider). The raw blob never
// lands in a log or a response.
func (s *Server) handleProviderImport(w http.ResponseWriter, r *http.Request) {
	if !s.checkProviderOAuthAuth(w, r) {
		return
	}
	providerID := r.PathValue("provider")
	// Import-only providers are exactly who this route serves — their
	// ErrImportOnly is not a refusal here. Anything else the registry
	// refuses stays refused.
	if _, err := providers.Get(providerID); err != nil && !errors.Is(err, providers.ErrImportOnly) {
		status, msg := providerOAuthRefusal(err)
		writeError(w, status, msg)
		return
	}
	var body struct {
		Blob string `json:"blob"`
	}
	if !decodeProviderOAuthBody(w, r, &body) {
		return
	}
	if body.Blob == "" {
		writeError(w, http.StatusBadRequest, "oauth: blob is required")
		return
	}
	if err := s.ImportProviderBlob(providerID, []byte(body.Blob)); err != nil {
		status, msg := providerOAuthRefusal(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": providerID, "status": "connected"})
}
