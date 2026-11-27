package webexec

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// WebSurface is the static per-provider description of one catalog
// WebCookie entry's web surface: where the web UI lives, what its
// session-chat shape is, whether a temporary-chat (no-history) surface
// exists, and — critically — whether any of that is officially
// DOCUMENTED.
//
// Honesty rule for this table: EndpointDocumented is true ONLY when the
// provider itself publishes a cookie-session chat API. Community /
// reverse-engineering observations are recorded as facts with their
// source URL (a community blog or GitHub project is NOT a provider
// doc), and the endpoint is left EMPTY for them — the executor then
// runs against the mock-surface contract and refuses live URLs with
// ErrEndpointUndocumented until someone verifies the real shape. No
// invented URLs anywhere in this file.
type WebSurface struct {
	CatalogID string
	Name      string
	WebOrigin string // the provider's real web-UI origin (from the catalog entry's Website)

	// EndpointDocumented is true only for provider-published APIs.
	EndpointDocumented bool
	// ChatEndpoint is the documented (or community-observed, when
	// EndpointDocumented is false AND CommunityEndpoint is true) path,
	// recorded as a FACT for future live wiring. Empty when nothing
	// beyond the web UI is known. The executor never sends to a live
	// origin with an empty ChatEndpoint — it refuses (honest TODO).
	ChatEndpoint string
	// CommunityEndpoint marks ChatEndpoint as a community observation
	// rather than a provider doc.
	CommunityEndpoint bool
	// EndpointSource is where the endpoint fact came from ("" when
	// nothing is known).
	EndpointSource string
	// RequestShape / ResponseShape describe the wire shape as far as
	// it is known ("" when unknown).
	RequestShape  string
	ResponseShape string
	// SessionCookieNames are the session-carrying cookie names observed
	// in public references (informational; the executor replays whatever
	// the operator's captured state contains).
	SessionCookieNames []string
	// TemporaryChat is true when the provider's web UI offers a
	// temporary-chat (no history) surface; TemporaryChatNote explains.
	TemporaryChat     bool
	TemporaryChatNote string
	// EndpointNote carries the TODO-and-reason for surfaces with no
	// known endpoint (the honest per-provider gap).
	EndpointNote string
	// ModelIDs are the model names the executor advertises (kept to the
	// catalog ID unless a real model name is documented).
	ModelIDs []string
}

// mockContractChatPath is the endpoint the executor drives on a MOCK
// web surface (see mock_surface_test.go): the fixed contract every
// mock implements — POST {origin}/api/chat with
// {"messages":[{"role","content"}],"model","temporary"} answered with
// {"content":"..."}; 401 on a missing/stale session cookie, 429 for an
// exhausted quota, honoring Retry-After echo.
const mockContractChatPath = "/api/chat"

