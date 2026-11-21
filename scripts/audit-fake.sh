#!/usr/bin/env bash
# audit-fake.sh — M0.4 (Master Prompt v1, §M0.4 / §0.3 Definition of Done)
#
# Static audit for "UI without backend" in the Codvian Router dashboard.
#
#   1. Greps web/index.html (single file: HTML + inline CSS + inline JS,
#      no separate .js) for hard-coded numbers, placeholder badges, mock
#      data, and "coming soon" controls that look live.
#   2. Lists every UI control whose backend route returns 404/501/static.
#      Route existence is determined STATICALLY by grepping route
#      registration in internal/server/*.go (read-only greps).
#      It NEVER starts a server, runs go test, or runs go build.
#   3. Prints a "UI without backend" findings list, each item written so
#      it can feed the internal parity tracker as a WRONG/MISSING row.
#   4. (§5b, Phase 0) Checks production Go code for the known backend
#      fakes removed on 2026-11-21 and the contracts that replaced them:
#      fabricated values, removed routes, 501s outside
#      internal/server/unavailable_routes.go, unavailable routes whose
#      guard description hides it, process-global settings stores,
#      the web-fetch egress guard, and /admin/restart classification.
#
# Honesty rule (§0.3): a route whose existence is ambiguous from static
# greps is reported UNVERIFIED, never as a finding. Only controls proven
# (by the code in front of us) to have no backend call / no route are
# FINDINGs. A control that only shows an honest "isn't wired yet" toast
# is still a finding (MISSING): per §0.3 it must not exist, or must
# render an explicit "Not implemented yet" state counted as MISSING.
#
# Exit code: 0 = audit ran and found zero UI-without-backend controls;
#            1 = audit ran and found >= 1 confirmed control (gate signal
#            for M10: "audit-fake.sh returns ZERO UI controls without
#            live backend"); 2 = the audit itself could not run.
# The tree may be mid-flight when this runs: re-run after the gate.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UI="$ROOT/web/index.html"
SRV="$ROOT/internal/server/server.go"
GUARD="$ROOT/internal/server/guard.go"

FINDINGS=0
UNVERIFIED=0
REVIEW=0

say() { printf '%s\n' "$*"; }
finding() { FINDINGS=$((FINDINGS+1)); say "  [FINDING] $*"; }
unverified() { UNVERIFIED=$((UNVERIFIED+1)); say "  [UNVERIFIED] $*"; }
review() { REVIEW=$((REVIEW+1)); say "  [REVIEW] $*"; }

