# OmniRoute Parity Matrix (Phase 3 comparison snapshot)

Comparison date: 2026-11-22. Reference: https://github.com/cheaper-inference/OmniRoute
(public README/feature list studied clean-room; no code/assets copied). CODVIAN main: 6cadbda.
Statuses: PARITY / CODVIAN_AHEAD / OMNIROUTE_AHEAD / PARTIAL / NOT_IMPLEMENTED / NOT_RELEVANT / BLOCKED_EXTERNAL.

| Capability | OmniRoute evidence | CODVIAN evidence | Status |
|---|---|---|---|
| OpenAI-compatible endpoint | one base URL + gateway key | /v1/* behind gateway key (internal/server) | PARITY |
| Routing strategies | ~20-21 named | 35 in internal/strategy.All (superset) | CODVIAN_AHEAD |
| Combos (fusion/pipeline/auto) | yes | combo.go fusion/pipeline/jev + auto family | PARITY |
| Quota-aware fallback | yes | quota-weighted, headroom, quota pools/schedules | PARITY |
| Compression | RTK/Caveman | internal/compress lossless-first | PARITY/CODVIAN_AHEAD (measured ratio not claimed) |
| MCP / A2A | yes | internal/mcp + internal/a2a | PARITY |
| Per-key routing control | forcedModel/access | per-key models/routing + budgets | PARITY |
| Cost/usage ledger + reconciliation | pricing | ledger reported-vs-priced (T6) | PARITY/CODVIAN_AHEAD |
| Team budgets | - | checkTeamBudget (server/a2a.go) | CODVIAN has; org/RBAC partial |
| Scheduled evals + regression | eval/skills docs | P3-B core added (internal/eval/schedule.go) | PARTIAL (core built, API unwired) |
| Org/RBAC management | - | not implemented in CODVIAN | NOT_IMPLEMENTED (Phase 3 P3-A, deferred) |
| Context/output guardrails | - | catalog context_window + deprecation; per-path enforcement partial | PARTIAL (P3-D deferred) |
| Pre-dispatch cost estimation | - | budget + cost-aware strategies; no pre-dispatch estimate | PARTIAL (P3-C deferred) |
| Provider ecosystem breadth | 375 providers | catalog + config-driven registry (self-hosted scope) | PARTIAL/NOT_RELEVANT (CODVIAN is self-hosted, not a reseller) |

## Key findings
- CODVIAN routing intelligence (35 strategies, conformance-tested) is ahead of OmniRoute's documented set.
- OmniRoute's reseller business model (fixed-price resale) is NOT_RELEVANT to CODVIAN's self-hosted gateway.
- Genuine CODVIAN gaps identified for Phase 3: P3-A org/team/RBAC, P3-B server-wiring, P3-C cost estimation, P3-D guardrails.
- No unsafe competitor technique (cookie scraping / session theft / MITM) is appropriate; none adopted.
