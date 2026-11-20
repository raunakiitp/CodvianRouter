# Slice 175–176 — Multi-key pooling + preferred/reserve accounts

**Date:** 2026-11-18. **Worktree:** `~/workspace/codvian-router/`. **Status:** Built + Verified (see gate below).

## What it is

A provider may hold several API keys (a pool). The router tries the preferred key first, fails over to the next key in the pool on 401/429/quota (402), cools a dead key per-key (never the whole provider), and holds reserve keys back until every preferred key is exhausted. Per-key spend is tracked honestly in the ledger — each key's usage attributed to that key, never pooled into one number. The dashboard Providers view shows keys per provider with live status, honestly from real data.

## Backend

- `internal/provider/keys.go` (new): `ProviderKey`, roles `preferred`/`reserve`, `EffectiveKeys()` (synthesizes one preferred key from the legacy `APIKeyEncrypted`, stable ID `key_` + first 12 hex of SHA-256(ciphertext)), `PrimaryKey`, `KeyByID`, `PreferredKeys`/`ReserveKeys`, `legacyKeyID`, `EffectiveRole`/`IsReserve`.
- `internal/provider/registry.go`: `Keys []ProviderKey` on `Provider`/`ProviderConfig`; `AddKey` (live-tests the key, refuses 401 keys and duplicate credentials by fingerprint — including the legacy key, whose stored fingerprint is empty, via decrypt-and-compare), `RemoveKey` (refuses the last key), `SetKeyRole`, `KeyPool`; `ClientForKey` per key with per-key auth-header cache (`authFingerprint` covers the key ciphertext); `ClientFor` serves the primary key.
- `internal/router/router.go`: `Attempt.KeyID`/`Reserve`; `ClientKey`/`SplitClientKey` composite identity; `ProviderKeyLedger` interface preferred over `KeyedLedger`; `isTerminalErrFor`/`hasSiblingKeyAttempt` — a 401 is terminal only while no sibling key remains (applied at all four loop sites: hedge-not-launched, hedge-launched, sequential, streaming pre-commit); `coolDeadKey` — hard mode 5 min, soft mode the doubling ladder, `KeyID != ""` only (legacy single-key 401 keeps today's behaviour: chain stops, nothing cools); reserve partition in `applyTailPartitions` (final ordering step; reserve attempts are never hedge targets); trace funnels stamp `KeyID`.
- `internal/router/cooldown.go`: `CoolingTarget.KeyID`; `CooldownTargets()` splits composite keys; cooldown helpers take the composite key (a success on one key never clears another key's cooldown).
- `internal/routetrace/routetrace.go`: `AttemptTrace.KeyID`.
- `internal/ledger/ledger.go` + `async.go`: `Usage.ProviderKeyID`, `provider_key_id` column + migration (default `''` — pre-pooling rows stay unattributed, never retroactively assigned), both INSERT paths, `ProviderKeySpendTotals` grouped by (provider, key) with the same spend definition as `SpendFor`.
- `internal/config/config.go`: `Clone()` deep-copies `p.Keys`.
- `cmd/codvian-router/main.go`: one engine client per (provider, key) (`ClientForKey`; no-auth providers keep one legacy client under the bare provider ID); `chainFor`/`contextFallbackChain`/`RefusalChainFor`/group chains/`rankModels`/`ResolveProvider` expand each provider into per-key attempts; provider-added hook registers all keys' clients, provider-removed drops all; new `WithProviderKeyChangedHook` re-syncs one provider's clients from its current pool; `ledgerAdapter.RecordAttemptWithProviderKey`; startup line counts provider-key clients honestly.
- `internal/server/provider_keys.go` (new): `POST /api/providers/{id}/keys` (201), `DELETE /api/providers/{id}/keys/{keyId}` (200), `POST /api/providers/{id}/keys/{keyId}/role` (200) — all Tier 2 ALWAYS_PROTECTED (guard table + `guard_test.go` counts 9/12/48); responses carry id/role/fingerprint only; hook failure never fails the request (`routing_live=false` + note). `GET /api/providers` enriches each key with live status (`active`/`cooling`/`reserve`/`cooling-reserve`, cooling-until + mode) and honest per-key spend (`has_spend` false before any row); `''` cooling/spend rows land on the provider's legacy key (the one with no fingerprint) and on no other key. `GET /dashboard/cooldown` targets carry `key_id`.

## Dashboard

`web/index.html`: per-provider key-pool block under each provider card — fingerprint (or key ID), role pill, status pill (Active / Cooling until HH:MM:SS / Reserve — held back), honest spend line (requests · tokens · cost, `—` when none recorded); per-card add-key form (password + role select → `POST /api/providers/{id}/keys`, gateway key from the Add-provider form, cleared after the request either way); per-key role toggle + Remove key button; Keys section lists pool size per provider. Zero-dependency vanilla HTML/CSS/JS, dark pure black / light pure white, borderless 26–28px cards, dark-lime palette, no Playground (`grep -c Playground web/index.html` = 0; extracted inline script passes `node --check`).

## Tests (new)

- `internal/router/keypool_test.go` (10 tests): preferred 401/429/quota failover to the second key, per-key cooldown isolation (dead key skipped next request, provider not skipped, trace `skipped_cooldown`), reserve-only-when-exhausted (incl. reserve listed first still sorts last), per-key ledger attribution via a fake `ProviderKeyLedger`, breaker stays closed on 401, single-key 401 still stops the chain (legacy behaviour preserved), streaming pre-commit 401 failover, trace carries `key_id`.
- `internal/ledger/keypool_test.go` (3 tests): per-key spend attribution (incl. the unattributed `''` group and the no-double-counting sum), empty-ledger honest empty, pre-pooling DB migration (old table gains the column with default `''`, old row stays unattributed, new rows record their key).
- `internal/server/provider_keys_test.go` (11 tests): add 201 + routing_live + hook called + feed shows both keys, bad key 401, duplicate 409 (incl. the legacy credential), auth required 401, role validation + empty-role default, delete 200 + hook, last-key 400, unknown key/provider 404, role flip both ways + unknown role/key, feed spend + cooling enrichment, legacy-key `''` fallback (fingerprint-less key inherits `''` rows; a provider with no legacy key leaves them unattributed).

## Gate (2026-11-18, Go 1.27.4)

- `go build ./...` clean; `go vet` clean on touched packages; `gofmt -l` clean on touched files.
- Full suite `go test -count=1 ./...`: exit 0, 36/36 packages ok (log `$HOME/.tmp/fulltest-175-176.log`); verbose recount: 1,713 PASS events (1,371 top-level + 342 subtests), 0 fail (log `$HOME/.tmp/fulltest-175-176-verbose.log`).
- `go test -count=1 -race` on touched packages (router, provider, ledger, server, routetrace, config, cmd/codvian-router): clean on router/provider/ledger/routetrace/config/cmd and on this slice's server tests; the full server package under `-race` fails once on `TestAsyncGuardrailCutsStream` (another slice's async-guardrail test — timing-flaky under full-suite load, passes standalone under `-race` immediately after, as the 169–170 and 173–174 slices also recorded; no data race was reported anywhere).
- Live smoke 26/26 green (real binary + 1 key-aware Python mock, `$HOME/.tmp/smoke-175-176.sh` — scratch, not committed): API add 201 + routing_live, duplicate 409, no-auth 401; preferred 401 → second key serves (reserve 0 hits); dead key not re-dialled on the next request (trace `skipped_cooldown`, cooldown feed carries `key_id`); preferred 429 → reserve serves (1 hit); streaming pre-commit 401 → reserve streams (401'd key attempted once); providers feed shows 4 keys with per-key spend + live status; role flips + delete via API; last-key delete 400; dashboard serves with the API keys block, no Playground, healthz 200.

## Honest limitations

- A dead key in **soft-demote** mode is demoted to the end of the order, not skipped: it is still tried once per request if every preferred key fails. Only hard mode skips it for 5 minutes.
- `''` ledger/cooldown rows (pre-pooling history) are attributed to the fingerprint-less legacy key; if the legacy key was removed after pooling began, they stay unattributed history on no key.
- The smoke's first run caught no code bug but two script bugs (identical prompts were cache-served, hiding the failover; a second provider on the same base URL is a 409) — both fixed in the script, 26/26 on the re-run.
- The first smoke run also showed a cache-served request leaves the last-trace unchanged (pre-existing behaviour, outside this slice).

## Concurrency notes (for the coordinator)

- A concurrent slice broke the server build twice during this slice (`applyGuardrails` signature change; `ledger.LogFilter` becoming a string type) and fixed both itself within ~1 minute — no intervention needed.
- A concurrent slice added my three key routes to `guard.go` as a minimal unblock (my `register()` calls panic `New()` without table entries) but duplicated each entry (wrong "primary/fallback" role wording in the first set). I removed the duplicate set (keeping the correct preferred/reserve wording), added all five missing routes to the `guard_test.go` expected list (my 3 + embeddings + rerank), and pinned the tier counts to 9/12/48.
- `zz_debug_test.go` (another slice's debug test) was deleted mid-gate by its owner; the stale vet cache complained once, then cleared.
- `web/index.html`, `internal/server/guard.go`, `internal/server/guard_test.go` and `internal/server/server.go` contain other slices' uncommitted edits alongside mine — this slice did NOT commit, so the coordinator can commit per slice without sweeping in others' work.