[ -f "$UI" ] || { say "ERROR: $UI not found"; exit 2; }
[ -f "$SRV" ] || { say "ERROR: $SRV not found"; exit 2; }
[ -f "$GUARD" ] || { say "ERROR: $GUARD not found"; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say "=== Codvian Router audit-fake (M0.4) — STATIC ONLY, no server started ==="
say "Date (UTC): $(date -u '+%Y-%m-%d %H:%M:%S')"
say "Repo root: $ROOT"
if git -C "$ROOT" rev-parse --short HEAD >/dev/null 2>&1; then
  say "Git HEAD: $(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null)"
else
  say "Git HEAD: n/a (no .git in worktree — local-only tree)"
fi
say "UI source: web/index.html ($(wc -l < "$UI") lines, HTML + inline JS/CSS)"
say ""

# ------------------------------------------------------------------
# 1. ROUTE INVENTORY (static, from Go source)
# ------------------------------------------------------------------
say "--- 1. Route inventory (static grep of internal/server/*.go) ---"
# Registered routes: s.register(mux, "METHOD", "PATH", ...) anywhere in
# internal/server/*.go except *_test.go — helpers register routes outside
# server.go (registerWebhooksRoutes in webhooks_api.go, provider_v1.go),
# so a server.go-only grep misses them and reports live routes as 404.
# Full-line // comments are dropped first: several files quote
# s.register(...) calls in doc comments. Test files are excluded:
# their registrations are test-only, not production routes.
for _f in "$ROOT"/internal/server/*.go; do
  case "$_f" in *_test.go) continue;; esac
  grep -E 's\.register\(mux, "[A-Z]+", "[^"]+"' "$_f" 2>/dev/null \
    | grep -vE '^[[:space:]]*//' || true
done | grep -oE 's\.register\(mux, "[A-Z]+", "[^"]+"' \
  | sed 's/s\.register(mux, "//; s/", "/ /; s/"$//' | sort -u > "$TMP/registered.txt"
# Routes registered in a loop by registerUnavailable (Phase 0): the
# "METHOD path" keys of unavailableRoutes in unavailable_routes.go.
UNAV="$ROOT/internal/server/unavailable_routes.go"
[ -f "$UNAV" ] || { say "ERROR: $UNAV not found"; exit 2; }
grep -oE '^[[:space:]]*"[A-Z]+ /[^"]+":' "$UNAV" \
  | sed -E 's/^[[:space:]]*"([A-Z]+) ([^"]+)":/\1 \2/' | sort -u > "$TMP/unavailable.txt"
sort -u "$TMP/registered.txt" "$TMP/unavailable.txt" -o "$TMP/registered.txt"
# Guard classification table: {Method: "M", Path: "P", ...} in guard.go.
# register() panics at startup for a route with no table entry, so the
# two lists must agree; a mismatch is a consistency flag, not a finding.
grep -oE '\{Method: "[A-Z]+", Path: "[^"]+"' "$GUARD" \
  | sed 's/{Method: "//; s/", Path: "/ /; s/"$//' | sort -u > "$TMP/guardtable.txt"
NREG="$(wc -l < "$TMP/registered.txt")"
NGUARD="$(wc -l < "$TMP/guardtable.txt")"
say "Registered routes (internal/server/*.go s.register, no tests/comments): $NREG"
say "Guard-table routes (guard.go guardRouteTable): $NGUARD"
if ! diff -q "$TMP/registered.txt" "$TMP/guardtable.txt" >/dev/null 2>&1; then
  say "  Routes in guard table but NOT registered:"
  comm -23 "$TMP/guardtable.txt" "$TMP/registered.txt" | sed 's/^/    /' || true
  say "  Routes registered but NOT in guard table:"
  comm -13 "$TMP/guardtable.txt" "$TMP/registered.txt" | sed 's/^/    /' || true
  unverified "route inventory mismatch between guard table and register() calls — re-check after the gate"
else
  say "  Guard table and register() calls agree exactly."
fi
# Routes whose handler is the documented 501 feature_unavailable
# handler (unavailable_routes.go): "METHOD path" per line.
cp "$TMP/unavailable.txt" "$TMP/routes501.txt"
say "Routes answering 501 feature_unavailable (unavailable_routes.go): $(wc -l < "$TMP/routes501.txt")"
if [ -s "$TMP/routes501.txt" ]; then sed 's/^/    /' "$TMP/routes501.txt"; else say "    (none)"; fi
say ""

# ------------------------------------------------------------------
# 2. UI ENDPOINT INVENTORY — every path the dashboard references
# ------------------------------------------------------------------
say "--- 2. UI endpoint inventory (web/index.html) ---"
# All string literals that look like a router path: fetch("...") targets,
# endpoint: "..." values (danger-zone actions), tunnel paths, and
# concatenation bases such as "/api/providers/" + id. Query strings and
# template/concatenation tails are stripped for matching.
grep -oE '"(/(dashboard|api|admin|v1)/[^" ]*)"' "$UI" \
  | tr -d '"' | sed -E 's/\?.*$//; s/\$\{.*$//; s/[`+].*$//' \
  | grep -vE '/$' > "$TMP/ui_paths_raw.txt"   # full paths (no trailing /)
grep -oE '"(/(dashboard|api|admin|v1)/[^" ]*)"' "$UI" \
  | tr -d '"' | sed -E 's/\?.*$//; s/\$\{.*$//; s/[`+].*$//' \
  | grep -E '/$' > "$TMP/ui_prefixes_raw.txt" # concatenation bases, keep /
# Backtick template literals, if any (fetch(`/dashboard/...`) style).
grep -oE '`(/(dashboard|api|admin|v1)/[^` ]*)`' "$UI" \
  | tr -d '`' | sed -E 's/\?.*$//; s/\$\{.*$//' >> "$TMP/ui_paths_raw.txt" || true
sort -u "$TMP/ui_paths_raw.txt" > "$TMP/ui_paths.txt"
sort -u "$TMP/ui_prefixes_raw.txt" > "$TMP/ui_prefixes.txt"
say "Distinct literal UI paths referenced: $(wc -l < "$TMP/ui_paths.txt")"
say "Distinct UI path prefixes (fetch/concat bases, e.g. /api/providers/ + id): $(wc -l < "$TMP/ui_prefixes.txt")"
say "fetch( call sites in UI: $(grep -c 'fetch(' "$UI")"
say "Button elements in UI: $(grep -c '<button' "$UI")"

# Build matchable route patterns: {param} segments become [^/]+.
awk '{ gsub(/\{[^}]*\}/, "[^/]+"); print $2 "\t" $1 "\t" $0 }' "$TMP/registered.txt" > "$TMP/route_patterns.txt"
# 501 paths, method-stripped, for the lookup below.
awk '{ print $2 }' "$TMP/routes501.txt" > "$TMP/paths501.txt"

route_status() { # $1 = UI path -> OK | 501 | MISSING
  local p="$1" pat
  while IFS=$'\t' read -r regex _method _orig; do
    if [[ "$p" =~ ^${regex}$ ]]; then
      if grep -qxF "$p" "$TMP/paths501.txt" 2>/dev/null; then echo "501"; return; fi
      # A literal UI path can also equal a 501 route pattern only via
      # exact match; parameterised 501 routes do not exist today.
      echo "OK"; return
    fi
  done < "$TMP/route_patterns.txt"
  echo "MISSING"
}

say ""
say "UI path -> backend route (static match; {id} segments are wildcards):"
while read -r p; do
  st="$(route_status "$p")"
  case "$st" in
    OK)  say "  OK       $p" ;;
    501) finding "UI calls $p but its backend route answers 501 (not implemented yet)" ;;
    MISSING) finding "UI calls $p but NO backend route is registered for it (would 404)" ;;
  esac
