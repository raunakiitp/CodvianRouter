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

// unimplemented lists the 19 catalog OAuth providers whose flow is not
// publicly documented as of this registry's research. Each reason is
// specific: what IS documented, and what is missing. Endpoints are
// never invented for these — the provider gets added the day its
// docs (or an official open-source client) publish the surface.
var unimplemented = []unimplementedInfo{
	{
		id:     "codex",
		name:   "OpenAI Codex",
		alias:  "cx",
		reason: "handler lands in the follow-up PR of this same stack (area/a3-oauth-handlers-2); the documented auth.openai.com PKCE flow ships there.",
	},
	{
		id:     "chatgpt",
		name:   "ChatGPT",
		reason: "handler lands in the follow-up PR of this same stack (area/a3-oauth-handlers-2); the documented ChatGPT-account PKCE sign-in ships there.",
	},
	{
		id:     "kimi-coding",
		name:   "Kimi Code CLI",
		alias:  "kmc",
		reason: "handler lands in the follow-up PR of this same stack (area/a3-oauth-handlers-2); the documented device flow ships there.",
	},
	{
		id:     "gitlab-duo",
		name:   "GitLab Duo",
		alias:  "gitlab-duo",
		reason: "handler lands in the follow-up PR of this same stack (area/a3-oauth-handlers-2); the documented GitLab OAuth endpoints ship there.",
	},
	{
		id:     "kiro",
		name:   "Kiro AI",
		alias:  "kr",
		reason: "handler lands in the final PR of this same stack (area/a3-oauth-handlers-3); the documented AWS SSO OIDC device flow ships there.",
	},
	{
		id:     "amazon-q",
		name:   "Amazon Q",
		alias:  "aq",
		reason: "handler lands in the final PR of this same stack (area/a3-oauth-handlers-3); the documented AWS SSO OIDC device flow ships there.",
	},
	{
		id:   "ghe-copilot",
		name: "GitHub Enterprise Copilot",
		reason: "GitHub documents the Copilot device flow for GitHub.com; on GitHub Enterprise the flow needs the operator's GHE host and an instance-registered OAuth app, " +
			"and GitHub.com's public Copilot client credentials are not valid there — no documented generic flow to implement.",
	},
	{
		id:    "xai-oauth",
		name:  "xAI OAuth (Grok)",
		alias: "xao",
		reason: "xAI's Grok CLI documents OAuth sign-in (auth.x.ai) and a device-code mode, but publishes no endpoint paths or client IDs (the CLI carries them internally); " +
			"inventing them would fabricate the flow.",
	},
	{
		id:     "openference",
		name:   "Openference",
		reason: "no public OAuth endpoint documentation found for Openference; the provider documents API-key access only.",
	},
	{
		id:     "renderhour",
		name:   "RenderHour",
		reason: "no public OAuth endpoint documentation found for RenderHour.",
	},
	{
		id:     "grok-cli",
		name:   "Grok Build",
		alias:  "gc",
		reason: "the Grok CLI documents OAuth sign-in at auth.x.ai plus a device-code mode, but publishes no endpoint paths or client IDs; refusing to invent endpoints.",
	},
	{
		id:     "qoder",
		name:   "Qoder",
		alias:  "if",
		reason: "no public OAuth endpoint documentation found for Qoder.",
	},
	{
		id:     "cursor",
		name:   "Cursor IDE",
		alias:  "cu",
		reason: "Cursor documents API keys for its API (cursor.com/dashboard/api); its IDE OAuth surface is undocumented.",
	},
	{
		id:     "zed",
		name:   "Zed IDE",
		alias:  "zd",
		reason: "Zed's hosted-model authentication is internal to the editor account; no OAuth endpoints are published.",
	},
	{
		id:     "zed-hosted",
		name:   "Zed Hosted Models",
		reason: "Zed's hosted-model authentication is internal to the editor account; no OAuth endpoints are published.",
	},
	{
		id:     "trae",
		name:   "Trae",
		alias:  "tr",
		reason: "no public OAuth endpoint documentation found for Trae.",
	},
	{
		id:     "kilocode",
		name:   "Kilo Code",
		alias:  "kc",
		reason: "Kilo Code documents API keys; its account OAuth surface is undocumented.",
	},
	{
		id:     "megabrain",
		name:   "MegaBrain",
		alias:  "mb",
		reason: "no public OAuth endpoint documentation found for MegaBrain.",
	},
	{
		id:     "devin-desktop",
		name:   "Devin Desktop",
		reason: "Devin documents `devin auth login` (browser login) but publishes no OAuth endpoint paths or client registration; the token exchange is internal.",
	},
	{
		id:     "devin-cli",
		name:   "Devin CLI",
		alias:  "dv",
		reason: "Devin documents `devin auth login` (browser login) but publishes no OAuth endpoint paths or client registration; the token exchange is internal.",
	},
	{
		id:     "codebuddy-cn",
		name:   "CodeBuddy CN",
		alias:  "cbcn",
		reason: "no public OAuth endpoint documentation found for CodeBuddy (CN region).",
	},
	{
		id:     "codebuddy-intl",
		name:   "CodeBuddy Intl",
		alias:  "cbai",
		reason: "no public OAuth endpoint documentation found for CodeBuddy (international).",
	},
	{
		id:     "workbuddy",
		name:   "WorkBuddy",
		alias:  "wb",
		reason: "no public OAuth endpoint documentation found for WorkBuddy.",
	},
	{
		id:     "muse-code",
		name:   "Muse Code (Meta)",
		alias:  "mc",
		reason: "no public OAuth endpoint documentation found for Muse Code beyond an auth doc index; the flow's endpoints and client registration are not published.",
	},
	{
		id:     "freebuff",
		name:   "Freebuff",
		alias:  "freebuff",
		reason: "Freebuff documents API keys for its paid API; its OAuth surface (official-client free tier) is undocumented.",
	},
}
