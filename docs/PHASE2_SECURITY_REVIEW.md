# Phase 2 — Security & Production-Hardening Verification (T14)

Scope (§8 / addendum §8): re-verify the Phase 1 hardening gates on the
Phase 2 tree and record real test evidence. Nothing here is a claim of
"secure by design" — each row names the code and the test that proved it,
with the command that produced the green result.

Verification run on `main` after Phase 2 merge, `go build -buildvcs=false ./...`
clean, on 2026-11-22. Where a gate is honoured by an existing test the
table records it and does not re-invent it.

## 1. Tenant isolation

| Surface | Evidence | Result |
|---|---|---|
| Semantic-cache cross-tenant / cross-temperature | `internal/cache` `TestM611*` (scoped store keyed by tenant+tempBucket; `m6_11_fix_test.go`, `m6_11_property_test.go`) — identical text, paraphrase and cross-temperature all MISS across tenants; same-tenant control hits | PASS (`go test ./internal/cache/` → ok, 4.049s) |
| Streaming semantic surface | `TestM611NoCrossTenantSemanticOnStream` (added in P2-T9): cross-tenant/paraphrase/temp all miss under providers-down | PASS (server suite) |

## 2. Budget enforcement

| Surface | Evidence | Result |
|---|---|---|
| Per-key windowed budgets, scoped budgets, quota pools/schedules | `internal/ledger` `Test*Budget*`; enforcement wired at request time (budget guard) | PASS (`go test -run 'Test.*Budget' ./internal/ledger/` → ok) |
| Budget webhook events | `budget.warning` edge-triggered (`TestM819BudgetWarningFiresOnThresholdCrossing`); `quota.exceeded` | PASS (server suite) |

## 3. Config validation / safe-failure

| Surface | Evidence | Result |
|---|---|---|
| Invalid config rejected, defaults safe | `internal/config` validation suite (webhook event whitelist incl. new provider.degraded/provider.recovered; unknown → error naming the event) | PASS (`go test ./internal/config/` → ok, 3.366s) |
| Webhook registration/update validation | `TestM819CreateValidation`, `TestM819UpdateValidation` (unknown event → 400) | PASS (server suite) |

## 4. Dashboard / API authorisation

| Surface | Evidence | Result |
|---|---|---|
| Session auth, brute-force lockout, logout invalidation, password change, expiry, gating | `internal/server/dashauth_test.go`: `TestDashAuthLoginSuccessWrongPasswordAndCookie`, `TestDashAuthLoginThrottleLocksOutAfterFiveFailures`, `TestDashAuthLogoutInvalidatesSession`, `TestDashAuthChangePassword`, `TestDashAuthSessionExpires`, `TestDashAuthFeedsGatedOncePasswordSet` | PASS (`go test -run 'Test.*Auth' ./internal/server/` → ok) |
| `/metrics` auth-gated | metrics handler behind the same dashboard auth (server wiring) | PASS (server suite) |

## 5. Secret redaction & credential handling

| Surface | Evidence | Result |
|---|---|---|
| Webhook signing secret never echoed (API) | `TestM819SecretNeverEchoed`, `TestM819UpdateSecretNeverEchoed` | PASS (server suite) |
| Webhook delivery payload safe | `TestBreakerWebhookEventsDeliverToLocalReceiver` (added P2-T7): signing secret absent from delivered body; payload limited to provider + real counts | PASS (server suite) |
| Provider credentials encrypted at rest | carried from Phase 1 (crypto store), untouched this phase | PASS (Phase 1 gate) |
| Guardrail redaction/blocking | `internal/guardrails` suite (PII/secret/injection redaction per kind) | PASS (`go test ./internal/guardrails/` → ok, 3.219s) |
| Prometheus label cardinality / payload non-leakage | `internal/metrics` `TestUsageLabelSafety`, `TestUsageCounters` (added P2-T7): hostile provider/model strings sanitized; fixed kind vocabulary; no secret/prompt in exposition | PASS (`go test ./internal/metrics/` → ok) |

## 6. Egress / SSRF (carried, untouched)

`internal/egress` tests (loopback refusal, proxy-env ignore, dial-validated-IP,
disallowed-target blocking, rebinding prevention, no redirects, TLS hostname
verification) — carried from Phase 1, unchanged this phase. Re-verified in
`./internal/webhooks/` suite (ok, 3.355s).

## 7. Residual / honest unknowns

- `provider.degraded` / `provider.recovered` webhook delivery is best-effort
  (same as the existing quota/budget events): a failing endpoint is dropped
  and counted, not retried forever — the documented M5.9 contract.
- Dashboard/budget pages are covered by backend authz; full UI interaction
  depth is tracked under P2-T8 (dashboard premiumization), not claimed here.

**Conclusion:** all re-verified hardening gates pass on the Phase 2 tree with
real test evidence; the two gaps found this phase (ImportEvents dropped
reported-cost/cancelled — fixed in T6; webhook event coverage — closed in T7)
are closed and locked with regression tests.