done < "$TMP/ui_paths.txt"

say ""
say "UI path prefixes (dynamic fetch bases — matched as route prefixes):"
while read -r pre; do
  # A prefix matches if some registered route path starts with it and
  # continues past it (a parameterised or deeper segment).
  hit=0
  while IFS= read -r rp; do
    rpath="${rp#* }"
    if [ "$rpath" != "$pre" ] && [ "${rpath#"$pre"}" != "$rpath" ]; then hit=1; break; fi
  done < "$TMP/registered.txt"
  if [ "$hit" -eq 1 ]; then
    say "  OK       $pre  (prefix of a registered route)"
  else
    unverified "UI fetch base $pre could not be matched to a registered route statically — check its call sites by hand"
  fi
done < "$TMP/ui_prefixes.txt"

# Dynamic fetch() calls whose URL is a variable, not a literal.
say ""
say "Dynamic fetch() calls (URL is a variable — resolved by hand below):"
grep -nE 'fetch\((action\.endpoint|path|url)(,| |$)' "$UI" | sed 's/^/  web\/index.html:/' || true
say "  Known resolutions (checked by literal scan above):"
say "    fetch(action.endpoint) — danger-zone actions; endpoints are the"
say "      endpoint: \"...\" literals: /admin/keys/revoke-all, /admin/config/reset"
say "      (+ export/wipe via literal fetch calls). Verified OK in §2 if listed OK."
grep -oE 'endpoint: "[^"]+"' "$UI" | sort -u | sed 's/^/      /'
say "    fetch(url) in keyReq() — provider-key actions; URLs are built from"
say "      \"/api/providers/\" + id + \"/keys...\" (prefix listed above)."
say "    fetch(path) in tunnelCall() — paths are \"/admin/tunnel/status|start|stop\""
say "      literals passed by the tunnel buttons (listed above if present)."
say "    fetch(url) in runAutoRoute() — url is \"/dashboard/auto-route?model=...\";"
say "      path /dashboard/auto-route is listed above."
say ""

# ------------------------------------------------------------------
# 3. HARD-CODED NUMBERS / PLACEHOLDERS / MOCK DATA
# ------------------------------------------------------------------
say "--- 3. Hard-coded numbers / placeholders / mock data (web/index.html) ---"
for pat in 'coming soon' 'lorem' 'sample data' 'demo data' 'fake data'; do
  n="$(grep -ci "$pat" "$UI" || true)"
  say "  \"$pat\": $n match(es)"
  if [ "$n" -gt 0 ]; then grep -in "$pat" "$UI" | head -10 | sed 's/^/    web\/index.html:/'; fi
done
say "  \"mock\" (case-insensitive): $(grep -ci 'mock' "$UI") match(es) — listed for review:"
grep -in 'mock' "$UI" | head -10 | sed 's/^/    web\/index.html:/'
if grep -qi 'mock' "$UI"; then
  review "lines containing 'mock' are benchmark labels fed by real benchmark JSON (direct-mock canary, Mock cadence table) — verify no mock NUMBER is rendered as a live stat; no hard-coded stat found by this scan"
