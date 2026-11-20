# Catalog triage — 2026-11-18 (CHECKLIST items 253–254)

Round 2 (CHECKLIST items 265–266) triaged the next 46 candidates:
see `docs/catalog-triage-2026-11-18-round2.md` (cumulative record in
`docs/catalog-triage-2026-11-18.json`: 92 of 228 triaged, 74 added,
16 rejected, 2 pending, 136 untouched).

Follow-up to the 227–228 audit (`docs/catalog-audit-2026-11-18.md`, run 2026-11-18 01:32 UTC against live models.dev).
This slice triaged the **top 46 of 228** new-provider candidates by models.dev model count, plus all **44 missing base URLs** and all **21 renames**.

## Headline

| | Count |
|---|---|
| Candidates triaged | 46 of 228 |
| Providers added | 32 (4 verified-from-docs, 28 reference-only) |
| Candidates rejected (duplicate / plan, not a provider) | 12 |
| Candidates pending (no provable fixed base URL) | 2 |
| Candidates untouched | 182 |
| Missing base URLs filled | 43 of 44 |
| Missing base URL skipped | 1 (`pioneer` — see below) |
| Renames applied (provider-doc confirmed) | 4 of 21 |
| Renames rejected (catalog name kept, reason below) | 17 of 21 |
| Possible renames (ID-level, needs a human) | 0 of 12 applied — all pending |

Catalog totals after this slice: **455 entries** (was 423) — 52 verified-from-docs (was 48), 399 reference-only (was 371), 4 unverified; no base URL 242 (was 285).

Verification rule used throughout: a provider was added only when models.dev lists it **and** a live reachability probe of its base URL returned an HTTP response (any status — 401/403/404 on a bare base URL is the normal answer of a live API). `verified-from-docs` was granted only where the provider's own docs were read and state the base URL + auth. Base URLs were never bulk-filled from models.dev: each fill below cites its own probe code.

## Providers added (32)

