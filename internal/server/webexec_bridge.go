// Package server — web-exec bridge (A3 Item 4).
//
// This file is the ONLY server-side wiring for the web-cookie executor
// harness (internal/webexec). It deliberately does NOT touch the shared
// Server struct, the provider registry, or the OAuth connection store:
// the per-Server web-exec state lives in a side table keyed by the
// *Server pointer, created lazily on first use, exactly like the
// OAuth store's lazy vault handling in oauth_conn.go.
//
// What it wires:
//
//   - WebExecSessionStore: the vault-backed session-state store. When
//     the server has a data dir, the store seals every captured browser
//     session state with the per-data-dir Vault (Argon2id-derived key)
//     following the oauth_conn.go sealing conventions; with no data dir
//     the store is memory-only.
//   - WebExecExecutor: the ready-to-use executor for one of the 37
//     WebCookie catalog IDs over that store. Production wiring never
//     sets the mock-contract flag, so a provider whose web-session
//     endpoint is not officially documented REFUSES to execute live
//     (webexec.ErrEndpointUndocumented) — no invented URLs, ever.
//   - WebExecClientFactory: a provider.ClientFactory wrapper that
//     routes WebCookie catalog origins to a webexec-backed
//     provider.Client (401 on stale session state, 429 passthrough via
//     provider.RateLimitError) while every other provider keeps the
//     base factory untouched.
//
// Security posture (unchanged from the harness): no MITM, no root-CA,
// no stealth tricks. The operator hands over their own session state;
// stale state is a loud 401, quota is a real 429.
package server

import (
	"net/url"
	"strings"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/webexec"
)

// webexecBridge is the per-Server web-exec wiring state.
type webexecBridge struct {
	mu    sync.Mutex
	store *webexec.SessionStore
}

var (
	webexecBridgeMu sync.Mutex
	webexecBridges  = map[*Server]*webexecBridge{}
)

// webexecBridgeFor returns (creating on first use) the web-exec state
// for a Server. A hand-built &Server{} in tests gets a memory-only
// store, same as the OAuth store's nil-vault path.
func (s *Server) webexecBridgeFor() *webexecBridge {
	webexecBridgeMu.Lock()
	defer webexecBridgeMu.Unlock()
	b, ok := webexecBridges[s]
	if !ok {
		var vault *crypto.Vault
		if s.cfg != nil && s.cfg.DataDir != "" {
			if v, err := crypto.New(s.cfg.DataDir); err == nil {
				vault = v
			}
		}
		b = &webexecBridge{store: webexec.NewSessionStore(vault)}
		webexecBridges[s] = b
	}
	return b
}

// WebExecSessionStore returns the server's vault-backed web session
// store (one per Server instance; memory-only without a data dir).
func (s *Server) WebExecSessionStore() *webexec.SessionStore {
	return s.webexecBridgeFor().store
}

// WebExecExecutor returns the executor for one of the 37 WebCookie
// catalog IDs, wired to the server's session store. Production wiring:
// real origins, no mock contract — surfaces without an officially
// documented endpoint refuse to execute (honest TODO in the harness),
// which is exactly the behavior until a verified live capture exists.
func (s *Server) WebExecExecutor(catalogID string) (webexec.Executor, error) {
	return webexec.New(catalogID, s.WebExecSessionStore())
}

// WebExecImportSession stores an operator-provided session state for a
// catalog ID (the operator's own session, by their own choice). The
// state is validated and vault-sealed by the store.
func (s *Server) WebExecImportSession(catalogID string, state *webexec.SessionState) error {
	return s.WebExecSessionStore().Save(catalogID, state)
}

// WebExecClientFactory wraps a base provider.ClientFactory: requests
// whose base URL matches a WebCookie surface's web origin resolve to a
// webexec-backed provider.Client; everything else goes to the base
// factory untouched. A nil base falls back to the provider default
// (NewClient with plain auth headers).
func WebExecClientFactory(base provider.ClientFactory, store *webexec.SessionStore) provider.ClientFactory {
	return func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (provider.Client, error) {
		if id := webexecSurfaceIDForBaseURL(baseURL); id != "" {
			// The web origin match is by construction a WebCookie
			// catalog entry; the executor validates the ID again.
			return webexec.NewWebExecClient(id, store)
		}
		if base != nil {
			return base(baseURL, apiKey, authHeaders, wireFormat)
		}
		return provider.NewClient(baseURL, apiKey, provider.WithAuthHeaders(authHeaders), provider.WithWireFormat(wireFormat))
	}
}

// webexecSurfaceIDForBaseURL matches a base URL against the 37 WebCookie
// surfaces' web origins (scheme + host compare; origin paths like
// m365.cloud.microsoft/chat are normalized to the host). Returns ""
// when nothing matches.
func webexecSurfaceIDForBaseURL(baseURL string) string {
	host := ""
	if u, err := url.Parse(baseURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host == "" {
		return ""
	}
	for _, id := range webexec.CatalogIDsWithWebExec() {
		surface, err := webexec.Surface(id)
		if err != nil {
			continue
		}
		if u, err := url.Parse(surface.WebOrigin); err == nil && strings.EqualFold(u.Hostname(), host) {
			return id
		}
	}
	return ""
}
