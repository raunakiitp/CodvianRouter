// Catalog triage fixes, round 4 — CHECKLIST items 281–282 (2026-11-18).
//
// The 2026-11-18 audit found 228 models.dev providers absent from the
// catalog; rounds 1–3 triaged 138 of them (klavo … berget). This file
// lands the fourth batch: the next 46 candidates by models.dev model
// count (dinference … nokeep, 6 down to 3 models), plus the one
// possible rename the providers' own docs prove (novita → novita-ai).
// The full per-provider record (triaged / added / rejected / pending,
// with probe codes and reasons) is
// docs/catalog-triage-2026-11-18-round4.md.
//
// Rules this file follows (same as rounds 1–3):
//   - A new provider is added ONLY when models.dev lists it AND a
//     live reachability probe of its base URL returned an HTTP
//     response (any status: 401/403/404 on a bare base URL is the
//     normal answer of a live API). Its Verification stays
//     "from-reference-metadata-only" unless the provider's own docs
//     were read — docs-verified additions live in verified.go,
//     never here.
//   - A base-URL fill only applies to a reference entry that
//     records no base URL (see applyTriageFixes). Verified-from-docs
//     entries are never touched, and an existing base URL is never
//     replaced. The two round-4 fills are `xiaomi-mimo` and
//     `friendliai`: models.dev lists the same providers under the
//     IDs `xiaomi` and `friendli`, which are rejected as duplicates
//     — but the existing entries had no base URL, so they gain the
//     probed ones. The audit keeps flagging `xiaomi` and `friendli`
//     as missing_in_catalog (ID mismatch), exactly like the
//     ID-level possible_rename pairs and round 3's `inference`.
//   - An ID move (triageIDMovesRound4) applies ONLY with
//     provider-doc confirmation that the models.dev ID is the
//     canonical product identifier. The old ID is kept as the
//     entry's Alias, so `provider add-catalog <old-id>` and stored
//     configs that reference it keep resolving (via the ByAlias
//     fallback). The display name never changes in a move. A move
//     may also carry a base-URL fill (same guards as a fill).
//   - A display-name rename applies ONLY with provider-doc
//     confirmation (SourceURL). The catalog ID never changes
//     except through triageIDMovesRound4 above.
//   - Website points at the provider's docs site as listed by
//     models.dev, not a separately verified homepage.
package catalog