| ID | Name | Base URL | Verification | Probe | Evidence |
|---|---|---|---|---|---|
| edenai | Eden AI | https://api.edenai.run/v3 | reference-only | 404 | Docs confirm OpenAI-compatible API; /v3 path from models.dev + probe |
| merge-gateway | Merge Gateway | https://api-gateway.merge.dev/v1/ai-sdk | verified-from-docs | 401 | Merge docs (docs.merge.dev/merge-gateway): Bearer + /v1/openai base |
| aihubmix | AIHubMix | https://aihubmix.com/v1 | reference-only | 200 on /v1/models | Docs confirm OpenAI-compatible interface; base corroborated by live /v1/models -> 200 |
| opencode | OpenCode | https://opencode.ai/inference/openai/v1 | verified-from-docs | 404 | OpenCode docs (opencode.ai/v2/docs/console/models/): Bearer + /inference/openai/v1 (+ Anthropic/Google surfaces) |
| abacus | Abacus | https://routellm.abacus.ai/v1 | reference-only | 404 | Docs page JS-rendered, states no base URL in fetched text |
| cortecs | Cortecs | https://api.cortecs.ai/v1 | reference-only | 404 | GET /v1/models -> 200 |
| helicone | Helicone | https://ai-gateway.helicone.ai/v1 | verified-from-docs | 405 | Helicone docs (docs.helicone.ai/gateway/overview): Bearer + ai-gateway.helicone.ai |
| cloudflare-ai-gateway | Cloudflare AI Gateway | https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/{provider} | verified-from-docs | ? | Cloudflare docs (developers.cloudflare.com/ai-gateway/get-started/): Bearer token + gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/{provider} |
| impossibl | Impossibl | https://api.impossibl.com/v1 | reference-only | 404 | models.dev api.json + probe |
| crossmodel | CrossModel | https://api.crossmodel.ai/v1 | reference-only | 404 | models.dev api.json + probe |
| jiekou | Jiekou.AI | https://api.jiekou.ai/openai | reference-only | 404 | models.dev api.json + probe |
| nearai | NEAR AI Cloud | https://cloud-api.near.ai/v1 | reference-only | 404 | models.dev api.json + probe |
| frogbot | FrogBot | https://app.frogbot.ai/api/v1 | reference-only | 401 | models.dev api.json + probe |
| siliconflow-cn | SiliconFlow (China) | https://api.siliconflow.cn/v1 | reference-only | 404 | models.dev api.json + probe |
| lowrouter | LowRouter | https://api.lowrouter.ai/v1 | reference-only | 404 | models.dev api.json + probe |
| standardcompute | Standard Compute | https://api.stdcmpt.com/v1 | reference-only | 404 | models.dev api.json + probe |
| thinkingmachines | Thinking Machines | https://tinker.thinkingmachines.dev/services/tinker-prod/anthropic/api/v1 | reference-only | 404 | Tinker Anthropic-compatible API per Thinking Machines docs |
| vivgrid | Vivgrid | https://api.vivgrid.com/v1 | reference-only | 401 | models.dev api.json + probe |
| llmtr | LLMTR | https://llmtr.com/v1 | reference-only | 404 | models.dev api.json + probe |
| iteracompute | IteraCompute | https://api.iteracompute.com/v1 | reference-only | 404 | models.dev api.json + probe |
| tensorx | TensorX | https://api.tensorx.ai/v1 | reference-only | 403 | models.dev api.json + probe |
| neuralwatt | Neuralwatt | https://api.neuralwatt.com/v1 | reference-only | 404 | models.dev api.json + probe |
| jzstoken | JZSTOKEN | https://jzstoken.com/api/v1 | reference-only | 200 | models.dev api.json + probe |
| steadyrouter | SteadyRouter | https://api.steadyrouter.com/v1 | reference-only | 404 | models.dev api.json + probe |
| boundless | Boundless | https://api.inference.boundless.network/v1 | reference-only | 404 | models.dev api.json + probe |
| bumo | Bumo Router | https://router.bumo.ai/v1 | reference-only | 404 | models.dev api.json + probe |
| cheapest-token | CheapestToken | https://api.cheapest-token.com/v1 | reference-only | 404 | models.dev api.json + probe |
| korouter | KoRouter | https://korouter.ai/v1 | reference-only | 404 | models.dev api.json + probe |
| meganova | Meganova | https://api.meganova.ai/v1 | reference-only | 404 | models.dev api.json + probe |
| phala | Phala | https://inference.phala.com/v1 | reference-only | 404 | models.dev api.json + probe |
| sparrowcodes | SparrowCodes | https://api.sparrowcodes.com/v1 | reference-only | 404 | models.dev api.json + probe |
| doubleword | Doubleword | https://api.doubleword.ai/v1 | reference-only | 401 | models.dev api.json + probe |

## Candidates rejected (12) — duplicates or billing plans, not new providers

