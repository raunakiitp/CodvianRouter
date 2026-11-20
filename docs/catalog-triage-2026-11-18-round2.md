# Catalog triage round 2 — 2026-11-18 (CHECKLIST items 265–266)

Round 3 (CHECKLIST items 275–276) triaged the next 46 candidates:
see `docs/catalog-triage-2026-11-18-round3.md` (cumulative record in
`docs/catalog-triage-2026-11-18.json`: 138 of 228 triaged, 108 added,
25 rejected, 5 pending, 90 untouched; 4 of 12 possible renames
applied).

Follow-up to round 1 (`docs/catalog-triage-2026-11-18.md`, which triaged
the top 46 of 228 candidates). This slice triaged the **next 46 of the
182 remaining** candidates by models.dev model count (outerlabs …
infomaniak, 18 down to 10 models), from the same 2026-11-18 audit.

## Headline

| | Count |
|---|---|
| Candidates triaged (round 2) | 46 of 182 |
| Providers added | 42 (3 verified-from-docs, 39 reference-only) |
| Candidates rejected (duplicate / plan, not a provider) | 4 |
| Candidates pending | 0 |
| Candidates untouched after round 2 | 136 |
| Extra base-URL fill (outside the audit's 44) | 1 (`wafer` — see below) |

Cumulative (rounds 1 + 2): **92 of 228 triaged, 74 added (7 verified
from docs, 67 reference-only), 16 rejected, 2 pending, 136 untouched**.

Catalog totals after this slice: **497 entries** (was 455) — 55
verified-from-docs (was 52), 438 reference-only (was 399), 4
unverified; no base URL 241 (was 242 — the `wafer` fill).

Verification rule used throughout (same as round 1): a provider was
added only when models.dev lists it **and** a live reachability probe
of its base URL returned an HTTP response (any status — 401/403/404 on
a bare base URL is the normal answer of a live API). `verified-from-docs`
was granted only where the provider's own docs were read and state the
base URL + auth. Base URLs were never bulk-filled from models.dev: each
addition below cites its own probe code.

## Providers added, verified from docs (3)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| gmicloud | GMI Cloud | https://api.gmi-serving.com/v1 | 405 | GMI docs (docs.gmicloud.ai/inference-engine/api-reference/llm-api-reference): Bearer API key, GET https://api.gmi-serving.com/v1/models, POST /v1/chat/completions |
| vultr | Vultr | https://api.vultrinference.com/v1 | 200 | Vultr docs (api.vultrinference.com): requests go to https://api.vultrinference.com/v1 with the API key as a Bearer token; OpenAI/OpenRouter shapes |
| privatemode-ai | Privatemode AI | https://proxyless-api.privatemode.ai/v1 | 404 | Privatemode docs (docs.privatemode.ai/api/proxyless-api/): proxyless API base + API key. models.dev lists the LOCAL proxy base http://localhost:8080/v1 instead — that is the endpoint of the Privatemode proxy app/Docker on the user's own machine, not the cloud API, so the catalog uses the docs-verified cloud base |

## Providers added, reference-only (39)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| outerlabs | Outer Labs | https://api.outerlabs.ai/v1 | 404 | models.dev api.json + probe |
| watt | Watt Cloud | https://api.watt-cloud.com/v1 | 400 | models.dev api.json + probe |
| io-net | IO.NET | https://api.intelligence.io.solutions/api/v1 | 404 | models.dev api.json + probe; IO Intelligence overview page states no base URL in fetched text |
| jalapeno | Jalapeno Cloud | https://api.jalapeno-cloud.ai/v1 | 400 | models.dev api.json + probe |
| the-grid-ai | The Grid AI | https://api.thegrid.ai/v1 | 404 | models.dev api.json + probe |
| zhipuai | Zhipu AI | https://open.bigmodel.cn/api/paas/v4 | 401 | models.dev api.json + probe; models.dev doc link is a pricing page, not the API reference |
| dothoosh | DotHoosh | https://gateway.dothoosh.com/v1 | 404 | models.dev api.json + probe |
| evroc | evroc | https://models.think.evroc.com/v1 | 404 | models.dev api.json + probe |
| hubris | Hubris | https://api.hubris.pw/v1 | 404 | models.dev api.json + probe |
| kunavo | Kunavo | https://api.kunavo.com/v1 | 404 | models.dev api.json + probe |
| xxapi | xxapi | https://xxapi.ai/v1 | 404 | models.dev api.json + probe |
| abliteratedllm | AbliteratedLLM | https://api.abliteratedllm.dev/v1 | 404 | models.dev api.json + probe |
| model-oracle-ai | Model Oracle AI | https://api.modeloracle.com/api/v1 | 404 | models.dev api.json + probe |
| obscurify | Obscurify.ai | https://obscurify.ai/v1 | 404 | models.dev api.json + probe |
| routing-run | routing.run | https://api.routing.run/v1 | 401 | models.dev api.json + probe |
| stepping-stone | stepping stone AG | https://llm.stoney-cloud.com/v1 | 404 | models.dev api.json + probe |
| keln | Keln | https://api.keln.ai/v1 | 404 | models.dev api.json + probe |
| scx-ai | SCX.ai | https://api.scx.ai/v1 | 401 | models.dev api.json + probe |
| aixy | Aixy | https://api.aixy-gateway.com/v1 | 404 | models.dev api.json + probe |
| tokengo | TokenGo | https://api.tokengo.com/v1 | 404 | models.dev api.json + probe |
| xpersona | Xpersona | https://www.xpersona.co/v1 | 404 | models.dev api.json + probe |
| zenifra | Zenifra | https://ai.zenifra.com/v1 | 404 | models.dev api.json + probe |
| adzuki | Adzuki AI | https://adzukiai.com/v1 | 404 | models.dev api.json + probe |
| aiand | ai& | https://api.aiand.com/v1 | 401 | models.dev api.json + probe |
| inferx | InferX | https://model.inferx.net/endpoints/v1 | 404 | models.dev api.json + probe |
| moneo | Moneo | https://api.moneo.one/v1 | 404 | models.dev api.json + probe |
| synthorai | Synthorai | https://synthorai.io/v1 | 401 | models.dev api.json + probe |
| uplinkswitch | UplinkSwitch | https://api.uplinkswitch.com/v1 | 404 | models.dev api.json + probe |
| ambient | Ambient | https://api.ambient.xyz/v1 | 401 | models.dev api.json + probe |
| amd | AMD | https://developer.amd.com.cn/radeon/api/v1 | 401 | models.dev api.json + probe; AMD China Radeon token-factory surface |
| arcee | Arcee | https://api.arcee.ai/api/v1 | 404 | models.dev api.json + probe |
| crusoe | Crusoe | https://api.inference.crusoecloud.com/v1 | 401 | models.dev api.json + probe; Crusoe docs confirm an OpenAI-compatible API but state no base URL in fetched text |
| inferenceapis | Inference APIs | https://api.inferenceapis.com/v1 | 404 | models.dev api.json + probe |
| above | above.dev | https://api.above.dev/v1 | 200 | models.dev api.json + probe |
| aistupidlevel | AI Stupid Level | https://aistupidlevel.info/v1 | 404 | models.dev api.json + probe |
| engy | engy | https://api.engy.ai/v1 | 404 | models.dev api.json + probe |
| freemodel | FreeModel | https://cc.freemodel.dev/v1 | 403 | models.dev api.json + probe |
| infer | Infer by Flow7 | https://infer.flow7.org/v1 | 404 | models.dev api.json + probe |
| infomaniak | Infomaniak | https://api.infomaniak.com/2/ai/${INFOMANIAK_PRODUCT_ID}/openai/v1 | 404 | models.dev api.json + probe on the literal templated URL (host api.infomaniak.com responds). Template — substitute your Infomaniak product ID; the per-product path itself is unprobed |

## Candidates rejected (4) — duplicates or billing plans, not new providers

| ID | Reason |
|---|---|
| tencent-token-plan | Billing plan for Tencent MaaS (token hub), not a provider — same class as round 1's alibaba-token-plan-cn / scnet-token-plan rejections. |
| wafer.ai | Duplicate of catalog `wafer` (Wafer AI, same website wafer.ai). The existing entry had no base URL, so it gains one in this round (see fill below) — but the `wafer.ai` candidate itself is rejected, and the audit keeps flagging it as missing_in_catalog because the catalog ID is `wafer`, not `wafer.ai` (ID mismatch, like the possible_rename pairs). |
| alibaba-coding-plan | Coding plan for Alibaba Cloud Model Studio, not a provider; catalog has alibaba / alibaba-cn. |
| alibaba-coding-plan-cn | Coding plan for Alibaba Cloud Model Studio (China), not a provider; catalog has alibaba / alibaba-cn. |

## Candidates pending (0)

None in this round. Every candidate had a provable fixed base URL or a
rejection reason; `infomaniak`'s templated base was added with the
template (databricks precedent from round 1), not left pending.

## Extra base-URL fill (1) — outside the audit's 44

| ID | Base URL added | Probe | Note |
|---|---|---|---|
| wafer | https://pass.wafer.ai/v1 | 401 | models.dev lists the same provider as `wafer.ai`; the existing catalog entry had no base URL. This is NOT one of the audit's 44 missing base URLs — the audit never paired catalog `wafer` with models.dev `wafer.ai` (ID mismatch) — so the round-1 JSON ledger (43 filled + 1 skipped = 44) is left untouched and this fill is recorded here instead. |

## Candidates untouched (136)

The remaining models.dev providers absent from the catalog, not triaged
in this slice (next slices work down the same list, largest first).
Model counts are from the 2026-11-18 audit.

klavo (10), moark (10), sandbase (10), teamorouter (10), trustedtokens (10), umans-ai-coding-plan (10), aigrow (9), hpc-ai (9), inference (9), kitani (9), minimax-cn-coding-plan (9), minimax-coding-plan (9), modelis (9), qihang-ai (9), qvac (9), ruapi (9), runinfra (9), submodel (9), tinfoil (9), umans-ai (9), wally (9), zerogpu (9), aki-io (8), bothub (8), hostinger-router (8), huawei-maas (8), inference-coop (8), melious (8), stackit (8), tencent-coding-plan (8), vancine (8), adaption (7), graphn (7), inco (7), meta (7), powerspring (7), stepfun-ai (7), teapot-router (7), trustedrouter (7), xiaomi-token-plan-ams (7), xiaomi-token-plan-cn (7), xiaomi-token-plan-sgp (7), zai-coding-plan (7), augure (6), bee (6), berget (6), dinference (6), inceptron (6), inferall (6), infersia (6), kimi-code-plan-cn (6), kimi-code-plan-global (6), moonshotai (6), moonshotai-cn (6), pendra (6), refuseless (6), xiaomi (6), ai-router (5), atomic-chat (5), bailing (5), cheapaimodel (5), cloudferro-sherlock (5), ezkielyna (5), friendli (5), munito (5), sokkan (5), stepfun-step-plan (5), tokenify (5), zro (5), abliteration-ai (4), ebcloud (4), entrim (4), inferris (4), lucidquery (4), modular (4), munito-eu (4), neosmith (4), proxystacks (4), sakana (4), stepfun-ai-step-plan (4), tencent-tokenhub (4), tenstorrent (4), unrestricted (4), zhipuai-coding-plan (4), anonrouter (3), apiko (3), drun (3), jad-ai (3), joingonka (3), klokintegration (3), lithosai (3), nokeep (3), openreason (3), pareto (3), toapis (3), uncensuredai (3), vispark (3), wallaby (3), aipg (2), claudinio (2), hopper (2), humain (2), isoquant (2), logits (2), nova (2), oathbreaker (2), phase (2), rayline (2), subconscious (2), superbot-gg (2), autarcho (1), blueclaw (1), cedarllm (1), cheapllm (1), confidential-ai (1), coralbricks (1), echo (1), fiftyx (1), fortytwo (1), frequency (1), kosmik (1), kuae-cloud-coding-plan (1), llmtech (1), lynkr (1), mallard (1), meragpt (1), nodecode (1), reflection (1), sagg (1), salad-cloud (1), therouter (1), tokenlab (1), tokenwatt (1), unbiased (1), verda (1), zeldoc (1)

## Audit re-run (before → after)

Re-run `codvian-router catalog audit`, 2026-11-18 04:30 UTC (live
models.dev fetch + reachability probes; cumulative triage record
embedded). Before is the round-1 re-run (03:49 UTC).

| Kind | Before (03:49 UTC) | After (04:30 UTC) |
|---|---|---|
| missing_in_catalog | 196 | 154 (−42, exactly the providers added) |
| base_url_missing_in_catalog | 1 | 1 (pioneer only — unchanged, still not filled) |
| renamed | 19 | 19 (unchanged) |
| possible_rename | 12 | 12 (unchanged — ID-level, needs a human) |
| base_url_differs | 1 | 2 (+1: `privatemode-ai` — deliberate docs-over-models.dev choice: the docs-verified proxyless cloud base vs models.dev's localhost proxy base) |
| base_url_path_differs | 3 | 3 (unchanged) |
| base_url_missing_in_modelsdev | 9 | 9 (unchanged) |
| website_unreachable | 14 | 16 (+2 net: `routing-run`'s docs site gave EOF on connect during this run — its API base is live at 401, so the addition stands; `huggingface` + `huggingchat` timed out this run, both pre-existing entries. `routeway`, unreachable last run, was reachable this time. All three are probe flake, not a triage effect) |
| catalog total | 455 | 497 (+42) |
| verified-from-docs | 52 | 55 (+3) |
| reference-only | 399 | 438 (+39) |
| no base URL | 242 | 241 (−1: the `wafer` fill) |
| reachability probed / reachable / unreachable | 621 / 602 / 19 | 702 / 681 / 21 |
| loopback skipped | 5 | 5 (unchanged) |

The drift shrank by exactly the proven fixes: −42 candidates, −1
missing base URL. Nothing else moved except the two explained
consequences above (`privatemode-ai` base_url_differs, website
probe flake). `wafer.ai` stays in missing_in_catalog by design (ID
mismatch with catalog `wafer`); the 4 rejected candidates stay out
by design (3 plans, 1 duplicate).

## Honest limitations

- Only the next 46 of 182 candidates were triaged; 136 remain.
- Reference-only additions rest on models.dev + a live probe, not the
  provider's own docs. A probe returning 401/403/404 proves the host
  answers, not that the path is the right chat-completions base — the
  wire format is assumed OpenAI-compatible per models.dev and must be
  confirmed before any of these is used as a routing target.
- `wafer.ai` stays flagged missing_in_catalog by the audit (ID mismatch
  with catalog `wafer`); only a human-approved ID move would clear it,
  like the 12 possible_rename pairs.
- `infomaniak`'s base URL is a template: the per-product path is
  unprobed until a real product ID is substituted.
- models.dev itself is a moving target: the re-run fetches it live, so
  its provider/model counts may differ slightly from the 01:32 UTC audit.
