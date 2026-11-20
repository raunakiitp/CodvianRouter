# Catalog triage round 4 — 2026-11-18 (CHECKLIST items 281–282)

Follow-up to round 3 (`docs/catalog-triage-2026-11-18-round3.md`, which
triaged the next 46 of 136 candidates). This slice triaged the
**next 46 of the 90 remaining** candidates by models.dev model count
(dinference … nokeep, 6 down to 3 models), from the same 2026-11-18
audit, and resolved the 5 possible renames where the providers' own
docs prove them.

## Headline

| | Count |
|---|---|
| Candidates triaged (round 4) | 46 of 90 |
| Providers added | 39 (2 verified-from-docs, 37 reference-only) |
| Candidates rejected (duplicate / plan, not a provider) | 5 |
| Candidates pending | 2 |
| Candidates untouched after round 4 | 44 |
| Possible renames applied (ID moves, docs-proven) | 1 of 5 |
| Possible renames decided-keep (docs prove the catalog ID) | 1 of 5 |
| Possible renames still pending (ID-level, needs a human) | 3 of 5 |
| Extra base-URL fills (outside the audit's 44) | 2 (`xiaomi-mimo`, `friendliai` — see below) |
| Rename-consequence base-URL fills | 1 (`novita-ai` — see below) |

Cumulative (rounds 1 + 2 + 3 + 4): **184 of 228 triaged, 147 added
(12 verified from docs, 135 reference-only), 30 rejected, 7 pending,
44 untouched**.

Catalog totals after this slice: **570 entries** (was 531) — 60
verified-from-docs (was 58), 506 reference-only (was 469), 4
unverified; no base URL 234 (was 237 — the 2 duplicate-consequence
fills and the 1 rename-consequence fill; the 39 new entries each
carry a base URL, so they add to the total without adding to the
no-base count).

Verification rule used throughout (same as rounds 1–3): a provider
was added only when models.dev lists it **and** a live reachability
probe of its base URL returned an HTTP response (any status —
401/403/404 on a bare base URL is the normal answer of a live API).
`verified-from-docs` was granted only where the provider's own docs
were read and state the base URL + auth. Base URLs were never
bulk-filled from models.dev: each addition below cites its own probe
code.

## Providers added, verified from docs (2)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| kimi-code-plan-cn | Kimi For Coding (kimi.com) | https://api.kimi.com/coding/v1 | 404 | Kimi's own docs (www.kimi.com/code/docs/en/kimi-code/models.html): the Kimi Code API supports OpenAI (Responses / Chat Completions) and Anthropic protocols; the OpenAI base URL for China is https://api.kimi.com/coding/v1, and an API key is created in the Kimi Code Console and filled in with the base URL. |
| kimi-code-plan-global | Kimi For Coding (kimi.ai) | https://api.kimi.ai/coding/v1 | 404 | Same page on the overseas docs (www.kimi.ai/code/docs/en/kimi-code/models.html): the OpenAI base URL overseas is https://api.kimi.ai/coding/v1, same API-key auth. |

Why these two are added, not rejected as plans: the round-3 rule
rejected `minimax-coding-plan` because it shared minimax's **exact**
base URL (`https://api.minimax.io/anthropic/v1`) — a plan label on
the same surface. The Kimi For Coding surfaces have their **own**
API hosts (`api.kimi.com/coding/v1`, `api.kimi.ai/coding/v1`), their
own docs page listing the base URLs and the models, and no catalog
entry carries either base — the same distinct-surface test that
kept `alibaba-token-plan` (its own regional base) in the catalog.
The name "Kimi For Coding" is the product the docs describe, not a
billing label on the `kimi` entry (which has no base URL at all).

## Providers added, reference-only (37)

| ID | Name | Base URL | Probe | Evidence |
|---|---|---|---|---|
| dinference | DInference | https://api.dinference.com/v1 | 404 | models.dev api.json + probe |
| inceptron | Inceptron | https://api.inceptron.io/v1 | 404 | models.dev api.json + probe |
| inferall | InferAll | https://api.inferall.ai/v1 | 404 | models.dev api.json + probe |
| infersia | Infersia | https://api.infersia.com/v1 | 404 | models.dev api.json + probe |
| moonshotai | Moonshot AI | https://api.moonshot.ai/v1 | 404 | models.dev api.json + probe; Moonshot's own docs confirm the chat-completions API with Bearer MOONSHOT_API_KEY auth but state no base URL in the fetched text. Global surface; `moonshotai-cn` is the China surface, the stepfun/stepfun-ai regional precedent. The legacy catalog entries `moonshot` and `kimi` (no base URL, different IDs) are left untouched. |
| moonshotai-cn | Moonshot AI (China) | https://api.moonshot.cn/v1 | 404 | models.dev api.json + probe; China surface; `moonshotai` is the global surface |
| pendra | Pendra | https://api.pendra.ai/api/v1 | 404 | models.dev api.json + probe |
| refuseless | Refuseless | https://api.refuseless.com/v1 | 403 | models.dev api.json + probe (curl; the first probe client saw the connection close without a response, the retry answered 403 twice) |
| ai-router | AI-ROUTER | https://api.ai-router.dev/v1 | 404 | models.dev api.json + probe |
| atomic-chat | Atomic Chat | http://127.0.0.1:1337/v1 | loopback | models.dev api.json; local loopback default, not probed from here (lm-studio precedent from round 1) |
| bailing | Bailing | https://api.ant-ling.com/v1 | 404 | models.dev api.json + probe |
| cheapaimodel | cheapaimodel | https://api.cheapaimodel.ai/v1 | 404 | models.dev api.json + probe |
| cloudferro-sherlock | CloudFerro Sherlock | https://api-sherlock.cloudferro.com/openai/v1/ | 200 | models.dev api.json + probe |
| ezkielyna | Ezkielyna | https://api.ezkielyna.store/v1 | 200 | models.dev api.json + probe |
| munito | Munito AI | https://api.munito.ai/inference/v1 | 404 | models.dev api.json + probe; `munito-eu` is the EU surface |
| sokkan | SOKKAN Router | https://router.sokkan.ch/v1 | 404 | models.dev api.json + probe |
| tokenify | Tokenify | https://api.tokenify.dev/v1 | 404 | models.dev api.json + probe |
| zro | Zro | https://zro.moonmath.ai/v1 | 404 | models.dev api.json + probe |
| abliteration-ai | abliteration.ai | https://api.abliteration.ai/v1 | 404 | models.dev api.json + probe |
| ebcloud | EBCloud | https://maas-api.ebcloud.com/v1 | 404 | models.dev api.json + probe |
| entrim | Entrim | https://api.entrim.ai/v1 | 404 | models.dev api.json + probe |
| inferris | Inferris | https://api.inferris.ai/v1 | 404 | models.dev api.json + probe |
| lucidquery | LucidQuery | https://api.lucidquery.com/v1 | 404 | models.dev api.json + probe |
| modular | Modular | https://api.modular.com/v1 | 401 | models.dev api.json + probe; Modular's own docs confirm shared (serverless) endpoints but state no base URL in the fetched text |
| munito-eu | Munito AI (EU) | https://api.eu.munito.ai/inference/v1 | 404 | models.dev api.json + probe; EU surface; `munito` is the global surface |
| proxystacks | ProxyStacks Relay | https://proxystacks.com/v1 | 404 | models.dev api.json + probe |
| sakana | Sakana AI | https://api.sakana.ai/v1 | 404 | models.dev api.json + probe |
| tencent-tokenhub | Tencent TokenHub | https://tokenhub.tencentmaas.com/v1 | 404 | models.dev api.json + probe; Tencent's TokenHub MaaS surface with its own API host — added, not rejected as a plan, on the alibaba-token-plan precedent (a token-plan entry with its own regional base), unlike round 3's minimax-coding-plan which shared minimax's exact base URL |
| tenstorrent | Tenstorrent | https://console.tenstorrent.com/v1 | 404 | models.dev api.json + probe |
| unrestricted | Unrestricted | https://api.unrestricted.sh/v1 | 404 | models.dev api.json + probe |
| anonrouter | AnonRouter | https://api.anonrouter.ai/v1 | 404 | models.dev api.json + probe |
| apiko | APIKO | https://api.apiko.app/v1 | 301 | models.dev api.json + probe (redirects to http://api.apiko.app/v1/ — a live host answering, so the models.dev base is kept as listed) |
| jad-ai | Jad AI | https://jadai.freesrv.com/v1 | 404 | models.dev api.json + probe |
| joingonka | JoinGonka | https://gate.joingonka.ai/v1 | 404 | models.dev api.json + probe |
| klokintegration | klokintegration.se | https://api-gw.klok.ipaas.se/proxy/kloker-key/v1 | 401 | models.dev api.json + probe |
| lithosai | LithosAI | https://api.lithosai.cloud/v1 | 404 | models.dev api.json + probe |
| nokeep | nokeep | https://nokeep.ai/v1 | 404 | models.dev api.json + probe |

## Candidates rejected (5) — duplicates or billing plans, not new providers

| ID | Reason |
|---|---|
| xiaomi | Duplicate of catalog `xiaomi-mimo` (Xiaomi MiMo — models.dev's `xiaomi` lists exactly the MiMo models, same xiaomimimo.com surface). The existing entry had no base URL, so it gains one in this round (see fill below) — but the `xiaomi` candidate itself is rejected, and the audit keeps flagging it as missing_in_catalog because the catalog ID is `xiaomi-mimo`, not `xiaomi` (ID mismatch, like the possible_rename pairs and round 3's `inference`). |
| friendli | Duplicate of catalog `friendliai` (FriendliAI — the catalog entry already carries the alias `friendli`, same friendli.ai site). The existing entry had no base URL, so it gains one in this round (see fill below) — but the `friendli` candidate itself is rejected, and the audit keeps flagging it as missing_in_catalog (ID mismatch). |
| stepfun-step-plan | Step Plan for StepFun (China), not a provider; catalog has stepfun / stepfun-ai. The models are the same StepFun models served through a plan endpoint. |
| stepfun-ai-step-plan | Step Plan for StepFun (Global), not a provider; catalog has stepfun / stepfun-ai. Same reason as above. |
| zhipuai-coding-plan | Coding plan for Zhipu AI, not a provider; catalog has zhipuai (open.bigmodel.cn/api/paas/v4). Same plan-rejection precedent as round 3's minimax-coding-plan. |

## Candidates pending (2)

| ID | Reason |
|---|---|
| neosmith | The API host closes the connection without any HTTP response: router.neosmith.ai/v1 and /v1/models return nothing on every attempt (TLS handshake completes, then the server drops the connection), while the homepage neosmith.ai answers 200 — the product exists but its API host cannot be proven live from here. Not added, not rejected: re-probe later. |
| drun | Same shape: chat.d.run/v1 and /v1/models close the connection without any HTTP response on every attempt, while www.d.run answers 200. Not added, not rejected: re-probe later. |

## Extra base-URL fills (2) — outside the audit's 44

| ID | Base URL added | Probe | Note |
|---|---|---|---|
| xiaomi-mimo | https://api.xiaomimimo.com/v1 | 404 | models.dev lists the same provider as `xiaomi`; the existing catalog entry had no base URL. This is NOT one of the audit's 44 missing base URLs — the audit never paired catalog `xiaomi-mimo` with models.dev `xiaomi` (ID mismatch) — so the round-1 JSON ledger (43 filled + 1 skipped = 44) is left untouched and this fill is recorded here instead, exactly like round 3's `inference-net` fill. |
| friendliai | https://api.friendli.ai/serverless/v1 | 400 | models.dev lists the same provider as `friendli`; the existing catalog entry had no base URL. Same ledger treatment as above. Friendli's own docs confirm an OpenAI-compatible serverless API but state no base URL in the fetched text. |

## Possible renames resolved (5) — 1 applied, 1 decided-keep, 3 still pending

An ID move was applied ONLY where the provider's own docs prove
the models.dev ID is the canonical product identifier. Every move
keeps the old ID as the entry's Alias, so `provider add-catalog
<old-id>` and stored configs that reference it keep resolving (via
the ByAlias fallback); the display name never changes in a move.
The audit's `possible_rename` count drops by exactly the 1 applied
move.

### Applied (1)

| Catalog ID → new ID | Name | Base fill | Confirming doc |
|---|---|---|---|
| novita → novita-ai | Novita AI | https://api.novita.ai/openai (probe 404; note the /openai path, no /v1 suffix) | Novita's own docs (novita.ai/docs/guides/introduction) publish `novita-ai` as the identifier for its API surface — the official skill is `novita-ai` (`npx skills add novitalabs/novita-skills --skill novita-ai`, "The `novita-ai` skill covers model APIs…") — the brand is Novita AI and the API host is api.novita.ai, so the models.dev ID is the canonical product identifier. Old alias "novita" is kept as the alias (it already was the alias). |

### Decided-keep (1) — docs prove the catalog ID, no human needed

| Catalog ID (kept) | models.dev ID | Name | Reason |
|---|---|---|---|
| prime-intellect | prime | Prime Inference | The provider's docs brand the product "Prime Inference" (docs.primeintellect.ai/inference/overview) — the catalog name already is Prime Inference — and use `prime` only as the CLI binary name (`prime inference models`, `PRIME_API_KEY`), not as a provider identifier. "prime" alone is generic, and the catalog ID disambiguates the company; neither the product name nor the CLI name proves the bare word is the canonical provider ID. This stays flagged `possible_rename` by the audit by design, with the reason recorded here. |

### Still pending (3) — ID-level, needs a human, do not guess

| Catalog ID | models.dev ID | Name | Reason |
|---|---|---|---|
| charm-hyper | hyper | Charm Hyper | The product is Hyper by Charm (hyper.charm.land — the docs brand it "Charm Hyper" / "Hyper" and confirm the OpenAI-compatible protocols /v1/chat/completions, /v1/responses, /v1/messages), but "hyper" is a generic word and the provider's docs state no canonical machine ID — the catalog ID disambiguates, and neither ID is proven. |
| qiniu | qiniu-ai | Qiniu | Qiniu's own docs present the service as Qiniu Cloud's AI large-model inference (developer.qiniu.com/aitokenapi) without stating a canonical "qiniu-ai" identifier; the catalog name "Qiniu" is the company, and the catalog entry already carries the models.dev base URL (api.qnaigc.com/v1). |
| regolo | regolo-ai | Regolo AI | The brand is "Regolo" (docs.regolo.ai brands the docs "Regolo Docs" and the product Regolo); neither "regolo" nor "regolo-ai" is stated as a canonical ID by the provider's docs, and the catalog entry already carries the models.dev base URL (api.regolo.ai/v1). |

## Candidates untouched (44)

The remaining models.dev providers absent from the catalog, not
triaged in this slice (next slices work down the same list, largest
first). Model counts are from the 2026-11-18 audit.

openreason (3), pareto (3), toapis (3), uncensuredai (3), vispark (3), wallaby (3), aipg (2), claudinio (2), hopper (2), humain (2), isoquant (2), logits (2), nova (2), oathbreaker (2), phase (2), rayline (2), subconscious (2), superbot-gg (2), autarcho (1), blueclaw (1), cedarllm (1), cheapllm (1), confidential-ai (1), coralbricks (1), echo (1), fiftyx (1), fortytwo (1), frequency (1), kosmik (1), kuae-cloud-coding-plan (1), llmtech (1), lynkr (1), mallard (1), meragpt (1), nodecode (1), reflection (1), sagg (1), salad-cloud (1), therouter (1), tokenlab (1), tokenwatt (1), unbiased (1), verda (1), zeldoc (1)

## Audit re-run (before → after)

Re-run `codvian-router catalog audit`, 2026-11-18 (live models.dev
fetch + reachability probes; cumulative triage record embedded).
Before is the fresh baseline re-run at 06:30 UTC (same numbers as
the round-3 after-run at 06:05 UTC except probe flake:
`base_url_unreachable` 4 → 6, `website_unreachable` 14 → 15,
reachability 756/774 → 753/774).

| Kind | Before (06:30 UTC) | After (06:40 UTC) |
|---|---|---|
| missing_in_catalog | 120 | 81 (−39, exactly the providers added) |
| base_url_missing_in_catalog | 1 | 1 (pioneer only — unchanged; the 2 duplicate-consequence fills + the novita-ai rename fill kept it flat) |
| renamed | 19 | 19 (unchanged) |
| possible_rename | 8 | 7 (−1, exactly the applied ID move; the decided-keep + 3 pending stay flagged by design) |
| base_url_differs | 2 | 2 (unchanged) |
| base_url_path_differs | 3 | 3 (unchanged) |
| base_url_missing_in_modelsdev | 9 | 9 (unchanged) |
| base_url_unreachable | 6 | 6 (unchanged — the preliminary after-run at 06:37 UTC read 5 because `speka` answered once, then timed out again in the final run; the final unreachable set is identical to the before-run set) |
| verified_base_unreachable | 1 | 1 (unchanged — `clawcu` stays) |
| website_unreachable | 15 | 15 (unchanged — identical set to the before-run) |
| missing_in_modelsdev | 327 | 327 (unchanged — `novita` was paired by name as a possible_rename, so it was never counted here; after the move `novita-ai` matches by ID) |
| catalog total | 531 | 570 (+39) |
| verified-from-docs | 58 | 60 (+2) |
| reference-only | 469 | 506 (+37) |
| no base URL | 237 | 234 (−3: the xiaomi-mimo, friendliai and novita-ai fills) |
| reachability probed / reachable / unreachable | 774 / 753 / 21 | 853 / 832 / 21 (+79 probed: the 39 new entries' bases and websites plus the 3 fills, less the atomic-chat loopback skip; reachable +79, unreachable unchanged) |
| loopback skipped | 5 | 6 (+1: atomic-chat) |
| models.dev providers / models | 324 / 9978 | 324 / 9981 (+3 models — models.dev moved slightly between the runs) |

The drift shrank by exactly the proven fixes: −39 candidates, −1
possible rename, −3 missing base URLs. Nothing else moved at all
in the final comparison — the unreachable set is identical to the
before-run set, so zero round-4 additions are unreachable and zero
pre-existing entries changed state; the only other move is +3 models
on models.dev's side. (A preliminary after-run at 06:37 UTC read
base_url_unreachable 5 / reachable 833 because `speka` answered
once — probe flake, gone in the final run.)

## Honest limitations

- Only the next 46 of 90 candidates were triaged; 44 remain.
- Reference-only additions rest on models.dev + a live probe, not
  the provider's own docs. A probe returning 401/403/404 proves the
  host answers, not that the path is the right chat-completions base
  — the wire format is assumed OpenAI-compatible per models.dev and
  must be confirmed before any of these is used as a routing target.
- `xiaomi` and `friendli` stay flagged missing_in_catalog by the
  audit (ID mismatch with catalog `xiaomi-mimo` / `friendliai`);
  only a human-approved ID move would clear them, like the 3
  pending possible_rename pairs.
- `atomic-chat`'s base URL is a loopback default (127.0.0.1:1337),
  unprobed — the lm-studio precedent. `apiko`'s base answers 301
  to an http:// URL; the models.dev https base is kept as listed.
- `neosmith` and `drun` could not be proven live from this
  environment at all (their API hosts close the connection without
  any HTTP response, though their homepages answer 200). Both stay
  pending rather than guessed.
- models.dev itself is a moving target: the re-run fetches it live,
  so its provider/model counts may differ slightly from the 06:30 UTC
  audit.
