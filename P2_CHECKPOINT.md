# P2 Implementation Checkpoint

## Current State
- **Branch:** `p2-implementation`
- **HEAD:** `4970fb1` — published; `origin/p2-implementation` matches
- **Working tree:** clean except the owner-supplied untracked
  `P2_FINAL_FORENSIC_AUDIT.md` (intentionally preserved)
- **main / origin/main:** `a45b4ff` (P1 closure baseline)
- **Pull request:** [#81](https://github.com/raunakiitp/CodvianRouter/pull/81)
  — open, unmerged; 71 commits ahead of `main`, 124 files changed
  (+5,674 / −314); all 9 GitHub Actions checks green.

## Status — P2 scope complete, one item intentionally open

Implementation and verification for the P2 scope are complete: the
branch passed the local consolidated verification campaign and GitHub
CI. The only open item is WS3 batch re-queue (documented below, with
rationale). Release approval remains the owner's decision; nothing is
deployed. `P2_FINAL_FORENSIC_AUDIT.md` (repo root) is the authoritative
backlog document.

## Workstream status (independently audited, not ledger-copied)

### Strong
- WS4 Protocol Truthfulness — envelopes + query-key auth implemented
- WS5 Capacity — stream byte counter, ledger read pool implemented
- WS9A Routing — fallback chains wired + pinned tests

### Gaps closed by the current campaign (P2_FINAL_FORENSIC_AUDIT.md backlog)
- A1/A2/A3 (P0): canonical principalFor, webhook 429 retry, A2A panic containment
- B1 (P1): ledger async batch bounded retry (AUD-REL-002)
- B2 (P1): Docker digest pinning (AUD-CFG-001)
- B3 (P1): /readyz readiness probe wired into healthcheck (AUD-CFG-004)
- B4 (P1): automatic benchmark regression gate (cmd/bench-compare + bench-run.yml)
- B5/B6 (P1): compression pool integration; PrefixFreeze stale comment corrected
- 4.1: WS9 mapping corrected in docs/P2_REMAINING_FINDINGS_DISPOSITION.md
- 4.3: TestProbeReachability determinism (injected dialer, no external DNS)

### Verification campaign outcome (2026-11-26)
The one authorized consolidated campaign ran to completion at the then-current
head. Two concrete defects were found and fixed (commit 07ba920: gofmt drift
in 6 files; default_trace_test.go scoped //go:build !otel), and the
owner approved fixing the third: the forecast window-boundary
defect, FIXED in commit 2a8257f — the forecast's 1h/24h request
summaries now use SpendFor's exact window (zero until, unbounded);
Summary's display-form exclusive `ts < until` semantics are pinned
unchanged for display callers; the ledger-level boundary test pins
all three semantics at one boundary millisecond. The
TestUsageBudgetForecastProjected flake is eliminated (full suite
55 ok / 0 FAIL after the fix).

### Publication (2026-11-27)
Published with normal non-force pushes; remote HEAD = local HEAD =
`4970fb1`. Three further defects were found by CI and fixed before
green: the `secret-scan` job pinned a nonexistent `gitleaks-action`
commit and then hit the action's paid-license gate, so it now installs
the MIT gitleaks CLI v8.30.1 pinned with a sha256 check (`ab45d5e`,
`5067d46`); a Windows timer-granularity test flake in
`TestProbeHealthz` was made deterministic (`ec25413`); and nine files
the Linux gofmt gate flags were genuinely unformatted in committed
content (`4970fb1`). PR #81 is open and unmerged.

### Closure status after the final closure session (items 1-5)

1. WS9D OAuth inventory — DONE (commit 69ef9f0): all 31 CategoryOAuth
   entries classified and pinned: claude = connectable via external
   login (docs-verified messages surface, oauth-pkce Exchange; the
   router dials with the user-obtained token, no in-router Claude
   OAuth client — providerlogin has only OpenRouter); github-copilot
   + clinepass = partially documented (base URL probed, no auth
   surface, not docs-verified); 28 = metadata stubs. The claude
   Notes referenced GET /api/oauth/usage, a route that does not
   exist — replaced with the honest external-login disclosure.
2. WS9E structured output + modality matrix — DONE (commit 8aeb723):
   parametersJsonSchema mapping pinned (verbatim when classic absent,
   classic wins when both, {"type":"object"} default); structured
   OUTPUT (responseMimeType/responseSchema) rejection asserted;
   full endpoint → autorouter projection matrix pinned, unknown kind
   fails loudly.
3. WS9F default-build tracing — DONE (commit 95546f8): the default
   build (no -tags otel) pinned as an honest no-op — spans never
   record, context unchanged, Traceparent "" (never fabricated).
4. WS6 stream re-open idempotency — DONE (commit 4a2d60e): ledger
   double-settle idempotency pinned (hold releases exactly once);
   re-open bound + classification pinned (only pre-output
   truncations re-open; statuses/panics/cancellation never do);
   billing stays one-settle-per-request (single deferred settle onto
   an idempotent ledger delete).
5. WS3 batch re-queue — EXPLICITLY OPEN, documented (this section):
   the BATCHES API implements the OpenAI contract (create/list/get/
   cancel/delete, output_file_id, error_file_id, per-batch counters);
   interrupted batches are marked failed on load ("server_restart")
   and deliberately NOT re-run. A safe in-server re-queue would need
   durable per-row served/failed accounting plus a guarantee the
   provider did not serve a row whose response died mid-stream —
   without that, re-queueing risks double-charging rows that were
   actually served. The OpenAI API itself has no batch re-queue
   endpoint; the sanctioned client-side recovery is: read
   output_file_id (succeeded) + error_file_id (failed), correct the
   input, submit a NEW batch. Implementing a lesser re-queue would
   be unsafe; documenting instead per the closure rule.

### Still open
- **WS3 batch re-queue at the BATCHES-API level.** Interrupted batches are
  marked failed on load (`"server_restart"`, `internal/server/batches.go`)
  and deliberately NOT re-run: a safe automatic re-queue needs durable
  per-row served/failed accounting plus a guarantee the provider did not
  serve a row whose response died mid-stream, or it risks double-charging
  rows that were served. The OpenAI API has no batch re-queue endpoint;
  the sanctioned client-side recovery is to read `output_file_id` +
  `error_file_id`, correct the input, and submit a new batch. The private
  benchmark (the earlier router) is no safer: `the earlier implementation`
  retries within a batch but `recoverStaleBatch` explicitly does not
  replay an interrupted batch "to avoid duplicate provider work", and
  `src/lib/batches/retryFailed.ts` is a manual helper only. Documented
  rather than implemented, per the closure rule.

### Audit items DISPROVED by source inspection (no code change — recorded per campaign rules)
- B7/B8 "MCP SSE + Streamable HTTP transports missing": FALSE. Both remote
  transports are FULLY IMPLEMENTED in internal/server/mcp_http.go (2,514
  lines) — SSE (GET /api/mcp/sse stream + POST with session_id) and
  Streamable HTTP (POST /api/mcp/stream, GET stream, DELETE session), with
  gateway-key auth in the disjoint mcp: scope namespace (admin/chat/* scope
  escalation refused, per M9.1 N4), per-key + per-IP rate limits, bounded
  crypto-random sessions (64/transport, 16/key, 5-min idle eviction),
  fail-closed transport toggles (default OFF, flipped via POST
  /api/mcp/status), and the M9.3 ledger-backed methods. Registered in the
  guard table (TierManagement) and covered by 27 tests (mcp_http_test.go).
  THE AUDIT MISSED THIS FILE — the "missing transports" verdict was based
  on internal/mcp/identity.go comments (the stdio-only package) and never
  searched the server layer.
- B9 "five dead dashboard views": FALSE. All views have real load
  dispatches wired to live endpoints — keys -> loadRouterKeys()
  (GET /dashboard/keys), health -> m10_11LoadHealth() (GET
  /dashboard/memstats), resilience -> m10_11LoadResilience()
  (GET /api/resilience with admin-key handling), webhooks ->
  m10_12LoadWebhooks() (GET /api/webhooks), verification (singular)
  -> loadVerification() (GET /dashboard/verification). The audit's
  name-pattern grep (load<View>) missed the actual function names.
- B10 "keyboard accessibility gap": FALSE as stated. The dashboard
  uses native interactive elements exclusively (224 <button>, plus
  inputs/selects/textareas — keyboard-interactive by default, which
  is what B10 itself prescribes; adding tabindex=0 to native
  buttons would be wrong). Visible keyboard focus is implemented
  via the modern :focus-visible pattern (outline suppressed only
  for mouse focus, line ~487), a modal focus trap exists
  (~9486-9503), Escape closes modals and danger dialogs (5418,
  9563, 13676), and 52 aria- attributes are present.
- A redundant transport built during the campaign
  (internal/mcp/http.go) was deleted: the server-layer transport
  already existed; two competing transports would be worse than one.

## Test Evidence

Local consolidated campaign (2026-11-26, Windows host):
- `go test -count=1 ./...` → **55 packages ok, 0 FAIL** (exit 0)
- `go build ./...` / `go vet ./...` → exit 0 with `-buildvcs=false`
  (plain form is blocked on this host: VCS stamping shells out to an
  app-bundled git that is ACL-denied)
- `ci.sh` → green except two host-only blocks: `gofmt` (Windows
  `core.autocrlf` makes committed-LF files look unformatted locally; the
  Linux gate is green on CI) and `-race` (no gcc/CGO on this host)
- secrets scan PASS (1,768 tracked files); naming gate PASS;
  `audit-fake.sh` PASS; route-guard PASS; `govulncheck ./...` no
  vulnerabilities; SBOM self-check 44 components; both fuzz targets PASS

The consolidated final verification campaign HAS been run (see just
above) and GitHub CI is green on `4970fb1`: `ci`, `windows`,
`secret-scan`, `fuzz`, `redis`, `playwright`, `client-smoke`,
`bench-smoke`, `sbom-and-vuln`.

## Rules in force
- No production deployment, no paid API spending, no secret rotation.
- No history rewriting (no reset/rebase/amend/squash/force-push).
- PR #81 remains open and unmerged until the owner decides.
- Do not create `docs/PARITY.md`; no private competitive material in the
  public repository.
- Preserve `P2_FINAL_FORENSIC_AUDIT.md` (owner-supplied, untracked).