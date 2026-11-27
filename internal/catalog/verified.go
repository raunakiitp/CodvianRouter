package catalog

// AnthropicVersion is the anthropic-version header value Anthropic's
// official docs use as their current example (2026-10-21, research
// 2026-11-17). The old 2023-06-01 value is superseded. This value
// changes over time: when Anthropic publishes a newer example, update
// this ONE constant — never hard-code the version anywhere else.
const AnthropicVersion = "2026-10-21"

// verifiedProviders holds the providers whose base URL, auth scheme,
// and wire format were checked against the provider's own official
// documentation in the 2026-11-17 auth research pass
// (internal research notes, site-crawl auth report, 2026-11-17).
//
// These entries OVERLAY the reference entries with the same ID in All():
// the verified facts replace the reference metadata, and the entry's
// Verification becomes "verified-from-docs". Entries whose ID does not
// match a reference entry are added as new providers.
//
// Free-tier notes appear ONLY where the research verified one from the
// provider's own docs. No aggregate free-token headline is computed
// from this list.
var verifiedProviders = []Provider{
	{
		ID:          "openai",
		Name:        "OpenAI",
		Website:     "https://platform.openai.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.openai.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyPrefix:    "sk-proj-",
		KeyFormat:    "sk-proj-... (project keys); legacy sk-... also accepted",
		Verification: VerificationDocs,
		SourceURL:    "https://platform.openai.com/docs/api-reference/authentication",
		Notes:        "Optional OpenAI-Organization / OpenAI-Project headers. 64 KiB total header budget.",
	},
	{
		ID:          "anthropic",
		Name:        "Anthropic",
		Website:     "https://www.anthropic.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.anthropic.com"},
		WireFormats: []WireFormat{WireAnthropic},
		Endpoints: []EndpointAuth{
			{
				Endpoint:     EndpointMessages,
				AuthKind:     AuthBearer,
				HeaderName:   "Authorization",
				ExtraHeaders: map[string]string{"anthropic-version": AnthropicVersion},
			},
			{
				Endpoint:     EndpointMessages,
				Variant:      "x-api-key",
				AuthKind:     AuthXAPIKey,
				HeaderName:   "x-api-key",
				ExtraHeaders: map[string]string{"anthropic-version": AnthropicVersion},
			},
		},
		KeyPrefix:    "sk-ant-",
		KeyFormat:    "sk-ant-... / sk-ant-api03-...",
		Verification: VerificationDocs,
		SourceURL:    "https://platform.claude.com/docs/en/api/getting-started",
		Notes:        "Bearer is now preferred; x-api-key is the legacy fallback. anthropic-version is REQUIRED. max_tokens required per request. anthropic-workspace-id required for unscoped keys.",
	},
	{
		ID:          "claude",
		Name:        "Claude Code",
		Alias:       "cc",
		Website:     "https://claude.com",
		Category:    CategoryOAuth,
		BaseURLs:    []string{"https://api.anthropic.com"},
		WireFormats: []WireFormat{WireAnthropic},
		Endpoints: []EndpointAuth{
			{
				Endpoint:   EndpointMessages,
				AuthKind:   AuthOAuth,
				HeaderName: "Authorization",
				ExtraHeaders: map[string]string{
					"anthropic-beta": "oauth-2025-04-20",
				},
				Exchange: &ExchangeAuth{
					Kind:        "oauth-pkce",
					Description: "Claude Code subscription login: token sk-ant-oat01-* via `claude setup-token` (1 yr) or PKCE to claude.ai. Requires User-Agent claude-cli/<ver> and the system-prompt prefix 'You are Claude Code, Anthropic's official CLI for Claude.' or Opus/Sonnet get rate-limited.",
					DocURL:      "https://github.com/oakimov/claude-code-router/blob/HEAD/docs/docs/server/guides/claude-auth.md",
				},
			},
		},
		KeyPrefix:    "sk-ant-oat01-",
		Verification: VerificationDocs,
		ToSGrey:      true,
		SourceURL:    "https://github.com/oakimov/claude-code-router/blob/HEAD/docs/docs/server/guides/claude-auth.md",
		Notes:        "Subscription login, not an API key. Use only the user's own login (claude setup-token or a PKCE client); the router dials the surface with the resulting token — it does not perform the login itself.",
	},
	{
		ID:          "gemini",
		Name:        "Google Gemini",
		Alias:       "google-ai-studio",
		Website:     "https://aistudio.google.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://generativelanguage.googleapis.com/v1beta"},
		WireFormats: []WireFormat{WireGemini, WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointGenerate, AuthKind: AuthXGoogAPIKey, HeaderName: "x-goog-api-key"},
		},
		KeyFormat:    "No documented prefix — keys created since 2026-05-28 are service-account-bound auth keys. Do NOT validate by prefix.",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.getbifrost.ai/providers/supported-providers/gemini",
		Notes:        "x-goog-api-key header is recommended; ?key= query param is legacy and leaks into logs. Unrestricted standard keys rejected from Sep 2026. Also has an OpenAI-compatible endpoint.",
	},
	{
		ID:          "groq",
		Name:        "Groq",
		Website:     "https://groq.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.groq.com/openai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyPrefix:    "gsk_",
		Verification: VerificationDocs,
		SourceURL:    "https://console.groq.com/docs/prometheus-metrics",
		Notes:        "Key is per-project; org-wide rate limits.",
	},
	{
		ID:          "mistral",
		Name:        "Mistral",
		Website:     "https://mistral.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.mistral.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyFormat:    "No documented prefix/format.",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.mistral.ai/resources/mcp",
		Notes:        "Keys are workspace-scoped.",
	},
	{
		ID:          "deepseek",
		Name:        "DeepSeek",
		Website:     "https://www.deepseek.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.deepseek.com", "https://api.deepseek.com/anthropic"},
		WireFormats: []WireFormat{WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{
				Endpoint:   EndpointChat,
				BaseURL:    "https://api.deepseek.com",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
			{
				Endpoint:   EndpointAnthropic,
				BaseURL:    "https://api.deepseek.com/anthropic",
				AuthKind:   AuthXAPIKey,
				HeaderName: "x-api-key",
			},
		},
		KeyFormat:    "No documented prefix (not verified).",
		Verification: VerificationDocs,
		SourceURL:    "https://platform.deepseek.com/api-docs/function_calling",
		Notes:        "Same key, two surfaces: OpenAI base has NO /v1 in the chat path (/chat/completions); Anthropic base is /anthropic. No OAuth, no key scopes.",
	},
	{
		ID:          "xai",
		Name:        "xAI",
		Alias:       "grok",
		Website:     "https://x.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.x.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyPrefix:    "xai-",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.x.ai/developers/rest-api-reference/inference",
		Notes:        "OAuth is NOT documented for api.x.ai (only /v1/me accepts OAuth tokens). Grok Build CLI OAuth goes to cli-chat-proxy.grok.com instead.",
	},
	{
		ID:          "cohere",
		Name:        "Cohere",
		Website:     "https://cohere.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.cohere.com", "https://api.cohere.com/compatibility/v1"},
		WireFormats: []WireFormat{WireCohere, WireOpenAI},
		Endpoints: []EndpointAuth{
			{
				Endpoint:   EndpointNative,
				BaseURL:    "https://api.cohere.com",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
			{
				Endpoint:   EndpointChat,
				BaseURL:    "https://api.cohere.com/compatibility/v1",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
		},
		KeyFormat:    "No documented prefix (not verified).",
		HasFreeTier:  true,
		FreeTierNote: "Trial keys: 1,000 requests/month, non-commercial use only.",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.cohere.com/v1/docs/connector-authentication",
		Notes:        "Native chat is POST /v2/chat — NOT OpenAI wire (typed content blocks, SSE content-delta/message-end). OpenAI-compatible shim at /compatibility/v1.",
	},
	{
		ID:          "together",
		Name:        "Together",
		Website:     "https://www.together.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.together.xyz/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyFormat:    "No documented prefix (not verified).",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.nexla.com/user-guides/connectors/together_ai_api",
		Notes:        "Legacy host api.together.ai also seen; canonical is .xyz.",
	},
	{
		ID:          "fireworks",
		Name:        "Fireworks",
		Website:     "https://fireworks.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.fireworks.ai/inference/v1", "https://api.fireworks.ai/inference"},
		WireFormats: []WireFormat{WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{
				Endpoint:   EndpointChat,
				BaseURL:    "https://api.fireworks.ai/inference/v1",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
			{
				Endpoint:   EndpointChat,
				Variant:    "alt-header",
				BaseURL:    "https://api.fireworks.ai/inference/v1",
				AuthKind:   AuthXFireworksAPIKey,
				HeaderName: "X-Fireworks-Api-Key",
			},
			{
				Endpoint:   EndpointAnthropic,
				BaseURL:    "https://api.fireworks.ai/inference",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
			{
				Endpoint:   EndpointResponses,
				BaseURL:    "https://api.fireworks.ai/inference/v1",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
		},
		KeyPrefix:    "fw_",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.fireworks.ai/nexus/apis-and-sdks",
		Notes:        "Two bases: inference /inference/v1 (OpenAI) vs /inference (Anthropic, no /v1). Control plane is https://api.fireworks.ai/v1. Nexus Auto takes extra x-anthropic-api-key / x-openai-api-key headers. US residency host us.api.fireworks.ai.",
	},
	{
		ID:          "perplexity",
		Name:        "Perplexity",
		Website:     "https://www.perplexity.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.perplexity.ai"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyPrefix:    "pplx-",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.perplexity.ai/docs/orchestrator/quickstart",
		Notes:        "Sonar /chat/completions + /v1/sonar; Search POST /search; Orchestrator https://api.perplexity.ai/orchestrator/v1. MCP accepts OAuth 2.1+PKCE or Bearer key — the one officially documented OAuth flow here.",
	},
	{
		ID:          "ollama-local",
		Name:        "Ollama",
		Alias:       "ollama",
		Website:     "https://ollama.com",
		Category:    CategoryLocal,
		BaseURLs:    []string{"http://localhost:11434"},
		WireFormats: []WireFormat{WireOllama, WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthNone},
			{Endpoint: EndpointNative, AuthKind: AuthNone},
			{Endpoint: EndpointAnthropic, AuthKind: AuthNone},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://refusalrate.com/ollama-openai-compatible-api",
		Notes:        "No auth at all. Binds 127.0.0.1:11434 by default — never expose 11434 publicly (no auth on pull/delete/create). SDKs need a placeholder key, e.g. \"ollama\". Native /api/*, OpenAI /v1, Anthropic /v1/messages on the same port.",
	},
	{
		ID:          "ollama-cloud",
		Name:        "Ollama Cloud",
		Website:     "https://ollama.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://ollama.com/v1", "https://ollama.com/api"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyFormat:    "OLLAMA_API_KEY from ollama.com/settings/keys.",
		Verification: VerificationDocs,
		SourceURL:    "https://convly.ai/ollama-api-key-authentication/",
		Notes:        "Only for direct cloud calls; local Ollama needs no key.",
	},
	{
		ID:          "openrouter",
		Name:        "OpenRouter",
		Website:     "https://openrouter.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://openrouter.ai/api/v1", "https://eu.openrouter.ai/api/v1"},
		WireFormats: []WireFormat{WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{
				Endpoint:   EndpointChat,
				BaseURL:    "https://openrouter.ai/api/v1",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
			{
				Endpoint: EndpointChat,
				Variant:  "jwt",
				BaseURL:  "https://openrouter.ai/api/v1",
				AuthKind: AuthExchange,
				Exchange: &ExchangeAuth{
					Kind:        "jwt",
					Description: "Signed JWT access token (typ at+jwt, iss https://openrouter.ai, ES256, max 15 min, JWKS at https://openrouter.ai/api/v1/oauth/jwks). Minted per session via OAuth PKCE; never stored as a static key.",
					TokenTTL:    "15m",
					DocURL:      "https://openrouter.ai/docs/api_reference/authentication",
				},
			},
			{
				Endpoint:   EndpointAnthropic,
				BaseURL:    "https://openrouter.ai/api/v1",
				AuthKind:   AuthXAPIKey,
				HeaderName: "x-api-key",
			},
			{
				Endpoint:   EndpointResponses,
				BaseURL:    "https://openrouter.ai/api/v1",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
		},
		KeyPrefix:    "sk-or-v1-",
		KeyFormat:    "sk-or-v1- + 64 lowercase hex (73 chars total); or a signed JWT access token (detected by prefix/issuer).",
		Verification: VerificationDocs,
		SourceURL:    "https://openrouter.ai/docs/api_reference/authentication",
		Notes:        "Optional HTTP-Referer + X-Title headers (rankings). EU host https://eu.openrouter.ai/api/v1. Keys can carry credit limits/expiry. OAuth PKCE + BYOK exist. Account login (Phase 1): `codvian-router provider login openrouter` or POST /admin/provider/login — the official PKCE flow returns a user-controlled API key (no refresh token), stored encrypted in the provider key pool.",
	},
	{
		ID:          "azure-openai",
		Name:        "Azure OpenAI",
		Alias:       "azure",
		Website:     "https://azure.microsoft.com/products/ai-services/openai-service",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://{resource}.openai.azure.com/openai/v1/"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{
				Endpoint: EndpointChat,
				AuthKind: AuthExchange,
				Exchange: &ExchangeAuth{
					Kind:        "entra-id",
					Description: "Foundry /openai/v1 with Microsoft Entra ID: Bearer token minted for scope https://cognitiveservices.azure.com/.default (or https://ai.azure.com/.default). The api-key header also works on this surface. Implemented (Phase 1): store {tenant_id, client_id, client_secret} JSON as the provider key; the router mints the token with the client-credentials grant and re-mints it on expiry (no refresh token exists in this grant).",
					TokenTTL:    "1h",
					DocURL:      "https://learn.microsoft.com/en-us/azure/foundry/openai/reference",
				},
			},
			{
				Endpoint:   EndpointChat,
				Variant:    "api-key",
				AuthKind:   AuthAPIKey,
				HeaderName: "api-key",
			},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://learn.microsoft.com/en-us/azure/foundry/openai/reference",
		Notes:        "Endpoint is per-resource; {resource} must be substituted. model = deployment name, not model ID. /openai/v1 needs no api-version; legacy path needs ?api-version=.",
	},
	{
		ID:          "bedrock",
		Name:        "AWS Bedrock",
		Website:     "https://aws.amazon.com/bedrock",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://bedrock-runtime.{region}.amazonaws.com", "https://bedrock-mantle.{region}.api.aws"},
		WireFormats: []WireFormat{WireBedrock, WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{
				Endpoint: EndpointConverse,
				BaseURL:  "https://bedrock-runtime.{region}.amazonaws.com",
				AuthKind: AuthExchange,
				Exchange: &ExchangeAuth{
					Kind:        "sigv4",
					Description: "AWS SigV4 signing with AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY and optional AWS_SESSION_TOKEN. Region is part of the signature. Converse is POST /model/{id}/converse.",
					DocURL:      "https://symfony.com/doc/current/ai/components/platform/bedrock.html",
				},
			},
			{
				Endpoint:   EndpointChat,
				Variant:    "mantle",
				BaseURL:    "https://bedrock-mantle.{region}.api.aws",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
			{
				Endpoint:   EndpointAnthropic,
				Variant:    "mantle",
				BaseURL:    "https://bedrock-mantle.{region}.api.aws",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://symfony.com/doc/current/ai/components/platform/bedrock.html",
		Notes:        "SigV4 by default; the newer Bedrock API key (Bearer) takes precedence on Mantle endpoints. Mantle endpoints are OpenAI/Anthropic-compatible.",
	},
	{
		ID:          "vertex",
		Name:        "Google Vertex AI",
		Alias:       "vertex-ai",
		Website:     "https://cloud.google.com/vertex-ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://{LOCATION}-aiplatform.googleapis.com/v1/projects/…"},
		WireFormats: []WireFormat{WireGemini},
		Endpoints: []EndpointAuth{
			{
				Endpoint: EndpointGenerate,
				AuthKind: AuthExchange,
				Exchange: &ExchangeAuth{
					Kind:        "service-account-json",
					Description: "Service-account JSON (client_email / private_key / token_uri) exchanged via RS256 JWT-bearer grant (RFC 7523), scope https://www.googleapis.com/auth/cloud-platform, role roles/aiplatform.user. The minted OAuth2 access token is cached ~1h.",
					TokenTTL:    "1h",
					DocURL:      "https://github.com/maruel/genai/blob/HEAD/providers/gemini/docs/vertex_ai.md",
				},
			},
			{
				Endpoint:   EndpointGenerate,
				Variant:    "express",
				AuthKind:   AuthXGoogAPIKey,
				HeaderName: "x-goog-api-key",
			},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://github.com/maruel/genai/blob/HEAD/providers/gemini/docs/vertex_ai.md",
		Notes:        "Project + location are in the path. Vertex Express mode accepts x-goog-api-key / ?key= like Gemini AI Studio.",
	},
	{
		ID:          "huggingface",
		Name:        "Hugging Face",
		Alias:       "hf",
		Website:     "https://huggingface.co",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://router.huggingface.co/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyPrefix:    "hf_",
		HasFreeTier:  true,
		FreeTierNote: "Credit-metered; free credit ~$0.10/month, requests return HTTP 402 when exhausted.",
		Verification: VerificationDocs,
		SourceURL:    "https://huggingface.co/docs/inference-providers/index",
		Notes:        "Model IDs are case-sensitive. Routing suffixes :fastest / :cheapest / :preferred / :<provider>. Optional X-HF-Bill-To header.",
	},
	{
		ID:          "cerebras",
		Name:        "Cerebras",
		Website:     "https://cerebras.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.cerebras.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyFormat:    "CEREBRAS_API_KEY; prefix not verified.",
		Verification: VerificationDocs,
		SourceURL:    "https://github.com/arthurlacoste/freellmpool/blob/HEAD/FAQ.md",
	},
	{
		ID:          "nvidia",
		Name:        "NVIDIA NIM",
		Website:     "https://www.nvidia.com/en-us/ai/",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://integrate.api.nvidia.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		KeyFormat:    "NVIDIA_API_KEY; prefix not verified.",
		Verification: VerificationDocs,
		SourceURL:    "https://github.com/arthurlacoste/freellmpool/blob/HEAD/FAQ.md",
		Notes:        "Model IDs carry the nvidia/ prefix.",
	},
	{
		ID:          "cloudflare-ai",
		Name:        "Cloudflare Workers AI",
		Alias:       "cloudflare",
		Website:     "https://developers.cloudflare.com/workers-ai/",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{
				Endpoint:   EndpointChat,
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
			{
				Endpoint:   EndpointNative,
				BaseURL:    "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/run",
				AuthKind:   AuthBearer,
				HeaderName: "Authorization",
			},
		},
		KeyFormat:    "CLOUDFLARE_API_TOKEN.",
		Verification: VerificationDocs,
		SourceURL:    "https://github.com/shaivpidadi/freeridev3/blob/HEAD/docs/providers/SURVEY.md",
		Notes:        "Account ID is in the URL, not the key — the router needs both token + account_id. Native shape is {prompt} at .../ai/run/{model}.",
	},
	{
		ID:          "codvian",
		Name:        "Codvian",
		Website:     "https://codvian.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.codvian.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "internal/provider registry (CodvianPreset)",
		Notes:        "Codvian's own API, OpenAI-compatible. The user pastes their own Codvian API key.",
	},
	{
		// 2026-11-18 catalog triage (CHECKLIST 253): base URLs and
		// auth read from Merge's own docs (docs.merge.dev/merge-gateway).
		ID:          "merge-gateway",
		Name:        "Merge Gateway",
		Website:     "https://gateway.merge.dev",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api-gateway.merge.dev/v1/openai", "https://api-gateway.merge.dev/v1/ai-sdk"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		HasFreeTier:  true,
		FreeTierNote: "Free plan covers a list of free models up to a daily request cap (cap not stated on the docs page); signup needs no card.",
		Verification: VerificationDocs,
		SourceURL:    "https://docs.merge.dev/merge-gateway",
		Notes:        "OpenAI base is /v1/openai per Merge docs; /v1/ai-sdk is the Vercel AI SDK variant. Anthropic surface /v1/anthropic exists but is not the default chat base.",
	},
	{
		// 2026-11-18 catalog triage (CHECKLIST 253): base URL and auth
		// from Helicone's own docs (docs.helicone.ai/gateway/overview,
		// quick-start).
		ID:          "helicone",
		Name:        "Helicone",
		Website:     "https://helicone.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://ai-gateway.helicone.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.helicone.ai/gateway/overview",
		Notes:        "AI Gateway: 100+ models behind one key, OpenAI-compatible. Helicone announced joining Mintlify in Mar 2026; third-party reports say the gateway is in maintenance mode — verify before new builds.",
	},
	{
		// 2026-11-18 catalog triage (CHECKLIST 253): bases and auth
		// from OpenCode's own docs (opencode.ai/v2/docs/console/models/).
		ID:          "opencode",
		Name:        "OpenCode",
		Alias:       "oc",
		Website:     "https://opencode.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://opencode.ai/inference/openai/v1", "https://opencode.ai/inference/anthropic/v1", "https://opencode.ai/inference/google/v1beta"},
		WireFormats: []WireFormat{WireOpenAI, WireAnthropic, WireGemini},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointMessages, BaseURL: "https://opencode.ai/inference/anthropic/v1", AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://opencode.ai/v2/docs/console/models/",
		Notes:        "Console pay-as-you-go (credits + API key, Bearer). Free-tier chat models require the OpenCode client, so no free tier is claimed here. Distinct from opencode-go (subscription surface).",
	},
	{
		// 2026-11-18 catalog triage (CHECKLIST 253): endpoint pattern
		// and auth from Cloudflare's own AI Gateway docs (get-started).
		ID:          "cloudflare-ai-gateway",
		Name:        "Cloudflare AI Gateway",
		Website:     "https://developers.cloudflare.com/ai-gateway/",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/{provider}"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://developers.cloudflare.com/ai-gateway/get-started/",
		Notes:        "Gateway proxy, not a model host: {account_id} and {gateway_id} must be substituted and {provider} selects the upstream (openai, anthropic, google-ai-studio, workers-ai, bedrock, azure-openai, ...). Auth is the Cloudflare API token (Bearer). Distinct from cloudflare-workers-ai (Workers AI inference).",
	},
	{
		// 2026-11-18 catalog triage round 2 (CHECKLIST 266): base URL
		// and auth from GMI Cloud's own docs
		// (docs.gmicloud.ai/inference-engine/api-reference/llm-api-reference):
		// Bearer API key, GET https://api.gmi-serving.com/v1/models,
		// POST https://api.gmi-serving.com/v1/chat/completions.
		ID:          "gmicloud",
		Name:        "GMI Cloud",
		Website:     "https://gmicloud.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.gmi-serving.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.gmicloud.ai/inference-engine/api-reference/llm-api-reference",
		Notes:        "GMI Inference: OpenAI-compatible chat completions and model list per the provider's own API reference. Added 2026-11-18 triage round 2; live reachability probe HTTP 405 on the bare base.",
	},
	{
		// 2026-11-18 catalog triage round 2 (CHECKLIST 266): base URL
		// and auth from Vultr's own docs (api.vultrinference.com):
		// requests go to https://api.vultrinference.com/v1 with the
		// API key as a Bearer token; OpenAI/OpenRouter shapes on
		// chat, responses, image, video, audio and embeddings, and
		// Anthropic's Messages API on /v1/messages.
		ID:          "vultr",
		Name:        "Vultr",
		Website:     "https://api.vultrinference.com/",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.vultrinference.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://api.vultrinference.com/",
		Notes:        "Vultr Serverless Inference: one API for chat, reasoning, images, video, music, speech, embeddings and document search. Added 2026-11-18 triage round 2; live reachability probe HTTP 200 on the bare base. Anthropic Messages surface on /v1/messages is documented but not a separate endpoint here.",
	},
	{
		// 2026-11-18 catalog triage round 2 (CHECKLIST 266): base URL
		// and auth from Privatemode's own docs
		// (docs.privatemode.ai/api/proxyless-api/): the proxyless
		// API's OpenAI-compatible base is
		// https://proxyless-api.privatemode.ai/v1 with a Privatemode
		// API key. models.dev lists the LOCAL proxy base
		// http://localhost:8080/v1 instead — that is the endpoint
		// of the Privatemode proxy app/Docker on the user's own
		// machine, not the cloud API, so the catalog uses the
		// docs-verified cloud base.
		ID:          "privatemode-ai",
		Name:        "Privatemode AI",
		Website:     "https://privatemode.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://proxyless-api.privatemode.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.privatemode.ai/api/proxyless-api/",
		Notes:        "Privatemode (Edgeless Systems): confidential-computing inference API, OpenAI-compatible; the proxyless API needs no local proxy (the SDK/proxy path adds client-side attestation). Added 2026-11-18 triage round 2; live reachability probe HTTP 404 on the proxyless base.",
	},
	{
		// 2026-11-18 catalog triage round 3 (CHECKLIST 275): base URL
		// and auth from Klavo's own docs (ai.klavo.ru/docs): the
		// OpenAI SDK quickstart uses base_url
		// https://api.klavo.ru/v1 with Authorization: Bearer <key>;
		// endpoints /v1/chat/completions, /v1/completions,
		// /v1/embeddings, /v1/messages, GET /v1/models.
		ID:          "klavo",
		Name:        "Klavo AI",
		Website:     "https://ai.klavo.ru",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.klavo.ru/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://ai.klavo.ru/docs",
		Notes:        "Klavo AI: OpenAI-compatible chat, completions, embeddings and Anthropic-messages surfaces per the provider's own docs; optional x-klavo-session-id header for agent sessions. Added 2026-11-18 triage round 3; live reachability probe HTTP 404 on the bare base.",
	},
	{
		// 2026-11-18 catalog triage round 3 (CHECKLIST 275): base URL
		// and auth from Huawei Cloud's own MaaS docs
		// (support.huaweicloud.com/model-call-maas/model-call-021.html):
		// the OpenAI-compatible API address is
		// https://api.modelarts-maas.com/openai/v1/chat/completions
		// (base_url https://api.modelarts-maas.com/openai/v1) with
		// Authorization: Bearer $MaaS_API_Key. Only the
		// Southwest-Guiyang 1 region is supported per the same page.
		ID:          "huawei-maas",
		Name:        "Huawei Cloud MAAS",
		Website:     "https://support.huaweicloud.com/model-call-maas/model-call-021.html",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.modelarts-maas.com/openai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://support.huaweicloud.com/model-call-maas/model-call-021.html",
		Notes:        "Huawei Cloud MaaS (Model-as-a-Service): OpenAI-compatible chat completions per Huawei's own docs; regional — only Southwest-Guiyang 1 is supported, substitute your region if Huawei opens others. Added 2026-11-18 triage round 3; live reachability probe HTTP 404 on the bare base.",
	},
	{
		// 2026-11-18 catalog triage round 3 (CHECKLIST 275): base URL
		// and auth from STACKIT's own docs
		// (docs.stackit.cloud/products/data-and-ai/ai-model-serving/):
		// every shared model lists URL
		// https://api.openai-compat.model-serving.eu01.onstackit.cloud/v1
		// with OpenAI-compatible POST /chat/completions, /completions,
		// /embeddings and GET /models; the getting-started guide
		// creates an AI Model Serving auth token and calls the API
		// with Authorization: Bearer [auth-token].
		ID:          "stackit",
		Name:        "STACKIT",
		Website:     "https://docs.stackit.cloud/products/data-and-ai/ai-model-serving/basics/available-shared-models",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.openai-compat.model-serving.eu01.onstackit.cloud/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.stackit.cloud/products/data-and-ai/ai-model-serving/basics/available-shared-models",
		Notes:        "STACKIT AI Model Serving (EU01): OpenAI-compatible shared-model inference per STACKIT's own docs; auth token created in the customer portal. Added 2026-11-18 triage round 3; live reachability probe HTTP 403 on the bare base (key required).",
	},
	{
		// 2026-11-18 catalog triage round 4 (CHECKLIST 281): base URL
		// and auth from Kimi's own docs
		// (www.kimi.com/code/docs/en/kimi-code/models.html): the Kimi
		// Code API supports OpenAI (Responses / Chat Completions)
		// and Anthropic protocols; the OpenAI base URL for China is
		// https://api.kimi.com/coding/v1, and an API key is created
		// in the Kimi Code Console and filled in with the base URL.
		ID:          "kimi-code-plan-cn",
		Name:        "Kimi For Coding (kimi.com)",
		Website:     "https://www.kimi.com/code",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.kimi.com/coding/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://www.kimi.com/code/docs/en/kimi-code/models.html",
		Notes:        "Kimi For Coding, China surface (kimi.com): OpenAI-compatible Responses / chat-completions API per Kimi's own docs, which list the China base URL and API-key auth (key created in the Kimi Code Console). Added 2026-11-18 triage round 4; live reachability probe HTTP 404 on the bare base. The overseas surface is `kimi-code-plan-global` (api.kimi.ai/coding/v1).",
	},
	{
		// 2026-11-18 catalog triage round 4 (CHECKLIST 281): base URL
		// and auth from Kimi's own docs
		// (www.kimi.ai/code/docs/en/kimi-code/models.html): the Kimi
		// Code API supports OpenAI (Responses / Chat Completions)
		// and Anthropic protocols; the OpenAI base URL overseas is
		// https://api.kimi.ai/coding/v1, and an API key is created
		// in the Kimi Code Console and filled in with the base URL.
		ID:          "kimi-code-plan-global",
		Name:        "Kimi For Coding (kimi.ai)",
		Website:     "https://www.kimi.ai/code",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.kimi.ai/coding/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://www.kimi.ai/code/docs/en/kimi-code/models.html",
		Notes:        "Kimi For Coding, overseas surface (kimi.ai): OpenAI-compatible Responses / chat-completions API per Kimi's own docs, which list the overseas base URL and API-key auth (key created in the Kimi Code Console). Added 2026-11-18 triage round 4; live reachability probe HTTP 404 on the bare base. The China surface is `kimi-code-plan-cn` (api.kimi.com/coding/v1).",
	},
	{
		// 2026-11-18 catalog triage round 5 (CHECKLIST 301): base URL
		// and auth from SaladCloud's own docs
		// (docs.salad.com/ai-gateway/explanation/overview): the AI
		// Gateway is an OpenAI-compatible LLM API; the docs tell
		// tools to point the base URL to
		// https://ai.salad.cloud/v1 with the organization-specific
		// AI Gateway key as `Authorization: Bearer`, and list the
		// model `qwen3.6-35b-a3b` that models.dev also lists.
		// models.dev lists NO api field for salad-cloud, so this
		// base comes from the docs, not from models.dev. Live
		// reachability probe HTTP 401 on the bare base, and GET
		// https://ai.salad.cloud/v1/models -> 200.
		ID:          "salad-cloud",
		Name:        "SaladCloud AI Gateway",
		Website:     "https://docs.salad.com/ai-gateway/explanation/overview",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://ai.salad.cloud/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.salad.com/ai-gateway/explanation/overview",
		Notes:        "SaladCloud AI Gateway: OpenAI-compatible LLM API per SaladCloud's own docs, which state the base URL https://ai.salad.cloud/v1 and Bearer auth (organization-specific AI Gateway key). Added 2026-11-18 triage round 5; live reachability probe HTTP 401 on the bare base, GET /v1/models -> 200.",
	},
	{
		// 2026-11-18 catalog triage round 5 (CHECKLIST 301): base URL
		// and auth from Verda's own docs
		// (docs.verda.com/inference/quickstart): the quickstart sets
		// OPENAI_BASE_URL=https://inference.verda.com/v1 with a
		// Bearer API key and calls /v1/chat/completions with the
		// OpenAI SDK.
		ID:          "verda",
		Name:        "Verda",
		Website:     "https://docs.verda.com/inference/quickstart/",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://inference.verda.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.verda.com/inference/quickstart/",
		Notes:        "Verda: OpenAI-compatible inference API per Verda's own docs, which state the base URL https://inference.verda.com/v1 and Bearer auth. Added 2026-11-18 triage round 5; live reachability probe HTTP 404 on the bare base.",
	},
	{
		// 2026-11-18 catalog triage round 5 (CHECKLIST 301): base URL
		// and auth from Confidential AI's own docs
		// (confidential.ai/docs/inference-api/get-started): the
		// production base URL is https://api.confidential.ai, the
		// API is OpenAI-compatible, and calls use
		// `Authorization: Bearer $CONFIDENTIAL_API_KEY` against
		// https://api.confidential.ai/v1/chat/completions.
		ID:          "confidential-ai",
		Name:        "Confidential AI",
		Website:     "https://confidential.ai/docs/inference-api/get-started",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.confidential.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://confidential.ai/docs/inference-api/get-started",
		Notes:        "Confidential AI: OpenAI-compatible Confidential Inference API per its own docs, which state the production base URL https://api.confidential.ai (so /v1) and Bearer auth. Added 2026-11-18 triage round 5; live reachability probe HTTP 401 on the bare base.",
	},
	{
		// 2026-11-18 catalog triage round 5 (CHECKLIST 301): base URL
		// and auth from TheRouter's own docs
		// (therouter.ai/docs/quickstart): the global endpoint for
		// all requests is https://api.therouter.ai/v1, the API is
		// OpenAI-compatible, and calls use
		// `Authorization: Bearer $THEROUTER_API_KEY`.
		ID:          "therouter",
		Name:        "TheRouter",
		Website:     "https://therouter.ai/docs/quickstart/",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.therouter.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://therouter.ai/docs/quickstart/",
		Notes:        "TheRouter: unified OpenAI-compatible API per its own docs, which state the global endpoint https://api.therouter.ai/v1 and Bearer auth. Added 2026-11-18 triage round 5; live reachability probe HTTP 404 on the bare base.",
	},
	{
		// 2026-11-18 catalog triage round 5 (CHECKLIST 301): base URL
		// and auth from LLM Tech's own docs
		// (llmtech.eu/models/qwen3.8-27b): the quickstart sets
		// base_url="https://api.llmtech.eu/v1" with an API key
		// against an OpenAI-compatible endpoint, and the page states
		// the service is in production, serving live traffic since
		// Aug 22, 2026. The bare base and /v1/models answer HTTP
		// 410 to unauthenticated probes — the host answers, and the
		// docs prove the endpoint, so this is verified-from-docs,
		// not a probe-only addition.
		ID:          "llmtech",
		Name:        "LLM Tech",
		Website:     "https://llmtech.eu/models/qwen3.8-27b",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.llmtech.eu/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://llmtech.eu/models/qwen3.8-27b",
		Notes:        "LLM Tech: OpenAI-compatible hosted inference per its own docs, which state the base URL https://api.llmtech.eu/v1 and API-key auth, in production since Aug 22, 2026. Added 2026-11-18 triage round 5; live reachability probe HTTP 410 on the bare base and on /v1/models (the host answers; the docs prove the endpoint).",
	},
	{
		// Verified 2026-11-27 against DeepInfra's own docs: the
		// OpenAI-compatible surface is /v1/openai (not the bare /v1),
		// and the Responses API shares that same base.
		ID:          "deepinfra",
		Name:        "DeepInfra",
		Website:     "https://deepinfra.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.deepinfra.com/v1/openai"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.deepinfra.com/chat/overview",
		Notes:        "OpenAI-compatible chat, embeddings and image generation at /v1/openai; the Responses API shares that base. Native per-model endpoints (/v1/inference/{model}) fall outside the OpenAI shape.",
	},
	{
		// Verified 2026-11-27: the current inference base is
		// api.hyperbolic.ai/inference/v1. The provider's own docs
		// state the previous host (api.hyperbolic.xyz/v1) now answers
		// 404 — recorded here so the catalog never ships the dead URL.
		ID:          "hyperbolic",
		Name:        "Hyperbolic",
		Website:     "https://hyperbolic.xyz",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.hyperbolic.ai/inference/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://www.hyperbolic.ai/docs/inference/overview",
		Notes:        "Base moved to api.hyperbolic.ai/inference/v1; the docs state the previous api.hyperbolic.xyz/v1 host returns 404. sk_live_ keys, Bearer auth.",
	},
	{
		// Verified 2026-11-27 against nscale's own chat and
		// image-generation guides (both pin the same base).
		ID:          "nscale",
		Name:        "nScale",
		Website:     "https://nscale.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://inference.api.nscale.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.nscale.com/docs/use-cases/chat",
		Notes:        "Serverless inference: OpenAI-compatible chat and image generation on one base; service-token Bearer auth.",
	},
	{
		// Verified 2026-11-27 against Voyage's own docs: embeddings
		// only — there is no chat surface, so a chat request must be
		// refused by capability, not by a missing URL.
		ID:          "voyage-ai",
		Name:        "Voyage AI",
		Website:     "https://www.voyageai.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.voyageai.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.voyageai.com/docs/api-key-and-installation",
		Notes:        "Embeddings-only API (/v1/embeddings); no chat surface exists.",
	},
	{
		// Verified 2026-11-27 against LLM7's own quickstart, whose
		// OpenAI-SDK example pins the base.
		ID:          "llm7",
		Name:        "LLM7.io",
		Website:     "https://llm7.io",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.llm7.io/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.llm7.io/quickstart",
		Notes:        "OpenAI-compatible aggregator; the docs' OpenAI-SDK example sets base https://api.llm7.io/v1.",
	},
	{
		// Verified 2026-11-27 against Jina's own API reference:
		// embeddings, rerank and chat live on api.jina.ai; document
		// OCR is served over the chat-completions schema. Reader and
		// Search are separate hosts with their own catalog entries.
		ID:          "jina-ai",
		Name:        "Jina AI (Foundation API)",
		Website:     "https://jina.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.jina.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointRerank, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.jina.ai/",
		Notes:        "Foundation API on api.jina.ai: embeddings, rerank, and chat (document OCR uses the chat-completions schema). Reader and Search are separate hosts (r.jina.ai, s.jina.ai).",
	},
	{
		// Verified 2026-11-27 against Lambda's own inference-API docs:
		// the OpenAI-compatible gateway base, plus the legacy host the
		// same docs still reference.
		ID:          "lambda-ai",
		Name:        "Lambda AI",
		Website:     "https://lambda.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.lambda.ai/v1", "https://api.lambdalabs.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.lambda.ai/public-cloud/lambda-inference-api/",
		Notes:        "OpenAI-compatible chat/completions gateway; primary base api.lambda.ai/v1 with the legacy api.lambdalabs.com/v1 still serving the same surface.",
	},
	{
		// Verified 2026-11-27 against Featherless's own getting-started
		// and speech guides (chat + audio/speech on one base).
		ID:          "featherless-ai",
		Name:        "Featherless AI",
		Website:     "https://featherless.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.featherless.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://featherless.ai/docs/api-overview-and-common-options",
		Notes:        "OpenAI-compatible chat and audio/speech; Bearer FEATHERLESS_API_KEY.",
	},
	{
		// Verified 2026-11-27 against the Kimi API platform docs: the
		// OpenAI surface (chat + responses) shares one base, and an
		// Anthropic-format surface lives at the /anthropic path.
		ID:          "moonshot",
		Name:        "Kimi",
		Website:     "https://platform.kimi.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.moonshot.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointMessages, AuthKind: AuthBearer, HeaderName: "Authorization", BaseURL: "https://api.moonshot.ai/anthropic"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://platform.kimi.ai/docs/api/overview",
		Notes:        "OpenAI chat/responses at api.moonshot.ai/v1; Anthropic Messages at api.moonshot.ai/anthropic/v1/messages; Bearer MOONSHOT_API_KEY.",
	},
	{
		// Verified 2026-11-27 against 01.AI's own platform docs
		// (api.01.ai) and the Lingyiwanwu platform docs (the same Yi
		// family behind a second host) - both are the provider's own
		// documentation, so both bases are recorded.
		ID:          "yi",
		Name:        "Yi (01.AI)",
		Website:     "https://01.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.01.ai/v1", "https://api.lingyiwanwu.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://platform.01.ai/docs",
		Notes:        "OpenAI-compatible chat for the Yi family; two documented bases (api.01.ai and the Lingyiwanwu platform).",
	},
	{
		// Verified 2026-11-27 against Baidu's own Qianfan docs: the
		// v2 OpenAI-compatible surface (chat + embeddings).
		ID:          "qianfan",
		Name:        "Baidu Qianfan",
		Website:     "https://cloud.baidu.com/product-s/qianfan_home",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://qianfan.baidubce.com/v2"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://intl.cloud.baidu.com/doc/qianfan/index.html",
		Notes:        "OpenAI-compatible v2 surface hosting ERNIE and open models; Bearer QIANFAN_API_KEY.",
	},
	{
		// Verified 2026-11-27 against Heroku's own dev center: the
		// Managed Inference add-on exposes an INFERENCE_URL per add-on
		// with us/eu regional hosts documented - recorded as both
		// documented regions; the add-on's own INFERENCE_URL config
		// var remains authoritative per app.
		ID:          "heroku",
		Name:        "Heroku AI",
		Website:     "https://www.heroku.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://us.inference.heroku.com/v1", "https://eu.inference.heroku.com/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointRerank, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://devcenter.heroku.com/articles/openai-compatibility-chat-completions",
		Notes:        "Managed Inference add-on: OpenAI-compatible chat/embeddings/images/rerank; the app's INFERENCE_URL config var (us or eu) is authoritative - the recorded bases are the documented regions.",
	},
	{
		// Verified 2026-11-27 against Sber's own GigaChat API reference:
		// REST base gigachat.devices.sberbank.ru/api/v1 (the same page
		// documents the OpenAI-compatible message format).
		ID:          "gigachat",
		Name:        "GigaChat (Sber)",
		Website:     "https://developers.sber.ru",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://gigachat.devices.sberbank.ru/api/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://developers.sber.ru/docs/en/gigachat/api/reference",
		Notes:        "GigaChat REST API at gigachat.devices.sberbank.ru/api/v1; Bearer token from the OAuth service; message format documented as OpenAI-compatible.",
	},
	{
		// Verified 2026-11-27 against Zhipu's own BigModel docs: the
		// general API endpoint open.bigmodel.cn/api/paas/v4 (chat
		// completions and the other model APIs live under it).
		ID:          "bigmodel",
		Name:        "BigModel.cn (Zhipu)",
		Website:     "https://open.bigmodel.cn",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://open.bigmodel.cn/api/paas/v4"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.bigmodel.cn/cn/guide/develop/http/introduction",
		Notes:        "General API base https://open.bigmodel.cn/api/paas/v4 hosting the GLM family; Bearer; the Coding-plan variant uses its own dedicated endpoint per the docs.",
	},
	{
		// Verified 2026-11-27 against InternLM's own puyu docs: the
		// OpenAI-SDK-compatible chat surface lives at
		// chat.intern-ai.org.cn/api/v1 (Bearer; the same site also
		// documents a Claude-style /v1/messages surface).
		ID:          "internlm",
		Name:        "InternLM (Intern-S1)",
		Website:     "https://internlm.intern-ai.org.cn/",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://chat.intern-ai.org.cn/api/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://internlm.intern-ai.org.cn/docEn/docs/Chat/",
		Notes:        "Intern chat completions at chat.intern-ai.org.cn/api/v1 (LMDeploy-based, OpenAI-SDK compatible for chat/messages/tools/stream); 30 req/min default rate limit.",
	},
	{
		// Verified 2026-11-27 against Pollinations' own API docs (the
		// official APIDOCS in the pollinations repo and the gen-API
		// docs): OpenAI-style POST at text.pollinations.ai/openai and
		// the newer gen.pollinations.ai OpenAI-SDK base.
		ID:          "pollinations",
		Name:        "Pollinations AI",
		Website:     "https://pollinations.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://text.pollinations.ai/openai", "https://gen.pollinations.ai"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://github.com/pollinations/pollinations/blob/master/APIDOCS.md",
		Notes:        "OpenAI-compatible chat: classic POST https://text.pollinations.ai/openai (free tier, rate-limited) and the gen API base https://gen.pollinations.ai ('use an OpenAI SDK by changing its base URL').",
	},
	{
		// Verified 2026-11-27 against Volcengine's own Ark docs: the
		// data-plane base ark.cn-beijing.volces.com/api/v3 (chat,
		// responses, messages, image and video APIs live under it).
		ID:          "doubao",
		Name:        "Doubao",
		Website:     "https://doubao.com",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://ark.cn-beijing.volces.com/api/v3"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.volcengine.com/docs/ark/base-url-and-authentication?lang=zh",
		Notes:        "Ark data plane at ark.cn-beijing.volces.com/api/v3 (chat/completions, responses, messages, image/video generation); API-Key auth; region hosts vary per deployment per the docs.",
	},
	{
		// Verified 2026-11-27 against Z.ai's own devpack quick-start
		// (GLM Coding Plan endpoint guide): Anthropic Messages at
		// api.z.ai/api/anthropic, OpenAI chat completions at
		// api.z.ai/api/coding/paas/v4, OpenAI Responses at api.z.ai/api/v1.
		ID:          "glm",
		Name:        "GLM Coding",
		Website:     "https://z.ai/subscribe",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.z.ai/api/coding/paas/v4", "https://api.z.ai/api/anthropic", "https://api.z.ai/api/v1"},
		WireFormats: []WireFormat{WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization", BaseURL: "https://api.z.ai/api/v1"},
			{Endpoint: EndpointAnthropic, AuthKind: AuthBearer, HeaderName: "Authorization", BaseURL: "https://api.z.ai/api/anthropic"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.z.ai/devpack/quick-start",
		Notes:        "GLM Coding Plan serves all three protocols on dedicated bases (Anthropic Messages, OpenAI chat completions, OpenAI Responses); coding-plan API keys are separate from standard model-API keys per the docs.",
	},
	{
		// Verified 2026-11-27 against the Kimi API platform's own docs
		// (the platform doc set calls the service Kimi; the legacy
		// Moonshot API base carries the OpenAI and Anthropic surfaces).
		ID:          "kimi",
		Name:        "Kimi (Legacy Moonshot API)",
		Website:     "https://platform.kimi.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.moonshot.ai/v1", "https://api.moonshot.ai/anthropic"},
		WireFormats: []WireFormat{WireOpenAI, WireAnthropic},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointResponses, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointAnthropic, AuthKind: AuthBearer, HeaderName: "Authorization", BaseURL: "https://api.moonshot.ai/anthropic"},
			{Endpoint: EndpointModels, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://platform.kimi.ai/docs/api/overview",
		Notes:        "Kimi API platform: OpenAI chat/responses at api.moonshot.ai/v1, Anthropic Messages at api.moonshot.ai/anthropic; Bearer MOONSHOT_API_KEY (same platform the moonshot entry documents).",
	},
	{
		// Verified 2026-11-27 against Arcee's own docs: the platform
		// API is OpenAI-compatible at api.arcee.ai/api/v1.
		ID:          "arcee-ai",
		Name:        "Arcee AI",
		Website:     "https://arcee.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.arcee.ai/api/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointChat, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.arcee.ai/api-reference/your-first-api-call",
		Notes:        "OpenAI-compatible chat completions at api.arcee.ai/api/v1/chat/completions; Bearer; reasoning field surfaced on assistant messages per the docs.",
	},
	{
		// Verified 2026-11-27 against Stability's own API reference:
		// every request goes to api.stability.ai with current
		// endpoints under /v2beta (stable-image generate/edit,
		// audio, 3D), Bearer auth.
		ID:          "stability-ai",
		Name:        "Stability AI",
		Website:     "https://stability.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.stability.ai/v2beta", "https://api.stability.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointNative, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.stability.ai/api-reference",
		Notes:        "REST base api.stability.ai: v2beta hosts stable-image generate/edit (multipart, image/* or JSON base64 responses) and audio; some services are async (submit, then poll the results endpoint).",
	},
	{
		// Verified 2026-11-27 against Recraft's own quickstart: the
		// external API base external.api.recraft.ai/v1 with an
		// OpenAI-SDK-compatible images surface, Bearer token.
		ID:          "recraft",
		Name:        "Recraft",
		Website:     "https://recraft.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://external.api.recraft.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://www.recraft.ai/docs/api-reference/getting-started",
		Notes:        "OpenAI-SDK-compatible image generation at external.api.recraft.ai/v1/images/generations (the quickstart uses the OpenAI SDK against this base); Bearer RECRAFT_API_TOKEN.",
	},
	{
		// Verified 2026-11-27 against Nomic's own platform docs: the
		// Atlas embedding endpoints live under api-atlas.nomic.ai/v1.
		ID:          "nomic",
		Name:        "Nomic",
		Website:     "https://nomic.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api-atlas.nomic.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointEmbeddings, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.nomic.ai/reference/api/embed-text-v-1-embedding-text-post",
		Notes:        "Embeddings at api-atlas.nomic.ai/v1/embedding/text and /embedding/image; Bearer.",
	},
	{
		// Verified 2026-11-27 against Black Forest Labs' own docs and
		// official endpoint reference: api.bfl.ai/v1 hosts the FLUX
		// generation submit + result-polling endpoints.
		ID:          "black-forest-labs",
		Name:        "Black Forest Labs",
		Website:     "https://blackforestlabs.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://api.bfl.ai/v1"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://docs.bfl.ai/api-reference/utility/get-result",
		Notes:        "FLUX generation is async: submit to a generation endpoint, poll GET api.bfl.ai/v1/get_result with the id; API-key auth via header; region variants documented in the official endpoint reference.",
	},
	{
		// Verified 2026-11-27 against fal's own docs: the sync API at
		// fal.run and the queue API at queue.fal.run, model-scoped
		// paths under fal-ai/<model>.
		ID:          "fal-ai",
		Name:        "Fal.ai",
		Website:     "https://fal.ai",
		Category:    CategoryAPIKey,
		BaseURLs:    []string{"https://fal.run", "https://queue.fal.run"},
		WireFormats: []WireFormat{WireOpenAI},
		Endpoints: []EndpointAuth{
			{Endpoint: EndpointImages, AuthKind: AuthBearer, HeaderName: "Authorization"},
			{Endpoint: EndpointNative, AuthKind: AuthBearer, HeaderName: "Authorization"},
		},
		Verification: VerificationDocs,
		SourceURL:    "https://fal.ai/docs/api-reference/inference-api",
		Notes:        "Inference over HTTP: synchronous POST https://fal.run/<model-path> or the queue API at https://queue.fal.run (submit, then poll/stream status); Key-based auth.",
	},
}
