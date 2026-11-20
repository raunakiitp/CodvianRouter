package server

// OAuth 2.0 gateway auth (PARITY.md:678, B3-07): the gateway is
// its own authorization server. An operator registers an OAuth
// client (POST /oauth/clients), approves a grant at
// GET /oauth/authorize (authorization code + PKCE S256), the
// client redeems the code at POST /oauth/token for a JWT access
// token (HS256, 1h) + a rotating refresh token (30d), and then
// calls /v1 routes with Authorization: Bearer <jwt>. The JWT is
// accepted by the same auth funnel as gateway keys (authorized /
// requireScope / callerKeyID in server.go), so a token's scopes
// gate routes exactly like a key's scopes, and "admin" implies
// every scope. Per-key IP/endpoint policy does not apply to
// OAuth tokens — their scope IS the policy — so keyPolicyAllows
// passes them untouched (callerKeyID names them "oauth:<id>",
// which no stored gateway key matches).
//
// The issuer lives in internal/oauth (stdlib HS256, no new
// dependency). Its signing key, clients and refresh tokens
// persist to <data-dir>/oauth.json (0600); with no data dir the
// issuer is in-memory only and tokens die with the process.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/oauth"
)

// oauthIssuer returns this server's OAuth issuer, creating it on
// first use from cfg.DataDir (in-memory only when there is no
// config or no data dir). A corrupt persisted state fails here
// and the OAuth endpoints answer 503 — a broken issuer must
// never silently mint tokens on a fresh key.
func (s *Server) oauthIssuer() (*oauth.Issuer, error) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauth != nil {
		return s.oauth, nil
	}
	if s.oauthErr != nil {
		return nil, s.oauthErr
	}
	dataDir := ""
	if s.cfg != nil {
		dataDir = s.cfg.DataDir
	}
	iss, err := oauth.NewIssuer(dataDir)
	if err != nil {
		s.oauthErr = err
		return nil, err
	}
	s.oauth = iss
	return iss, nil
}

// oauthTokenValid reports whether token is a currently-valid
// gateway JWT access token (auth-funnel helper).
func (s *Server) oauthTokenValid(token string) bool {
	iss, err := s.oauthIssuer()
	if err != nil || iss == nil {
		return false
	}
	_, err = iss.VerifyAccessToken(token)
	return err == nil
}

// oauthTokenScopes returns the scopes a JWT access token carries
// (scope-wrapper helper). It reports false for anything that is
// not a currently-valid gateway JWT.
func (s *Server) oauthTokenScopes(token string) ([]string, bool) {
	iss, err := s.oauthIssuer()
	if err != nil || iss == nil {
		return nil, false
	}
	return iss.TokenScopes(token)
}

type oauthClientCreateRequest struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	Scopes       []string `json:"scopes"`
	Public       bool     `json:"public"`
}

type oauthClientCreateResponse struct {
	oauth.ClientView
	// ClientSecret is present only for confidential clients, and
	// only in this response — it is shown once and never stored.
	ClientSecret string `json:"client_secret,omitempty"`
}

// handleOAuthCreateClient serves POST /oauth/clients (Tier 2,
// admin scope): registers one OAuth client. A confidential
// client's secret is returned once and never logged; a public
// (PKCE-only) client gets no secret.
func (s *Server) handleOAuthCreateClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	iss, err := s.oauthIssuer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oauth unavailable: "+err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req oauthClientCreateRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid oauth client: expected {\"name\": string, \"redirect_uris\": [string], \"scopes\": [string] (optional), \"public\": bool (optional)}")
		return
	}
	client, secret, err := iss.RegisterClient(req.Name, req.RedirectURIs, req.Scopes, req.Public)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, oauthClientCreateResponse{ClientView: client.View(), ClientSecret: secret})
}

