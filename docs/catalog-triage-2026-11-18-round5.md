# Catalog triage round 5 (FINAL) — 2026-11-18 (CHECKLIST items 301–302)

Follow-up to round 4 (`docs/catalog-triage-2026-11-18-round4.md`, which
triaged the next 46 of 90 candidates). This slice triaged the **LAST
44 candidates** by models.dev model count (openreason … zeldoc, 3 down
to 1 models), from the same 2026-11-18 audit, and re-checked the 3
still-pending possible renames against the providers' own docs.

## Headline

| | Count |
|---|---|
| Candidates triaged (round 5) | 44 of 44 |
| Providers added | 44 (5 verified-from-docs, 39 reference-only) |
| Candidates rejected | 0 |
| Candidates pending | 0 |
| Candidates untouched after round 5 | 0 — **the triage is complete: 228 of 228 triaged** |
| Possible renames applied (ID moves, docs-proven) | 0 of 3 |
| Possible renames still pending (ID-level, needs a human) | 3 of 3 |

Cumulative (rounds 1 + 2 + 3 + 4 + 5): **228 of 228 triaged, 191 added
(17 verified from docs, 174 reference-only), 30 rejected, 7 pending,
0 untouched**.

Catalog totals after this slice: **614 entries** (was 570) — 65
verified-from-docs (was 60), 545 reference-only (was 506), 4
unverified; no base URL 234 (unchanged — every one of the 44 new
entries carries a base URL, and round 5 applies no base-URL fills).

Verification rule used throughout (same as rounds 1–4): a provider
was added only when models.dev lists it **and** a live reachability
probe of its base URL returned an HTTP response (any status —
401/403/404 on a bare base URL is the normal answer of a live API).
`verified-from-docs` was granted only where the provider's own docs
were read and state the base URL + auth. Base URLs were never
bulk-filled from models.dev: each addition below cites its own probe
code. `pioneer` is still NOT filled from models.dev (see round 1).

## Providers added, verified from docs (5)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| salad-cloud | SaladCloud AI Gateway | https://ai.salad.cloud/v1 | 401 (GET /v1/models → 200) | SaladCloud's own docs (docs.salad.com/ai-gateway/explanation/overview): the AI Gateway is an OpenAI-compatible LLM API; tools are told to point the base URL to `https://ai.salad.cloud/v1` with the organization-specific AI Gateway key as `Authorization: Bearer`, and the docs list the model `qwen3.6-35b-a3b` that models.dev also lists. models.dev lists NO api field for salad-cloud, so this base comes from the docs, not from models.dev. |
| verda | Verda | https://inference.verda.com/v1 | 404 | Verda's own docs (docs.verda.com/inference/quickstart): the quickstart sets `OPENAI_BASE_URL=https://inference.verda.com/v1` with a Bearer API key and calls `/v1/chat/completions` with the OpenAI SDK. |
| confidential-ai | Confidential AI | https://api.confidential.ai/v1 | 401 | Confidential AI's own docs (confidential.ai/docs/inference-api/get-started): the production base URL is `https://api.confidential.ai`, the API is OpenAI-compatible, and calls use `Authorization: Bearer $CONFIDENTIAL_API_KEY` against `https://api.confidential.ai/v1/chat/completions`. |
| therouter | TheRouter | https://api.therouter.ai/v1 | 404 | TheRouter's own docs (therouter.ai/docs/quickstart): the global endpoint for all requests is `https://api.therouter.ai/v1`, the API is OpenAI-compatible, and calls use `Authorization: Bearer $THEROUTER_API_KEY`. |
| llmtech | LLM Tech | https://api.llmtech.eu/v1 | 410 (bare base and /v1/models) | LLM Tech's own docs (llmtech.eu/models/qwen3.8-27b): the quickstart sets `base_url="https://api.llmtech.eu/v1"` with an API key against an OpenAI-compatible endpoint, and the page states the service is in production, serving live traffic since Aug 22, 2026. The 410 is the API's answer to unauthenticated path probes — the host answers, and the docs prove the endpoint, so this is verified-from-docs, not a probe-only addition. |

