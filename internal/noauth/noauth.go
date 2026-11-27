// Package noauth holds the no-auth connection mechanisms for the ten
// CategoryNoAuth catalog entries (A3 item 5).
//
// The catalog records FACTS (base URLs, auth kinds, free-tier notes);
// this package records the MECHANISM behind each no-auth entry — how a
// keyless request actually reaches the provider — and every fact here
// was verified against the provider's own documentation (each
// Mechanism carries its DocURL). Nothing is invented: where a
// provider's own docs give NO usable anonymous endpoint, the
// mechanism is KindNone and the catalog row stays nil with its honest
// UnroutableReason.
//
// Three mechanism kinds exist:
//
//   - KindAnonymous: the provider documents a keyless HTTP endpoint.
//     The request carries no credential (or a documented shared
//     anonymous key) and the provider's own documented per-IP rate
//     limit is enforced client-side before dialing.
//
//   - KindCLIBridge: the provider's own CLI owns the credentials
//     (`auggie auth login`, `codex app-server`, `devin auth login`,
//     `mcode login`, ZCode's Z.ai sign-in). The router spawns the CLI
//     as a subprocess and never sees or stores any credential; there
//     is no HTTP base URL to fill and the catalog row stays nil.
//
//   - KindNone: the provider's docs give no usable anonymous endpoint
//     (web-only surfaces, reverse-engineered protocols, or a claimed
//     limit the provider's own docs do not state). Catalogued for
//     completeness, never routed.
//
// The empty-key allowance the router applies for CategoryNoAuth
// entries (DefaultAuthKind() == AuthNone, so allowEmptyKey is set) is
// sound only when a mechanism exists here: AllowEmptyKey answers
// that question per provider, so wiring can fail closed.
package noauth

import (
	"strconv"
	"time"
)

// Kind is the connection mechanism behind a no-auth catalog entry.
type Kind string

const (
	// KindAnonymous is a provider-documented keyless HTTP endpoint.
	KindAnonymous Kind = "anonymous-endpoint"

	// KindCLIBridge spawns the provider's own CLI, which owns its
	// credentials; the router never sees them.
	KindCLIBridge Kind = "cli-bridge"

	// KindNone means the provider's own docs give no usable
	// anonymous endpoint. The catalog row stays nil; the entry is
	// never a routing target.
	KindNone Kind = "none"
)

// Limit is one documented per-IP request budget (a fixed window).
type Limit struct {
	// Requests is how many requests the window allows.
	Requests int
	// Window is the fixed window length.
	Window time.Duration
}

// String renders the limit in the provider's own terms, e.g. "5 per second".
func (l Limit) String() string {
	unit := l.Window.String()
	if l.Window == time.Second {
		unit = "second"
	} else if l.Window == time.Minute {
		unit = "minute"
	} else if l.Window == time.Hour {
		unit = "hour"
	}
	return strconv.Itoa(l.Requests) + " per " + unit
}

// Mechanism describes how one no-auth catalog entry connects.
type Mechanism struct {
	// ID is the catalog provider ID this mechanism serves.
	ID string
	// Kind is the mechanism type.
	Kind Kind
	// DocURL is the provider's own documentation the mechanism was
	// verified against ("" only for KindNone, where no doc gives an
	// endpoint — Reason carries the honest refusal).
	DocURL string
	// Reason states, in one or two sentences, why the mechanism is
	// what it is. For KindNone it is the honest reason the row
	// stays nil; it must never be empty.
	Reason string

	// Anonymous endpoints (KindAnonymous only):
	// BaseURLs are the provider-documented keyless API bases.
	BaseURLs []string
	// AnonymousKey is a documented shared anonymous credential sent
	// instead of a user key ("" = send no credential at all).
	AnonymousKey string
	// Limits are the provider's own documented per-IP request
	// budgets, enforced client-side before dialing. Nil means the
	// provider documents no numeric limit (the Reason says what the
	// provider does document, e.g. abuse throttling).
	Limits []Limit
	// LimitNote records how the provider enforces limits when no
	// numeric budget is published ("429 TooManyPrompts", ToS
	// throttling, queue priority).
	LimitNote string

	// CLI bridge (KindCLIBridge only):
	// Command is the provider CLI's non-interactive argv as
	// documented, with a "{prompt}" placeholder for the user prompt
	// (nil for codex-app-server: the bridge runs a JSON-RPC
	// app-server process rather than a one-shot command).
	Command []string
	// AuthNote says how the CLI obtains its credentials.
	AuthNote string
}

