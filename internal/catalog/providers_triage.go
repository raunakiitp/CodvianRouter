// Catalog triage fixes — CHECKLIST items 253-254 (2026-11-18).
//
// The 227-228 audit (docs/catalog-audit-2026-11-18.md) found 228
// models.dev providers absent from the catalog, 44 catalog entries
// with no base URL, and 21 renames. This file lands the first
// verified batch of fixes. Every fact here cites its source; the
// full per-provider record (triaged / added / rejected / pending,
// with probe codes and reasons) is internal catalog triage record.
//
// Rules this file follows:
//   - A new provider is added ONLY when models.dev lists it AND a
//     live reachability probe of its base URL returned an HTTP
//     response (any status: 401/403/404 on a bare base URL is the
//     normal answer of a live API). Its Verification stays
//     "from-reference-metadata-only" unless the provider's own docs
//     were read — docs-verified additions live in verified.go,
//     never here.
//   - A base-URL fill only applies to a reference entry that
//     records no base URL (see applyTriageFixes in this file).
//     Verified-from-docs entries are never touched, and an
//     existing base URL is never replaced. `pioneer` is
//     deliberately NOT filled: models.dev points it at
//     api.fastino.ai, which belongs to a different provider
//     (Fastino) and looks like a models.dev data error.
//   - A display-name rename applies ONLY with provider-doc
//     confirmation (SourceURL). The catalog ID never changes.
//   - Website points at the provider's docs site as listed by
//     models.dev, not a separately verified homepage.
package catalog

