# Catalog triage round 3 — 2026-11-18 (CHECKLIST items 275–276)

Follow-up to round 2 (`docs/catalog-triage-2026-11-18-round2.md`, which
triaged the next 46 of 182 candidates). This slice triaged the
**next 46 of the 136 remaining** candidates by models.dev model count
(klavo … berget, 10 down to 6 models), from the same 2026-11-18 audit,
and resolved the 12 possible renames where the providers' own docs
prove them.

## Headline

| | Count |
|---|---|
| Candidates triaged (round 3) | 46 of 136 |
| Providers added | 34 (3 verified-from-docs, 31 reference-only) |
| Candidates rejected (duplicate / plan, not a provider) | 9 |
| Candidates pending | 3 |
| Candidates untouched after round 3 | 90 |
| Possible renames applied (ID moves, docs-proven) | 4 of 12 |
| Possible renames decided-keep (docs prove the catalog ID) | 3 of 12 |
| Possible renames still pending (ID-level, needs a human) | 5 of 12 |
| Extra base-URL fill (outside the audit's 44) | 1 (`inference-net` — see below) |
| Rename-consequence base-URL fills | 3 (github-copilot, alibaba-token-plan, snowflake-cortex — see below; cloudflare-workers-ai already had its verified base) |

Cumulative (rounds 1 + 2 + 3): **138 of 228 triaged, 108 added
(10 verified from docs, 98 reference-only), 25 rejected, 5 pending,
90 untouched**.

Catalog totals after this slice: **531 entries** (was 497) — 58
verified-from-docs (was 55), 469 reference-only (was 438), 4
unverified; no base URL 237 (was 241 — the `inference-net` fill plus
3 rename-consequence fills; the cloudflare move filled nothing
because that entry already had its verified base).

Verification rule used throughout (same as rounds 1–2): a provider
was added only when models.dev lists it **and** a live reachability
probe of its base URL returned an HTTP response (any status —
401/403/404 on a bare base URL is the normal answer of a live API).
`verified-from-docs` was granted only where the provider's own docs
were read and state the base URL + auth. Base URLs were never
bulk-filled from models.dev: each addition below cites its own probe
code.

## Providers added, verified from docs (3)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| klavo | Klavo AI | https://api.klavo.ru/v1 | 404 | Klavo docs (ai.klavo.ru/docs): OpenAI SDK quickstart with base_url https://api.klavo.ru/v1, Authorization: Bearer; endpoints /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/messages, GET /v1/models |
| huawei-maas | Huawei Cloud MAAS | https://api.modelarts-maas.com/openai/v1 | 404 | Huawei Cloud MaaS docs (support.huaweicloud.com/model-call-maas/model-call-021.html): OpenAI-compatible API address https://api.modelarts-maas.com/openai/v1/chat/completions, base_url https://api.modelarts-maas.com/openai/v1, Authorization: Bearer $MaaS_API_Key. Regional: only Southwest-Guiyang 1 is supported per the same page |
| stackit | STACKIT | https://api.openai-compat.model-serving.eu01.onstackit.cloud/v1 | 403 | STACKIT docs (docs.stackit.cloud …/ai-model-serving/): every shared model lists this URL with OpenAI-compatible POST /chat/completions, /completions, /embeddings and GET /models; the getting-started guide creates an AI Model Serving auth token and calls the API with Authorization: Bearer [auth-token] |

## Providers added, reference-only (31)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| moark | Moark | https://moark.com/v1 | 200 | models.dev api.json + probe; the models.dev doc link itself failed to fetch from this environment, so no docs claim is made |
| sandbase | SandBase | https://api.sandbase.ai/v1 | 404 | models.dev api.json + probe |
| teamorouter | TeamoRouter | https://api.teamorouter.com/v1 | 404 | models.dev api.json + probe |
| trustedtokens | TrustedTokens | https://api.trustedtokens.eu/v1 | 404 | models.dev api.json + probe |
| aigrow | AIGROW | https://aigrow.gg/v1 | 404 | models.dev api.json + probe |
| hpc-ai | HPC-AI | https://api.hpc-ai.com/inference/v1 | 404 | models.dev api.json + probe |
| kitani | Kitani | https://api.kitani.ai/v1 | 404 | models.dev api.json + probe |
| qihang-ai | QiHang | https://api.qhaigc.net/v1 | 404 | models.dev api.json + probe |
| ruapi | RuAPI | https://www.ruapi.ai/v1 | 404 | models.dev api.json + probe |
| runinfra | RunInfra | https://api.runinfra.ai/v1 | 404 | models.dev api.json + probe |
| submodel | submodel | https://llm.submodel.ai/v1 | 404 | models.dev api.json + probe |
| tinfoil | Tinfoil | https://inference.tinfoil.sh/v1 | 404 | models.dev api.json + probe; Tinfoil docs confirm an OpenAI-compatible API but state no base URL in fetched text |
| umans-ai | Umans AI | https://api.code.umans.ai/v1 | 404 | models.dev api.json + probe; the rejected umans-ai-coding-plan billing plan shares this base URL |
| wally | Wally | https://inference.runanywhere.ai/v1 | 404 | models.dev api.json + probe; Wally is RunAnywhere's cloud inference product — its docs confirm an OpenAI-compatible API with a cloud API key but state no base URL in fetched text |
| zerogpu | ZeroGPU | https://api.zerogpu.ai/v1 | 404 | models.dev api.json + probe |
| aki-io | AKI.IO | https://aki.io/openai/v1 | 200 | models.dev api.json + probe |
| bothub | Bothub | https://openai.bothub.ru/v1 | 404 | models.dev api.json + probe |
| hostinger-router | Hostinger Router | https://router.hostinger.com/v1 | 404 | models.dev api.json + probe; Hostinger's page confirms an OpenAI-compatible router with one API key but states no base URL in fetched text |
| inference-coop | Inference Cooperative | https://gateway.inference.coop/v1 | 404 | models.dev api.json + probe |
| melious | Melious | https://api.melious.ai/v1 | 404 | models.dev api.json + probe |
| vancine | Vancine | https://vancine.com/v1 | 404 | models.dev api.json + probe |
| adaption | Adaption | https://api.prod.adaptionlabs.ai/api/v1 | 200 | models.dev api.json + probe |
| graphn | GraphN | https://model.graphn.ai/v1 | 401 | models.dev api.json + probe |
| inco | Inco | https://api.inco.ai/v1 | 404 | models.dev api.json + probe |
| powerspring | Powerspring | https://api.powerspring.eu/v1 | 404 | models.dev api.json + probe |
| stepfun-ai | StepFun (Global) | https://api.stepfun.ai/v1 | 404 | models.dev api.json + probe; global surface — catalog `stepfun` is the China surface (api.stepfun.com/v1), same regional-surface precedent as round 1's siliconflow/siliconflow-cn. StepFun's global docs state no base URL in fetched text |
| teapot-router | Teapot Router | https://api.teapotrouter.com/v1 | 404 | models.dev api.json + probe |
| trustedrouter | TrustedRouter | https://api.trustedrouter.com/v1 | 401 | models.dev api.json + probe |
| augure | Augure | https://api.augureai.ca/v1 | 401 | models.dev api.json + probe |
| bee | Bee by HEOSSI | https://api.bee.heossi.com/bee | 404 | models.dev api.json + probe; base has no /v1 suffix (path /bee) |
| berget | Berget.AI | https://api.berget.ai/v1 | 404 | models.dev api.json + probe |

## Candidates rejected (9) — duplicates or billing plans, not new providers

| ID | Reason |
|---|---|
| umans-ai-coding-plan | Coding plan for Umans AI, not a provider; catalog has umans-ai (added in this round, same base URL). |
| minimax-cn-coding-plan | Token plan for MiniMax (China), not a provider; catalog has minimax / minimax-cn. |
| minimax-coding-plan | Token plan for MiniMax, not a provider; catalog has minimax / minimax-cn. |
| inference | Duplicate of catalog `inference-net` (Inference.net, same website inference.net). The existing entry had no base URL, so it gains one in this round (see fill below) — but the `inference` candidate itself is rejected, and the audit keeps flagging it as missing_in_catalog because the catalog ID is `inference-net`, not `inference` (ID mismatch, like the possible_rename pairs and round 2's `wafer.ai`). |
| tencent-coding-plan | Coding plan for Tencent, not a provider; catalog has tencent. |
| xiaomi-token-plan-ams | Token plan for Xiaomi MiMo (Europe), not a provider; catalog has xiaomi-mimo / xiaomi-mimo-token-plan. |
| xiaomi-token-plan-cn | Token plan for Xiaomi MiMo (China), not a provider; catalog has xiaomi-mimo / xiaomi-mimo-token-plan. |
| xiaomi-token-plan-sgp | Token plan for Xiaomi MiMo (Singapore), not a provider; catalog has xiaomi-mimo / xiaomi-mimo-token-plan. |
| zai-coding-plan | Coding plan for Z.AI, not a provider; catalog has zai. |

## Candidates pending (3)

| ID | Reason |
|---|---|
| modelis | Base host returns Cloudflare HTTP 530 (origin unreachable) on both the API base (modelishub.com/v1) and the homepage — the site is down behind Cloudflare, so the API cannot be proven live. Not added, not rejected: re-probe later. |
| qvac | models.dev lists no API base URL (doc link is an npm package page), and the qvac.ai site closed the connection without a response — no provable fixed base URL. Not addable as a fixed-base provider. |
| meta | api.meta.ai/v1 is unreachable from this environment: the egress proxy refuses the connection (CONNECT tunnel 403) and the browser fetch path blocks the destination too — the host cannot be proven live or down from here. Not added on models.dev alone. |

## Extra base-URL fill (1) — outside the audit's 44

| ID | Base URL added | Probe | Note |
|---|---|---|---|
| inference-net | https://inference.net/v1 | 404 | models.dev lists the same provider as `inference`; the existing catalog entry had no base URL. This is NOT one of the audit's 44 missing base URLs — the audit never paired catalog `inference-net` with models.dev `inference` (ID mismatch) — so the round-1 JSON ledger (43 filled + 1 skipped = 44) is left untouched and this fill is recorded here instead, exactly like round 2's `wafer` fill. |

## Possible renames resolved (12) — 4 applied, 3 decided-keep, 5 still pending

An ID move was applied ONLY where the provider's own docs prove
the models.dev ID is the canonical product identifier. Every move
keeps the old ID as the entry's Alias, so `provider add-catalog
<old-id>` and stored configs that reference it keep resolving (via
the ByAlias fallback); the display name never changes in a move.
The audit's `possible_rename` count drops by exactly the 4 applied
moves.

### Applied (4)

| Catalog ID → new ID | Name | Base fill | Confirming doc |
|---|---|---|---|
| cloudflare-ai → cloudflare-workers-ai | Cloudflare Workers AI | none — the entry is verified-from-docs and already carries the same templated base (https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1) | developers.cloudflare.com/workers-ai brands the product Workers AI; the catalog name already is Cloudflare Workers AI. Old alias "cloudflare" is replaced by the old ID "cloudflare-ai"; the cloudflare-ai-gateway note that said "Distinct from cloudflare-ai" now says "Distinct from cloudflare-workers-ai". |
| snowflake → snowflake-cortex | Snowflake Cortex | template https://${SNOWFLAKE_ACCOUNT}.snowflakecomputing.com/api/v2/cortex/v1 (not probed — the host itself is templated) | docs.snowflake.com/en/user-guide/snowflake-cortex/cortex-rest-api brands the product Snowflake Cortex and gives the OpenAI-compatible Chat Completions base https://<account-identifier>.snowflakecomputing.com/api/v2/cortex/v1 with Bearer PAT auth — the docs confirm the template and the OpenAI wire format, so the entry stays reference-only but the fill is docs-shaped. Substitute your Snowflake account identifier. |
| github → github-copilot | GitHub Copilot | https://api.githubcopilot.com (probe 404) | docs.github.com/en/copilot brands the product GitHub Copilot and the API host is api.githubcopilot.com — the old ID was the company, not the product. The entry gains the docs site as its website (it had none). The short alias "gh" is replaced by the old ID "github". |
| bailian-coding-plan → alibaba-token-plan | Alibaba Token Plan | https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1 (probe 404) | The entry's own name is already Alibaba Token Plan and its website already is Alibaba Cloud's token-plan overview (www.alibabacloud.com/help/en/model-studio/token-plan-overview) — Bailian is the legacy Chinese brand for Model Studio — so the models.dev ID matches the product the entry already describes. Regional (ap-southeast-1); substitute your region. The short alias "bcp" is replaced by the old ID "bailian-coding-plan". |

### Decided-keep (3) — docs prove the catalog ID, no human needed

| Catalog ID (kept) | models.dev ID | Name | Reason |
|---|---|---|---|
| cloudcode-one | cloudcode | CloudCode.ONE | The brand is CloudCode.ONE (cloudcode.one) — the ".ONE" is part of the brand, so the catalog ID matches the provider's own branding and models.dev's shorter ID is not proven canonical. |
| kilo-gateway | kilo | Kilo Gateway | kilo.ai brands the gateway product "Kilo Gateway" ("via Kilo Gateway", "top up the Kilo gateway balance") — the catalog ID names the product precisely; models.dev uses the company name. Same as round 1's nvidia precedent (catalog more precise, kept deliberately). |
| vercel-ai-gateway | vercel | Vercel AI Gateway | Vercel's own docs brand the product "AI Gateway" (vercel.com/docs/ai-gateway) — the catalog ID names the product; models.dev uses the company name. Also models.dev lists no API base for it, so nothing is gained by the move. |

These three stay flagged `possible_rename` by the audit (their IDs
did not change) — deliberately, with the reason recorded here.

### Still pending (5) — ID-level, needs a human, do not guess

| Catalog ID | models.dev ID | Name | Reason |
|---|---|---|---|
| charm-hyper | hyper | Charm Hyper | The product is Hyper by Charm (hyper.charm.land), but "hyper" is a generic word and the provider's docs state no canonical machine ID — the catalog ID disambiguates, and neither ID is proven. |
| qiniu | qiniu-ai | Qiniu | Qiniu's own docs present the service as Qiniu Cloud's AI large-model inference (developer.qiniu.com/aitokenapi) without stating a canonical "qiniu-ai" identifier; the catalog name "Qiniu" is the company. |
| novita | novita-ai | Novita AI | The brand is "Novita AI" (novita.ai) and the API host is api.novita.ai; neither "novita" nor "novita-ai" is stated as a canonical ID by the provider's docs. |
| regolo | regolo-ai | Regolo AI | Same as novita: the brand is "Regolo AI" (regolo.ai, docs.regolo.ai); neither ID is proven canonical by the provider's docs. |
| prime-intellect | prime | Prime Inference | The provider's docs live at docs.primeintellect.ai and the product is "Prime Inference" (api.pinference.ai); "prime" is an abbreviation the provider's docs do not use as an identifier. |

## Candidates untouched (90)

The remaining models.dev providers absent from the catalog, not
triaged in this slice (next slices work down the same list, largest
first). Model counts are from the 2026-11-18 audit.

dinference (6), inceptron (6), inferall (6), infersia (6), kimi-code-plan-cn (6), kimi-code-plan-global (6), moonshotai (6), moonshotai-cn (6), pendra (6), refuseless (6), xiaomi (6), ai-router (5), atomic-chat (5), bailing (5), cheapaimodel (5), cloudferro-sherlock (5), ezkielyna (5), friendli (5), munito (5), sokkan (5), stepfun-step-plan (5), tokenify (5), zro (5), abliteration-ai (4), ebcloud (4), entrim (4), inferris (4), lucidquery (4), modular (4), munito-eu (4), neosmith (4), proxystacks (4), sakana (4), stepfun-ai-step-plan (4), tencent-tokenhub (4), tenstorrent (4), unrestricted (4), zhipuai-coding-plan (4), anonrouter (3), apiko (3), drun (3), jad-ai (3), joingonka (3), klokintegration (3), lithosai (3), nokeep (3), openreason (3), pareto (3), toapis (3), uncensuredai (3), vispark (3), wallaby (3), aipg (2), claudinio (2), hopper (2), humain (2), isoquant (2), logits (2), nova (2), oathbreaker (2), phase (2), rayline (2), subconscious (2), superbot-gg (2), autarcho (1), blueclaw (1), cedarllm (1), cheapllm (1), confidential-ai (1), coralbricks (1), echo (1), fiftyx (1), fortytwo (1), frequency (1), kosmik (1), kuae-cloud-coding-plan (1), llmtech (1), lynkr (1), mallard (1), meragpt (1), nodecode (1), reflection (1), sagg (1), salad-cloud (1), therouter (1), tokenlab (1), tokenwatt (1), unbiased (1), verda (1), zeldoc (1)

## Audit re-run (before → after)

Re-run `codvian-router catalog audit`, 2026-11-18 06:05 UTC (live
models.dev fetch + reachability probes; cumulative triage record
embedded). Before is the fresh baseline re-run at 05:54 UTC (same
numbers as the round-2 after-run at 04:30 UTC except probe flake:
`base_url_unreachable` 5 → 6, `website_unreachable` 16 → 18).

| Kind | Before (05:54 UTC) | After (06:05 UTC) |
|---|---|---|
| missing_in_catalog | 154 | 120 (−34, exactly the providers added) |
| base_url_missing_in_catalog | 1 | 1 (pioneer only — unchanged; the 4 rename-consequence fills + the inference-net fill kept it flat) |
| renamed | 19 | 19 (unchanged) |
| possible_rename | 12 | 8 (−4, exactly the applied ID moves; the 3 decided-keep + 5 pending stay flagged by design) |
| base_url_differs | 2 | 2 (unchanged) |
| base_url_path_differs | 3 | 3 (unchanged) |
| base_url_missing_in_modelsdev | 9 | 9 (unchanged) |
| base_url_unreachable | 6 | 4 (−2: probe flake — `huggingface` and `speka` timed out in the before-run and answered in the after-run) |
| verified_base_unreachable | 2 | 1 (−1: same `huggingface` flake; `clawcu` stays) |
| website_unreachable | 18 | 14 (−4: probe flake — `huggingface`, `huggingchat`, `speka` and `vercel-ai-gateway` timed out in the before-run and answered in the after-run) |
| missing_in_modelsdev | 327 | 327 (unchanged) |
| catalog total | 497 | 531 (+34) |
| verified-from-docs | 55 | 58 (+3) |
| reference-only | 438 | 469 (+31) |
| no base URL | 241 | 237 (−4: the inference-net fill + the github-copilot, alibaba-token-plan and snowflake-cortex rename fills; the cloudflare move filled nothing because the verified base already existed) |
| reachability probed / reachable / unreachable | 702 / 678 / 24 | 774 / 756 / 18 (+72 probed: 34 new bases + 34 new websites + 4 fills; −6 unreachable is the flake above, zero newly unreachable — no round-3 addition appears in the unreachable lists) |
| loopback skipped | 5 | 5 (unchanged) |
| models.dev providers / models | 324 / 9978 | 324 / 9978 (unchanged — models.dev was stable between the runs) |

The drift shrank by exactly the proven fixes: −34 candidates, −4
possible renames, −4 missing base URLs. Nothing else moved except
probe flake in the reachability/website counts (recorded in the
after-run). `inference` stays in missing_in_catalog by design (ID
mismatch with catalog `inference-net`); the 8 rejected plans and
the 3 pending candidates stay out by design.

## Honest limitations

- Only the next 46 of 136 candidates were triaged; 90 remain.
- Reference-only additions rest on models.dev + a live probe, not
  the provider's own docs. A probe returning 401/403/404 proves the
  host answers, not that the path is the right chat-completions base
  — the wire format is assumed OpenAI-compatible per models.dev and
  must be confirmed before any of these is used as a routing target.
- `inference` stays flagged missing_in_catalog by the audit (ID
  mismatch with catalog `inference-net`); only a human-approved ID
  move would clear it, like the 5 pending possible_rename pairs.
- `snowflake-cortex`'s base URL is a template: the per-account path
  is unprobed until a real account identifier is substituted
  (databricks/infomaniak precedent). The Cloudflare template fill
  was NOT made: the entry already carries the verified
  `{account_id}` template.
- `meta` could not be verified from this environment at all (the
  egress proxy blocks api.meta.ai); `modelis`'s origin is down
  (Cloudflare 530); `qvac` has no fixed base URL. All three stay
  pending rather than guessed.
- models.dev itself is a moving target: the re-run fetches it live,
  so its provider/model counts may differ slightly from the 01:32 UTC
  audit.