// RunnableCommand reports whether the bridge has a documented one-shot
// argv (codex app-server is a long-lived JSON-RPC process instead).
func (m Mechanism) RunnableCommand() bool { return m.Kind == KindCLIBridge && len(m.Command) > 0 }

// registry holds one mechanism per no-auth catalog entry, in catalog
// order. Every entry cites the provider's own docs in DocURL/Reason.
//
// Verified 2026-11-27 against each provider's own documentation:
//
//   - aihorde: the AI Horde README documents the anonymous key
//     "0000000000" ("no need to register") and the OpenAI-compatible
//     front's own OpenAPI spec states a request without a credential
//     header uses the anonymous key; chat_requests.md documents the
//     per-IP submit limits (5/second and 300/minute, 429 over limit).
//
//   - uncloseai: the site's own docs list the free endpoints
//     (hermes/qwen.ai.unturf.com/v1) and state the endpoints need no
//     key; the Terms of Use document IP-based throttling with no
//     numeric budget.
//
//   - auggie: Augment's CLI docs require `auggie auth login`; the
//     router spawns `auggie --print` and never sees the token.
//
//   - codex-app-server: OpenAI's app-server docs run `codex
//     app-server` as a JSON-RPC process authenticated with the
//     user's ChatGPT-plan token (sign-in with ChatGPT); no keyless
//     HTTP endpoint exists.
//
//   - devin-cli-agentic: Devin's docs require `devin auth login`
//     (or DEVIN_API_KEY); the CLI exposes an ACP server
//     (`devin acp`) the router can spawn.
//
//   - minimax-code: MiniMax's docs require an M Plan credential
//     (`mcode login` or a MiniMax API key); no anonymous endpoint.
//
//   - zcode: ZCode uses the Z.ai sign-in or a GLM Coding Plan API
//     key; no anonymous endpoint.
//
//   - duckduckgo-web: Duck.ai has no public API — the chat protocol
//     is undocumented and reverse-engineered (x-vqd handshake); no
//     provider documentation exists to verify an endpoint against.
//
//   - cloudflare-playground: Cloudflare's own docs describe the
//     LLM Playground as an unauthenticated BROWSER surface ("does
//     not require any setup or authentication") and document no HTTP
//     API for it; chat runs over an internal browser-only protocol.
//
//   - veoaifree-web: veoaifree.com's own pages claim unlimited free
//     web generation and document no API and no per-IP request
//     budget; the widely-quoted "6 requests/hour" figure appears in
//     no provider documentation and is not recorded here.
var registry = []Mechanism{
	{
		ID:           "aihorde",
		Kind:         KindAnonymous,
		DocURL:       "https://github.com/Haidra-Org/AI-Horde/blob/main/README.md",
		Reason:       "AI Horde's README documents anonymous use with the shared key 0000000000 (\"no need to register\", lowest queue priority), and the OpenAI-compatible front's OpenAPI spec states a request without a credential header uses the anonymous key.",
		BaseURLs:     []string{"https://oai.aihorde.net/v1"},
		AnonymousKey: "0000000000",
		Limits: []Limit{
			{Requests: 5, Window: time.Second},
			{Requests: 300, Window: time.Minute},
		},
		LimitNote: "Per-IP chat-submit limits documented in AI-Horde docs/reference/chat_requests.md: 5/second and 300/minute (flask-limiter), a request over a limit gets 429; anonymous requests additionally sit at the lowest queue priority.",
	},
	{
		ID:        "uncloseai",
		Kind:      KindAnonymous,
		DocURL:    "https://uncloseai.com/",
		Reason:    "uncloseai's own pages list the free chat endpoints (hermes.ai.unturf.com/v1, qwen.ai.unturf.com/v1) and the CLI docs state \"The free uncloseai inference endpoints above need no key\"; the OpenAI-compatible surface takes no credential.",
		BaseURLs:  []string{"https://hermes.ai.unturf.com/v1", "https://qwen.ai.unturf.com/v1"},
		Limits:    nil,
		LimitNote: "No numeric limit published: the Terms of Use (https://uncloseai.com/terms-of-use.html) document IP-based rate limiting — excessive requests from a single IP may be automatically throttled or temporarily blocked.",
	},
	{
		ID:       "auggie",
		Kind:     KindCLIBridge,
		DocURL:   "https://docs.augmentcode.com/cli/setup-auggie/authentication",
		Reason:   "Augment's docs require an authenticated session: `auggie auth login` (or AUGMENT_SESSION_AUTH / --augment-session-json) before any use — there is no anonymous endpoint.",
		Command:  []string{"auggie", "--print", "{prompt}"},
		AuthNote: "The router spawns `auggie --print <prompt>` as a subprocess; the CLI owns the session token obtained by `auggie auth login` and the router never sees or stores it.",
	},
	{
		ID:       "codex-app-server",
		Kind:     KindCLIBridge,
		DocURL:   "https://developers.openai.com/codex/app-server",
		Reason:   "OpenAI's app-server docs run `codex app-server` as a local JSON-RPC 2.0 process (stdio/WS) authenticated with the user's ChatGPT-plan OAuth token (sign-in with ChatGPT) — there is no keyless HTTP endpoint.",
		Command:  nil, // long-lived JSON-RPC process, not a one-shot argv
		AuthNote: "The bridge spawns `codex app-server` over stdio and speaks the documented JSON-RPC protocol; credentials stay inside the Codex process (ChatGPT-plan token via sign-in with ChatGPT).",
	},
	{
		ID:       "devin-cli-agentic",
		Kind:     KindCLIBridge,
		DocURL:   "https://docs.devin.ai/cli/reference/commands",
		Reason:   "Devin's docs require `devin auth login` (or a manually supplied token) before use; the CLI exposes an ACP server (`devin acp`) whose credentials come from that login — no anonymous endpoint.",
		Command:  []string{"devin", "acp"},
		AuthNote: "The bridge spawns `devin acp` (Agent Client Protocol); credentials are read from what `devin auth login` stored and the router never sees them.",
	},
	{
		ID:       "minimax-code",
		Kind:     KindCLIBridge,
		DocURL:   "https://code.minimax.io/docs/cli/features",
		Reason:   "MiniMax Code's docs gate the CLI on a MiniMax credential: `mcode login` (MiniMax OAuth) or a MiniMax API key (MCODE_PROVIDER_API_KEY / Token Plan) — there is no anonymous endpoint.",
		Command:  []string{"mcode"},
		AuthNote: "The bridge spawns the `mcode` CLI; the credential is the MiniMax account login or the user's own MiniMax API key and the router never sees it.",
	},
	{
		ID:       "zcode",
		Kind:     KindCLIBridge,
		DocURL:   "https://zcode.z.ai/en/docs/configuration",
		Reason:   "ZCode's docs connect GLM models through the Z.ai sign-in or a GLM Coding Plan API key (Coding-only endpoint https://api.z.ai/api/coding/paas/v4) — there is no anonymous endpoint; the keyed surface is the CLI's own, not a router-held key.",
		Command:  []string{"zcode"},
		AuthNote: "The bridge spawns the `zcode` CLI; the GLM Coding Plan key or Z.ai account session stays inside the CLI's own config (~/.zcode/v2/provider_config.json).",
	},
	{
		ID:     "duckduckgo-web",
		Kind:   KindNone,
		Reason: "Duck.ai has no public API and DuckDuckGo documents none: chat runs over an undocumented browser protocol (x-vqd handshake). No provider documentation exists to verify an anonymous endpoint against, so the row stays nil.",
	},
	{
		ID:     "cloudflare-playground",
		Kind:   KindNone,
		Reason: "Cloudflare's own docs describe the Workers AI LLM Playground as an unauthenticated browser surface and document no HTTP API for it; chat runs over an internal browser-only protocol, so there is no documented anonymous endpoint to fill.",
		DocURL: "https://developers.cloudflare.com/workers-ai/llms-full.txt",
	},
	{
		ID:     "veoaifree-web",
		Kind:   KindNone,
		Reason: "veoaifree.com's own pages claim unlimited free web video generation and document no API and no per-IP request budget; the widely-quoted \"6 requests/hour per IP\" figure appears in no provider documentation and is not recorded here. The row stays nil.",
		DocURL: "https://veoaifree.com/",
	},
}

// For returns the mechanism for a no-auth catalog ID.
func For(id string) (Mechanism, bool) {
	for _, m := range registry {
		if m.ID == id {
			return m, true
		}
	}
	return Mechanism{}, false
}

// All returns every documented mechanism, catalog order.
func All() []Mechanism { return append([]Mechanism(nil), registry...) }

// AllowEmptyKey reports whether the no-auth catalog entry's empty-key
// allowance (the router sets allowEmptyKey when the resolved auth kind
// is AuthNone) is backed by a documented mechanism: a keyless endpoint
// the provider documents, or a CLI the user authenticates locally.
// KindNone entries must never pass an empty key to the network — the
// router refuses them upstream of this package via the catalog's
// ToS/avoid handling, and this answer fails closed for them.
func AllowEmptyKey(id string) bool {
	m, ok := For(id)
	if !ok {
		return false
	}
	return m.Kind == KindAnonymous || m.Kind == KindCLIBridge
}
