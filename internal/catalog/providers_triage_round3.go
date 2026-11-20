// Catalog triage fixes, round 3 — CHECKLIST items 275–276 (2026-11-18).
//
// The 227–228 audit (docs/catalog-audit-2026-11-18.md) found 228
// models.dev providers absent from the catalog; round 1
// (docs/catalog-triage-2026-11-18.md) triaged the top 46 and round 2
// (docs/catalog-triage-2026-11-18-round2.md) the next 46. This file
// lands the third batch: the next 46 candidates by models.dev
// model count (klavo … berget, 10 down to 6 models). The full
// per-provider record (triaged / added / rejected / pending, with
// probe codes and reasons) is
// docs/catalog-triage-2026-11-18-round3.md.
//
// Rules this file follows (same as rounds 1–2):
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
//     replaced. The one round-3 fill is `inference-net`: models.dev
//     lists the same provider under the ID `inference`, which is
//     rejected as a duplicate — but the existing `inference-net`
//     entry had no base URL, so it gains the probed one. The audit
//     keeps flagging `inference` as missing_in_catalog (ID
//     mismatch), exactly like the ID-level possible_rename pairs
//     and round 2's `wafer.ai`; the round-3 doc says so.
//   - An ID move (triageIDMovesRound3) applies ONLY with
//     provider-doc confirmation that the models.dev ID is the
//     canonical product identifier. The old ID is kept as the
//     entry's Alias, so `provider add-catalog <old-id>` and stored
//     configs that reference it keep resolving (via the ByAlias
//     fallback). The display name never changes in a move. A move
//     may also carry a base-URL fill (same guards as a fill) and a
//     website (only when the entry had none).
//   - A display-name rename applies ONLY with provider-doc
//     confirmation (SourceURL). The catalog ID never changes
//     except through triageIDMovesRound3 above.
//   - Website points at the provider's docs site as listed by
//     models.dev, not a separately verified homepage.
package catalog

