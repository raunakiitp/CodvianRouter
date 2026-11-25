# Provider catalog freshness audit — 2026-11-18

Source: [https://models.dev/api.json](https://models.dev/api.json), fetched live for this run. Local catalog: `internal/catalog` (Go data, 614 entries). This audit only reports — it never edits the catalog. Fixes land separately, each with its cited source.

## Counts

| Side | Count |
|---|---|
| Catalog entries | 614 |
| — verified from docs | 65 |
| — reference metadata only | 545 |
| — unverified | 4 |
| — no base URL | 234 |
| — templated base URL | 9 |
| — ToS-grey | 38 |
| models.dev providers | 324 |
| models.dev models | 9981 (863 deprecated) |
| Reachability probed | 940 (917 reachable, 23 unreachable, 234 skipped — no base URL, 7 loopback skipped — local runtimes, unprobable from here) |

## Triage 2026-11-18 (committed record: `internal catalog triage record`)

| | Count |
|---|---|
| Candidates triaged | 228 of 228 |
| Providers added | 191 (17 verified from docs, 174 reference-only) |
| Candidates rejected | 30 |
| Candidates pending | 7 |
| Candidates untouched | 0 |
| Missing base URLs filled | 43 of 44 |
| Renames applied | 4 of 21 |
| Possible renames applied | 5 of 12 |

## Findings by kind

| Kind | Count |
|---|---|
| verified_base_unreachable | 1 |
| base_url_unreachable | 6 |
| website_unreachable | 17 |
| base_url_differs | 2 |
| renamed | 19 |
| possible_rename | 7 |
| base_url_missing_in_catalog | 1 |
| base_url_path_differs | 3 |
| base_url_missing_in_modelsdev | 10 |
| missing_in_catalog | 37 |
| missing_in_modelsdev | 327 |

## Verified entries with an unreachable base URL (downgrade candidates) (1)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| clawcu |  | Clawcu | verified-from-docs entry whose base URL "https://token.clawcu.ai/v1" is unreachable (timeout) — downgrade candidate, re-verify against the provider's own docs before relying on it |

## Unreachable base URLs (no HTTP response at all) (6)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| speka |  | Speka AI | catalog base URL "https://speka.me/v1" gave no HTTP response (connect: Get "https://speka.me/v1": EOF) |
| empower |  | Empower | catalog base URL "https://app.empower.dev/api/v1" gave no HTTP response (connect: Get "https://app.empower.dev/api/v1": EOF) |
| clarifai |  | Clarifai | catalog base URL "https://api.clarifai.com/v2/ext/openai/v1" gave no HTTP response (connect: Get "https://api.clarifai.com/v2/ext/openai/v1": EOF) |
| claudinio |  | Claudinio | catalog base URL "https://api.claudin.io/v1" gave no HTTP response (connect: Get "https://api.claudin.io/v1": EOF) |
| clawcu |  | Clawcu | catalog base URL "https://token.clawcu.ai/v1" gave no HTTP response (timeout: Get "http://token.clawcu.ai:8080/v1/": context deadline exceeded) |
| kluster |  | Kluster.ai | catalog base URL "https://api.kluster.ai/v1" gave no HTTP response (connect: Get "https://api.kluster.ai/v1": EOF) |

## Unreachable websites (no HTTP response at all) (17)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| speka |  | Speka AI | catalog website "https://speka.me" gave no HTTP response (connect: Get "https://speka.me": EOF) |
| llamagate |  | LlamaGate | catalog website "https://llamagate.ai" gave no HTTP response (connect: Get "https://llamagate.ai": EOF) |
| thebai |  | TheB.AI | catalog website "https://theb.ai" gave no HTTP response (connect: Get "https://theb.ai": EOF) |
| empower |  | Empower | catalog website "https://docs.empower.dev" gave no HTTP response (connect: Get "https://docs.empower.dev": EOF) |
| nara |  | NaraRouter | catalog website "https://bynara.id" gave no HTTP response (timeout: Get "https://bynara.id": context deadline exceeded) |
| void-ai |  | Void AI | catalog website "https://voidai.app" gave no HTTP response (connect: Get "https://voidai.app": EOF) |
| pioneer |  | Pioneer AI | catalog website "https://pioneer.ai" gave no HTTP response (timeout: Get "https://pioneer.ai": context deadline exceeded) |
| meta-llama |  | Meta Llama API | catalog website "https://llama.developer.meta.com" gave no HTTP response (connect: Get "https://dev.meta.ai/": Forbidden) |
| monsterapi |  | MonsterAPI | catalog website "https://monsterapi.ai" gave no HTTP response (connect: Get "https://monsterapi.ai": EOF) |
| clarifai |  | Clarifai | catalog website "https://docs.clarifai.com" gave no HTTP response (connect: Get "https://docs.clarifai.com": EOF) |
| codebuddy-intl |  | CodeBuddy Intl | catalog website "https://www.codebuddy.ai" gave no HTTP response (connect: Get "https://www.codebuddy.ai": EOF) |
| muse-code |  | Muse Code (Meta) | catalog website "https://ai.developer.meta.com/docs/muse-code/auth" gave no HTTP response (connect: Get "https://dev.meta.ai/docs/muse-code/auth": Forbidden) |
| playht |  | PlayHT | catalog website "https://play.ht" gave no HTTP response (connect: Get "https://play.ht": EOF) |
| muse-spark-web |  | Muse Spark Web (Meta AI) | catalog website "https://www.meta.ai" gave no HTTP response (connect: Get "https://www.meta.ai": Forbidden) |
| routing-run |  | routing.run | catalog website "https://docs.routing.run/api-reference/models" gave no HTTP response (connect: Get "https://docs.routing.run/api-reference/models": EOF) |
| claudinio |  | Claudinio | catalog website "https://claudin.io" gave no HTTP response (connect: Get "https://claudin.io": EOF) |
| kluster |  | Kluster.ai | catalog website "https://www.kluster.ai" gave no HTTP response (connect: Get "https://www.kluster.ai": EOF) |

## Base URL host differs from models.dev `api` (2)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| fastrouter | fastrouter | FastRouter | catalog base "https://api.fastrouter.ai/api/v1" (host https://api.fastrouter.ai), models.dev api "https://go.fastrouter.ai/api/v1" (host https://go.fastrouter.ai) |
| privatemode-ai | privatemode-ai | Privatemode AI | catalog base "https://proxyless-api.privatemode.ai/v1" (host https://proxyless-api.privatemode.ai), models.dev api "http://localhost:8080/v1" (host http://localhost:8080) |

## Renamed (same ID, different name) (19)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| anyapi | anyapi | AnyAPI AI | catalog name "AnyAPI AI", models.dev name "AnyAPI" |
| llmgateway | llmgateway | LLM Gateway | catalog name "LLM Gateway", models.dev name "DevPass (LLM Gateway)" |
| nan | nan | NaN.Builders | catalog name "NaN.Builders", models.dev name "NaN" |
| chutes | chutes | Chutes.ai | catalog name "Chutes.ai", models.dev name "Chutes" |
| pioneer | pioneer | Pioneer AI | catalog name "Pioneer AI", models.dev name "Pioneer" |
| liquid | liquid | Liquid AI | catalog name "Liquid AI", models.dev name "Liquid Inference" |
| nvidia | nvidia | NVIDIA NIM | catalog name "NVIDIA NIM", models.dev name "Nvidia" |
| nebius | nebius | Nebius AI | catalog name "Nebius AI", models.dev name "Nebius Token Factory" |
| hetzner | hetzner | Hetzner Inference API | catalog name "Hetzner Inference API", models.dev name "Hetzner" |
| watsonx | watsonx | IBM watsonx.ai Gateway | catalog name "IBM watsonx.ai Gateway", models.dev name "watsonx.ai" |
| scaleway | scaleway | Scaleway AI | catalog name "Scaleway AI", models.dev name "Scaleway" |
| minimax | minimax | MiniMax | catalog name "MiniMax", models.dev name "MiniMax (minimax.io)" |
| minimax-cn | minimax-cn | MiniMax (China) | catalog name "MiniMax (China)", models.dev name "MiniMax (minimax.cn)" |
| alibaba | alibaba | Alibaba Cloud Model Studio | catalog name "Alibaba Cloud Model Studio", models.dev name "Alibaba" |
| longcat | longcat | LongCat AI | catalog name "LongCat AI", models.dev name "LongCat" |
| volcengine | volcengine | Volcengine | catalog name "Volcengine", models.dev name "Volcengine Ark" |
| stepfun | stepfun | StepFun | catalog name "StepFun", models.dev name "StepFun (China)" |
| sensenova | sensenova | SenseNova | catalog name "SenseNova", models.dev name "SenseNova (China)" |
| gitlab | gitlab | GitLab Duo PAT | catalog name "GitLab Duo PAT", models.dev name "GitLab Duo" |

## Possible renames (same name, different ID — needs a human) (7)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| charm-hyper | hyper | Charm Hyper | catalog ID "charm-hyper" but models.dev lists the same name under ID "hyper" (20 models) — possible rename, needs a human to confirm |
| cloudcode-one | cloudcode | CloudCode.ONE | catalog ID "cloudcode-one" but models.dev lists the same name under ID "cloudcode" (6 models) — possible rename, needs a human to confirm |
| qiniu | qiniu-ai | Qiniu | catalog ID "qiniu" but models.dev lists the same name under ID "qiniu-ai" (87 models) — possible rename, needs a human to confirm |
| kilo-gateway | kilo | Kilo Gateway | catalog ID "kilo-gateway" but models.dev lists the same name under ID "kilo" (465 models) — possible rename, needs a human to confirm |
| vercel-ai-gateway | vercel | Vercel AI Gateway | catalog ID "vercel-ai-gateway" but models.dev lists the same name under ID "vercel" (416 models) — possible rename, needs a human to confirm |
| regolo | regolo-ai | Regolo AI | catalog ID "regolo" but models.dev lists the same name under ID "regolo-ai" (18 models) — possible rename, needs a human to confirm |
| prime-intellect | prime | Prime Inference | catalog ID "prime-intellect" but models.dev lists the same name under ID "prime" (70 models) — possible rename, needs a human to confirm |

## No base URL in the catalog, models.dev lists one (1)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| pioneer | pioneer | Pioneer AI | catalog records no base URL; models.dev lists api "https://api.fastino.ai/v1" |

## Same host, path differs (usually the /v1 convention) (3)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| minimax | minimax | MiniMax | same host https://api.minimax.io; catalog path "/v1", models.dev path "/anthropic/v1" |
| minimax-cn | minimax-cn | MiniMax (China) | same host https://api.minimax.cn; catalog path "/v1", models.dev path "/anthropic/v1" |
| merge-gateway | merge-gateway | Merge Gateway | same host https://api-gateway.merge.dev; catalog path "/v1/openai", models.dev path "/v1/ai-sdk" |

## Base URL in the catalog, models.dev lists none (informational) (10)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| openai | openai | OpenAI | catalog base URL "https://api.openai.com/v1"; models.dev lists no api base for this provider |
| anthropic | anthropic | Anthropic | catalog base URL "https://api.anthropic.com"; models.dev lists no api base for this provider |
| groq | groq | Groq | catalog base URL "https://api.groq.com/openai/v1"; models.dev lists no api base for this provider |
| xai | xai | xAI | catalog base URL "https://api.x.ai/v1"; models.dev lists no api base for this provider |
| mistral | mistral | Mistral | catalog base URL "https://api.mistral.ai/v1"; models.dev lists no api base for this provider |
| perplexity | perplexity | Perplexity | catalog base URL "https://api.perplexity.ai"; models.dev lists no api base for this provider |
| cohere | cohere | Cohere | catalog base URL "https://api.cohere.com"; models.dev lists no api base for this provider |
| cerebras | cerebras | Cerebras | catalog base URL "https://api.cerebras.ai/v1"; models.dev lists no api base for this provider |
| salad-cloud | salad-cloud | SaladCloud AI Gateway | catalog base URL "https://ai.salad.cloud/v1"; models.dev lists no api base for this provider |
| aihubmix | aihubmix | AIHubMix | catalog base URL "https://aihubmix.com/v1"; models.dev lists no api base for this provider |

## In models.dev, absent from the catalog (new-provider candidates) (37)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
|  | alibaba-coding-plan | Alibaba Coding Plan | listed by models.dev with 10 model(s) but absent from the catalog |
|  | alibaba-coding-plan-cn | Alibaba Coding Plan (China) | listed by models.dev with 10 model(s) but absent from the catalog |
|  | alibaba-token-plan-cn | Alibaba Token Plan (China) | listed by models.dev with 29 model(s) but absent from the catalog |
|  | amazon-bedrock | Amazon Bedrock | listed by models.dev with 208 model(s) but absent from the catalog |
|  | azure | Azure | listed by models.dev with 97 model(s) but absent from the catalog |
|  | azure-cognitive-services | Azure Cognitive Services | listed by models.dev with 97 model(s) but absent from the catalog |
|  | drun | D.Run (China) | listed by models.dev with 3 model(s) but absent from the catalog |
|  | fireworks-ai | Fireworks AI | listed by models.dev with 27 model(s) but absent from the catalog |
|  | friendli | Friendli | listed by models.dev with 5 model(s) but absent from the catalog |
|  | google | Google | listed by models.dev with 38 model(s) but absent from the catalog |
|  | google-vertex | Vertex | listed by models.dev with 64 model(s) but absent from the catalog |
|  | google-vertex-anthropic | Vertex (Anthropic) | listed by models.dev with 18 model(s) but absent from the catalog |
|  | inference | Inference | listed by models.dev with 9 model(s) but absent from the catalog |
|  | llmgateway-providers | LLM Gateway | listed by models.dev with 501 model(s) but absent from the catalog |
|  | meta | Meta | listed by models.dev with 7 model(s) but absent from the catalog |
|  | minimax-cn-coding-plan | MiniMax Token Plan (minimax.cn) | listed by models.dev with 9 model(s) but absent from the catalog |
|  | minimax-coding-plan | MiniMax Token Plan (minimax.io) | listed by models.dev with 9 model(s) but absent from the catalog |
|  | modelis | Modelis | listed by models.dev with 9 model(s) but absent from the catalog |
|  | neon | Neon | listed by models.dev with 57 model(s) but absent from the catalog |
|  | neosmith | NeoSmith | listed by models.dev with 4 model(s) but absent from the catalog |
|  | ofox | Ofox | listed by models.dev with 154 model(s) but absent from the catalog |
|  | qvac | QVAC | listed by models.dev with 9 model(s) but absent from the catalog |
|  | sap-ai-core | SAP AI Core | listed by models.dev with 51 model(s) but absent from the catalog |
|  | scnet-token-plan | SCNet Token Plan | listed by models.dev with 19 model(s) but absent from the catalog |
|  | stepfun-ai-step-plan | StepFun Step Plan (Global) | listed by models.dev with 4 model(s) but absent from the catalog |
|  | stepfun-step-plan | StepFun Step Plan (China) | listed by models.dev with 5 model(s) but absent from the catalog |
|  | tencent-coding-plan | Tencent Coding Plan (China) | listed by models.dev with 8 model(s) but absent from the catalog |
|  | tencent-token-plan | Tencent Token Plan | listed by models.dev with 15 model(s) but absent from the catalog |
|  | togetherai | Together AI | listed by models.dev with 33 model(s) but absent from the catalog |
|  | umans-ai-coding-plan | Umans AI Coding Plan | listed by models.dev with 10 model(s) but absent from the catalog |
|  | wafer.ai | Wafer | listed by models.dev with 11 model(s) but absent from the catalog |
|  | xiaomi | Xiaomi | listed by models.dev with 6 model(s) but absent from the catalog |
|  | xiaomi-token-plan-ams | Xiaomi Token Plan (Europe) | listed by models.dev with 7 model(s) but absent from the catalog |
|  | xiaomi-token-plan-cn | Xiaomi Token Plan (China) | listed by models.dev with 7 model(s) but absent from the catalog |
|  | xiaomi-token-plan-sgp | Xiaomi Token Plan (Singapore) | listed by models.dev with 7 model(s) but absent from the catalog |
|  | zai-coding-plan | Z.AI Coding Plan | listed by models.dev with 7 model(s) but absent from the catalog |
|  | zhipuai-coding-plan | Zhipu AI Coding Plan | listed by models.dev with 4 model(s) but absent from the catalog |

## In the catalog, absent from models.dev (NOT a retirement verdict) (327)

| Catalog ID | models.dev ID | Name | Detail |
|---|---|---|---|
| onomeo |  | onomeo | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| unifically |  | Unifically | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| oneminai |  | 1min.AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| reference-catalog-provider |  | Reference Catalog Provider | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| zylo-api |  | Zylo API | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| electronhub |  | Electron Hub | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| llm-kiwi |  | LLM.Kiwi | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| literouter |  | LiteRouter | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| eurouter |  | EURouter | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| y-api |  | Y-API | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| mnn-ai |  | MNN AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| meganova-ai |  | MegaNova AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| speka |  | Speka AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| tokenreply |  | TokenReply | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| yolo-auto |  | Yolo-Auto | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| dxnt |  | DXNT / DX Token | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| ofoxai |  | OfoxAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| zerolimitai |  | ZeroLimitAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| chatanywhere |  | ChatAnywhere | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| helyxai |  | Helyx AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| poixe-ai |  | Poixe AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| naga-ai |  | Naga AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| chat-oripe |  | Chat Oripe | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| freeinference |  | FreeInference | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| free-ai |  | Free.ai | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| dgrid |  | DGrid | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| api-airforce |  | Api.airforce | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| crof |  | CrofAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| bazaarlink |  | BazaarLink | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| wafer |  | Wafer AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| opencode-zen |  | OpenCode Zen | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| dahl |  | Dahl | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| freetheai |  | FreeTheAi | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| g4f-groq |  | g4f.space — Groq | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| g4f-gemini |  | g4f.space — Gemini | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| g4f-pollinations |  | g4f.space — Pollinations | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| g4f-ollama |  | g4f.space — Ollama | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| g4f-nvidia |  | g4f.space — NVIDIA | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| llm7 |  | LLM7.io | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| llamagate |  | LlamaGate | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| gitlawb |  | Gitlawb Opengateway (MiMo) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| gitlawb-gmi |  | Gitlawb Opengateway (GMI Cloud) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| piapi |  | PiAPI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| getgoapi |  | GoAPI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| laozhang |  | LaoZhang AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| thebai |  | TheB.AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| bai |  | b.ai | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| fenayai |  | FenayAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| empower |  | Empower | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| factory |  | Factory | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| bluesminds |  | BluesMinds | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| freemodel-dev |  | FreeModel.dev | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| freeaiapikey |  | FreeAIAPIKey | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| openadapter |  | OpenAdapter | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| dit |  | DIT.ai | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| token-kiosk |  | Token Kiosk | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| sumopod |  | SumoPod | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| x5lab |  | X5Lab | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| chenzk |  | Chenzk API | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| tokenmarket |  | Token Market | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| tokenharbor |  | Token Harbor | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| navy |  | NavyAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| ainative |  | AINative Studio | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| aion |  | Aion Labs | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| routeway |  | Routeway | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| nara |  | NaraRouter | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| xkiro |  | xKiro | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| naga-ac |  | Naga.ac | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| void-ai |  | Void AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| helixmind |  | HelixMind | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| logfare |  | Logfare | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| tabitoken |  | TabiToken | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| seekai |  | SeekAi | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| demonroute |  | Demon Route | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| beatapi |  | BeatAPI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| reka |  | Reka | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| uc-direct |  | UC Direct (uncensored.com) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| gemini |  | Google Gemini | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| blackbox |  | Blackbox AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| meta-llama |  | Meta Llama API | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| galadriel |  | Galadriel | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| codestral |  | Codestral | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| maritalk |  | Maritalk | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| nous-research |  | Nous Research | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| arcee-ai |  | Arcee AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| writer |  | Writer | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| together |  | Together | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| openvecta |  | OpenVecta | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| openference-api |  | Openference API | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| fireworks |  | Fireworks | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| nube |  | Nube.sh | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| hyperbolic |  | Hyperbolic | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| lambda-ai |  | Lambda AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| nscale |  | nScale | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| publicai |  | PublicAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| featherless-ai |  | Featherless AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| friendliai |  | FriendliAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| inference-net |  | Inference.net | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| replicate |  | Replicate | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| cerebrium |  | Cerebrium | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| grokified |  | Grokified | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| predibase |  | Predibase | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| bytez |  | Bytez | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| monsterapi |  | MonsterAPI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| azure-openai |  | Azure OpenAI | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| azure-ai |  | Azure AI Foundry | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| bedrock |  | AWS Bedrock | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| sap |  | SAP Generative AI Hub | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| vertex |  | Google Vertex AI | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| vertex-partner |  | Vertex AI Partners | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| heroku |  | Heroku AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| datarobot |  | DataRobot | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| qianfan |  | Baidu Qianfan | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| glm |  | GLM Coding | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| glm-cn |  | GLM Coding (China) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| bigmodel |  | BigModel.cn (Zhipu) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| glmt |  | GLM Thinking | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| qwen-cloud |  | Qwen Cloud | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| qwen-cloud-token-plan |  | Qwen Cloud Token Plan | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| kimi |  | Kimi (Legacy Moonshot API) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| kimi-coding-apikey |  | Kimi Code API Key | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| moonshot |  | Kimi | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| volcengine-agent-plan |  | Volcengine Ark Agent Plan | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| gigachat |  | GigaChat (Sber) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| xiaomi-mimo |  | Xiaomi MiMo | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| xiaomi-mimo-token-plan |  | Xiaomi MiMo Token Plan | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| baidu |  | Baidu (ERNIE) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| tencent |  | Tencent Hunyuan | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| iflytek |  | iFlytek Spark | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| baichuan |  | Baichuan | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| yi |  | Yi (01.AI) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| coze |  | Coze | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| 360ai |  | 360 AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| doubao |  | Doubao | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| sparkdesk |  | SparkDesk | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| hcnsec |  | Huancheng Public API | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| agnes-cn |  | Agnes AI (China) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| sealion |  | SEA-LION | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| clova-studio |  | Naver CLOVA Studio | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| internlm |  | InternLM (Intern-S1) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| ant-ling |  | Ant Ling / Ring (inclusionAI) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| plamo |  | PLaMo | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| typhoon |  | Typhoon | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| apmix |  | Apmix | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| nlpcloud |  | NLP Cloud | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| runwayml |  | Runway | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| kie |  | KIE.AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| pollinations |  | Pollinations AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| haiper |  | Haiper | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| leonardo |  | Leonardo AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| ideogram |  | Ideogram | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| magnific |  | Magnific | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| udio |  | Udio | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| v0-vercel |  | v0 (Vercel) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| voyage-ai |  | Voyage AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| jina-ai |  | Jina AI (Foundation API) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| typesafe |  | TypeSafe AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| fal-ai |  | Fal.ai | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| stability-ai |  | Stability AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| black-forest-labs |  | Black Forest Labs | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| recraft |  | Recraft | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| topaz |  | Topaz | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| segmind |  | Segmind | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| dify |  | Dify | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| nomic |  | Nomic | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| mixedbread |  | Mixedbread AI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| jina-reader |  | Jina Reader (r.jina.ai) | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| tinyfish |  | TinyFish Fetch | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| deepai |  | DeepAI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| deapi |  | deAPI | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| cursor-api |  | Cursor API | in the catalog but absent from models.dev (api-key; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| chatgpt |  | ChatGPT | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| ghe-copilot |  | GitHub Enterprise Copilot | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| xai-oauth |  | xAI OAuth (Grok) | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| openference |  | Openference | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| renderhour |  | RenderHour | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| grok-cli |  | Grok Build | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| qoder |  | Qoder | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| agy |  | Antigravity CLI | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| kiro |  | Kiro AI | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| amazon-q |  | Amazon Q | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| claude |  | Claude Code | in the catalog but absent from models.dev (oauth; verification verified-from-docs) — verify manually: untracked there, or retired |
| antigravity |  | Antigravity | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| codex |  | OpenAI Codex | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| gitlab-duo |  | GitLab Duo | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| cursor |  | Cursor IDE | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| zed |  | Zed IDE | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| zed-hosted |  | Zed Hosted Models | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| trae |  | Trae | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| kimi-coding |  | Kimi Code CLI | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| kilocode |  | Kilo Code | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| megabrain |  | MegaBrain | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| cline |  | Cline | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| devin-desktop |  | Devin Desktop | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| devin-cli |  | Devin CLI | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| codebuddy-cn |  | CodeBuddy CN | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| codebuddy-intl |  | CodeBuddy Intl | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| workbuddy |  | WorkBuddy | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| muse-code |  | Muse Code (Meta) | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| freebuff |  | Freebuff | in the catalog but absent from models.dev (oauth; verification from-reference-metadata-only) — verify manually: untracked there, or retired |
| mlx-gemma |  | MLX Gemma 26B | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| mlx-qwen |  | MLX Qwen 3.8 27B | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| ollama-local |  | Ollama | in the catalog but absent from models.dev (local; verification verified-from-docs) — expected: this category is not tracked by models.dev |
| opencode-server |  | OpenCode CLI Server | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| vllm |  | vLLM | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| lemonade |  | Lemonade Server | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| llamafile |  | Llamafile | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| llama-cpp |  | llama.cpp | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| triton |  | NVIDIA Triton | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| docker-model-runner |  | Docker Model Runner | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| xinference |  | XInference | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| oobabooga |  | oobabooga | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| sdwebui |  | SD WebUI | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| comfyui |  | ComfyUI | in the catalog but absent from models.dev (local; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| devin-cli-agentic |  | Devin CLI Agentic Bridge | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| duckduckgo-web |  | DuckDuckGo AI Chat | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| cloudflare-playground |  | Cloudflare AI Playground | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| veoaifree-web |  | Veo AI Free | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| auggie |  | Augment (Auggie CLI) | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| zcode |  | ZCode (GLM Coding Plan) | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| minimax-code |  | MiniMax Code | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| codex-app-server |  | OpenAI Codex (App-Server) | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| uncloseai |  | UncloseAI | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| aihorde |  | AI Horde | in the catalog but absent from models.dev (no-auth; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| codex-search |  | Codex Web Search (ChatGPT) | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| perplexity-search |  | Perplexity Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| serper-search |  | Serper Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| brave-search |  | Brave Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| exa-search |  | Exa Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| tavily-search |  | Tavily Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| anysearch-search |  | AnySearch | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| firecrawl |  | Firecrawl | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| google-pse-search |  | Google Programmable Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| nimble-search |  | Nimble Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| parallel-search |  | Parallel Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| keenable-search |  | Keenable Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| linkup-search |  | Linkup Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| searchapi-search |  | SearchAPI | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| youcom-search |  | You.com Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| searxng-search |  | SearXNG Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| x-search |  | X Search (Grok) | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| xquik-search |  | Xquik X Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| ollama-search |  | Ollama Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| kimi-search |  | Kimi Search | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| context7 |  | Context7 (library docs) | in the catalog but absent from models.dev (search; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| deepgram |  | Deepgram | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| assemblyai |  | AssemblyAI | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| soniox |  | Soniox | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| elevenlabs |  | ElevenLabs | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| cartesia |  | Cartesia | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| fishaudio |  | Fish Audio | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| playht |  | PlayHT | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| inworld |  | Inworld | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| aws-polly |  | AWS Polly | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| gladia |  | Gladia | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| rev-ai |  | Rev AI | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| speechmatics |  | Speechmatics | in the catalog but absent from models.dev (audio; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| chatgpt-web |  | ChatGPT Web (Clean Room) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| chatgpt-web-codex |  | ChatGPT Web (Codex) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| grok-web |  | Grok Web (Subscription) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| gemini-web |  | Gemini Web (Free) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| perplexity-web |  | Perplexity Web (Pro/Max) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| blackbox-web |  | Blackbox Web (Subscription) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| muse-spark-web |  | Muse Spark Web (Meta AI) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| claude-web |  | Claude Web | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| deepseek-web |  | DeepSeek Web | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| copilot-web |  | Microsoft Copilot Web | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| copilot-m365-web |  | Microsoft 365 Copilot | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| t3-web |  | t3.chat (Pro/Free) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| inner-ai |  | Inner.ai (Subscription) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| adapta-web |  | Adapta.org (Adapta One Web) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| lmarena |  | Arena (Free) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| yuanbao-web |  | Tencent Yuanbao (Free) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| tencent-aistudio-web |  | Tencent AI Studio (Free) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| huggingchat |  | HuggingChat (Free) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| poe-web |  | Poe Web (Subscription) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| venice-web |  | Venice Web (Privacy) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| v0-vercel-web |  | v0 Vercel Web (Code Gen) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| kimi-web |  | Kimi Web | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| doubao-web |  | Dola Web (ByteDance) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| zenmux-free |  | ZenMux Free (Web) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| tinycms-web |  | TinyCMS Web (Free/Sub) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| zai-web |  | Z.ai Web | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| promptql |  | PromptQL (Unofficial/Experimental) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| notion-web |  | Notion AI Web (Unofficial/Experimental) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| adobe-firefly |  | Adobe Firefly (Image/Video) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| hyperagent |  | HyperAgent (Unofficial/Experimental) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| conol-web |  | Conol (Unofficial/Experimental) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| twinmind |  | Twinmind (Unofficial/Experimental) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| syntx |  | SYNTX.ai (Unofficial/Experimental) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| maxai |  | MaxAI | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| uc |  | UC (uncensored.com) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| chatplayground |  | ChatPlayground | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| notrack-web |  | NoTrack Web (Free) | in the catalog but absent from models.dev (web-cookie; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| cliproxyapi |  | CLIProxyAPI | in the catalog but absent from models.dev (upstream-proxy; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| 9router |  | 9router | in the catalog but absent from models.dev (upstream-proxy; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| jules |  | Google Jules | in the catalog but absent from models.dev (cloud-agent; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| devin |  | Devin | in the catalog but absent from models.dev (cloud-agent; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| codex-cloud |  | Codex Cloud | in the catalog but absent from models.dev (cloud-agent; verification from-reference-metadata-only) — expected: this category is not tracked by models.dev |
| codvian |  | Codvian | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| parasail |  | Parasail | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| anyscale |  | Anyscale Endpoints | in the catalog but absent from models.dev (api-key; verification unverified) — verify manually: untracked there, or retired |
| avian |  | Avian | in the catalog but absent from models.dev (api-key; verification unverified) — verify manually: untracked there, or retired |
| io-intelligence |  | IO Intelligence | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| runpod |  | RunPod Serverless | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| cluster-protocol |  | Cluster Protocol | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| clawcu |  | Clawcu | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| cloakapi |  | CloakAPI | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| tokenflux |  | TokenFlux | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| infermatic |  | Infermatic | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| tokenlio |  | Tokenlio | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| tokenomix |  | TokenomixAI | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| tokensg |  | TokenSG | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| inferway |  | Inferway | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| lattice |  | Lattice | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| infer-flow7 |  | Infer | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| comtegra |  | Comtegra LLM API | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| kluster |  | Kluster.ai | in the catalog but absent from models.dev (api-key; verification unverified) — verify manually: untracked there, or retired |
| localai |  | LocalAI | in the catalog but absent from models.dev (local; verification verified-from-docs) — expected: this category is not tracked by models.dev |
| jan |  | Jan | in the catalog but absent from models.dev (local; verification verified-from-docs) — expected: this category is not tracked by models.dev |
| gpt4all |  | GPT4All | in the catalog but absent from models.dev (local; verification verified-from-docs) — expected: this category is not tracked by models.dev |
| mancer |  | Mancer | in the catalog but absent from models.dev (api-key; verification unverified) — verify manually: untracked there, or retired |
| kiomon |  | Kiomon | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| ai-tokens |  | AI Tokens | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| aoru |  | Aoru AI | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| modavio |  | Modavio | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |
| cscs |  | CSCS Inference API | in the catalog but absent from models.dev (api-key; verification verified-from-docs) — verify manually: untracked there, or retired |

## Limitations

- Absent from models.dev is not retirement: models.dev tracks a subset of providers, and local, no-auth, web-cookie and cloud-agent entries are expected to be absent.
- models.dev lists no per-provider model list in this catalog: 'new models missing' is reported only as providers models.dev lists that the catalog does not, with their model counts. Per-model drift for connected providers is covered by the opt-in models.dev price/context sync (CHECKLIST 179-180).
- A base URL that differs from models.dev's `api` field is drift, not proof the catalog is wrong: models.dev stores the bare host where the catalog stores the /v1 base, and either side can be stale. A fix needs the provider's own docs as the cited source.
- Reachability is a transport fact, not a correctness verdict: a host that answers 401/404 exists; a host that times out once may be a transient failure, not a dead provider.
- Reachability is probed from the machine running the audit, through whatever egress it has: in this VM that is a shared egress proxy, which refuses some hosts outright and is slow for others, so a connect/EOF/timeout here is a re-check signal, not proof the provider is dead. Loopback base URLs (localhost / 127.0.0.0/8 / ::1 — local runtimes such as Ollama, LocalAI, Jan, GPT4All) are never probed: they can only be checked on the operator's own machine.

Generated 2026-11-18T07:32:11Z by `codvian-router catalog audit`. JSON: `catalog-audit-2026-11-18.json`.