## Providers added, reference-only (39)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| openreason | OpenReason | https://api.openreason.app/v1 | 404 | models.dev api.json + probe |
| pareto | Pareto Inference | https://api.paretoinference.com/v1 | 404 | models.dev api.json + probe |
| toapis | ToAPIs | https://toapis.com/v1 | 404 | models.dev api.json + probe |
| uncensuredai | UncensuredAI | https://uncensuredai.com/v1 | 404 | models.dev api.json + probe |
| vispark | Vispark | https://api.lab.vispark.in/v1 | 404 | models.dev api.json + probe |
| wallaby | Wallaby | https://api.wallabytoken.com/v1 | 404 | models.dev api.json + probe |
| aipg | AI Power Grid | https://api.aipowergrid.io/v1 | 404 | models.dev api.json + probe |
| claudinio | Claudinio | https://api.claudin.io/v1 | 403 | models.dev api.json + probe (403 on curl retry — 2 of 10 attempts across two batches answered 403; the rest closed the connection without a response, like round 4's refuseless — and the audit's own prober could not reach it, so the after-run flags it base_url_unreachable + website_unreachable; kept on the refuseless precedent with the flakiness recorded here, not hidden) |
| hopper | Hopper | https://api.withhopper.com/v1 | 404 | models.dev api.json + probe |
| humain | HUMAIN | https://api.node.humainaic.com/v1 | 404 | models.dev api.json + probe; /v1/models answers 401 |
| isoquant | Isoquant | https://api.isoquant.ai/v1 | 401 | models.dev api.json + probe |
| logits | Logits | https://api.logits.com/v1 | 404 | models.dev api.json + probe |
| nova | Nova | https://api.nova.amazon.com/v1 | 500 | models.dev api.json + probe; the bare base and /v1/models both answer 500 — the host answers, but returns a server error (the docs page at nova.amazon.com/dev/documentation returned no extractable content, so no docs-verified claim is made); the base URL is kept as listed by models.dev and must be confirmed before use as a routing target |
| oathbreaker | Oathbreaker AI | https://oathbreaker.ai/v1 | 404 | models.dev api.json + probe |
| phase | Phase Router | https://router.phase.xyz/v1 | 200 | models.dev api.json + probe |
| rayline | Rayline | https://api.rayline.ai/v1 | 404 | models.dev api.json + probe |
| subconscious | Subconscious | https://api.subconscious.dev/v1 | 404 | models.dev api.json + probe |
| superbot-gg | superbot | https://api.superbot.gg/v1 | 401 | models.dev api.json + probe |
| autarcho | Autarcho | https://api.autarcho.com/v1 | 404 | models.dev api.json + probe |
| blueclaw | Blue Claw | https://openai.blueclaw.network/v1 | 404 | models.dev api.json + probe |
| cedarllm | CedarLLM | https://api.cedarllm.com/v1 | 404 | models.dev api.json + probe |
| cheapllm | cheapllm.shop | https://cheapllm.shop/v1 | 200 | models.dev api.json + probe |
| coralbricks | CoralBricks | https://inference.coralbricks.ai/v1 | 404 | models.dev api.json + probe |
| echo | Echo | https://echo.tracerml.ai/v1 | 401 | models.dev api.json + probe |
| fiftyx | 50X | https://api.ai.50x.energy/v1 | 404 | models.dev api.json + probe |
| fortytwo | Fortytwo | https://api.fortytwo.network/v1 | 404 | models.dev api.json + probe |
| frequency | Frequency | https://llm.scalefrequency.com/v2 | 404 | models.dev api.json + probe; note the /v2 path, not the usual /v1 |
| kosmik | Kosmik Compute | https://api.koscompute.com/v1 | 401 | models.dev api.json + probe |
| kuae-cloud-coding-plan | KUAE Cloud Coding Plan | https://coding-plan-endpoint.kuaecloud.net/v1 | 404 | models.dev api.json + probe; KUAE Cloud's coding-plan surface (Moore Threads' KUAE Cloud docs title the page "programming plan/package"): kept on the Kimi Code / alibaba-token-plan distinct-surface precedent — its own endpoint host, its own docs page, no catalog entry carries this base, and no parent KUAE Cloud entry exists for it to duplicate — unlike round 3's minimax-coding-plan, which shared minimax's exact base URL. Reference-only: the docs page text did not render, so no docs-verified claim is made |
| lynkr | Lynkr | http://127.0.0.1:8081/v1 | loopback | models.dev api.json; local loopback default, not probed from here (lm-studio / atomic-chat precedent) |
| mallard | Mallard AI | https://api.mallardai.co.uk/v1 | 404 | models.dev api.json + probe |
| meragpt | meraGPT | https://meragpt.com/v1 | 404 | models.dev api.json + probe |
| nodecode | Nodecode | https://api.nodecode.ai/v1 | 404 | models.dev api.json + probe |
| reflection | Reflection | https://api.reflection.ai/openai/v1 | 404 | models.dev api.json + probe; note the /openai/v1 path, no /v1 suffix on the host |
| sagg | SAGG | https://api.privatedeskai.com/v1 | 404 | models.dev api.json + probe; GET /v1/models → 200 (live model list) |
| tokenlab | TokenLab | https://api.tokenlab.sh/v1 | 200 | models.dev api.json + probe |
| tokenwatt | TokenWatt | https://api.tokenwatt.io/v1 | 404 | models.dev api.json + probe |
| unbiased | Pareto by Unbiased | https://api.unbiased.ai/v1 | 401 | models.dev api.json + probe; models.dev lists its single model as "pareto" — a distinct host from Pareto Inference (api.paretoinference.com), so a separate entry, not a duplicate |
| zeldoc | Zeldoc | https://api.zeldoc.ai/v1 | 404 | models.dev api.json + probe |