// triageProvidersRound4 holds the new reference-only providers
// added by the 2026-11-18 catalog triage round 4. Verification is
// "from-reference-metadata-only" for every entry: base URLs came
// from models.dev api.json plus a live reachability probe, NOT from
// the provider's own docs.
var triageProvidersRound4 = []Provider{
	{
		ID:           "dinference",
		Name:         "DInference",
		Website:      "https://dinference.com",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.dinference.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://dinference.com",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "inceptron",
		Name:         "Inceptron",
		Website:      "https://docs.inceptron.io",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.inceptron.io/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.inceptron.io",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "inferall",
		Name:         "InferAll",
		Website:      "https://inferall.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.inferall.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://inferall.ai/docs",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "infersia",
		Name:         "Infersia",
		Website:      "https://infersia.com/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.infersia.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://infersia.com/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "moonshotai",
		Name:         "Moonshot AI",
		Website:      "https://platform.moonshot.ai/docs/api/chat",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.moonshot.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://platform.moonshot.ai/docs/api/chat",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Moonshot's own docs confirm the chat-completions API with Bearer MOONSHOT_API_KEY auth but state no base URL in the fetched text. Global surface; catalog `moonshotai-cn` is the China surface (api.moonshot.cn/v1), the same regional-surface precedent as round 3's stepfun/stepfun-ai. The legacy catalog entries `moonshot` and `kimi` (no base URL, different IDs) are left untouched.",
	},
	{
		ID:           "moonshotai-cn",
		Name:         "Moonshot AI (China)",
		Website:      "https://platform.moonshot.cn/docs/api/chat",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.moonshot.cn/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://platform.moonshot.cn/docs/api/chat",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. China surface; catalog `moonshotai` is the global surface (api.moonshot.ai/v1).",
	},
	{
		ID:           "pendra",
		Name:         "Pendra",
		Website:      "https://pendra.ai/docs/integrations/opencode",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.pendra.ai/api/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://pendra.ai/docs/integrations/opencode",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "refuseless",
		Name:         "Refuseless",
		Website:      "https://docs.refuseless.com/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.refuseless.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.refuseless.com/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 403 (curl; the first probe client saw the connection close without a response, the retry answered 403 twice); not verified against the provider's own docs.",
	},
	{
		ID:           "ai-router",
		Name:         "AI-ROUTER",
		Website:      "https://ai-router.dev/openai-compatible-api-gateway/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.ai-router.dev/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://ai-router.dev/openai-compatible-api-gateway/",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "atomic-chat",
		Name:         "Atomic Chat",
		Website:      "https://atomic.chat",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"http://127.0.0.1:1337/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://atomic.chat",
		Notes:        "Added 2026-11-18 triage round 4: local loopback default from models.dev (http://127.0.0.1:1337/v1); loopback, not probed from here — the lm-studio precedent from round 1. Not verified against the provider's own docs.",
	},
	{
		ID:           "bailing",
		Name:         "Bailing",
		Website:      "https://developer.ant-ling.com/zh-CN/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.ant-ling.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://developer.ant-ling.com/zh-CN/docs",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "cheapaimodel",
		Name:         "cheapaimodel",
		Website:      "https://cheapaimodel.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.cheapaimodel.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://cheapaimodel.ai/docs",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "cloudferro-sherlock",
		Name:         "CloudFerro Sherlock",
		Website:      "https://docs.sherlock.cloudferro.com/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api-sherlock.cloudferro.com/openai/v1/"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.sherlock.cloudferro.com/",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs.",
	},
	{
		ID:           "ezkielyna",
		Name:         "Ezkielyna",
		Website:      "https://ezkielyna.store/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.ezkielyna.store/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://ezkielyna.store/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs.",
	},
	{
		ID:           "munito",
		Name:         "Munito AI",
		Website:      "https://docs.munito.ai/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.munito.ai/inference/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.munito.ai/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Catalog `munito-eu` (added in this round) is the EU surface.",
	},
	{
		ID:           "sokkan",
		Name:         "SOKKAN Router",
		Website:      "https://router.sokkan.ch/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://router.sokkan.ch/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://router.sokkan.ch/docs",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "tokenify",
		Name:         "Tokenify",
		Website:      "https://www.tokenify.dev/docs/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.tokenify.dev/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.tokenify.dev/docs/",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "zro",
		Name:         "Zro",
		Website:      "https://zro.moonmath.ai/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://zro.moonmath.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://zro.moonmath.ai/",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "abliteration-ai",
		Name:         "abliteration.ai",
		Website:      "https://docs.abliteration.ai/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.abliteration.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.abliteration.ai/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "ebcloud",
		Name:         "EBCloud",
		Website:      "https://docs.ebtech.com/ai/model-api.html",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://maas-api.ebcloud.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.ebtech.com/ai/model-api.html",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "entrim",
		Name:         "Entrim",
		Website:      "https://docs.entrim.ai/model-api/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.entrim.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.entrim.ai/model-api/",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "inferris",
		Name:         "Inferris",
		Website:      "https://inferris.ai/developers.md",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.inferris.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://inferris.ai/developers.md",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "lucidquery",
		Name:         "LucidQuery",
		Website:      "https://lucidquery.com/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.lucidquery.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://lucidquery.com/docs",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "modular",
		Name:         "Modular",
		Website:      "https://docs.modular.com/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.modular.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.modular.com/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs. Modular's own docs confirm shared (serverless) endpoints but state no base URL in the fetched text.",
	},
	{
		ID:           "munito-eu",
		Name:         "Munito AI (EU)",
		Website:      "https://docs.munito.ai/regions",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.eu.munito.ai/inference/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.munito.ai/regions",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. EU surface; catalog `munito` (added in this round) is the global surface.",
	},
	{
		ID:           "proxystacks",
		Name:         "ProxyStacks Relay",
		Website:      "https://proxystacks.com/en/docs/opencode",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://proxystacks.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://proxystacks.com/en/docs/opencode",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "sakana",
		Name:         "Sakana AI",
		Website:      "https://console.sakana.ai/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.sakana.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://console.sakana.ai/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "tencent-tokenhub",
		Name:         "Tencent TokenHub",
		Website:      "https://cloud.tencent.com/document/product/1823/130050",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://tokenhub.tencentmaas.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://cloud.tencent.com/document/product/1823/130050",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Tencent's TokenHub MaaS surface with its own API host; the alibaba-token-plan precedent (a token-plan entry with its own regional base) is why it is added rather than rejected as a plan — unlike round 3's minimax-coding-plan, which shared minimax's exact base URL.",
	},
	{
		ID:           "tenstorrent",
		Name:         "Tenstorrent",
		Website:      "https://console.tenstorrent.com",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://console.tenstorrent.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://console.tenstorrent.com",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "unrestricted",
		Name:         "Unrestricted",
		Website:      "https://unrestricted.sh/reference",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.unrestricted.sh/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://unrestricted.sh/reference",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "anonrouter",
		Name:         "AnonRouter",
		Website:      "https://docs.anonrouter.ai/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.anonrouter.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.anonrouter.ai/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "apiko",
		Name:         "APIKO",
		Website:      "https://apiko.app/docs/introduction",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.apiko.app/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://apiko.app/docs/introduction",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 301 (redirects to http://api.apiko.app/v1/ — a live host answering, so the models.dev base is kept as listed); not verified against the provider's own docs.",
	},
	{
		ID:           "jad-ai",
		Name:         "Jad AI",
		Website:      "https://jadai.freesrv.com/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://jadai.freesrv.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://jadai.freesrv.com/docs",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "joingonka",
		Name:         "JoinGonka",
		Website:      "https://gate.joingonka.ai/docs/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://gate.joingonka.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://gate.joingonka.ai/docs/models",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "klokintegration",
		Name:         "klokintegration.se",
		Website:      "https://klokintegration.se/docs/ai-api",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api-gw.klok.ipaas.se/proxy/kloker-key/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://klokintegration.se/docs/ai-api",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs.",
	},
	{
		ID:           "lithosai",
		Name:         "LithosAI",
		Website:      "https://docs.lithosai.com",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.lithosai.cloud/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.lithosai.com",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "nokeep",
		Name:         "nokeep",
		Website:      "https://nokeep.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://nokeep.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://nokeep.ai/docs",
		Notes:        "Added 2026-11-18 triage round 4: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
}

// triageBaseURLFixesRound4 holds the round-4 base-URL fills on
// existing reference-only entries that record no base URL. Both
// targets are the duplicates rejected in this round: models.dev
// lists the same providers under the IDs `xiaomi` and `friendli`.
// This is NOT one of the audit's 44 missing base URLs — the audit
// never paired catalog `xiaomi-mimo`/`friendliai` with models.dev
// `xiaomi`/`friendli` (ID mismatch) — so the round-1 JSON ledger
// (43 filled + 1 skipped = 44) is left untouched and these fills
// are recorded here instead, exactly like round 3's `inference-net`
// fill.
var triageBaseURLFixesRound4 = []triageBaseURLFix{
	{ID: "xiaomi-mimo", BaseURLs: []string{"https://api.xiaomimimo.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage round 4: models.dev lists the same provider (Xiaomi MiMo models) under the ID `xiaomi` (rejected as a duplicate in this round); models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "friendliai", BaseURLs: []string{"https://api.friendli.ai/serverless/v1"}, SourceURL: "https://models.dev/api.json", Probe: "400", Note: "Base URL added 2026-11-18 triage round 4: models.dev lists the same provider under the ID `friendli` (rejected as a duplicate in this round — the catalog entry already carries the alias \"friendli\"); models.dev api.json + live reachability probe HTTP 400; not verified against the provider's own docs. Friendli's own docs confirm an OpenAI-compatible serverless API but state no base URL in the fetched text."},
}

// triageIDMovesRound4 holds the one possible_rename pair the
// providers' own docs prove in round 4. Evidence is in
// docs/catalog-triage-2026-11-18-round4.md; the other four pairs
// stay pending (three undecided, one decided-keep) with reasons
// recorded there.
var triageIDMovesRound4 = []triageIDMove{
	{
		OldID:     "novita",
		NewID:     "novita-ai",
		NewAlias:  "novita",
		BaseURLs:  []string{"https://api.novita.ai/openai"},
		SourceURL: "https://models.dev/api.json",
		Probe:     "404",
		Note:      "Catalog ID moved 2026-11-18 triage round 4: Novita's own docs publish `novita-ai` as the identifier for its API surface (novita.ai/docs/guides/introduction: the official skill is `novita-ai`, `npx skills add novitalabs/novita-skills --skill novita-ai`), the brand is Novita AI and the API host is api.novita.ai, so the models.dev ID is the canonical product identifier; the old ID stays as the alias. Base URL from models.dev api.json + live reachability probe HTTP 404 (note the /openai path, no /v1 suffix); not verified against the provider's own docs for the base itself.",
	},
}
