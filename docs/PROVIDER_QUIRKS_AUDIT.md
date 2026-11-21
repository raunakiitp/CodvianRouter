# Provider Quirks Audit — Phase 1 slice (2026-11-21)

**Scope:** the three quirks discovered by the live OpenRouter PKCE verification, plus a repository-evidence sweep of provider-specific compatibility (wires, auth headers, discovery, retry/cooldown policy). Every claim below cites code or a recorded live trace; statuses use the tiers defined in [PROVIDER_AUTH_MATRIX.md](PROVIDER_AUTH_MATRIX.md) (`metadata-only` / `key-configured` / `mock-tested` / `real-provider-tested` / `unsupported`). No vendor credentials are used in default CI; no paid probes were made in this slice.

**Evidence base:** merged `main` after PR #20 (`e94f51f`), the live verification log in PROVIDER_AUTH_MATRIX.md (operator OpenRouter account, 2026-11-21), and direct code inspection of `internal/router`, `internal/provider`, `internal/server`, `internal/catalog`.

---

## 1. Nested model-ID addressing (`openrouter/<id>`) — FIXED

**Symptom (live):** `/v1/models` lists OpenRouter's nested IDs verbatim (`liquid/lfm-3-1.7b:free`), but sending that exact ID to `/v1/chat/completions` failed with `400 routing.only: unknown provider "liquid"`. Only `openrouter/liquid/lfm-3-1.7b:free` worked.

**Root cause (code):** `internal/server/provider_v1.go` `applyPrefixedModelRouting` (B2-29) splits EVERY model string at the first `/` and treats the prefix as a provider pin. `resolveProviderV1` also matches static catalog entries, and `liquid` is a catalog provider (Liquid AI) — so the nested ID was pinned to an unconfigured catalog provider, which `applyContract` then rejected (`hasClient` false). Aggregator vendor namespaces (`liquid/…`, `openai/…`, `mistralai/…`) collide with catalog IDs by design of the upstream's naming, not ours.

**Fix:** the prefix is only a pin when the named provider actually serves the BARE model. When it does not, and the FULL string is a model ID served verbatim by a registered provider, the request is left untouched and the chain builder matches the full ID (exactly what the explicit `openrouter/<id>` pin already produced on the wire). New registry queries `OffersModel` / `ProviderOffersModel` (`internal/provider/registry.go`) mirror the chain builder's offer rule (enabled provider; empty model list serves any model) and resolve pins by registry ID, catalog ID or display name — the same matching the engine's `ResolveProvider` does.

**Behavior preserved:**
- Explicit pins still pin: `openrouter/liquid/lfm-…` (OpenRouter serves the bare nested ID), `openai/gpt-4o` when an OpenAI provider serves `gpt-4o` (direct provider intent wins over an aggregator listing the same full ID).
- B2-29 conflict rules unchanged (routing.group/only/order/exclude conflicts still error).
- Adders without the offer surface (test fakes) keep the historical pin behavior — the escape hatch is opt-in via interface assertion.
- `auto/…` virtual prefix untouched.

**Tests:** `internal/server/prefixed_model_routing_test.go` — `TestPrefixedModelRoutingNestedIDs` (6 cases: listed nested ID, aggregator pin, registry-ID pin, direct-pin precedence, unknown prefix, bare model), `TestPrefixedModelRoutingPinConflictsStillError`, `TestPrefixedModelRoutingLegacyAdderKeepsPin`; `internal/provider/registry_offers_test.go` — `TestProviderOffersModelLocked`, `TestRegistryOffersModel` (discovery-backed offers, name/ID matching, disabled providers).

**Status:** fix is **mock-tested** + code-verified. Live re-verification (send a listed nested ID verbatim against the operator account) is **pending** the quota unblock — the routing path up to the upstream is already live-proven via the explicit-pin form, which produces the identical wire request.

## 2. 429 retry storms and per-key cooldown — ROOT CAUSE FIXED (PR #20); policy documented, unchanged

**Symptom (live, pre-fix):** repeated within-request attempts on the SAME key at ~300–650 ms intervals, all 429, before hard cooldowns engaged.

**Root cause:** the pool-join stale-snapshot bug (PROVIDER_AUTH_MATRIX.md live log): the engine only ever registered key #1, so the chain had ONE target. The 429 policy fails over immediately when a next target exists; with no next target, the bounded same-target retry path runs (`failoverRetryAllowed`, `internal/router/router.go`; `FailoverMaxRetries` default 3, `MaxAttempts` 5). The "storm" was that bounded retry against a single-key chain — not an unbounded loop and not a defect in the retry policy itself. After the PR #20 fix, live traces show the intended behavior: 429 on key #2 → immediate attempt on key #1; 402 likewise; repeated 429s → per-key hard cooldowns with `skipped_cooldown` attempts.