## Candidates rejected (0)

None. Every one of the last 44 candidates is a distinct, live API
surface: no duplicate of an existing catalog entry and no plan label
on an existing provider's surface was found among them. The one
plan-named candidate, `kuae-cloud-coding-plan`, was added, not
rejected — see its row above for the distinct-surface reasoning.

## Candidates pending (0)

None. No candidate in this round failed to be proven live: the only
first-pass probe failure, `claudinio` (connection closed without a
response), answered 403 on curl retry — twice across two batches.
One honest caveat, recorded in its row and in the audit table below:
the audit's own prober could not reach `claudinio`'s base or website,
so the after-run flags it base_url_unreachable + website_unreachable.
It is kept on the round-4 `refuseless` precedent (same shape, proved
live by the retry), with the disagreement stated here rather than
hidden — a future re-probe that never answers should move it to
pending.

## Possible renames — 0 applied, 3 still pending (re-checked 2026-11-18)

An ID move was applied ONLY where the provider's own docs prove
the models.dev ID is the canonical product identifier. All three
pending pairs were re-read against the providers' own docs today
and still state no canonical machine ID — so none moves, and all
three stay flagged `possible_rename` by the audit by design.

| Catalog ID | models.dev ID | Name | Reason |
|---|---|---|---|
| charm-hyper | hyper | Charm Hyper | Re-checked 2026-11-18 (hyper.charm.land): the site brands the product "Charm Hyper" / "Hyper" and confirms the OpenAI-compatible protocols /v1/chat/completions, /v1/responses, /v1/messages, but states no canonical machine ID — "hyper" is a generic word and the catalog ID disambiguates, so neither ID is proven. |
| qiniu | qiniu-ai | Qiniu | Re-checked 2026-11-18 (developer.qiniu.com/aitokenapi): Qiniu's own docs present the service as Qiniu Cloud's AI large-model inference (AI 大模型推理) without stating a canonical "qiniu-ai" identifier; the catalog name "Qiniu" is the company, and the catalog entry already carries the models.dev base URL (api.qnaigc.com/v1). |
| regolo | regolo-ai | Regolo AI | Re-checked 2026-11-18 (docs.regolo.ai): the brand is "Regolo" (the docs are branded "Regolo Docs" and the product Regolo); neither "regolo" nor "regolo-ai" is stated as a canonical ID by the provider's docs, and the catalog entry already carries the models.dev base URL (api.regolo.ai/v1). |

The fourth pair from round 4, `prime-intellect` / `prime`, stays
decided-keep for the reason recorded in round 4 (docs brand the
product "Prime Inference" and use `prime` only as the CLI binary
name) — it was not re-opened in this round.

## Audit re-run (before → after)

Re-run `codvian-router catalog audit`, 2026-11-18 (live models.dev
fetch + reachability probes; cumulative triage record embedded).
Before is the fresh baseline re-run at ~07:26 UTC (same numbers as
the round-4 after-run at 06:40 UTC except probe flake:
`base_url_unreachable` 6 → 5, `website_unreachable` 15 → 14,
reachability 853/832 → 853/834 — two hosts that timed out in the
round-4 run answered in this one; the unreachable SETS differ only
by that flake, see the note below).