fi
say "  TODO/FIXME/XXX markers: $(grep -c -E 'TODO|FIXME|XXX' "$UI" || true)"
say "  Disabled <button>s (may be honest gating — listed for review):"
grep -oE '<button[^>]*disabled[^>]*>[^<]{0,60}' "$UI" | sed 's/^/    /'
say "  Known hard-coded catalog numbers that must come from a feed, not literals:"
for pat in '1,491' '614 providers'; do
  n="$(grep -c "$pat" "$UI" || true)"; say "    \"$pat\": $n"
done
say ""

# ------------------------------------------------------------------
# 4. TOAST-ONLY / NO-BACKEND CONTROLS (the "UI without backend" core)
# ------------------------------------------------------------------
say "--- 4. Controls with no backend call (toast-only handlers) ---"
say "  Handlers that admit 'isn't wired yet' (each is a live-looking control"
say "  with no backend route — MISSING per §0.3, even though the toast is honest):"
grep -n "isn't wired yet" "$UI" | sed 's/^/    web\/index.html:/'
while read -r line; do
  finding "toast-only control (web/index.html:$line): $(sed -n "${line}p" "$UI" | sed 's/^ *//; s/ *$//' | cut -c1-120)"
done < <(grep -n "isn't wired yet" "$UI" | cut -d: -f1)