// triageProvidersRound3 holds the new reference-only providers
// added by the 2026-11-18 catalog triage round 3. Verification is
// "from-reference-metadata-only" for every entry: base URLs came
// from models.dev api.json plus a live reachability probe, NOT from
// the provider's own docs.
var triageProvidersRound3 = []Provider{
	{
		ID:           "moark",
		Name:         "Moark",
		Website:      "https://moark.com/docs/openapi/v1#tag/%E6%96%87%E6%9C%AC%E7%94%9F%E6%88%90",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://moark.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://moark.com/docs/openapi/v1#tag/%E6%96%87%E6%9C%AC%E7%94%9F%E6%88%90",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs. The models.dev doc link itself failed to fetch from this environment, so no docs claim is made.",
	},
	{
		ID:           "sandbase",
		Name:         "SandBase",
		Website:      "https://www.sandbase.ai/docs/model-api-reference/llm-models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.sandbase.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.sandbase.ai/docs/model-api-reference/llm-models",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "teamorouter",
		Name:         "TeamoRouter",
		Website:      "https://teamorouter.com/docs/api-integration",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.teamorouter.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://teamorouter.com/docs/api-integration",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "trustedtokens",
		Name:         "TrustedTokens",
		Website:      "https://trustedtokens.eu/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.trustedtokens.eu/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://trustedtokens.eu/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "aigrow",
		Name:         "AIGROW",
		Website:      "https://aigrow.gg/ai-api/guide",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://aigrow.gg/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://aigrow.gg/ai-api/guide",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "hpc-ai",
		Name:         "HPC-AI",
		Website:      "https://www.hpc-ai.com/doc/docs/quickstart/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.hpc-ai.com/inference/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.hpc-ai.com/doc/docs/quickstart/",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "kitani",
		Name:         "Kitani",
		Website:      "https://kitani.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.kitani.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://kitani.ai/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "qihang-ai",
		Name:         "QiHang",
		Website:      "https://www.qhaigc.net/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.qhaigc.net/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.qhaigc.net/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "ruapi",
		Name:         "RuAPI",
		Website:      "https://www.ruapi.ai/pricing?lng=en",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://www.ruapi.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.ruapi.ai/pricing?lng=en",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "runinfra",
		Name:         "RunInfra",
		Website:      "https://runinfra.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.runinfra.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://runinfra.ai/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "submodel",
		Name:         "submodel",
		Website:      "https://submodel.gitbook.io",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://llm.submodel.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://submodel.gitbook.io",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "tinfoil",
		Name:         "Tinfoil",
		Website:      "https://docs.tinfoil.sh",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://inference.tinfoil.sh/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.tinfoil.sh",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Tinfoil docs confirm an OpenAI-compatible API but state no base URL in fetched text.",
	},
	{
		ID:           "umans-ai",
		Name:         "Umans AI",
		Website:      "https://app.umans.ai/offers/code/docs/orgs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.code.umans.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://app.umans.ai/offers/code/docs/orgs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. The umans-ai-coding-plan billing plan (rejected in this round) shares this base URL; the provider entry carries it.",
	},
	{
		ID:           "wally",
		Name:         "Wally",
		Website:      "https://docs.runanywhere.ai/wally-cloud/api",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://inference.runanywhere.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.runanywhere.ai/wally-cloud/api",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Wally is RunAnywhere's cloud inference product; its docs confirm an OpenAI-compatible API with a cloud API key but state no base URL in fetched text.",
	},
	{
		ID:           "zerogpu",
		Name:         "ZeroGPU",
		Website:      "https://docs.zerogpu.ai/docs/model-catalog",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.zerogpu.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.zerogpu.ai/docs/model-catalog",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "aki-io",
		Name:         "AKI.IO",
		Website:      "https://aki.io/docs/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://aki.io/openai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://aki.io/docs/",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs.",
	},
	{
		ID:           "bothub",
		Name:         "Bothub",
		Website:      "https://bothub.ru/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://openai.bothub.ru/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://bothub.ru/models",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "hostinger-router",
		Name:         "Hostinger Router",
		Website:      "https://www.hostinger.com/ai-router",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://router.hostinger.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.hostinger.com/ai-router",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Hostinger's page confirms an OpenAI-compatible router with one API key but states no base URL in fetched text.",
	},
	{
		ID:           "inference-coop",
		Name:         "Inference Cooperative",
		Website:      "https://inference.coop",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://gateway.inference.coop/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://inference.coop",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "melious",
		Name:         "Melious",
		Website:      "https://melious.ai/docs/reference/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.melious.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://melious.ai/docs/reference/models",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "vancine",
		Name:         "Vancine",
		Website:      "https://vancine.com/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://vancine.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://vancine.com/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "adaption",
		Name:         "Adaption",
		Website:      "https://docs.adaptionlabs.ai",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.prod.adaptionlabs.ai/api/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.adaptionlabs.ai",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs.",
	},
	{
		ID:           "graphn",
		Name:         "GraphN",
		Website:      "https://graphn.ai/docs/getting-started",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://model.graphn.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://graphn.ai/docs/getting-started",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs.",
	},
	{
		ID:           "inco",
		Name:         "Inco",
		Website:      "https://platform.inco.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.inco.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://platform.inco.ai/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "powerspring",
		Name:         "Powerspring",
		Website:      "https://powerspring.eu/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.powerspring.eu/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://powerspring.eu/models",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "stepfun-ai",
		Name:         "StepFun (Global)",
		Website:      "https://platform.stepfun.ai/docs/en/overview/concept",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.stepfun.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://platform.stepfun.ai/docs/en/overview/concept",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Global surface; the catalog `stepfun` entry is the China surface (api.stepfun.com/v1) — same siliconflow/siliconflow-cn regional-surface precedent as round 1. StepFun's global docs state no base URL in fetched text.",
	},
	{
		ID:           "teapot-router",
		Name:         "Teapot Router",
		Website:      "https://teapotrouter.com/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.teapotrouter.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://teapotrouter.com/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "trustedrouter",
		Name:         "TrustedRouter",
		Website:      "https://trustedrouter.com/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.trustedrouter.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://trustedrouter.com/docs",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs.",
	},
	{
		ID:           "augure",
		Name:         "Augure",
		Website:      "https://augureai.ca/docs/api",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.augureai.ca/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://augureai.ca/docs/api",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs.",
	},
	{
		ID:           "bee",
		Name:         "Bee by HEOSSI",
		Website:      "https://bee.heossi.com/docs/sdks",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.bee.heossi.com/bee"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://bee.heossi.com/docs/sdks",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Base has no /v1 suffix (path /bee).",
	},
	{
		ID:           "berget",
		Name:         "Berget.AI",
		Website:      "https://api.berget.ai",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.berget.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://api.berget.ai",
		Notes:        "Added 2026-11-18 triage round 3: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
}

// triageBaseURLFixesRound3 holds the round-3 base-URL fills on
// existing reference entries (same shape and guards as rounds 1–2).
var triageBaseURLFixesRound3 = []triageBaseURLFix{
	{ID: "inference-net", BaseURLs: []string{"https://inference.net/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage round 3: models.dev lists the same provider under the ID `inference` (rejected as a duplicate in this round); models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
}

// triageIDMove is one docs-proven catalog ID move: the catalog ID
// changes to the models.dev ID, the old ID is kept as the Alias so
// `provider add-catalog <old-id>` and stored configs keep
// resolving, and the display name never changes. Website is applied
// only when non-empty and the entry had none; BaseURLs is applied
// only when the entry is reference-only and records no base URL.
type triageIDMove struct {
	OldID     string
	NewID     string
	NewAlias  string
	Website   string
	BaseURLs  []string
	SourceURL string
	Probe     string
	Note      string
}

// triageIDMovesRound3 holds the four possible_rename pairs the
// providers' own docs prove in round 3. Evidence for each move is
// in docs/catalog-triage-2026-11-18-round3.md; the other eight
// pairs stay pending (five undecided, three decided-keep) with
// reasons recorded there.
var triageIDMovesRound3 = []triageIDMove{
	{
		OldID:     "cloudflare-ai",
		NewID:     "cloudflare-workers-ai",
		NewAlias:  "cloudflare-ai",
		SourceURL: "https://models.dev/api.json",
		Probe:     "404",
		Note:      "Catalog ID moved 2026-11-18 triage round 3: Cloudflare's own docs brand the product Workers AI (developers.cloudflare.com/workers-ai) and the catalog name already is Cloudflare Workers AI, so the models.dev ID is the canonical product identifier; the old ID stays as the alias (replacing the short alias \"cloudflare\"). The entry is verified-from-docs and already carries the same templated base (https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1), so no base URL is filled or changed here.",
	},
	{
		OldID:     "snowflake",
		NewID:     "snowflake-cortex",
		NewAlias:  "snowflake",
		BaseURLs:  []string{"https://${SNOWFLAKE_ACCOUNT}.snowflakecomputing.com/api/v2/cortex/v1"},
		SourceURL: "https://models.dev/api.json",
		Probe:     "template",
		Note:      "Catalog ID moved 2026-11-18 triage round 3: Snowflake's own Cortex REST API docs (docs.snowflake.com/en/user-guide/snowflake-cortex/cortex-rest-api) brand the product Snowflake Cortex and give the OpenAI-compatible Chat Completions base https://<account-identifier>.snowflakecomputing.com/api/v2/cortex/v1 with Bearer PAT auth, so the models.dev ID is the canonical product identifier; the old ID stays as the alias. Base URL is a template — substitute your Snowflake account identifier; not probed (the host itself is templated).",
	},
	{
		OldID:     "github",
		NewID:     "github-copilot",
		NewAlias:  "github",
		Website:   "https://docs.github.com/en/copilot",
		BaseURLs:  []string{"https://api.githubcopilot.com"},
		SourceURL: "https://models.dev/api.json",
		Probe:     "404",
		Note:      "Catalog ID moved 2026-11-18 triage round 3: GitHub's own docs brand the product GitHub Copilot (docs.github.com/en/copilot) and the API host is api.githubcopilot.com, so the models.dev ID is the canonical product identifier — the old ID was the company, not the product; the old ID stays as the alias and the entry gains the docs site as its website. Base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		OldID:     "bailian-coding-plan",
		NewID:     "alibaba-token-plan",
		NewAlias:  "bailian-coding-plan",
		BaseURLs:  []string{"https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"},
		SourceURL: "https://models.dev/api.json",
		Probe:     "404",
		Note:      "Catalog ID moved 2026-11-18 triage round 3: the entry's own name is already Alibaba Token Plan and its website already is Alibaba Cloud's token-plan overview (www.alibabacloud.com/help/en/model-studio/token-plan-overview) — Bailian is the legacy Chinese brand for Model Studio — so the models.dev ID matches the product the entry already describes; the old ID stays as the alias. Base URL from models.dev api.json + live reachability probe HTTP 404; regional (ap-southeast-1), substitute your region.",
	},
}