// triageProviders holds the new providers added by the 2026-11-18
// catalog triage. Verification is "from-reference-metadata-only"
// for every entry: base URLs came from models.dev api.json plus a
// live reachability probe, NOT from the provider's own docs.
var triageProviders = []Provider{
	{
		ID:           "edenai",
		Name:         "Eden AI",
		Website:      "https://docs.edenai.co",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.edenai.run/v3"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.edenai.co",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Eden AI docs confirm an OpenAI-compatible API; the /v3 path itself comes from models.dev + probe.",
	},
	{
		ID:           "aihubmix",
		Name:         "AIHubMix",
		Website:      "https://docs.aihubmix.com",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://aihubmix.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.aihubmix.com",
		Notes:        "Added 2026-11-18 triage: models.dev lists no api; base corroborated by live GET https://aihubmix.com/v1/models -> 200; not verified against the provider's own docs. models.dev lists no api; base corroborated by live GET https://aihubmix.com/v1/models -> 200. Docs confirm OpenAI/Gemini/Anthropic-compatible interfaces.",
	},
	{
		ID:           "abacus",
		Name:         "Abacus",
		Website:      "https://abacus.ai/help/api",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://routellm.abacus.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://abacus.ai/help/api",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Abacus docs page is JS-rendered and states no base URL in fetched text; base from models.dev + probe.",
	},
	{
		ID:           "cortecs",
		Name:         "Cortecs",
		Website:      "https://api.cortecs.ai/v1/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.cortecs.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://api.cortecs.ai/v1/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. GET https://api.cortecs.ai/v1/models -> 200 (live model list).",
	},
	{
		ID:           "impossibl",
		Name:         "Impossibl",
		Website:      "https://impossibl.com/docs/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.impossibl.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://impossibl.com/docs/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "crossmodel",
		Name:         "CrossModel",
		Website:      "https://www.crossmodel.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.crossmodel.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.crossmodel.ai/docs",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "jiekou",
		Name:         "Jiekou.AI",
		Website:      "https://docs.jiekou.ai",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.jiekou.ai/openai"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.jiekou.ai",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. No /v1 suffix on this base.",
	},
	{
		ID:           "nearai",
		Name:         "NEAR AI Cloud",
		Website:      "https://docs.near.ai/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://cloud-api.near.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.near.ai/",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "frogbot",
		Name:         "FrogBot",
		Website:      "https://docs.frogbot.ai",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://app.frogbot.ai/api/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.frogbot.ai",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs.",
	},
	{
		ID:           "siliconflow-cn",
		Name:         "SiliconFlow (China)",
		Website:      "https://cloud.siliconflow.com/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.siliconflow.cn/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://cloud.siliconflow.com/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. China regional variant of siliconflow (api.siliconflow.com).",
	},
	{
		ID:           "lowrouter",
		Name:         "LowRouter",
		Website:      "https://lowrouter.ai/docs/models/available",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.lowrouter.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://lowrouter.ai/docs/models/available",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "standardcompute",
		Name:         "Standard Compute",
		Website:      "https://docs.standardcompute.com/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.stdcmpt.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.standardcompute.com/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "thinkingmachines",
		Name:         "Thinking Machines",
		Website:      "https://tinker-docs.thinkingmachines.ai/tinker/compatible-apis/anthropic/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://tinker.thinkingmachines.dev/services/tinker-prod/anthropic/api/v1"},
		WireFormats:  []WireFormat{WireAnthropic},
		Verification: VerificationReference,
		SourceURL:    "https://tinker-docs.thinkingmachines.ai/tinker/compatible-apis/anthropic/",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Tinker Anthropic-compatible API per Thinking Machines docs; auth scheme not verified beyond the API-key default.",
	},
	{
		ID:           "vivgrid",
		Name:         "Vivgrid",
		Website:      "https://docs.vivgrid.com/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.vivgrid.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.vivgrid.com/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs.",
	},
	{
		ID:           "llmtr",
		Name:         "LLMTR",
		Website:      "https://llmtr.com/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://llmtr.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://llmtr.com/docs",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "iteracompute",
		Name:         "IteraCompute",
		Website:      "https://iteracompute.com/docs.html",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.iteracompute.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://iteracompute.com/docs.html",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "tensorx",
		Name:         "TensorX",
		Website:      "https://docs.tensorx.ai/",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.tensorx.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.tensorx.ai/",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 403; not verified against the provider's own docs.",
	},
	{
		ID:           "neuralwatt",
		Name:         "Neuralwatt",
		Website:      "https://portal.neuralwatt.com/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.neuralwatt.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://portal.neuralwatt.com/docs",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "jzstoken",
		Name:         "JZSTOKEN",
		Website:      "https://jzstoken.com",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://jzstoken.com/api/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://jzstoken.com",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs.",
	},
	{
		ID:           "steadyrouter",
		Name:         "SteadyRouter",
		Website:      "https://www.steadyrouter.com/site/docs.html",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.steadyrouter.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://www.steadyrouter.com/site/docs.html",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "boundless",
		Name:         "Boundless",
		Website:      "https://inference.boundless.network/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.inference.boundless.network/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://inference.boundless.network/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "bumo",
		Name:         "Bumo Router",
		Website:      "https://bumo.ai/docs/api",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://router.bumo.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://bumo.ai/docs/api",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "cheapest-token",
		Name:         "CheapestToken",
		Website:      "https://cheapest-token.com/docs/opencode",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.cheapest-token.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://cheapest-token.com/docs/opencode",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "korouter",
		Name:         "KoRouter",
		Website:      "https://korouter.ai/docs",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://korouter.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://korouter.ai/docs",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "meganova",
		Name:         "Meganova",
		Website:      "https://docs.meganova.ai",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.meganova.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.meganova.ai",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "phala",
		Name:         "Phala",
		Website:      "https://docs.phala.com/phala-cloud/confidential-ai/confidential-model/confidential-ai-api",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://inference.phala.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.phala.com/phala-cloud/confidential-ai/confidential-model/confidential-ai-api",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Confidential-AI (TEE) inference.",
	},
	{
		ID:           "sparrowcodes",
		Name:         "SparrowCodes",
		Website:      "https://sparrowcodes.com/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.sparrowcodes.com/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://sparrowcodes.com/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs.",
	},
	{
		ID:           "doubleword",
		Name:         "Doubleword",
		Website:      "https://docs.doubleword.ai/inference-api/models",
		Category:     CategoryAPIKey,
		BaseURLs:     []string{"https://api.doubleword.ai/v1"},
		WireFormats:  []WireFormat{WireOpenAI},
		Verification: VerificationReference,
		SourceURL:    "https://docs.doubleword.ai/inference-api/models",
		Notes:        "Added 2026-11-18 triage: base URL from models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs.",
	},
}

// triageBaseURLFix fills a missing base URL on an existing
// reference entry. SourceURL is where the URL came from; Probe is
// the HTTP status of the 2026-11-18 live probe ("docs" when the
// provider's own docs state the URL, "template" / "loopback"
// when the URL cannot be probed).
type triageBaseURLFix struct {
	ID        string
	BaseURLs  []string
	SourceURL string
	Probe     string
	Note      string
}