// surfaces is the registry of all 37 catalog WebCookie entries. The
// list is pinned by TestSurfaceRegistryCoversCatalogIDs against the
// catalog, so an ID can never drift or disappear silently. IDs are
// sorted; NEVER remove an entry.
var surfaces = map[string]WebSurface{
	"adapta-web": {
		CatalogID:    "adapta-web",
		Name:         "Adapta.org (Adapta One Web)",
		WebOrigin:    "https://agent.adapta.one",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify the live UI's request shape from an operator capture before any live wiring)",
	},
	"adobe-firefly": {
		CatalogID:    "adobe-firefly",
		Name:         "Adobe Firefly (Image/Video)",
		WebOrigin:    "https://firefly.adobe.com",
		EndpointNote: "Firefly's official API is key-based (firefly.adobe.com API portal) and image/video generation, not cookie-session chat; the web UI's internal generation endpoint is undocumented (TODO: out of scope for a chat executor until an operator capture defines the shape)",
	},
	"blackbox-web": {
		CatalogID:    "blackbox-web",
		Name:         "Blackbox Web (Subscription)",
		WebOrigin:    "https://app.blackbox.ai",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"chatgpt-web": {
		CatalogID: "chatgpt-web",
		Name:      "ChatGPT Web (Clean Room)",
		WebOrigin: "https://chatgpt.com",
		// Community observation (NOT a provider doc): the web UI POSTs
		// to /backend-api/conversation and streams SSE; auth rides the
		// session JWT from /api/auth/session plus session cookies, and
		// the endpoint additionally demands CSRF/proof tokens.
		CommunityEndpoint:  true,
		ChatEndpoint:       "/backend-api/conversation",
		EndpointSource:     "https://github.com/Tryhard0909/ChatGPT-Reverse (community, observational)",
		RequestShape:       `POST JSON {"action":"next","messages":[{"id":"<uuid>","role":"user","content":{"content_type":"text","parts":["<prompt>"]}}],"model":"<model>"}`,
		ResponseShape:      "text/event-stream (data: {\"message\":{\"content\":{\"parts\":[...]}}})",
		SessionCookieNames: []string{"__Secure-next-auth.session-token", "__Host-next-auth.csrf-token"},
		TemporaryChat:      true,
		TemporaryChatNote:  "the ChatGPT web UI has an explicit temporary-chat mode (no conversation persisted); the executor passes its temporary flag through to the surface contract",
		EndpointNote:       "community-observed endpoint recorded as a fact; live wiring stays refused until the CSRF/proof-token handshake is implemented against a verified capture (the executor drives mock surfaces only today)",
	},
	"chatgpt-web-codex": {
		CatalogID:          "chatgpt-web-codex",
		Name:               "ChatGPT Web (Codex)",
		WebOrigin:          "https://chatgpt.com",
		CommunityEndpoint:  true,
		ChatEndpoint:       "/backend-api/conversation",
		EndpointSource:     "https://github.com/Tryhard0909/ChatGPT-Reverse (community, observational; codex model selected via payload \"model\")",
		RequestShape:       "same conversation POST as chatgpt-web with a codex-capable model id in the payload",
		ResponseShape:      "text/event-stream",
		SessionCookieNames: []string{"__Secure-next-auth.session-token"},
		EndpointNote:       "same community-observed surface as chatgpt-web; same refusal rule until verified live",
	},
	"chatplayground": {
		CatalogID:    "chatplayground",
		Name:         "ChatPlayground",
		WebOrigin:    "https://web.chatplayground.ai",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"claude-web": {
		CatalogID: "claude-web",
		Name:      "Claude Web",
		WebOrigin: "https://claude.ai",
		// Community observation (NOT a provider doc): completion rides
		// POST /api/organizations/{org_id}/chat_conversations/{conv_id}/
		// completion with the sessionKey cookie.
		CommunityEndpoint:  true,
		ChatEndpoint:       "/api/organizations/{org_id}/chat_conversations/{conv_id}/completion",
		EndpointSource:     "https://github.com/meera-pandey/reverse-claude (community, HAR-observational)",
		RequestShape:       `POST JSON {"prompt":"<prompt>","attachments":[]} (plus org/conv UUIDs in the path)`,
		ResponseShape:      "text/event-stream",
		SessionCookieNames: []string{"sessionKey"},
		EndpointNote:       "community-observed endpoint recorded as a fact; live wiring stays refused (org_id/conv_id provisioning flow unverified) — mock-surface contract only",
	},
	"conol-web": {
		CatalogID:    "conol-web",
		Name:         "Conol (Unofficial/Experimental)",
		WebOrigin:    "https://conol.ai",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"copilot-m365-web": {
		CatalogID:    "copilot-m365-web",
		Name:         "Microsoft 365 Copilot",
		WebOrigin:    "https://m365.cloud.microsoft/chat",
		EndpointNote: "M365 Copilot's internal conversation endpoint is undocumented and tied to the tenant session; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"copilot-web": {
		CatalogID:    "copilot-web",
		Name:         "Microsoft Copilot Web",
		WebOrigin:    "https://copilot.microsoft.com",
		EndpointNote: "the Copilot web UI's conversation endpoint is undocumented (consumer surface changed repeatedly); executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"deepseek-web": {
		CatalogID:    "deepseek-web",
		Name:         "DeepSeek Web",
		WebOrigin:    "https://chat.deepseek.com",
		EndpointNote: "DeepSeek's official API is key-based (platform.deepseek.com); the chat.deepseek.com web endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"doubao-web": {
		CatalogID:    "doubao-web",
		Name:         "Dola Web (ByteDance)",
		WebOrigin:    "https://www.dola.com",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"gemini-web": {
		CatalogID:         "gemini-web",
		Name:              "Gemini Web (Free)",
		WebOrigin:         "https://gemini.google.com",
		EndpointNote:      "gemini.google.com has no cookie-session API at all (the official API is key-based via generativelanguage.googleapis.com and is a different catalog entry); executor implemented against the mock-surface contract only (TODO: verify live shape)",
		TemporaryChat:     true,
		TemporaryChatNote: "the Gemini web UI offers temporary chats (no activity saved); the executor passes its temporary flag through to the surface contract",
	},
	"grok-web": {
		CatalogID:    "grok-web",
		Name:         "Grok Web (Subscription)",
		WebOrigin:    "https://grok.com",
		EndpointNote: "the grok.com web conversation endpoint is undocumented (xAI's official API is key-based, a different catalog entry); executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"huggingchat": {
		CatalogID: "huggingchat",
		Name:      "HuggingChat (Free)",
		WebOrigin: "https://huggingface.co/chat",
		// The OFFICIAL docs (https://huggingface.co/docs/chat-ui)
		// describe only the OpenAI-compatible, API-KEY surface
		// (router.huggingface.co/v1) — which is a different catalog
		// entry. The huggingface.co/chat cookie-session conversation
		// endpoint is NOT documented.
		EndpointDocumented: false,
		EndpointNote:       "official Chat UI docs cover only the key-based OpenAI-compatible router surface; the web UI's conversation endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"hyperagent": {
		CatalogID:    "hyperagent",
		Name:         "HyperAgent (Unofficial/Experimental)",
		WebOrigin:    "https://hyperagent.com",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"inner-ai": {
		CatalogID:    "inner-ai",
		Name:         "Inner.ai (Subscription)",
		WebOrigin:    "https://app.innerai.com",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"kimi-web": {
		CatalogID:    "kimi-web",
		Name:         "Kimi Web",
		WebOrigin:    "https://www.kimi.ai",
		EndpointNote: "Moonshot's official API is key-based (platform.moonshot.ai); the kimi web endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"lmarena": {
		CatalogID:    "lmarena",
		Name:         "Arena (Free)",
		WebOrigin:    "https://arena.ai",
		EndpointNote: "LMArena's web UI conversation endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"maxai": {
		CatalogID:    "maxai",
		Name:         "MaxAI",
		WebOrigin:    "https://www.maxai.co",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"muse-spark-web": {
		CatalogID:    "muse-spark-web",
		Name:         "Muse Spark Web (Meta AI)",
		WebOrigin:    "https://www.meta.ai",
		EndpointNote: "the meta.ai web conversation endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"notion-web": {
		CatalogID:    "notion-web",
		Name:         "Notion AI Web (Unofficial/Experimental)",
		WebOrigin:    "https://www.notion.so",
		EndpointNote: "Notion's official API covers pages/databases, not Notion-AI chat sessions; the web UI's AI endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"notrack-web": {
		CatalogID:    "notrack-web",
		Name:         "NoTrack Web (Free)",
		WebOrigin:    "https://notrack.ai",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"perplexity-web": {
		CatalogID:    "perplexity-web",
		Name:         "Perplexity Web (Pro/Max)",
		WebOrigin:    "https://www.perplexity.ai",
		EndpointNote: "Perplexity's official API is key-based (docs.perplexity.ai) and a different catalog entry; the web UI's conversation endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"poe-web": {
		CatalogID: "poe-web",
		Name:      "Poe Web (Subscription)",
		WebOrigin: "https://poe.com",
		// Community observation (NOT a provider doc): chat rides GraphQL
		// (gql_POST) authed by the p-b cookie plus a poe-formkey header.
		CommunityEndpoint:  true,
		ChatEndpoint:       "/api/gql_POST",
		EndpointSource:     "https://github.com/ading2210/poe-api (community, observational)",
		RequestShape:       `GraphQL mutation sendMessage with bot codename + text; poe-formkey header required`,
		ResponseShape:      "GraphQL JSON (message edges carry text)",
		SessionCookieNames: []string{"p-b", "p-lat"},
		EndpointNote:       "community-observed endpoint recorded as a fact; live wiring stays refused (formkey derivation and Cloudflare binding unverified) — mock-surface contract only",
	},
	"promptql": {
		CatalogID:    "promptql",
		Name:         "PromptQL (Unofficial/Experimental)",
		WebOrigin:    "https://prompt.ql.app",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"syntx": {
		CatalogID:    "syntx",
		Name:         "SYNTX.ai (Unofficial/Experimental)",
		WebOrigin:    "https://syntx.ai",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"t3-web": {
		CatalogID:    "t3-web",
		Name:         "t3.chat (Pro/Free)",
		WebOrigin:    "https://t3.chat",
		EndpointNote: "the t3.chat web conversation endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"tencent-aistudio-web": {
		CatalogID:    "tencent-aistudio-web",
		Name:         "Tencent AI Studio (Free)",
		WebOrigin:    "https://aistudio.tencent.ai",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"tinycms-web": {
		CatalogID:    "tinycms-web",
		Name:         "TinyCMS Web (Free/Sub)",
		WebOrigin:    "https://site.tinycms.xyz",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"twinmind": {
		CatalogID:    "twinmind",
		Name:         "Twinmind (Unofficial/Experimental)",
		WebOrigin:    "https://app.twinmind.com",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"uc": {
		CatalogID:    "uc",
		Name:         "UC (uncensored.com)",
		WebOrigin:    "https://uncensored.com",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"v0-vercel-web": {
		CatalogID:    "v0-vercel-web",
		Name:         "v0 Vercel Web (Code Gen)",
		WebOrigin:    "https://v0.dev",
		EndpointNote: "the v0.dev web generation endpoint is undocumented (Vercel's official v0 API is key-based and a different surface); executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"venice-web": {
		CatalogID:    "venice-web",
		Name:         "Venice Web (Privacy)",
		WebOrigin:    "https://venice.ai",
		EndpointNote: "Venice's official API is key-based (docs.venice.ai) and a different catalog entry; the web UI's conversation endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"yuanbao-web": {
		CatalogID:    "yuanbao-web",
		Name:         "Tencent Yuanbao (Free)",
		WebOrigin:    "https://yuanbao.tencent.com",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"zai-web": {
		CatalogID:    "zai-web",
		Name:         "Z.ai Web",
		WebOrigin:    "https://chat.z.ai",
		EndpointNote: "Z.ai's official API is key-based (api.z.ai) and a different catalog entry; the chat.z.ai web endpoint is undocumented; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
	"zenmux-free": {
		CatalogID:    "zenmux-free",
		Name:         "ZenMux Free (Web)",
		WebOrigin:    "https://zenmux.ai",
		EndpointNote: "no provider documentation of a cookie-session chat API; executor implemented against the mock-surface contract only (TODO: verify live shape)",
	},
}

// surfaceByID returns the surface descriptor for a catalog ID. ModelIDs
// default to the catalog ID itself unless a real model name is recorded.
func surfaceByID(catalogID string) (WebSurface, bool) {
	s, ok := surfaces[catalogID]
	if ok && len(s.ModelIDs) == 0 {
		s.ModelIDs = []string{catalogID}
	}
	return s, ok
}

// CatalogIDsWithWebExec returns all 37 supported catalog IDs, sorted.
func CatalogIDsWithWebExec() []string {
	ids := make([]string, 0, len(surfaces))
	for id := range surfaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ---------------------------------------------------------------------------
// Dispatch URLs: how a registry add carries its catalog identity
// ---------------------------------------------------------------------------

// The provider registry's ClientFactory signature is
// (baseURL, apiKey, authHeaders, wireFormat) — it does NOT name the
// catalog provider being built. A web-cookie add through the ToS gate
// (internal/provider/tos_gate.go) resolves auth itself and reaches the
// factory with the operator's cookie string and no ID, so the factory
// cannot tell WHICH of the 37 surfaces to serve. The bridge solves
// this with a dispatch base URL: the operator (or the dashboard wiring)
// adds the provider with CatalogAddOptions.BaseURL set to
//
//	<origin>/webexec/<catalog-id>
//
// and the factory parses the ID back out. The path segment is a
// dispatch key only — the executor never sends a request to it; it
// POSTs to the surface's chat endpoint on the origin (mock contract on
// loopback origins, documented-endpoint-or-refusal elsewhere).

// DispatchURLFor renders the dispatch base URL for a catalog ID against
// a surface origin (a mock surface URL in tests, a real origin in
// production wiring).
func DispatchURLFor(origin, catalogID string) (string, error) {
	if _, err := Surface(catalogID); err != nil {
		return "", err
	}
	if _, err := url.Parse(origin); err != nil {
		return "", fmt.Errorf("webexec: dispatch origin %q: %w", origin, err)
	}
	return strings.TrimSuffix(origin, "/") + "/webexec/" + catalogID, nil
}

// SurfaceIDForDispatchURL parses a dispatch base URL back to its
// catalog ID. ok is false for anything that is not a dispatch URL.
func SurfaceIDForDispatchURL(baseURL string) (string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	const marker = "/webexec/"
	if !strings.Contains(u.Path, marker) {
		return "", false
	}
	id := strings.TrimPrefix(u.Path[strings.Index(u.Path, marker):], marker)
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	if _, err := Surface(id); err != nil {
		return "", false
	}
	return id, true
}

// DispatchOrigin returns the scheme://host origin of a dispatch URL —
// where the executor actually POSTs.
func DispatchOrigin(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// IsLoopbackOrigin reports whether an origin targets the local machine
// (a mock web surface in tests / simulated wiring). Only loopback
// origins may run the mock-surface contract: a live origin always goes
// through the documented-endpoint-or-refusal path.
func IsLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// Surface returns the descriptor for a catalog ID (ErrUnknownSurface
// wrapped when the ID is not one of the 37 WebCookie entries).
func Surface(catalogID string) (WebSurface, error) {
	s, ok := surfaceByID(catalogID)
	if !ok {
		return WebSurface{}, fmt.Errorf("webexec: %w: %q", ErrUnknownSurface, catalogID)
	}
	return s, nil
}
