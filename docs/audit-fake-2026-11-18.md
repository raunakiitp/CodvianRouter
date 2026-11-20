# audit-fake — 2026-11-18 (M0.4)

**PRELIMINARY — the tree is mid-flight (Sprint 19 gate running). This audit MUST be re-run after the gate; findings below are static-grep evidence as of this run, not a final verdict.**

Run: `./scripts/audit-fake.sh` (exit 1 = findings present; static only — no server started, no go test/build). Git HEAD at run time is recorded in the output below.

```text
=== Codvian Router audit-fake (M0.4) — STATIC ONLY, no server started ===
Date (UTC): 2026-11-18 09:05:36
Repo root: /home/hatch/workspace/codvian-router
Git HEAD: e654248
UI source: web/index.html (8065 lines, HTML + inline JS/CSS)

--- 1. Route inventory (static grep of internal/server/*.go) ---
Registered routes (server.go s.register): 89
Guard-table routes (guard.go guardRouteTable): 89
  Guard table and register() calls agree exactly.
Routes answering 501 (notImplementedAdmin — feature does not exist yet):
    POST /admin/plugin/load (plugin load)
    POST /admin/provider/login (provider login)
    POST /admin/service/install (service install)
    POST /admin/update (auto-update)

--- 2. UI endpoint inventory (web/index.html) ---
Distinct literal UI paths referenced: 50
Distinct UI path prefixes (fetch/concat bases, e.g. /api/providers/ + id): 3
fetch( call sites in UI: 64
Button elements in UI: 122

UI path -> backend route (static match; {id} segments are wildcards):
  OK       /admin/config/reset
  OK       /admin/db/export
  OK       /admin/db/import
  OK       /admin/db/wipe
  OK       /admin/db/wipe/arm
  OK       /admin/keys/revoke-all
  OK       /admin/tunnel/start
  OK       /admin/tunnel/status
  OK       /admin/tunnel/stop
  OK       /api/catalog
  OK       /api/providers
  OK       /dashboard/auto-route
  OK       /dashboard/auto-route/last
  OK       /dashboard/benchmark
  OK       /dashboard/benchmark/history
  OK       /dashboard/catalog-audit
  OK       /dashboard/coalescing
  OK       /dashboard/code-outcomes
  OK       /dashboard/concurrency
  OK       /dashboard/cooldown
  OK       /dashboard/feedback
  OK       /dashboard/groups
  OK       /dashboard/guard
  OK       /dashboard/guardrails
  OK       /dashboard/headless
  OK       /dashboard/intent-tiers
  OK       /dashboard/key-budgets
  OK       /dashboard/keys
  OK       /dashboard/learned
  OK       /dashboard/logs
  OK       /dashboard/mcp
  OK       /dashboard/memstats
  OK       /dashboard/models
  OK       /dashboard/models/refresh
  OK       /dashboard/overview
  OK       /dashboard/plugins
  OK       /dashboard/pools
  OK       /dashboard/readiness
  OK       /dashboard/reasoning
  OK       /dashboard/release-verify
  OK       /dashboard/request-budgets
  OK       /dashboard/responses
  OK       /dashboard/responses/tree
  OK       /dashboard/strategies
  OK       /dashboard/stream-lifetime
  OK       /dashboard/streaming
  OK       /dashboard/suggestions
  OK       /dashboard/trace
  OK       /dashboard/usage
  OK       /dashboard/verification

UI path prefixes (dynamic fetch bases — matched as route prefixes):
  OK       /admin/keys/  (prefix of a registered route)
  OK       /api/providers/  (prefix of a registered route)
  OK       /v1/responses/  (prefix of a registered route)

Dynamic fetch() calls (URL is a variable — resolved by hand below):
  web/index.html:2937:        return fetch(url, {method: method, headers: headers, body: body === undefined ? undefined : JSON.stringify(body)}).then(function(r){
  web/index.html:6016:    fetch(action.endpoint, {
  web/index.html:6883:    fetch(path, {
  Known resolutions (checked by literal scan above):
    fetch(action.endpoint) — danger-zone actions; endpoints are the
      endpoint: "..." literals: /admin/keys/revoke-all, /admin/config/reset
      (+ export/wipe via literal fetch calls). Verified OK in §2 if listed OK.
      endpoint: "/admin/config/reset"
      endpoint: "/admin/keys/revoke-all"
    fetch(url) in keyReq() — provider-key actions; URLs are built from
      "/api/providers/" + id + "/keys..." (prefix listed above).
    fetch(path) in tunnelCall() — paths are "/admin/tunnel/status|start|stop"
      literals passed by the tunnel buttons (listed above if present).
    fetch(url) in runAutoRoute() — url is "/dashboard/auto-route?model=...";
      path /dashboard/auto-route is listed above.

--- 3. Hard-coded numbers / placeholders / mock data (web/index.html) ---
  "coming soon": 0 match(es)
  "lorem": 0 match(es)
  "sample data": 0 match(es)
  "demo data": 0 match(es)
  "fake data": 0 match(es)
  "mock" (case-insensitive): 8 match(es) — listed for review:
    web/index.html:2266:              <thead><tr><th>Mock cadence</th><th class="num">Tokens</th><th class="num">Median · batching off</th><th class="num">Median · batching on</th><th class="num">p99 · off</th><th class="num">p99 · on</th></tr></thead>
    web/index.html:5301:          $("#benchE1Meta").textContent = comp.clean ? "Both modes clean — direct-mock canary ≥ 12,000 RPS in every pass" : "POLLUTED — at least one pass failed the direct-mock canary; no verdict drawn";
    web/index.html:5337:          $("#benchCpuMeta").textContent = (cpu.clean ? "Both runs clean — direct-mock canary ≥ 12,000 RPS in every pass" : "POLLUTED — at least one pass failed the direct-mock canary; no verdict drawn") + (cpu.method ? " · " + cpu.method : "");
    web/index.html:5370:          $("#benchDefinitiveMeta").textContent = def.clean ? "Clean — direct-mock canary ≥ 12,000 RPS in every pass" : "POLLUTED — at least one pass failed the direct-mock canary; no verdict drawn";
    web/index.html:5388:          $("#benchKeyBudgetMeta").textContent = kb.clean ? "Clean — direct-mock canary ≥ 12,000 RPS in every pass" : "POLLUTED — at least one pass failed the direct-mock canary; no verdict drawn";
    web/index.html:5409:          $("#benchGlobalBudgetMeta").textContent = gb.clean ? "Clean — direct-mock canary ≥ 12,000 RPS in every pass" : "POLLUTED — at least one pass failed the direct-mock canary; no verdict drawn";
    web/index.html:5422:        $("#benchRpsNote").textContent = "Target > " + t.rps_ok.toLocaleString("en-US") + " · " + p1.errors.toLocaleString("en-US") + " errors (" + p1.error_rate_pct.toFixed(1) + "%) · direct mock " + Math.round(p1.direct_rps).toLocaleString("en-US") + " RPS";
    web/index.html:5652:     before (batching off) and after (batching on) at each mock
  [REVIEW] lines containing 'mock' are benchmark labels fed by real benchmark JSON (direct-mock canary, Mock cadence table) — verify no mock NUMBER is rendered as a live stat; no hard-coded stat found by this scan
  TODO/FIXME/XXX markers: 0
  Disabled <button>s (may be honest gating — listed for review):
    <button class="btn danger" id="dbWipeBtn" disabled>2 · Wipe now — irreversible
    <button class="btn danger" id="dangerConfirmBtn" disabled>Confirm
    <button class="btn small" disabled title="ToS-grey — cannot be added as a routing target">Add
  Known hard-coded catalog numbers that must come from a feed, not literals:
    "1,491": 0
    "614 providers": 0

--- 4. Controls with no backend call (toast-only handlers) ---
  Handlers that admit 'isn't wired yet' (each is a live-looking control
  with no backend route — MISSING per §0.3, even though the toast is honest):
    web/index.html:2860:            toast("Dashboard connect isn't wired yet — run: codvian-router provider add-catalog <id> <key>");
    web/index.html:3007:      if(inp){ inp.addEventListener("keydown", function(e){ if(e.key === "Enter"){ toast("Key rotation from the dashboard isn't wired yet — use the CLI to re-add the provider"); } }); }
    web/index.html:5925:    toast("Issuing keys from the dashboard isn't wired yet — use bootstrap (first key) or edit the config file");
    web/index.html:5932:      toast("Revoking keys from the dashboard isn't wired yet");
  [FINDING] toast-only control (web/index.html:2860): toast("Dashboard connect isn't wired yet — run: codvian-router provider add-catalog <id> <key>");
  [FINDING] toast-only control (web/index.html:3007): if(inp){ inp.addEventListener("keydown", function(e){ if(e.key === "Enter"){ toast("Key rotation from the dashboard isn'
  [FINDING] toast-only control (web/index.html:5925): toast("Issuing keys from the dashboard isn't wired yet — use bootstrap (first key) or edit the config file");
  [FINDING] toast-only control (web/index.html:5932): toast("Revoking keys from the dashboard isn't wired yet");

  Settings switches that only flip + toast '· saved' (generic $$('.switch')
  handler, no fetch, no settings-update route exists in the table):
    web/index.html:1744 wired (guardrail toggle -> POST /dashboard/guardrails; see §5 double-handler note)
    web/index.html:1749 wired (guardrail toggle -> POST /dashboard/guardrails; see §5 double-handler note)
    web/index.html:1754 wired (guardrail toggle -> POST /dashboard/guardrails; see §5 double-handler note)
  [FINDING] Settings switch 'Automatic failover' (web/index.html:2352) flips + toasts '· saved' but persists nothing — no fetch, no settings route
  [FINDING] Settings switch 'Prompt-cache pinning' (web/index.html:2356) flips + toasts '· saved' but persists nothing — no fetch, no settings route
  [FINDING] Settings switch 'Cost-aware routing' (web/index.html:2360) flips + toasts '· saved' but persists nothing — no fetch, no settings route
  [FINDING] Settings switch 'Health checks' (web/index.html:2373) flips + toasts '· saved' but persists nothing — no fetch, no settings route
  [FINDING] Settings switch 'Request logging' (web/index.html:2391) flips + toasts '· saved' but persists nothing — no fetch, no settings route
  Settings inputs with no listener at all (editable-looking, nothing persists):
    web/index.html:2369:                  <input class="num mono" type="number" value="30" min="5" max="300" aria-label="Request timeout in seconds" />
    web/index.html:2387:                  <input class="num mono" type="number" value="8080" min="1024" max="65535" aria-label="Port" />
  [FINDING] Settings inputs 'Request timeout' (value=30) and 'Port' (value=8080) plus the 'Listen address' select have no change/submit handler and no settings-update route — values shown are not read or saved
  501 routes referenced by the UI (would render a live-looking control
  that always answers 501):
    (none — no UI reference to /admin/service/install, /admin/plugin/load, /admin/update, /admin/provider/login)

--- 5. UNVERIFIED (static grep cannot settle these — live check needed) ---
  [UNVERIFIED] guardrail toggles #grTogglePII/#grToggleSecrets/#grToggleInjection have TWO click listeners that both flip aria-checked (generic $$('.switch') handler ~line 5938 + dedicated handler ~line 7770); statically the flips cancel and grPostConfig() POSTs the pre-click value, then loadGuardrails() re-renders server state — needs a LIVE click test to confirm or refute; do NOT count as a finding until then
  [UNVERIFIED] #revealKeyBtn ('About this key') only shows an explanatory toast — no reveal endpoint exists by design (keys shown once at issue); informational button, not a backend gap, but confirm the label does not read as a live action
  [UNVERIFIED] per-view honest-empty-state coverage (every dashboard view on a fresh router with 0 providers/keys) cannot be settled by grep — needs the Playwright smoke pass (M10 gate)

--- 6. FINDINGS: UI without backend (feed to PARITY.md) ---
  10 confirmed finding(s). Suggested PARITY rows (verify file:line before committing):
    - Dashboard '+ New router key' button -> MISSING: no key-creation route exists
      (only POST /admin/keys/{id} update, /{id}/rotate, /revoke-all, /admin/bootstrap);
      click only toasts 'isn't wired yet' (web/index.html:1931, :5925).
    - Dashboard per-key 'Revoke' button ([data-revoke]) -> MISSING: no per-key
      revoke route exists (only revoke-all); click only toasts (web/index.html:5752, :5932).
    - Dashboard provider-card 'Connect' button (unconnected catalog provider)
      -> MISSING: no dashboard provider-add call; click only toasts and points at
      the CLI (web/index.html:2842, :2860). (The Add-provider form itself POSTs
      /api/providers, which exists — only the per-card shortcut is unwired.)
    - Provider-card key input Enter (rotate key) -> MISSING: no provider-key
      rotate route (only add/remove/role); Enter only toasts (web/index.html:3007).
    - Settings switches Automatic failover / Prompt-cache pinning / Cost-aware
      routing / Health checks / Request logging -> WRONG: each flips + toasts
      '<label> turned on/off · saved' while persisting nothing (web/index.html:2352-2391,
      generic handler ~:5938). No settings-update route exists.
    - Settings 'Request timeout' (30), 'Port' (8080), 'Listen address' select
      -> MISSING/WRONG: editable-looking controls with no handler and no route;
      displayed values are never read or saved (web/index.html:2369, :2383-2387).

--- Summary ---
Routes registered: 89 (guard table: 89) | UI literal paths: 50 | fetch() sites: 64
FINDINGS (UI without backend): 10 | UNVERIFIED: 3 | REVIEW notes: 1
NOTE: this is a STATIC audit. Re-run after the Sprint gate; the tree may be mid-flight.
RESULT: FAIL — 10 UI control(s) without a live backend (M10 gate: must be 0).
```