var triageBaseURLFixes = []triageBaseURLFix{
	{ID: "agentrouter", BaseURLs: []string{"https://agentrouter.org/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "unorouter", BaseURLs: []string{"https://api.unorouter.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "403", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 403; not verified against the provider's own docs."},
	{ID: "command-code", BaseURLs: []string{"https://api.commandcode.ai/provider/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "synthetic", BaseURLs: []string{"https://api.synthetic.new/openai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "200", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs."},
	{ID: "opencode-go", BaseURLs: []string{"https://opencode.ai/inference/go/openai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. OpenCode Go subscription surface; distinct from the opencode pay-as-you-go base."},
	{ID: "nanogpt", BaseURLs: []string{"https://nano-gpt.com/api/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "aimlapi", BaseURLs: []string{"https://api.aimlapi.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "chutes", BaseURLs: []string{"https://llm.chutes.ai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "perplexity-agent", BaseURLs: []string{"https://api.perplexity.ai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Same base as Perplexity's API; agent surface."},
	{ID: "morph", BaseURLs: []string{"https://api.morphllm.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs."},
	{ID: "ai21", BaseURLs: []string{"https://api.ai21.com/studio/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "upstage", BaseURLs: []string{"https://api.upstage.ai/v1/solar"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Solar path: /v1/solar."},
	{ID: "liquid", BaseURLs: []string{"https://router.inference.ai.exchange/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "nebius", BaseURLs: []string{"https://api.tokenfactory.nebius.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Product is Nebius Token Factory."},
	{ID: "siliconflow", BaseURLs: []string{"https://api.siliconflow.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "paraloncloud", BaseURLs: []string{"https://paraloncloud.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "200", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs."},
	{ID: "runcrate", BaseURLs: []string{"https://api.runcrate.ai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs."},
	{ID: "sambanova", BaseURLs: []string{"https://api.sambanova.ai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "405", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 405; not verified against the provider's own docs."},
	{ID: "baseten", BaseURLs: []string{"https://inference.baseten.co/v1"}, SourceURL: "https://models.dev/api.json", Probe: "405", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 405; not verified against the provider's own docs."},
	{ID: "wandb", BaseURLs: []string{"https://api.inference.wandb.ai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs. Renamed to CoreWeave in the same triage (see triageRenames); the wandb.ai host still responds."},
	{ID: "hetzner", BaseURLs: []string{"https://inference.hetzner.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs."},
	{ID: "modelscope", BaseURLs: []string{"https://api-inference.modelscope.cn/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "byteplus", BaseURLs: []string{"https://ark.ap-southeast.bytepluses.com/api/v3"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs. Regional endpoint (ap-southeast in models.dev); substitute your region."},
	{ID: "digitalocean", BaseURLs: []string{"https://inference.do-ai.run/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "oci", BaseURLs: []string{"https://inference.generativeai.us-chicago-1.oci.oraclecloud.com/openai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Regional endpoint (us-chicago-1 in models.dev); substitute your region."},
	{ID: "modal", BaseURLs: []string{"https://inference.us-west.modal.direct/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Regional endpoint (us-west in models.dev); substitute your region."},
	{ID: "scaleway", BaseURLs: []string{"https://api.scaleway.ai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "ovhcloud", BaseURLs: []string{"https://oai.endpoints.kepler.ai.cloud.ovh.net/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs. Renamed to OVHcloud AI Endpoints in the same triage; OVHcloud docs confirm this base URL."},
	{ID: "databricks", BaseURLs: []string{"https://{DATABRICKS_HOST}/ai-gateway/mlflow/v1"}, SourceURL: "https://models.dev/api.json", Probe: "template", Note: "Base URL added 2026-11-18 triage: template from models.dev api.json; not probed. Template: substitute your Databricks workspace host. Not probed (template)."},
	{ID: "minimax", BaseURLs: []string{"https://api.minimax.io/v1"}, SourceURL: "https://platform.minimax.io/docs/guides/text-generation", Probe: "docs", Note: "Base URL added 2026-11-18 triage: from the provider's own docs (see Note); not from models.dev. OpenAI-compatible base per MiniMax's own docs (platform.minimax.io); models.dev /anthropic/v1 is wrong (extra /v1). Anthropic surface is https://api.minimax.io/anthropic."},
	{ID: "minimax-cn", BaseURLs: []string{"https://api.minimax.cn/v1"}, SourceURL: "https://platform.minimax.io/docs/guides/text-generation", Probe: "docs", Note: "Base URL added 2026-11-18 triage: from the provider's own docs (see Note); not from models.dev. OpenAI-compatible base following the global MiniMax docs' pattern; the cn Anthropic surface is not verified."},
	{ID: "zai", BaseURLs: []string{"https://api.z.ai/api/paas/v4"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs."},
	{ID: "alibaba", BaseURLs: []string{"https://dashscope-intl.aliyuncs.com/compatible-mode/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "alibaba-cn", BaseURLs: []string{"https://dashscope.aliyuncs.com/compatible-mode/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "longcat", BaseURLs: []string{"https://api.longcat.chat/openai"}, SourceURL: "https://models.dev/api.json", Probe: "200", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 200; not verified against the provider's own docs. No /v1 suffix on this base."},
	{ID: "volcengine", BaseURLs: []string{"https://ark.cn-beijing.volces.com/api/v3"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs. Regional endpoint (cn-beijing in models.dev); substitute your region."},
	{ID: "volcengine-coding-plan", BaseURLs: []string{"https://ark.cn-beijing.volces.com/api/coding/v3"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs. Regional endpoint (cn-beijing in models.dev); substitute your region."},
	{ID: "stepfun", BaseURLs: []string{"https://api.stepfun.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "sensenova", BaseURLs: []string{"https://token.sensenova.cn/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "agnes", BaseURLs: []string{"https://apihub.agnes-ai.com/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "sarvam", BaseURLs: []string{"https://api.sarvam.ai/v1"}, SourceURL: "https://models.dev/api.json", Probe: "404", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 404; not verified against the provider's own docs."},
	{ID: "clinepass", BaseURLs: []string{"https://api.cline.bot/api/v1"}, SourceURL: "https://models.dev/api.json", Probe: "401", Note: "Base URL added 2026-11-18 triage: models.dev api.json + live reachability probe HTTP 401; not verified against the provider's own docs."},
	{ID: "lm-studio", BaseURLs: []string{"http://127.0.0.1:1234/v1"}, SourceURL: "https://models.dev/api.json", Probe: "loopback", Note: "Base URL added 2026-11-18 triage: local loopback default from models.dev; not probed from here. Local loopback default per models.dev (LM Studio's documented local server); loopback, not probed from here."},
}

// triageRename changes a display name ONLY where the provider's
// own docs confirm the new name. SourceURL is the confirming doc.
type triageRename struct {
	ID        string
	NewName   string
	SourceURL string
	Note      string
}

var triageRenames = []triageRename{
	{ID: "wandb", NewName: "CoreWeave", SourceURL: "https://www.coreweave.com/companies/weights-and-biases", Note: "Display name changed 2026-11-18 triage: Formerly Weights & Biases Inference; CoreWeave completed the W&B acquisition in May 2025 and the products are folding into CoreWeave Forge."},
	{ID: "ovhcloud", NewName: "OVHcloud AI Endpoints", SourceURL: "https://docs.ovhcloud.com/en/guides/public-cloud/ai-machine-learning/ai-endpoints-responses-api", Note: "Display name changed 2026-11-18 triage: Product name per OVHcloud's own docs."},
	{ID: "minimax", NewName: "MiniMax", SourceURL: "https://platform.minimax.io/docs/guides/text-generation", Note: "Display name changed 2026-11-18 triage: Brand per MiniMax's own platform docs; the 'Coding' qualifier was wrong (a separate minimax-coding-plan exists in models.dev). Note: models.dev names it 'MiniMax (minimax.io)', so the audit still flags a name difference — the clean brand name is kept deliberately."},
	{ID: "minimax-cn", NewName: "MiniMax (China)", SourceURL: "https://platform.minimax.io/docs/guides/text-generation", Note: "Display name changed 2026-11-18 triage: Brand capitalization per MiniMax's own platform docs. Note: models.dev names it 'MiniMax (minimax.cn)', so the audit still flags a name difference — the clean brand name is kept deliberately."},
}

// appendTriageNote joins a triage note onto existing Notes.
func appendTriageNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + " " + add
}

// applyTriageFixes applies the triage overlays to the catalog slice
// built by All(): base-URL fills and renames on existing entries,
// then the new triage providers. It never touches a
// verified-from-docs entry and never replaces an existing base URL.
// Round 2 (CHECKLIST 266) adds its own fix/provider slices, applied
// the same way, in providers_triage_round2.go. Round 3 (CHECKLIST
// 275) adds its own fix/provider slices plus the docs-proven ID
// moves (triageIDMovesRound3), applied last, in
// providers_triage_round3.go. Round 4 (CHECKLIST 281) adds its own
// fix/provider slices plus the docs-proven ID move
// (triageIDMovesRound4), applied last, in
// providers_triage_round4.go. Round 5 (CHECKLIST 301) adds its own
// provider slice, applied the same way, in
// providers_triage_round5.go; it applies no base-URL fills and no
// ID moves (the 3 pending possible renames stay pending).
func applyTriageFixes(out []Provider, byID map[string]int) []Provider {
	for _, fix := range triageBaseURLFixes {
		i, ok := byID[fix.ID]
		if !ok {
			continue // target entry is gone; the fix is stale
		}
		if out[i].Verification != VerificationReference || len(out[i].BaseURLs) != 0 {
			continue // verified entries and filled entries are never touched
		}
		out[i].BaseURLs = append([]string(nil), fix.BaseURLs...)
		out[i].Notes = appendTriageNote(out[i].Notes, fix.Note)
	}
	for _, fix := range triageBaseURLFixesRound2 {
		i, ok := byID[fix.ID]
		if !ok {
			continue // target entry is gone; the fix is stale
		}
		if out[i].Verification != VerificationReference || len(out[i].BaseURLs) != 0 {
			continue // verified entries and filled entries are never touched
		}
		out[i].BaseURLs = append([]string(nil), fix.BaseURLs...)
		out[i].Notes = appendTriageNote(out[i].Notes, fix.Note)
	}
	for _, fix := range triageBaseURLFixesRound3 {
		i, ok := byID[fix.ID]
		if !ok {
			continue // target entry is gone; the fix is stale
		}
		if out[i].Verification != VerificationReference || len(out[i].BaseURLs) != 0 {
			continue // verified entries and filled entries are never touched
		}
		out[i].BaseURLs = append([]string(nil), fix.BaseURLs...)
		out[i].Notes = appendTriageNote(out[i].Notes, fix.Note)
	}
	for _, fix := range triageBaseURLFixesRound4 {
		i, ok := byID[fix.ID]
		if !ok {
			continue // target entry is gone; the fix is stale
		}
		if out[i].Verification != VerificationReference || len(out[i].BaseURLs) != 0 {
			continue // verified entries and filled entries are never touched
		}
		out[i].BaseURLs = append([]string(nil), fix.BaseURLs...)
		out[i].Notes = appendTriageNote(out[i].Notes, fix.Note)
	}
	for _, r := range triageRenames {
		i, ok := byID[r.ID]
		if !ok {
			continue
		}
		out[i].Name = r.NewName
		out[i].Notes = appendTriageNote(out[i].Notes, r.Note)
	}
	for _, p := range triageProviders {
		if _, ok := byID[p.ID]; ok {
			continue // a triage provider must never shadow an existing ID
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	for _, p := range triageProvidersRound2 {
		if _, ok := byID[p.ID]; ok {
			continue // a triage provider must never shadow an existing ID
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	for _, p := range triageProvidersRound3 {
		if _, ok := byID[p.ID]; ok {
			continue // a triage provider must never shadow an existing ID
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	for _, p := range triageProvidersRound4 {
		if _, ok := byID[p.ID]; ok {
			continue // a triage provider must never shadow an existing ID
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	for _, p := range triageProvidersRound5 {
		if _, ok := byID[p.ID]; ok {
			continue // a triage provider must never shadow an existing ID
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	// Round-3 ID moves run LAST: every earlier step above keys on the
	// pre-move IDs, and the byID map is repaired here so the
	// additional-providers loop in All() still sees a consistent map.
	for _, m := range triageIDMovesRound3 {
		i, ok := byID[m.OldID]
		if !ok {
			continue // target entry is gone; the move is stale
		}
		if _, taken := byID[m.NewID]; taken {
			continue // the new ID is already taken; never shadow it
		}
		if out[i].Verification == VerificationReference && len(out[i].BaseURLs) == 0 && len(m.BaseURLs) != 0 {
			out[i].BaseURLs = append([]string(nil), m.BaseURLs...)
		}
		if m.Website != "" && out[i].Website == "" {
			out[i].Website = m.Website
		}
		out[i].ID = m.NewID
		out[i].Alias = m.NewAlias
		out[i].Notes = appendTriageNote(out[i].Notes, m.Note)
		delete(byID, m.OldID)
		byID[m.NewID] = i
	}
	// Round-4 ID moves run after the round-3 moves, under the same
	// guards, for the same reason.
	for _, m := range triageIDMovesRound4 {
		i, ok := byID[m.OldID]
		if !ok {
			continue // target entry is gone; the move is stale
		}
		if _, taken := byID[m.NewID]; taken {
			continue // the new ID is already taken; never shadow it
		}
		if out[i].Verification == VerificationReference && len(out[i].BaseURLs) == 0 && len(m.BaseURLs) != 0 {
			out[i].BaseURLs = append([]string(nil), m.BaseURLs...)
		}
		if m.Website != "" && out[i].Website == "" {
			out[i].Website = m.Website
		}
		out[i].ID = m.NewID
		out[i].Alias = m.NewAlias
		out[i].Notes = appendTriageNote(out[i].Notes, m.Note)
		delete(byID, m.OldID)
		byID[m.NewID] = i
	}
	return out
}