**429 policy reference (code map, `internal/router/router.go` noteFailure path + `internal/provider/taxonomy.go`):**
- 429 classifies as `DecisionFallback` / `ReasonRateLimited` (taxonomy.go) — failover, never a breaker count (`breakerNeutral`).
- The 429 BODY is classified uniformly across all wires (M4.4):
  - **quota-exhausted text** → target abandoned for this request, hard cooldown = honoured Retry-After or 60 s (`QuotaExhaustedCooldown`);
  - **concurrency-limit text** → failover now, 1 s cool, no backoff climb, no breaker count;
  - **default** → `consecutive429` backoff ladder (`rateLimitWait`), hard cooldown = Retry-After floored at 10 s / capped at `RetryAfterCap`, or the per-auth base when no hint (OAuth 5 s / API-key 3 s / local 2 s).
- The computed wait is only slept when the failing target was the LAST in the chain; with further targets the loop fails over immediately.
- `FailoverBeforeRetry` (B2-30, default off — matches the reference router's effective default) skips same-target transient retries when a next target exists.
- Soft-demote cooldown mode (`CooldownSoftDemote`) demotes instead of hard-cooling; Retry-After still floored at 10 s, doubling capped at 5 min.

**Decision:** no policy change justified — the observed behavior was the single-key-chain symptom, the policy is bounded, classified and live-proven post-fix. Changing the reference-parity default without a measured need would violate the preserve-working-code rule.

**Status:** root cause **real-provider-tested** (post-fix cross-key failover traces); policy **mock-tested** (router retry/cooldown suite) + code-documented.

## 3. Empty-content `finish_reason: length` at small max_tokens — FIXED

**Symptom (live):** two free-tier requests reached the model and ended `finish_reason: length` with empty content at `max_tokens=30`; the router surfaced `upstream stream ended with no content (finish_reason: length)` after failing over.

**Root cause (code):** reasoning models can spend the entire token budget on thinking tokens and emit no answer text. The reassembled response carries that output as `Reasoning` (`internal/provider/client.go` aggregation sets `Reasoning: state`; `bridgeStream` puts it on the Done event's `Resp`), but:
- non-streaming: `isEmptyCompletion` (`internal/router/router.go`) checked only `Content == "" && len(ToolCalls) == 0` — reasoning was invisible, so a reasoning-only completion was treated as "served nothing" and failed over;
- streaming: the preread gate (`internal/router/preread.go`) treated a Done with no content events as an empty stream, ignoring `Done.Resp.Reasoning`.

Failing over cannot improve this: the same small budget on another target produces the same reasoning-exhausted shape, so the request burned every target and then errored.

**Fix:** reasoning-only output is real output.
- `isEmptyCompletion` now returns false when `resp.Reasoning` is non-empty (`reasoning.State.Empty()` is nil-safe);
- preread: `doneHasReasoning(ev.Resp)` at both Done branches commits the buffered events instead of returning `EmptyStreamError` (commit forwards the Done event with `Resp` intact, so reasoning continuity capture and the drain are unchanged).
- An empty reasoning state (no blocks) is still empty — the failover heuristic for genuinely broken upstreams is preserved.

**Tests:** `internal/router/cancel_test.go` — `reasoning-only-is-not-empty` (no failover, reasoning + finish reason preserved, 1 attempt), `empty-reasoning-state-still-fails-over`, `TestStreamingReasoningOnlyIsNotEmpty` (finish-only chunk + Done-with-reasoning commits; second target never called). Existing empty-stream failover tests (incl. `TestStreamingPrereadFailoverGate`) still pass — genuinely empty streams still fail over.

**Status:** **mock-tested** + code-verified. Live re-verification pending quota unblock. Note: the honest user-facing outcome for a reasoning-exhausted completion is now a 200 with `finish_reason: length`, empty content and the reasoning preserved for the next turn — clients that need answer text should raise `max_tokens`; the router no longer masks this as a provider failure.

---

## 4. Repository-evidence sweep: wire / auth / discovery coverage

Method: programmatic dump of `catalog.All()` (610 entries) by primary wire and verification tier, then code inspection of the client factory, auth resolution and per-wire clients. Counts are inventory, not support claims.

**Primary wire distribution (610 entries):** openai 473 (58 verified-from-docs), unknown 129 (all reference-metadata), anthropic 3, gemini 2, bedrock-converse 1, cohere 1, ollama 1. **60 verified-from-docs API-key providers** (full list reproducible from the catalog; includes openai, anthropic, gemini, openrouter, azure-openai, bedrock, vertex, cohere, groq, together, fireworks, deepseek, mistral, xai, perplexity, cerebras, nvidia, huggingface, cloudflare, ollama-cloud, …).

**Every declared wire has a real client (mock-tested):**

| Wire | Client | Test evidence (function counts) |
|---|---|---|
| openai (default) | `openAIClient` (`client.go`) | `client_test.go` + wire/routing/pool suites |
| anthropic | anthropic client (`client.go` wire switch) | `anthropic_client_test.go` (13), cache-tier suites |
| gemini | generateContent client (`internal/wire`) | `gemini_client_test.go` (10) |
| cohere (Chat API v2) | `cohereClient` (`cohere_client.go`) | `cohere_client_test.go` (6) |
| bedrock-converse | `bedrockClient` (`bedrock_client.go`, Amazon event-stream) | `bedrock_client_test.go` (5) + abort + incremental suites |
| vertex (service-account) | `vertex_client.go` (token mint + native wire) | `vertex_client_test.go` (14) |
| ollama native `/api/chat` | **no dedicated client** — see gap G2 below | — |

**Auth is catalog-driven, not hardcoded:** `catalog.ResolveAuth` resolves the exact (provider, endpoint, variant) pair into headers + wire; `ExtraHeaders` are merged (e.g. the REQUIRED `anthropic-version`), header names are per-surface (`x-api-key`, `x-goog-api-key`, Azure `api-key`, Fireworks `x-fireworks-api-key` variant). The client sends exactly the resolved headers (`newRequest`), never a guessed Bearer default when a resolution exists; an empty header map means the surface takes no credential (local runtimes). Exchange surfaces are refused at add time EXCEPT the four implemented exchanges: Bedrock SigV4, Vertex service-account JSON, Azure Entra client credentials, OpenRouter PKCE login.

**URL joining:** `joinAPIPath` appends `/v1` when the base does not end in `/v1` — so catalog bases like Ollama's `http://localhost:11434` correctly reach `/v1/chat/completions`, and `/v1`-suffixed bases are not doubled.

**Provider-specific handling already present (preserve, do not rewrite):**
- OpenRouter prompt-cache marking quirks: `g13_openrouter_cache_test.go` (7 tests — Claude/tools/cacheable families, non-OpenRouter providers unmarked, client markers preserved).
- Rate-limit header learning: `ratelimit_headers_test.go`; Retry-After body classification: `retryafter_body_test.go` (feeds the uniform 429 body classification above).
- Anthropic cache-write tiers, reasoning handoff across wires, per-key pools, model discovery/refresh with `models_fetched_at`.

## 5. Additional findings and prioritized backlog

| # | Finding | Evidence | Status / priority |
|---|---|---|---|
| G1 | **Streaming usage accounting was a local estimate.** The OpenAI client never requested `stream_options.include_usage`; the engine's drain dropped the upstream's reported usage from the Done response; the ledger always recorded the labelled local estimate for streams; the server neither parsed the client's `stream_options` nor emitted a usage chunk; streaming budget overages were invisible. | code: `client.go`, `router.go` drain/runStream, `server.go` handleChatStream, `requestbudget.go` | **FIXED (2026-11-21, G1 slice)** — see §7 below. mock-tested + integration-tested (real engine + real clients vs deterministic SSE mocks). Live re-verification pending quota unblock. |
| G2 | **Ollama native `/api/chat` wire has no client.** The supported path is the OpenAI-compatible `/v1` surface (catalog default chat endpoint + `joinAPIPath`); the catalog documents both. | `catalog/verified.go` ollama-local entry; client factory wire switch | **unsupported** (native wire), honestly documented; OpenAI path is mock-tested. Low priority — the compatibility surface covers routing. |
| G3 | **Cohere default endpoint is the OpenAI compatibility shim**, while the native v2 client also exists. Both paths are implemented; the catalog Notes say native chat is NOT OpenAI wire. Operators choosing the `native` endpoint get the v2 client. | `catalog/verified.go` cohere entry; `cohere_client.go` | mock-tested both paths; no action. Documented so the two surfaces are never conflated. |
| G4 | **Cross-account failover unverified.** Live pool evidence used two keys of the SAME OpenRouter account. The pool mechanics are account-agnostic, but the claim stays unverified until a second account is available. | live log, PROVIDER_AUTH_MATRIX.md | **P1 verification backlog** — needs a second operator account; no code change indicated. |
| G5 | **Successful OpenRouter completion QUOTA-BLOCKED / PENDING.** Free tier persistent 429; account never purchased credits (honest 402 on the single tiny paid probe; $0 spent). | live log | Verification pending operator unblock (credits or free-quota reset). No paid probes without explicit instruction. |
| G6 | **Azure Entra ID real-resource verification open.** Implementation + mock suite complete; no Azure resource available. | `entra.go` / `entra_test.go` (9 tests) | **mock-tested** until a real Azure resource is exercised. |
| G7 | **129 catalog entries have unknown primary wire** (all reference-metadata-only). They are inventory, not routable claims; custom-prefix and base-URL detection (`DetectWireFormat`, `catalogWireForBaseURL`) cover operator-added custom providers. | sweep dump; `registry.go` detection path | metadata-only; no action beyond the existing honest detection. |

## 6. What this slice changed (code)

- `internal/provider/registry.go` — `OffersModel`, `ProviderOffersModel` (+ shared offer rule helper).
- `internal/server/provider_v1.go` — nested-model-ID escape hatch in `applyPrefixedModelRouting`.
- `internal/router/router.go` — `isEmptyCompletion` counts reasoning as output.
- `internal/router/preread.go` — `doneHasReasoning` + both Done branches commit reasoning-only streams.
- Tests: `internal/server/prefixed_model_routing_test.go` (new), `internal/provider/registry_offers_test.go` (new), `internal/router/cancel_test.go` (3 new cases/tests).
- Docs: this audit + PROVIDER_AUTH_MATRIX.md updates (statuses aligned to the operator's evidence distinctions).

Working adapters, the 429 policy, the chain builder, and all Phase 0/1 behavior were preserved; no Phase 2 or dashboard work was started.

## 7. G1 — Streaming usage accounting (implemented 2026-11-21)

**What changed (all wires benefit):**

1. **Request flag** (`internal/provider/client.go`): every OpenAI-wire stream now sends `stream_options: {include_usage: true}` (OpenAI itself omits streaming usage without it; compat providers honor or ignore the field). Non-streaming requests never carry the block. The client's own `stream_options` is decoded leniently into `ChatRequest.StreamIncludeUsage` (unknown keys ignored, malformed `include_usage` is a 400).
2. **Aggregation already existed, now carried**: every wire client (openai/anthropic/gemini/cohere/bedrock/vertex) already aggregates its wire's streaming usage events into the reassembled response — OpenAI's final usage chunk (last report wins, never summed; cached/reasoning splits + OpenRouter/xAI reported cost decoded by the same `usageWire` path), Anthropic `message_delta`, Gemini `usageMetadata`, Cohere `message-end`, Bedrock metadata. The engine's drain (`chatStreamDrainChunks`, both Done branches) now copies `Usage` from `Done.Resp` instead of dropping it.
3. **Ledger** (`internal/router/router.go` runStream): a successful stream with a reported split is reconciled exactly like a non-streaming report (`reconcileUsage`: provider numbers verbatim, local estimate alongside as `LocalEstimatePrompt`, divergence counted/calibrated, reported `Cost` preserved). No report → the labelled `LocalEstimate` fallback (unchanged semantics). One row per attempt; failed/cancelled/incomplete streams keep their existing honest semantics (no invented completion split).
4. **Client-facing usage chunk** (`internal/server/server.go`): when the client requested `stream_options.include_usage` AND the upstream reported usage, the stream emits one final usage chunk (empty `choices` array, OpenAI wire shape) after the finish chunk and before `[DONE]`. No report → no chunk (a local estimate is never presented as provider-reported). Exactly one usage chunk — no duplicate accounting on the wire.
5. **Budget enforcement** (`internal/server/requestbudget.go`): the overage check is refactored into a shared priced-actual core; streams now get the same post-hoc overage recording as non-streaming responses (`checkRequestBudgetOverageStream` — no header, SSE headers are already committed; the spend rides the telemetry trailer, which now prices the reported usage automatically). Pre-send and mid-chain estimate gates are unchanged.

**Preserved:** non-streaming usage accounting, the local-estimate fallback for providers that omit usage, cancellation/disconnect semantics, the degrade wrapper's forwarding (resp passes through intact), cache-hit $0 accounting.

**Tests (all pass):** `internal/provider/stream_usage_test.go` (include_usage on the wire / absent non-streaming, last-report-wins aggregation with splits + cost, no-report stays zero, `stream_options` decode table), `internal/router/stream_usage_test.go` (reported split recorded verbatim + cost + estimate-alongside + reconciliation stats, no-report labelled fallback, drain carries usage), `internal/server/stream_usage_test.go` (usage chunk emitted exactly once with empty choices, omitted without the flag, omitted without a report, streaming budget overage recorded on the feed at the priced actual). Existing streaming/budget/async-guardrail/SSE/preread suites re-run green.

**Honest limits:** live OpenRouter re-verification pending the account-quota unblock (no paid probes). The Responses API's own streaming surface and combo multi-step orchestration keep their existing accounting (chat-completions surface was G1's scope); noted as follow-up, not silently claimed. Providers that neither honor `include_usage` nor send usage by default keep the labelled local-estimate fallback — never an invented split.