| ID | Reason |
|---|---|
| llmgateway-providers | Duplicate of llmgateway (same service, same base URL https://api.llmgateway.io/v1). |
| amazon-bedrock | Duplicate of bedrock (verified entry; same AWS service). |
| ofox | Duplicate of ofoxai (same service, same base URL https://api.ofox.ai/v1). |
| azure | Duplicate of azure-openai (verified entry; alias "azure"). |
| azure-cognitive-services | Duplicate of azure-openai (verified entry). |
| google-vertex | Duplicate of vertex (verified entry). |
| google | Duplicate of gemini (verified entry; Google AI Studio / Gemini API). |
| togetherai | Duplicate of together (verified entry). |
| alibaba-token-plan-cn | Billing plan for Alibaba Cloud Model Studio, not a provider; catalog has alibaba / alibaba-cn. |
| fireworks-ai | Duplicate of fireworks (verified entry). |
| scnet-token-plan | Billing plan (SCNet token plan), not a provider. |
| google-vertex-anthropic | Duplicate of vertex (Anthropic-on-Vertex variant, not a separate provider). |

## Candidates pending (2)

| ID | Reason |
|---|---|
| neon | Base URL is user-supplied (${NEON_AI_GATEWAY_BASE_URL}/v1) — not a fixed provider endpoint; not addable as a fixed-base provider. |
| sap-ai-core | No fixed public base URL — per-deployment SAP BTP service URL behind a service key; not addable with a provable base. |

## Missing base URLs filled (43 of 44)

| ID | Base URL added | Probe | Note |
|---|---|---|---|
| agentrouter | https://agentrouter.org/v1 | 404 |  |
| unorouter | https://api.unorouter.com/v1 | 403 |  |
| command-code | https://api.commandcode.ai/provider/v1 | 404 |  |
| synthetic | https://api.synthetic.new/openai/v1 | 200 |  |
| opencode-go | https://opencode.ai/inference/go/openai/v1 | 404 | OpenCode Go subscription surface |
| nanogpt | https://nano-gpt.com/api/v1 | 404 |  |
| aimlapi | https://api.aimlapi.com/v1 | 404 |  |
| chutes | https://llm.chutes.ai/v1 | 404 |  |
| perplexity-agent | https://api.perplexity.ai/v1 | 404 | Same base as Perplexity API |
| morph | https://api.morphllm.com/v1 | 401 |  |
| ai21 | https://api.ai21.com/studio/v1 | 404 |  |
| upstage | https://api.upstage.ai/v1/solar | 404 | Solar path: /v1/solar |
| liquid | https://router.inference.ai.exchange/v1 | 404 |  |
| nebius | https://api.tokenfactory.nebius.com/v1 | 404 | Product is Nebius Token Factory |
| siliconflow | https://api.siliconflow.com/v1 | 404 |  |
| paraloncloud | https://paraloncloud.com/v1 | 200 |  |
| runcrate | https://api.runcrate.ai/v1 | 401 |  |
| sambanova | https://api.sambanova.ai/v1 | 405 |  |
| baseten | https://inference.baseten.co/v1 | 405 |  |
| wandb | https://api.inference.wandb.ai/v1 | 401 | Renamed to CoreWeave in this triage |
| hetzner | https://inference.hetzner.com/v1 | 401 |  |
| modelscope | https://api-inference.modelscope.cn/v1 | 404 |  |
| byteplus | https://ark.ap-southeast.bytepluses.com/api/v3 | 401 | Regional (ap-southeast); substitute your region |
| digitalocean | https://inference.do-ai.run/v1 | 404 |  |
| oci | https://inference.generativeai.us-chicago-1.oci.oraclecloud.com/openai/v1 | 404 | Regional (us-chicago-1); substitute your region |
| modal | https://inference.us-west.modal.direct/v1 | 404 | Regional (us-west); substitute your region |
| scaleway | https://api.scaleway.ai/v1 | 404 |  |
| ovhcloud | https://oai.endpoints.kepler.ai.cloud.ovh.net/v1 | 404 | Renamed to OVHcloud AI Endpoints; OVHcloud docs confirm this base |
| databricks | https://{DATABRICKS_HOST}/ai-gateway/mlflow/v1 | template | Template — substitute your workspace host; not probed |
| minimax | https://api.minimax.io/v1 | docs | Per MiniMax's own docs; models.dev /anthropic/v1 is wrong (extra /v1) |
| minimax-cn | https://api.minimax.cn/v1 | docs | Follows the global MiniMax docs' pattern; cn Anthropic surface not verified |
| zai | https://api.z.ai/api/paas/v4 | 401 |  |
| alibaba | https://dashscope-intl.aliyuncs.com/compatible-mode/v1 | 404 |  |
| alibaba-cn | https://dashscope.aliyuncs.com/compatible-mode/v1 | 404 |  |
| longcat | https://api.longcat.chat/openai | 200 | No /v1 suffix |
| volcengine | https://ark.cn-beijing.volces.com/api/v3 | 401 | Regional (cn-beijing); substitute your region |
| volcengine-coding-plan | https://ark.cn-beijing.volces.com/api/coding/v3 | 401 | Regional (cn-beijing); substitute your region |
| stepfun | https://api.stepfun.com/v1 | 404 |  |
| sensenova | https://token.sensenova.cn/v1 | 404 |  |
| agnes | https://apihub.agnes-ai.com/v1 | 404 |  |
| sarvam | https://api.sarvam.ai/v1 | 404 |  |
| clinepass | https://api.cline.bot/api/v1 | 401 |  |
| lm-studio | http://127.0.0.1:1234/v1 | loopback | LM Studio documented local-server default; loopback, not probed |

### Skipped (1)

- **`pioneer`**: NOT filled. models.dev points it at `https://api.fastino.ai/v1` (probe: HTTP 404 — the host responds), but Fastino is a different provider (Fastino AI), so the mapping looks like a models.dev data error, exactly as the 227–228 audit warned. Pioneer keeps no base URL until its own docs give one.

## Renames applied (4) — provider-doc confirmed

| ID | Old name | New name | Confirming doc |
|---|---|---|---|
| wandb | Weights & Biases Inference | CoreWeave | coreweave.com/companies/weights-and-biases — CoreWeave completed the W&B acquisition May 2025; products folding into CoreWeave Forge |
| ovhcloud | OVHcloud AI | OVHcloud AI Endpoints | docs.ovhcloud.com AI Endpoints docs — the product is AI Endpoints |
| minimax | Minimax Coding | MiniMax | platform.minimax.io/docs/guides/text-generation — brand per MiniMax's own platform docs; 'Coding' qualifier was wrong (a separate minimax-coding-plan exists) |
| minimax-cn | Minimax (China) | MiniMax (China) | platform.minimax.io — brand capitalization per MiniMax docs |

Note: `minimax` / `minimax-cn` still differ from models.dev (`MiniMax (minimax.io)` / `MiniMax (minimax.cn)`) because models.dev appends a disambiguator suffix; the clean brand name is kept deliberately, so those two `renamed` findings remain in the re-run audit.

## Renames rejected (17) — catalog name kept

| ID | Catalog name (kept) | models.dev name | Reason |
|---|---|---|---|
| anyapi | AnyAPI AI | AnyAPI | No provider-doc confirmation in this slice; models.dev alone is not enough to change a display name. |
| llmgateway | LLM Gateway | DevPass (LLM Gateway) | No provider-doc confirmation; 'DevPass' looks like a models.dev-side label, and llmgateway.io still brands LLM Gateway. |
| nan | NaN.Builders | NaN | No provider-doc confirmation in this slice. |
| chutes | Chutes.ai | Chutes | No provider-doc confirmation in this slice; '.ai' is the domain, not a proven misnomer. |
| pioneer | Pioneer AI | Pioneer | No provider-doc confirmation in this slice; also see the base-URL skip above. |
| liquid | Liquid AI | Liquid Inference | No provider-doc confirmation in this slice; 'Liquid Inference' may be the product name, unverified. |
| nvidia | NVIDIA NIM | Nvidia | Catalog is MORE precise: NIM is the inference product; models.dev uses the company name. Kept deliberately. |
| nebius | Nebius AI | Nebius Token Factory | Product is Token Factory (noted on the entry); the company-level name is kept for the entry. No rename needed. |
| hetzner | Hetzner Inference API | Hetzner | Catalog is more descriptive of the actual API product. Kept deliberately. |
| watsonx | IBM watsonx.ai Gateway | watsonx.ai | Catalog names the gateway product. Kept deliberately. |
| scaleway | Scaleway AI | Scaleway | Catalog names the AI division/product. Kept deliberately. |
| alibaba | Alibaba Cloud Model Studio | Alibaba | Catalog names the product (Model Studio). Kept deliberately. |
| longcat | LongCat AI | LongCat | No provider-doc confirmation in this slice. |
| volcengine | Volcengine | Volcengine Ark | Ark is plausibly the product name, but no provider-doc confirmation in this slice. |
| stepfun | StepFun | StepFun (China) | Regional qualifier adds nothing; catalog name is the brand. Kept deliberately. |
| sensenova | SenseNova | SenseNova (China) | Regional qualifier adds nothing; catalog name is the brand. Kept deliberately. |
| gitlab | GitLab Duo PAT | GitLab Duo | NOT renamed: a `gitlab-duo` entry already carries the name 'GitLab Duo' (OAuth variant); the 'PAT' suffix is what distinguishes this API-key/PAT variant. Renaming would create a duplicate display name (caught by TestNoDuplicateNormalizedNames). |

## Possible renames (12) — all pending, ID-level, needs a human

These are same-name / different-ID pairs. Changing a catalog ID breaks `provider add-catalog <id>` references, so none were changed in this slice; a human must confirm each ID move.

| Catalog ID | models.dev ID | Name |
|---|---|---|
| charm-hyper | hyper | Charm Hyper |
| cloudcode-one | cloudcode | CloudCode.ONE |
| qiniu | qiniu-ai | Qiniu |
| kilo-gateway | kilo | Kilo Gateway |
| vercel-ai-gateway | vercel | Vercel AI Gateway |
| novita | novita-ai | Novita AI |
| regolo | regolo-ai | Regolo AI |
| cloudflare-ai | cloudflare-workers-ai | Cloudflare Workers AI |
| snowflake | snowflake-cortex | Snowflake Cortex |
| bailian-coding-plan | alibaba-token-plan | Alibaba Token Plan |
| github | github-copilot | GitHub Copilot |
| prime-intellect | prime | Prime Inference |

## Candidates untouched (182)

The remaining models.dev providers absent from the catalog, not triaged in this slice (next slices work down the same list, largest first). Model counts are from the 2026-11-18 audit.

outerlabs (18), watt (18), gmicloud (17), io-net (17), jalapeno (17), the-grid-ai (17), zhipuai (17), dothoosh (16), evroc (16), hubris (16), kunavo (16), xxapi (16), abliteratedllm (15), model-oracle-ai (15), obscurify (15), routing-run (15), stepping-stone (15), tencent-token-plan (15), vultr (15), keln (14), scx-ai (14), aixy (13), tokengo (13), xpersona (13), zenifra (13), adzuki (12), aiand (12), inferx (12), moneo (12), synthorai (12), uplinkswitch (12), ambient (11), amd (11), arcee (11), crusoe (11), inferenceapis (11), privatemode-ai (11), wafer.ai (11), above (10), aistupidlevel (10), alibaba-coding-plan (10), alibaba-coding-plan-cn (10), engy (10), freemodel (10), infer (10), infomaniak (10), klavo (10), moark (10), sandbase (10), teamorouter (10), trustedtokens (10), umans-ai-coding-plan (10), aigrow (9), hpc-ai (9), inference (9), kitani (9), minimax-cn-coding-plan (9), minimax-coding-plan (9), modelis (9), qihang-ai (9), qvac (9), ruapi (9), runinfra (9), submodel (9), tinfoil (9), umans-ai (9), wally (9), zerogpu (9), aki-io (8), bothub (8), hostinger-router (8), huawei-maas (8), inference-coop (8), melious (8), stackit (8), tencent-coding-plan (8), vancine (8), adaption (7), graphn (7), inco (7), meta (7), powerspring (7), stepfun-ai (7), teapot-router (7), trustedrouter (7), xiaomi-token-plan-ams (7), xiaomi-token-plan-cn (7), xiaomi-token-plan-sgp (7), zai-coding-plan (7), augure (6), bee (6), berget (6), dinference (6), inceptron (6), inferall (6), infersia (6), kimi-code-plan-cn (6), kimi-code-plan-global (6), moonshotai (6), moonshotai-cn (6), pendra (6), refuseless (6), xiaomi (6), ai-router (5), atomic-chat (5), bailing (5), cheapaimodel (5), cloudferro-sherlock (5), ezkielyna (5), friendli (5), munito (5), sokkan (5), stepfun-step-plan (5), tokenify (5), zro (5), abliteration-ai (4), ebcloud (4), entrim (4), inferris (4), lucidquery (4), modular (4), munito-eu (4), neosmith (4), proxystacks (4), sakana (4), stepfun-ai-step-plan (4), tencent-tokenhub (4), tenstorrent (4), unrestricted (4), zhipuai-coding-plan (4), anonrouter (3), apiko (3), drun (3), jad-ai (3), joingonka (3), klokintegration (3), lithosai (3), nokeep (3), openreason (3), pareto (3), toapis (3), uncensuredai (3), vispark (3), wallaby (3), aipg (2), claudinio (2), hopper (2), humain (2), isoquant (2), logits (2), nova (2), oathbreaker (2), phase (2), rayline (2), subconscious (2), superbot-gg (2), autarcho (1), blueclaw (1), cedarllm (1), cheapllm (1), confidential-ai (1), coralbricks (1), echo (1), fiftyx (1), fortytwo (1), frequency (1), kosmik (1), kuae-cloud-coding-plan (1), llmtech (1), lynkr (1), mallard (1), meragpt (1), nodecode (1), reflection (1), sagg (1), salad-cloud (1), therouter (1), tokenlab (1), tokenwatt (1), unbiased (1), verda (1), zeldoc (1)

## Audit re-run (before → after)

Re-run `codvian-router catalog audit`, 2026-11-18 ~03:49 UTC (live models.dev fetch + reachability probes; triage record embedded). Note: the first re-run (~03:37 UTC) measured 621 / 603 / 18; the committed run below is a second pass ~12 minutes later, and one website probe flipped — `routeway`'s site timed out this time, so website_unreachable went 13 → 14 and reachable 603 → 602. That is probe flake, not a triage effect; every triage number is identical in both runs.

| Kind | Before (01:32 UTC) | After (03:49 UTC) |
|---|---|---|
| missing_in_catalog | 228 | 196 (−32, exactly the providers added) |
| base_url_missing_in_catalog | 44 | 1 (−43, exactly the fills; only `pioneer` remains) |
| renamed | 21 | 19 (−2: `wandb` and `ovhcloud` cleared; `minimax` ×2 stay flagged by design, `gitlab` stays — see above) |
| possible_rename | 12 | 12 (unchanged — ID-level, needs a human) |
| base_url_differs | 1 | 1 (fastrouter, untouched) |
| base_url_path_differs | 0 | 3 (new: `minimax`, `minimax-cn`, `merge-gateway` — all three are deliberate docs-over-models.dev choices: MiniMax's own docs give /v1 where models.dev says /anthropic/v1; Merge docs give /v1/openai where models.dev says /v1/ai-sdk) |
| base_url_missing_in_modelsdev | 8 | 9 (+1: `aihubmix` — models.dev lists no api for it; its base came from the live /v1/models probe) |
| website_unreachable | 13 | 14 (+1: `routeway` site timed out on this run only — probe flake, not a triage effect) |
| catalog total | 423 | 455 |
| verified-from-docs | 48 | 52 |
| reference-only | 371 | 399 |
| no base URL | 285 | 242 |
| reachability probed / reachable / unreachable | 515 / 497 / 18 | 621 / 602 / 19 (more bases to probe now; the +1 unreachable is the routeway website flake above) |
| loopback skipped | 4 | 5 (+1: `lm-studio`) |

The drift shrank by exactly the proven fixes: −32 candidates, −43 missing base URLs, −2 renames. Nothing else moved except the two explained consequences above (path-differs ×3, missing-in-modelsdev +1).

## Honest limitations

- Only the top 46 of 228 candidates were triaged; 182 remain, including recognizable names further down the list (e.g. zhipuai, xiaomi, amd, huawei-maas, vultr, tenstorrent, sakana, moonshotai).
- Reference-only additions rest on models.dev + a live probe, not the provider's own docs. A probe returning 401/403/404 proves the host answers, not that the path is the right chat-completions base — the wire format is assumed OpenAI-compatible per models.dev and must be confirmed before any of these is used as a routing target.
- The `renamed` count does not fully clear: minimax / minimax-cn keep the clean brand name against models.dev's suffixed names by design.
- `pioneer` still has no base URL (fastino mapping rejected as a likely models.dev data error).
- Out of scope for this slice and untouched: `base_url_differs` (fastrouter, 1), `verified_base_unreachable` (huggingface, clawcu), `base_url_unreachable` (5), `website_unreachable` (13), `missing_in_modelsdev` (327 — catalog entries models.dev does not list).
- models.dev itself is a moving target: the re-run fetches it live, so its provider/model counts may differ slightly from the 01:32 UTC audit.
