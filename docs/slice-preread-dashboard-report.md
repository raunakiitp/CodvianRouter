# Slice report — items 133–134: streaming preread failover trace in the dashboard

CHECKLIST row 134 (quoted verbatim):

> Streaming preread failover (dashboard, same slice): Live Routing request detail + Request Logs trace rows show preread/failover-before-commit events from the real trace (`GET /dashboard/trace`, `preread` / `preread_events` / `preread_bytes`): `empty_stream` reason label ("empty stream — failed over before commit") + red dot; per-attempt preread label — "held N events (X bytes) before commit", "committed on first content (0 events held)", "empty stream — failed over before commit", "exempt (<slug>) — never replayed"; no label when preread is "" (non-streaming attempts, or a stream that errored before commit) — an absent label is more honest than an invented one. Same `renderTraceInto` covers both `#liveTrace` and `#logTrace`; honest empty states unchanged. `node --check` on the extracted inline script clean; "Playground" absent

## Verdict: already complete — audit only, zero code changes

Every clause of row 134 was already implemented and correct. This slice
was AUDIT + COMPLETE work; the audit found nothing missing or wrong, so
no file was edited (per the slice brief: do not rewrite working code).

## What was already present (line-by-line audit)

### Dashboard — `web/index.html`

| Row 134 requirement | Location | Status |
|---|---|---|
| `empty_stream` reason label "empty stream — failed over before commit" | `traceReasonLabel`, line 2802 | ✅ present |
| Red dot for `empty_stream` | `traceDotClass`, line 2817 (`"empty_stream"` → `"red"`) | ✅ present |
| "held N events (X bytes) before commit" | `prereadLabel`, lines 2833–2836 (rendered as `preread: held N event(s) (X bytes) before commit`) | ✅ present |
| "committed on first content (0 events held)" | `prereadLabel`, line 2835 (`n <= 0` branch) | ✅ present |
| "empty stream — failed over before commit" (preread) | `prereadLabel`, line 2838 (`pr === "empty"`) | ✅ present |
| "exempt (<slug>) — never replayed" | `prereadLabel`, line 2839 (`pr.indexOf("exempt:") === 0`, `pr.slice(7)`) | ✅ present |
| No label when preread is `""` | `prereadLabel` returns `""` (line 2832); `renderTraceInto` line 2865 renders the `<span class="preread">` only when the label is non-empty | ✅ present |
| Same `renderTraceInto` covers both `#liveTrace` and `#logTrace` | `renderTraces()`, lines 2870–2873, calls `renderTraceInto($("#liveTrace"))` and `renderTraceInto($("#logTrace"))`; containers exist at lines 781 and 1618 | ✅ present |
| Honest empty states unchanged | lines 2845–2850 ("Route trace unavailable…") and 2851–2858 ("No requests yet — the route trace appears here after the first request.") | ✅ present, untouched |
| "Playground" absent | grep over `web/index.html`: zero matches | ✅ present |
| Design system | `.preread` style, line 322 (`color:var(--text-3); font-size:12px`, ellipsis) — existing approved styling, no new UI added | ✅ compliant |

### Backend feed — `GET /dashboard/trace` serves the preread fields

- `internal/server/trace.go` (`handleTrace`, lines 22–36): serves
  `traceResponse{Attempts []routetrace.AttemptTrace}` — the trace's
  attempts verbatim, so every preread field reaches the dashboard.
- `internal/routetrace/routetrace.go` lines 36–38: `AttemptTrace` carries
  `Preread string \`json:"preread,omitempty"\``,
  `PrereadEvents int \`json:"preread_events,omitempty"\``,
  `PrereadBytes int \`json:"preread_bytes,omitempty"\``.
  (`omitempty` is safe here: the dashboard reads `at.preread || ""` and
  `Number(at.preread_events || 0)`.)
- `internal/routetrace/routetrace.go` line 71: `AddWithPreread(...)`
  records the preread state per attempt.

### Backend states the dashboard labels can actually fire on

Verified in `internal/router/preread.go` / `router.go` (not just the
dashboard's branches):

- `"held"` — `forwardStreamPreread`'s `commit` sets
  `prereadInfo{state: "held", events: len(buf), bytes: bufBytes}`; 0 events
  when the first event was already content, so "committed on first
  content" is distinguishable from "never ran" (`preread.go` lines
  176–186).
- `"empty"` — stream ends with no content before commit
  (`preread.go` lines 193–196, 225–227); `router.go` lines 2371–2380
  record it as outcome `"empty_stream"`.
- `"exempt:<slug>"` — `router.go` line 2243 (`pr = prereadInfo{state:
  "exempt:" + exempt}`), slugs codified once in
  `router.ReplayExemption` (`preread.go` lines 78–131:
  `embeddings`, `images`, `rerank`, `responses`,
  `responses_continuation`, `warm_up`).
- `""` — error/cancel before first content (`preread.go` returns the
  zero `prereadInfo`), and every non-streaming attempt
  (`addTraceAttempt` → `addTraceAttemptPreread(..., prereadInfo{})`,
  `router.go` lines 611–622).
- Reason slug: `provider.ReasonEmptyStream = "empty_stream"`
  (`internal/provider/taxonomy.go` line 117; `EmptyStreamError`
  classified at line 322), so the reason label and the red dot fire
  together with the `"empty"` preread label.

## What was missing

Nothing. No gap was found in the dashboard, the feed, or the engine
states behind it.

## What was changed

Nothing. No code file was edited. (Concurrency note: other builders
were editing the shared files concurrently; no edit was needed, so no
conflict was possible.)

## Checks run

- `go build ./...` — PASS (exit 0; only the pre-existing
  GOPATH/GOROOT-same-directory warning). Note: `go` is at
  `~/go/bin/go`, not on the default PATH.
- `node --check` on the extracted inline dashboard script — CLEAN.
  Single inline `<script>` at `web/index.html:1880`, 175,119 chars
  extracted to `/tmp/dashboard-inline.js`, `node --check` clean
  (node v24.22.1).
- Live binary check (binary built to `/tmp/codvian-router-test`,
  isolated `HOME=/tmp/cr-home`, fresh `init`, `serve` on port 18741,
  no mock upstream):
  - `GET /dashboard/trace` → HTTP 200, `{"attempts":[]}` (honest empty
    state, no invented traffic).
  - `GET /dashboard` → HTTP 200, 337,685 bytes; served HTML contains
    the `prereadLabel` code, the "empty stream — failed over before
    commit" label, and the "No requests yet…" empty-state string.
  - Test server stopped afterwards; port 18741 closed. (Two other
    `codvian-router-test` processes seen in `pgrep` belong to other
    builders' work — left running.)
- `go test` NOT run (per slice brief — one full gate runs at the end).

## Recommended CHECKLIST verdict

- **Item 133: Built ✅, Verified ✅** — backend preread failover was
  already built and its gate tests are recorded in the row; this
  slice's audit confirmed the trace states the dashboard depends on
  are genuinely produced by the engine. (No re-verification of the
  item-133 tests themselves was in this slice's scope.)
- **Item 134: Built ✅, Verified ✅** — every clause of the row is
  present in the dashboard and the feed, and the live binary check
  confirms the dashboard serves with the preread rendering code and
  the trace feed answers. The only part not verifiable in this
  environment is the live-browser click-through (stated as a
  limitation in row 133 itself).
