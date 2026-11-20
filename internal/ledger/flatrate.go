// Package ledger — flat-rate (subscription) provider classification
// (G16, Area 11 "Flat-rate / subscription $0 treatment").
//
// Some providers bill at a flat rate (a subscription or a coding
// plan), not per token — but they still carry per-token pricing
// rows (used for pre-flight estimates), so cost analytics computed
// from those rows shows an inflated dollar amount that does not
// match the user's actual bill. For those providers analytics
// should show $0 instead — opt-in via the flatRateAsZero option,
// exactly like OmniRoute's flatRateAsZero (src/lib/usage/
// costCalculator.ts + flatRateProviders.ts, issue #5552).
//
// The list below follows OmniRoute's classification, read from the
// reference clone (study only — this code is our own):
//
//   - The explicit subscription / coding-plan provider ids in
//     FLAT_RATE_SUBSCRIPTION_PROVIDER_IDS (Minimax Coding, Kimi
//     Coding, Muse Code, Xiaomi MiMo, Alibaba/Qwen token plans,
//     GLM Coding, Claude Code, OpenCode Go), including OmniRoute's
//     deliberate EXCLUSIONS: `codex`/`cx` (OmniRoute actively
//     tracks Codex token cost — Fast-tier multipliers and GPT-5.x
//     pricing — and Codex can be a metered API account), `byteplus`
//     (metered ModelArk host), `minimax-cn` (the metered Minimax
//     China API, distinct from the `minimax` Coding plan),
//     `glm-thinking` (metered tier, distinct from the `glm`
//     Coding plan) and `anthropic` (the metered Anthropic API,
//     distinct from the `claude`/`cc` Claude Code plan). We match
//     those exclusions: zeroing any of them would hide real cost.
//   - Every web-cookie provider (every web session is
//     subscription-backed): the WEB_COOKIE_PROVIDERS ids, read
//     from src/shared/constants/providers/web-cookie*.ts.
//
// Matching is case-insensitive on a trimmed provider id, like
// OmniRoute's isFlatRateProvider. The classification never
// rewrites a price and never touches quota / rate-limit paths:
// it only zeroes the DISPLAY/ledger cost when the caller opted
// in (Ledger.FlatRateAsZero / CostOptions.FlatRateAsZero).
// Per-request cost ESTIMATION is unchanged.
package ledger

import "strings"

// flatRateSubscriptionProviderIDs is the explicit set of
// subscription / coding-plan provider ids whose identity IS a
// flat-rate plan (OmniRoute flatRateProviders.ts,
// FLAT_RATE_SUBSCRIPTION_PROVIDER_IDS).
var flatRateSubscriptionProviderIDs = map[string]bool{
	"minimax":               true, // "Minimax Coding" plan
	"kimi-coding":           true, // Kimi Coding plan (OAuth)
	"kimi-coding-apikey":    true, // Kimi Coding plan (API-key auth, still flat-rate)
	"muse-code":             true, // Muse Code subscription
	"xiaomi-mimo":           true, // Xiaomi MiMo plan
	"bailian-coding-plan":   true, // Alibaba Token Plan (legacy provider ID)
	"qwen-cloud-token-plan": true, // Qwen Cloud Token Plan
	"glm":                   true, // GLM Coding plan
	"glm-cn":                true, // GLM Coding (China) plan
	"claude":                true, // Claude Code plan (OAuth-only — a Claude Pro/Max subscription)
	"cc":                    true, // Claude Code plan (alias id — same connection)
	"opencode-go":           true, // OpenCode Go subscription (flat monthly fee)
}

// webCookieProviderIDs is every web-cookie provider id (OmniRoute
// WEB_COOKIE_PROVIDERS, src/shared/constants/providers/
// web-cookie.ts + web-cookie-chatplayground.ts +
// web-cookie-notrack.ts, 37 keys). Every web session is
// subscription-backed, so every id here is flat-rate.
//
// Alias policy (decided explicitly, G16 re-review): OmniRoute's
// own isFlatRateProvider is keys-only
// (hasOwnProperty(WEB_COOKIE_PROVIDERS, id)), but this map also
// includes every alias listed on those entries. In this router an
// alias is a live provider id — every one of the 36 aliases is a
// live Alias value in our catalog — so a session addressed by its
// alias is still subscription-backed and must read $0, not a
// per-token price. Three aliases need no separate entry because
// they equal their key ("huggingchat", "kimi-web",
// "v0-vercel-web"). One alias is deliberately EXCLUDED: "poe" is
// also the id of the metered Poe API provider (CategoryAPIKey,
// creator.poe.com), so including it would zero real metered cost;
// a "poe" session is priced per token, and only "poe-web" reads $0.
var webCookieProviderIDs = map[string]bool{
	"chatgpt-web":          true,
	"chatgpt-web-codex":    true,
	"cgpt-codex":           true, // alias of chatgpt-web-codex
	"grok-web":             true,
	"gw":                   true, // alias of grok-web
	"gemini-web":           true,
	"gweb":                 true, // alias of gemini-web
	"perplexity-web":       true,
	"pplx-web":             true, // alias of perplexity-web
	"blackbox-web":         true,
	"bb-web":               true, // alias of blackbox-web
	"muse-spark-web":       true,
	"ms-web":               true, // alias of muse-spark-web
	"claude-web":           true,
	"deepseek-web":         true,
	"ds-web":               true, // alias of deepseek-web
	"copilot-web":          true,
	"copilot-m365-web":     true,
	"t3-web":               true,
	"inner-ai":             true,
	"adapta-web":           true,
	"adp-web":              true, // alias of adapta-web
	"lmarena":              true,
	"yuanbao-web":          true,
	"conol-web":            true,
	"tencent-aistudio-web": true,
	"huggingchat":          true,
	"poe-web":              true,
	"venice-web":           true,
	"v0-vercel-web":        true,
	"kimi-web":             true,
	"doubao-web":           true,
	"zenmux-free":          true,
	"tinycms-web":          true,
	"zai-web":              true,
	"promptql":             true,
	"notion-web":           true,
	"chatplayground":       true,
	"notrack-web":          true,
	// Six WEB_COOKIE_PROVIDERS keys the first pass missed (G16
	// re-review catch) — all six are providers in our catalog.
	"adobe-firefly": true,
	"hyperagent":    true,
	"maxai":         true,
	"syntx":         true,
	"twinmind":      true,
	"uc":            true,
	// Aliases of web-cookie providers (see the policy in the
	// comment above the map). "poe" is deliberately absent: it
	// collides with the metered Poe API provider id.
	"cnl": true, "copilot": true, "cpl": true, "cw": true,
	"db": true, "firefly": true, "ha": true, "in-ai": true,
	"lma": true, "m365copilot": true, "mx": true, "ntw": true,
	"nw": true, "pql": true, "stx": true, "t3chat": true,
	"tasw": true, "tcw": true, "tm": true, "ucn": true,
	"ven": true, "ybw": true, "zmf": true, "zw": true,
}

// IsFlatRateProvider reports whether a provider bills at a flat
// rate (subscription / coding plan / cookie web session) rather
// than per token, so its per-token cost estimate should be
// surfaced as $0 in analytics when the flatRateAsZero opt-in is
// on. "" (and whitespace) is never flat-rate. The check is
// case-insensitive on a trimmed id.
func IsFlatRateProvider(providerID string) bool {
	id := strings.ToLower(strings.TrimSpace(providerID))
	if id == "" {
		return false
	}
	if flatRateSubscriptionProviderIDs[id] {
		return true
	}
	return webCookieProviderIDs[id]
}
