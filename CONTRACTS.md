# Codvian Router — frozen contracts for parallel Phase 2–4 builders

Read this before writing code. These interfaces are FROZEN: build against
them, do not change them. The coordinator owns all integration wiring
(`cmd/codvian-router/main.go`, `internal/server/server.go`,
`internal/router/router.go` except the already-added `OrderHook`).

## Rules for every builder

- Backend only. **No UI work of any kind** (no dashboard, no HTML, no
  palette, no mockups). UI is paused until Raunak gives reference sites.
- Stdlib only, unless the task explicitly names a dependency. No network
  calls at build time beyond `go mod download` for pinned deps.
- Your module lives in its own directory/package. **Do not edit files
  outside your assigned paths.** Do not edit `main.go`, `server.go`,
  `router.go`, `README.md`, `ROADMAP.md`, `CHECKLIST.md`, or another
  builder's package. Do not push to GitHub; do not use a browser.
- Every new exported symbol gets a doc comment stating what it does and
  its honest limitations. Never invent numbers: benchmarks and claims
  must come from tests you actually ran.
- Tests live next to the code (`*_test.go`). Run `go test` for your
  package and `go vet` before reporting. Fast-check: if a command stalls
  >2–3 min, stop, check the error, retry once, then report.

## Frozen extension points (already in the tree)

- `router.Config.OrderHook func(chain []router.Attempt) []router.Attempt`
  — final reordering after quota filtering/sorting. Deterministic, must
  not drop/duplicate attempts, must not block.
- `router.Config.QuotaHint func(providerID string) int` — live registry
  hints (already wired in main.go via `router.QuotaHintFunc`).
- `router.Ledger` — `RecordAttempt(provider, model string, ok bool,
  latencyMs int64, usage provider.Usage)`.
- `provider.Client` — `Chat` / `ChatStream` over `provider.ChatRequest`
  / `provider.ChatResponse` (`Usage{PromptTokens, CompletionTokens,
  TotalTokens}`).
- `server.Engine` — `Chat` / `ChatStream`; `server.StreamResumer` —
  `ChatStreamFrom`. A wrapper implementing `server.Engine` can sit
  between the engine and the server without touching either.
  `degrade.StaleEngine` is such a wrapper: it also implements
  `server.StreamResumer` and forwards `LastTrace`.
- `internal/routetrace` — `RouteTrace` / `AttemptTrace` (leaf package,
  no internal imports): the per-request attempt record. `router.Engine`
  produces it (`ChatWithChain`, `LastTrace`); `server` reads it for
  metrics; `degrade` forwards it. It lives here (not in `router`)
  because `router_test` imports `server`, so `server` cannot import
  `router` — an import cycle. `RouteTrace.Add(provider, model,
  latencyMs, outcome, err)` appends one attempt.
- `cache.Cache` / `cache.SemanticCache` / `cache.ExactCache` /
  `cache.ExactKey(model, messages)` — the three cache layers.
- `ledger.Ledger.RecordUsage(ctx, ledger.Usage{...}, ledger.Prices)` —
  `Usage{GatewayKeyID, Provider, Model, PromptTokens, CompletionTokens,
  TotalTokens, SplitKnown, OK, LatencyMS}`; split-unknown usage MUST cost
  0, never invented.
- `config.Config` — v1 JSON (`version`, `listen`, `data_dir`,
  `gateway_keys`, `providers`, `budget_usd`, `fail_closed`,
  `telemetry_enabled`). Localhost-only bind, auth ON.
- `crypto.Vault` — `Seal` emits `v2:` (Argon2id), `Unseal` accepts
  `v1:` and `v2:`; `ReSeal` upgrades.

## Per-builder ownership (paths)

| Builder | Owns (only these paths) |
|---|---|
| scoring | `internal/scoring/` |
| eval | `internal/eval/` |
| metrics | `internal/metrics/` |
| health | `internal/health/` |
| degrade | `internal/degrade/` |
| config-stable | `internal/config/` (extensions + validate only), `internal/config/SCHEMA.md` |
| tray | `internal/tray/` |
| packaging | `Dockerfile`, `.dockerignore`, `docker-compose.yml`, `package.json`, `bin/`, `.goreleaser.yml`, `scripts/install.sh` |
| docs-phase4 | `docs/` (new files only), `CHANGELOG.md` |

Integration (wiring into main.go / server.go, the single test gate, the
GitHub push) is the coordinator's job after all builders return.
