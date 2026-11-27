package catalog

// UnroutableReason returns the honest, category-specific reason a
// catalog entry cannot be dialed, or "" when the entry is routable
// (a concrete base URL exists) or retired (retired entries have their
// own RetiredReason and are excluded from All()).
//
// Every entry without a routable base URL MUST produce a non-empty
// reason — "zero live entries are silently unroutable" is a pinned
// contract (TestEveryUnroutableEntryHasAReason). The reason is
// surfaced verbatim through /api/catalog (unroutable_reason).
func (p Provider) UnroutableReason() string {
	if p.Routable() {
		return ""
	}
	if len(p.BaseURLs) > 0 {
		// It has URLs but none dialable: they are templates.
		return "base URL is a template that must be completed with the operator's own identifiers (account, region or resource) before the provider can be added."
	}
	switch p.Category {
	case CategoryWebCookie:
		return "web-session-cookie provider: routing requires stored browser session state, which is not built yet (A3); the entry is refused at add time and is catalogued for completeness."
	case CategoryOAuth:
		if p.Verification == VerificationDocs {
			return "OAuth subscription surface: the router does not perform the login — obtain the token with the provider's own tooling and supply it; no auth surface is declared for automated exchange."
		}
		return "OAuth subscription entry with no documented auth surface and no base URL: not connectable until a permitted OAuth flow is implemented."
	case CategoryNoAuth:
		return "no-auth provider whose anonymous endpoint base URL is not documented yet; the connection mechanism (keyless endpoint or CLI bridge, with documented rate limits) lives in internal/noauth — the URL is never guessed — fill pending doc verification."
	case CategoryLocal:
		return "local provider whose local endpoint base URL is not documented yet (local surfaces depend on the operator's own installation); fill pending doc verification."
	case CategorySearch:
		return "search provider whose API base URL is not documented in the provider's own docs yet; fill pending doc verification."
	case CategoryAudio:
		return "audio provider whose API base URL is not documented in the provider's own docs yet; fill pending doc verification."
	case CategoryCloudAgent:
		return "cloud-agent surface that only works inside the vendor's own agent product; no independent API base URL exists to document."
	case CategoryUpstreamProxy:
		return "upstream-proxy surface that fronts another provider and has no independent API base URL of its own."
	default:
		return "no base URL documented in the provider's own docs yet; fill pending doc verification — the URL is never guessed."
	}
}