// handleOAuthListClients serves GET /oauth/clients (Tier 2, admin
// scope): every registered client, without secrets.
func (s *Server) handleOAuthListClients(w http.ResponseWriter, r *http.Request) {
	iss, err := s.oauthIssuer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oauth unavailable: "+err.Error())
		return
	}
	clients := iss.ListClients()
	if clients == nil {
		clients = []oauth.ClientView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

// handleOAuthDeleteClient serves DELETE /oauth/clients/{id}
// (Tier 2, admin scope): removes the client and revokes its
// outstanding codes and refresh tokens.
func (s *Server) handleOAuthDeleteClient(w http.ResponseWriter, r *http.Request) {
	iss, err := s.oauthIssuer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oauth unavailable: "+err.Error())
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "client id is required")
		return
	}
	if !iss.DeleteClient(id) {
		writeError(w, http.StatusNotFound, "unknown oauth client "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// oauthAuthorizeError answers an authorization error: when the
// redirect_uri is usable it redirects with ?error= (RFC 6749
// §4.1.2.1); otherwise it answers 400 JSON, because there is
// nowhere safe to send the error.
func oauthAuthorizeError(w http.ResponseWriter, r *http.Request, redirectURI, code, state string) {
	if redirectURI != "" {
		if u, err := url.Parse(redirectURI); err == nil && u.IsAbs() {
			q := u.Query()
			q.Set("error", code)
			if state != "" {
				q.Set("state", state)
			}
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
	}
	writeError(w, http.StatusBadRequest, "oauth authorize failed: "+code)
}

// handleOAuthAuthorize serves GET /oauth/authorize (Tier 2): the
// operator's gateway key approves the grant, and a single-use
// authorization code is delivered by 302 redirect to the
// client's registered redirect_uri (?code=&state=). The granted
// scopes must be a subset of the operator key's scopes, and the
// code_challenge_method must be S256 — "plain" is refused.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	if q.Get("response_type") != "code" {
		oauthAuthorizeError(w, r, redirectURI, "unsupported_response_type", state)
		return
	}
	clientID := q.Get("client_id")
	if clientID == "" {
		oauthAuthorizeError(w, r, redirectURI, "invalid_request", state)
		return
	}
	iss, err := s.oauthIssuer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oauth unavailable: "+err.Error())
		return
	}
	// The approving operator's scopes bound the grant: a key
	// that cannot call admin routes cannot mint a token that
	// can. A verifier that cannot name scopes refuses the grant
	// rather than guessing.
	sv, ok := s.verifier.(ScopeVerifier)
	if !ok {
		writeError(w, http.StatusForbidden, "oauth authorize refused: operator scopes unknown")
		return
	}
	operatorScopes, ok := sv.KeyScopes(r.Context(), bearerKey(r))
	if !ok {
		writeError(w, http.StatusForbidden, "oauth authorize refused: operator scopes unknown")
		return
	}
	var scopes []string
	if raw := strings.TrimSpace(q.Get("scope")); raw != "" {
		scopes = strings.Fields(raw)
	}
	code, err := iss.Authorize(clientID, redirectURI, scopes, q.Get("code_challenge"), q.Get("code_challenge_method"), s.callerKeyID(r), operatorScopes)
	if err != nil {
		oauthAuthorizeError(w, r, redirectURI, "invalid_request", state)
		return
	}
	u, err := url.Parse(redirectURI)
	if err != nil || !u.IsAbs() {
		writeError(w, http.StatusBadRequest, "oauth authorize failed: redirect_uri is not an absolute URL")
		return
	}
	qq := u.Query()
	qq.Set("code", code)
	if state != "" {
		qq.Set("state", state)
	}
	u.RawQuery = qq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// oauthTokenParams reads token-endpoint parameters from a
// form-encoded body (RFC 6749), a JSON body, or HTTP Basic auth
// (client_id:client_secret, RFC 6749 §2.3.1). Form wins over
// JSON when both are present; Basic auth fills client_id /
// client_secret only when the body does not name them.
func oauthTokenParams(r *http.Request) map[string]string {
	out := map[string]string{}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err == nil {
			for k, raw := range body {
				var s string
				if err := json.Unmarshal(raw, &s); err == nil {
					out[k] = s
				}
			}
		}
		return out
	}
	if err := r.ParseForm(); err == nil {
		for k, vs := range r.Form {
			if len(vs) > 0 {
				out[k] = vs[0]
			}
		}
	}
	if id, secret, ok := r.BasicAuth(); ok {
		if out["client_id"] == "" {
			out["client_id"] = id
		}
		if out["client_secret"] == "" {
			out["client_secret"] = secret
		}
	}
	return out
}

func oauthTokenError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

// handleOAuthToken serves POST /oauth/token (AuthExempt — it is
// the public token endpoint; client authentication is the PKCE
// verifier and, for confidential clients, the client secret).
// Grants: authorization_code (code + code_verifier) and
// refresh_token (rotating). Answers RFC 6749 §5.1 on success and
// §5.2 error bodies on failure.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	iss, err := s.oauthIssuer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oauth unavailable: "+err.Error())
		return
	}
	p := oauthTokenParams(r)
	switch p["grant_type"] {
	case "authorization_code":
		if p["code"] == "" || p["client_id"] == "" || p["redirect_uri"] == "" || p["code_verifier"] == "" {
			oauthTokenError(w, "invalid_request", "authorization_code requires code, client_id, redirect_uri and code_verifier")
			return
		}
		resp, err := iss.ExchangeCode(p["code"], p["client_id"], p["redirect_uri"], p["code_verifier"], p["client_secret"])
		if err != nil {
			oauthTokenError(w, "invalid_grant", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case "refresh_token":
		if p["refresh_token"] == "" || p["client_id"] == "" {
			oauthTokenError(w, "invalid_request", "refresh_token requires refresh_token and client_id")
			return
		}
		resp, err := iss.RefreshToken(p["refresh_token"], p["client_id"], p["client_secret"])
		if err != nil {
			oauthTokenError(w, "invalid_grant", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case "":
		oauthTokenError(w, "invalid_request", "grant_type is required")
	default:
		oauthTokenError(w, "unsupported_grant_type", "grant_type "+p["grant_type"]+" is not supported")
	}
}

// handleOAuthRevoke serves POST /oauth/revoke (AuthExempt, RFC
// 7009): revokes a refresh token or a JWT access token. It
// always answers 200 — even for a token that was never issued —
// so the endpoint never leaks whether a token existed.
func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	iss, err := s.oauthIssuer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "oauth unavailable: "+err.Error())
		return
	}
	p := oauthTokenParams(r)
	if p["token"] == "" {
		oauthTokenError(w, "invalid_request", "token is required")
		return
	}
	_ = iss.Revoke(p["token"])
	writeJSON(w, http.StatusOK, map[string]any{})
}