say ""
say "  Settings switches with no dedicated POST handler (a .switch is a"
say "  finding only when neither a grToggle nor a delegated per-type"
say "  toggle covers it — there is no generic handler in the page):"
# Switches with a dedicated POST handler are the guardrail toggles
# (grTogglePII/Secrets/Injection -> POST /dashboard/guardrails) and
# the per-type detection switches (data-pattern, rendered from the
# feed's types list, covered by the delegated #grTypes listener ->
# grPostTypeToggle -> POST /dashboard/guardrails disabled_types).
# There is NO generic $$('.switch') handler in the page any more, so
# an uncovered .switch is a finding, not a toast-only control.
grep -n 'class="switch"' "$UI" > "$TMP/switches.txt" || true
while read -r l; do
  ln="${l%%:*}"; txt="${l#*:}"
  if echo "$txt" | grep -q 'grToggle'; then
    say "    web/index.html:$ln wired (guardrail toggle -> POST /dashboard/guardrails; see §5 double-handler note)"
  elif echo "$txt" | grep -q 'data-pattern'; then
    if grep -q 'grPostTypeToggle' "$UI" && grep -q 'disabled_types: disabled' "$UI" && grep -q 'closest("button.switch")' "$UI"; then
      say "    web/index.html:$ln wired (per-type toggle -> POST /dashboard/guardrails disabled_types via #grTypes delegation)"
    else
      label="$(echo "$txt" | grep -oE 'aria-label="[^"]*"' | sed 's/aria-label=//; s/"//g')"
      finding "Settings switch '${label:-?}' (web/index.html:$ln) flips but persists nothing — no fetch, no settings route"
    fi
  else
    label="$(echo "$txt" | grep -oE 'aria-label="[^"]*"' | sed 's/aria-label=//; s/"//g')"
    finding "Settings switch '${label:-?}' (web/index.html:$ln) flips + toasts '· saved' but persists nothing — no fetch, no settings route"
  fi
done < "$TMP/switches.txt"
say "  Settings inputs with no listener at all (editable-looking, nothing persists):"
grep -nE 'aria-label="(Request timeout in seconds|Port)"' "$UI" | sed 's/^/    web\/index.html:/'
if grep -qE 'aria-label="(Request timeout in seconds|Port)"' "$UI"; then
  finding "Settings inputs 'Request timeout' (value=30) and 'Port' (value=8080) plus the 'Listen address' select have no change/submit handler and no settings-update route — values shown are not read or saved"
fi
say "  501 routes referenced by the UI (would render a live-looking control"
say "  that always answers 501):"
if grep -qE '/admin/(service/install|plugin/load|update|provider/login)' "$UI"; then
  grep -nE '/admin/(service/install|plugin/load|update|provider/login)' "$UI" | sed 's/^/    web\/index.html:/'
  finding "UI references a 501 route (see lines above)"
else
  say "    (none — no UI reference to /admin/service/install, /admin/plugin/load, /admin/update, /admin/provider/login)"
fi
say ""

# ------------------------------------------------------------------
# 5. UNVERIFIED — static evidence is ambiguous, needs a live check
# ------------------------------------------------------------------
say "--- 5. UNVERIFIED (static grep cannot settle these — live check needed) ---"
# Guardrail toggles: the generic $$(".switch") handler AND a dedicated
# grToggle handler both flip aria-checked on the same element. Listeners
# fire in registration order (generic first, dedicated second), so
# statically the two flips cancel and grPostConfig() POSTs the PRE-click
# value — but only a live click test can confirm the toggles are dead.
if grep -q 'class="switch" id="grToggle' "$UI" && grep -q '\$\$(".switch")' "$UI"; then
  unverified "guardrail toggles #grTogglePII/#grToggleSecrets/#grToggleInjection have TWO click listeners that both flip aria-checked (generic \$\$('.switch') handler ~line 5938 + dedicated handler ~line 7770); statically the flips cancel and grPostConfig() POSTs the pre-click value, then loadGuardrails() re-renders server state — needs a LIVE click test to confirm or refute; do NOT count as a finding until then"
fi
# #revealKeyBtn only toasts an explanation; no reveal endpoint exists by
# design (keys are shown once). Informational, not a missing backend.
if grep -q 'revealKeyBtn' "$UI"; then
  unverified "#revealKeyBtn ('About this key') only shows an explanatory toast — no reveal endpoint exists by design (keys shown once at issue); informational button, not a backend gap, but confirm the label does not read as a live action"
fi
# Dashboard is served static; fetch failures render honest empty states.
# Whether every view's empty state is honest can only be settled live.
unverified "per-view honest-empty-state coverage (every dashboard view on a fresh router with 0 providers/keys) cannot be settled by grep — needs the Playwright smoke pass (M10 gate)"
say ""

# ------------------------------------------------------------------
# 5b. PHASE 0 PRODUCTION CONTRACT — known fake backend behaviour
# ------------------------------------------------------------------
# Targets the specific fabrications removed in Phase 0 (2026-11-21)
# and the route contracts that replaced them. Production code only:
# *_test.go files are skipped (test mocks and fixtures are allowed).
# Each check names one prohibited behaviour, so a hit is a finding,
# not a fuzzy "TODO"/"mock" word match. Limitations: these are static
# greps; they catch the known patterns and their contracts, not every
# possible fake. The Go tests (TestUnavailableRoutesHonest,
# TestRemovedFakeRoutesAre404, the TestWebFetch* SSRF suite) prove the
# runtime behaviour.
say "--- 5b. Phase 0 production contract (backend fakes) ---"
prod_go() { # every non-test .go file under internal/ and cmd/
  find "$ROOT/internal" "$ROOT/cmd" -name '*.go' ! -name '*_test.go' -print 2>/dev/null
}
prod_grep() { # $1 = ERE; prints file:line matches in production Go
  prod_go | while IFS= read -r f; do grep -nE "$1" "$f" 2>/dev/null | sed "s|^|${f#$ROOT/}:|"; done
}
P0=0
p0_finding() { P0=$((P0+1)); finding "$*"; }

# P1. Fabricated responses and metrics removed in Phase 0.
for pat in 'processed successfully' '"motion_score"' '"frames_total"' 'systemone\.completion' \
           'standardAudioVoices' '"elo":[[:space:]]*1000' 'Value:[[:space:]]*0\.95'; do
  hits="$(prod_grep "$pat")"
  [ -n "$hits" ] && p0_finding "fabricated value pattern /$pat/ in production code: $(printf '%s' "$hits" | head -3 | tr '\n' ' ')"
done

# P2. Removed fake routes must stay removed (guard table AND register).
for r in "POST /v1/systemone" "GET /v1/video-bridge/drilldown" "POST /v1/video-bridge/drilldown" \
         "GET /v1/audio/voices" "GET /v1/voices"; do
  if grep -qxF "$r" "$TMP/registered.txt" || grep -qxF "$r" "$TMP/guardtable.txt"; then
    p0_finding "removed fake route is back: $r"
  fi
done

# P3. Every 501 in production server code must come from the
# documented unavailable handler. Allowed: unavailable_routes.go, and
# resilience_api.go's breaker-reset refusal (an engine without a
# reset method - a real capability check, not a stub).
hits="$(prod_grep 'StatusNotImplemented' | grep '^internal/server/' \
        | grep -vE '^internal/server/(unavailable_routes|resilience_api)\.go:')"
[ -n "$hits" ] && p0_finding "501 answered outside unavailable_routes.go: $(printf '%s' "$hits" | head -3 | tr '\n' ' ')"
hits="$(prod_grep 'notImplementedAdmin')"
[ -n "$hits" ] && p0_finding "legacy notImplementedAdmin stub still present: $(printf '%s' "$hits" | head -2 | tr '\n' ' ')"

# P4. Each unavailable route is in the guard table and its description
# says it is unavailable (so the generated API docs say so too).
while IFS= read -r r; do
  m="${r%% *}"; p="${r#* }"
  line="$(grep -F "{Method: \"$m\", Path: \"$p\"," "$GUARD" | head -1)"
  if [ -z "$line" ]; then
    p0_finding "unavailable route $r has no guard-table row"
  elif ! printf '%s' "$line" | grep -q 'Description: "Unavailable: answers 501 feature_unavailable'; then
    p0_finding "unavailable route $r: guard description does not say it is unavailable"
  fi
done < "$TMP/unavailable.txt"

# P5. In-memory settings stores nothing reads (the B8 pattern): a
# process-global "var globalX = &..." in production server code.
hits="$(prod_grep '^var global[A-Z][A-Za-z0-9_]* = &' | grep '^internal/server/')"
[ -n "$hits" ] && p0_finding "process-global settings store in server code (verify something reads it, or list the route as unavailable): $(printf '%s' "$hits" | head -3 | tr '\n' ' ')"

# P6. SSRF: web fetch must use the egress guard (validated-IP dial),
# never a plain client or a separate DNS pre-check.
WF="$ROOT/internal/server/web_fetch.go"
if [ -f "$WF" ]; then
  grep -q 'webFetchClient()' "$WF" || p0_finding "web_fetch.go does not use webFetchClient() (egress guard)"
  if grep -nE 'net\.LookupIP|http\.DefaultClient|&http\.Client\{|http\.Get\(' "$WF" >/dev/null; then
    p0_finding "web_fetch.go builds its own client or resolves DNS itself (DNS-rebinding risk)"
  fi
  grep -q 'DialContext:[[:space:]]*g\.dialContext' "$ROOT/internal/server/egress_guard.go" 2>/dev/null \
    || p0_finding "egress_guard.go transport does not dial through the validating dialContext"
  grep -qE 'Proxy:[[:space:]]*nil' "$ROOT/internal/server/egress_guard.go" 2>/dev/null \
    || p0_finding "egress_guard.go transport does not disable environment proxies"
fi

# P7. /admin/restart: spawn-capable pattern present, and the handler
# never falls back to the shutdown hook while saying "restarting".
grep -qF 'regexp.MustCompile(`^/admin/restart$`)' "$GUARD" \
  || p0_finding "/admin/restart is missing from spawnCapablePatterns"
grep -q 'shutdownHook' "$ROOT/internal/server/restart.go" 2>/dev/null \
  && p0_finding "restart.go references shutdownHook (restart must not silently become shutdown)"

if [ "$P0" -eq 0 ]; then
  say "  Phase 0 contract: all checks pass (P1-P7)."
fi
say ""

# ------------------------------------------------------------------
# 6. FINDINGS SUMMARY — feeds the internal parity tracker as WRONG/MISSING rows
# ------------------------------------------------------------------
say "--- 6. FINDINGS: UI without backend (feed to parity tracker) ---"
if [ "$FINDINGS" -eq 0 ]; then
  say "  (none — zero UI controls without a live backend)"
else
  say "  $FINDINGS confirmed finding(s) — see the [FINDING] lines in §2/§4/§5b above;"
  say "  each names its live file:line. Do NOT copy rows from memory: verify"
  say "  each finding against the current tree before committing to the parity tracker"
  say "  (a hardcoded list here went stale twice — new-key + revoke were"
  say "  wired while this section still called them MISSING)."
fi
say ""
say "--- Summary ---"
say "Routes registered: $NREG (guard table: $NGUARD) | UI literal paths: $(wc -l < "$TMP/ui_paths.txt") | fetch() sites: $(grep -c 'fetch(' "$UI")"
say "FINDINGS (UI without backend + Phase 0 backend fakes): $FINDINGS (Phase 0: $P0) | UNVERIFIED: $UNVERIFIED | REVIEW notes: $REVIEW"
say "NOTE: this is a STATIC audit. Re-run after the Sprint gate; the tree may be mid-flight."
if [ "$FINDINGS" -gt 0 ]; then
  say "RESULT: FAIL — $FINDINGS finding(s): UI controls without a live backend and/or Phase 0 backend fakes (gate: must be 0)."
  exit 1
fi
say "RESULT: PASS — zero UI controls without a live backend, Phase 0 contract holds."
exit 0
