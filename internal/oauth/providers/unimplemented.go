package providers

// pasteImportDocsURL cites the registry's own policy: the paste-token
// import path is an in-router capability (see ImportPasteToken), not a
// provider-documented OAuth flow.
const pasteImportDocsURL = "internal/oauth/providers/import.go (in-router paste-token import, allowlist enforced)"

// unimplementedInfo is one honestly-unimplemented provider: no
// interactive handler because its OAuth surface is not publicly
// documented. The reason string is the registry's public explanation.
type unimplementedInfo struct {
	id, name, alias, reason string
}

// unimplemented lists the 11 catalog OAuth providers whose flow is not
// publicly documented as of this registry's research (2026-11). Each
// reason records WHAT WAS CHECKED and what was missing — the proof the
// entry stays honest. Endpoints are never invented for these — the
// provider gets added the day its docs (or an official open-source
// client) publish the surface.
var unimplemented = []unimplementedInfo{
	{
		id:   "openference",
		name: "Openference",
		reason: "web search for the provider's own docs (site, OAuth/API pages) found no Openference documentation at all outside aggregator listings; " +
			"no OAuth endpoints, no client registration, no API-key docs to build from.",
	},
	{
		id:   "renderhour",
		name: "RenderHour",
		reason: "renderhour.com/docs documents its MCP sign-in only as 'OAuth with PKCE' driven by MCP client discovery; it publishes no authorize/token endpoint paths, " +
			"no client ID and no client registration for third-party applications, so no fixed flow can be implemented.",
	},
	{
		id:    "qoder",
		name:  "Qoder",
		alias: "if",
		reason: "docs.qoder.cn documents IDE login via Alibaba Cloud accounts (RAM/SSO onboarding pages) but publishes no OAuth endpoint paths or client registration; " +
			"the sign-in is internal to the IDE.",
	},
	{
		id:    "kilocode",
		name:  "Kilo Code",
		alias: "kc",
		reason: "kilo.ai/docs documents account sign-in (email/SSO browser flow) and API keys; the OAuth client.json at kilo.ai/docs/oauth/kilo/client.json is " +
			"Kilo's OWN MCP-client registration (client_id = the metadata URL, loopback MCP callback) — not a provider surface for third parties; " +
			"no authorize/token endpoints for the Kilo account are published.",
	},
	{
		id:    "megabrain",
		name:  "MegaBrain",
		alias: "mb",
		reason: "getmegabrain.com/help documents an OpenAI-compatible API with API keys and a browser consent sign-in for gateway tools, but publishes no OAuth endpoint paths, " +
			"client ID or token endpoint — the exchange is internal to its client.",
	},
	{
		id:   "devin-desktop",
		name: "Devin Desktop",
		reason: "docs.devin.ai documents `devin auth login` (browser login, --force-manual-token-flow) but publishes no OAuth endpoint paths or client registration; " +
			"the token exchange is internal. Checked docs.devin.ai/cli/reference, /cli/enterprise/devin-auth and /cli/troubleshooting.",
	},
	{
		id:    "devin-cli",
		name:  "Devin CLI",
		alias: "dv",
		reason: "docs.devin.ai documents `devin auth login` (browser login, --force-manual-token-flow) but publishes no OAuth endpoint paths or client registration; " +
			"the token exchange is internal. Checked docs.devin.ai/cli/reference, /cli/enterprise/devin-auth and /cli/troubleshooting.",
	},
	{
		id:    "codebuddy-cn",
		name:  "CodeBuddy CN",
		alias: "cbcn",
		reason: "Tencent's CodeBuddy docs (codebuddy.ai, tencentcloud.com) document IAM with API keys for the CLI; no OAuth endpoint paths or client registration are published " +
			"for the CN deployment.",
	},
	{
		id:    "codebuddy-intl",
		name:  "CodeBuddy Intl",
		alias: "cbai",
		reason: "Tencent's CodeBuddy docs (codebuddy.ai, tencentcloud.com) document IAM with API keys for the CLI; no OAuth endpoint paths or client registration are published " +
			"for the international deployment.",
	},
	{
		id:     "workbuddy",
		name:   "WorkBuddy",
		alias:  "wb",
		reason: "Tencent WorkBuddy Enterprise docs (tencentcloud.com) document IAM with API keys; no OAuth endpoint paths or client registration are published.",
	},
	{
		id:    "muse-code",
		name:  "Muse Code (Meta)",
		alias: "mc",
		reason: "dev.meta.ai/docs/muse-code/auth documents a browser sign-in or API key on first run, but publishes no OAuth endpoint paths, client ID or registration " +
			"(checked dev.meta.ai and its musemuse.dev mirror); the flow's endpoints are internal to the CLI.",
	},
}