| Kind | Before (~07:26 UTC) | After |
|---|---|---|
| missing_in_catalog | 81 | 37 (−44, exactly the providers added) |
| base_url_missing_in_catalog | 1 | 1 (pioneer only — unchanged) |
| renamed | 19 | 19 (unchanged) |
| possible_rename | 7 | 7 (unchanged — 0 moves applied; the 3 pending + prime-intellect decided-keep stay flagged by design) |
| base_url_differs | 2 | 2 (unchanged) |
| base_url_path_differs | 3 | 3 (unchanged) |
| base_url_missing_in_modelsdev | 9 | 10 (+1: salad-cloud — the catalog now carries its docs-stated base, models.dev lists no api for it) |
| base_url_unreachable | 5 | 6 (+1: `claudinio` — the one round-5 addition the audit's own prober could not reach; its host closes most connections, proved live twice via 403 on curl retry — see its row above) |
| verified_base_unreachable | 1 | 1 (unchanged — `clawcu` stays) |
| website_unreachable | 14 | 17 (+3: `claudinio` website, `nara`, `speka`; −1: `navy` now answers — `nara`/`speka`/`navy` are pre-existing entries, probe flake; `speka` flipped the same way in round 4) |
| missing_in_modelsdev | 327 | 327 (unchanged) |
| catalog total | 570 | 614 (+44) |
| verified-from-docs | 60 | 65 (+5) |
| reference-only | 506 | 545 (+39) |
| no base URL | 234 | 234 (unchanged) |
| reachability probed / reachable / unreachable | 853 / 834 / 19 | 940 / 917 / 23 (+87 probed: the 44 new entries' bases and websites, less the lynkr loopback skip; reachable +83, unreachable +4 — the +4 are claudinio's base + website and the nara/speka website flake) |
| loopback skipped | 6 | 7 (+1: lynkr) |
| models.dev providers / models | 324 / 9981 | 324 / 9981 (unchanged) |

The drift shrank by exactly the proven fixes: −44 candidates, +1
base_url_missing_in_modelsdev that is itself a proven fix
(salad-cloud's docs-stated base, where models.dev lists none).
Nothing else moved except the four unreachable entries above —
one is a round-5 addition whose host is genuinely flaky
(`claudinio`, documented in its row), the other three are
pre-existing entries flipping on probe flake (`nara`, `speka`
newly unreachable, `navy` newly reachable). The removed
missing_in_catalog set is exactly the 44 added IDs, verified by
diffing the before/after finding sets — no other candidate moved.

## Honest limitations

- Reference-only additions rest on models.dev + a live probe, not
  the provider's own docs. A probe returning 401/403/404 proves the
  host answers, not that the path is the right chat-completions base
  — the wire format is assumed OpenAI-compatible per models.dev and
  must be confirmed before any of these is used as a routing target.
- `nova` answers 500 on the bare base and on /v1/models — the host
  is live, but the endpoint is unproven beyond that; confirm before
  use as a routing target.
- `llmtech` answers 410 to unauthenticated probes; its verified
  status rests on its own docs, not on a successful probe.
- `lynkr`'s base URL is a loopback default (127.0.0.1:8081),
  unprobed — the lm-studio / atomic-chat precedent.
- `xiaomi` and `friendli` stay flagged missing_in_catalog by the
  audit (ID mismatch with catalog `xiaomi-mimo` / `friendliai`);
  only a human-approved ID move would clear them, like the 3
  pending possible_rename pairs. The remaining 37
  missing_in_catalog findings after this round are the same kind of
  residue: providers already in the catalog under a different ID
  (Amazon Bedrock, Azure, Google, Vertex, Fireworks AI, Together
  AI, Meta, Inference, Modelis, Neon, Ofox, QVAC, SAP AI Core,
  Wafer, D.Run (China), LLM Gateway), the pending `neosmith`,
  `friendli` / `xiaomi` ID mismatches, and plan entries that were
  deliberately never triaged as providers (Alibaba / MiniMax /
  SCNet / StepFun / Tencent / Xiaomi / Z.AI / Zhipu coding and
  token plans, Umans AI Coding Plan).
- `kuae-cloud-coding-plan` is a plan product with its own endpoint;
  it is recorded as such in its entry Notes so a future reviewer
  can re-decide it without re-doing the research.
- models.dev itself is a moving target: the re-run fetches it live,
  so its provider/model counts may differ slightly from the 07:26
  UTC baseline.
- The triage is complete for the 2026-11-18 audit's 228 candidates.
  A future audit against a newer models.dev snapshot will surface
  new candidates; that is a new triage, not a continuation of this
  one.
