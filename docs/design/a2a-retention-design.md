# A2A Task Retention Design (M9.8 follow-up, bug-hunt finding 4 — MEDIUM)

- Status: DESIGN ONLY. No code was written for this document; no Go/JS file,
  `CHECKLIST.md`, `parity tracker`, or `PLAN.md` was modified.
- Date / as-read: 2026-11-19 ~14:20 IST. Worktree `~/workspace/codvian-router/`,
  HEAD `035d1ed` (M9 merge wave). The worktree is DIRTY at the time of writing
  (concurrent M9 builders have uncommitted changes), so every `file:line`
  below is as-read at that time. Builders MUST re-verify line numbers at
  build time; file names and symbol names are the stable part.
- Scope: M9.8 A2A task retention — the store grows without bound today.
  M9.8 follow-up to the A2A core (built + reviewed PASS-WITH-NITS) and the
  M9.9 dashboard A2A section. This design answers six questions so a builder
  can execute without re-deciding them. Every claim cites worktree
  `file:line` evidence; estimates are labelled as estimates, not measurements.

## 0. The problem, verified against the code

`internal/a2a/store.go` persists every `message/send` and `message/stream`
call as 1 row in `a2a_tasks` + 2–4 rows in `a2a_task_events` (creation event
`"" → submitted` in `Create`, `store.go:180`; one event per transition in
`Transition`, `store.go:304`–`transitionOnce`). The `message` column holds the
caller's params verbatim (`store.go:180`, `a2aRPCRequest.Params` raw in
`internal/server/a2a.go:143`), and on success the `result` column holds the
router's full raw JSON reply (`service.go:RunTask`, `service.go:76`–`99`).

There is no deletion path anywhere in the repo:

- `grep -rn 'DELETE FROM a2a' internal/` returns nothing. The only
  `DELETE FROM` statements touching task-like tables are in `internal/ledger/`
  (reservations, pools, response trees) — none for A2A.
- `ListByOwner` (`store.go:236`) has no `LIMIT`/`OFFSET`; `Events`
  (`store.go:304`) is unbounded. `CountsByState` (`store.go:261`) is the only
  bounded read (five aggregate integers).
- No TTL, no prune job, no delete API, no per-key cap exists.

Exposure, stated honestly: `ListTasks`/`Events` have **no HTTP caller today**
— the only HTTP methods are `message/send`, `message/stream`, `tasks/get`,
`tasks/cancel` (`internal/server/a2a.go:486`–`498`) — so the live exposure is
**disk growth of `<data-dir>/a2a/tasks.db`, not an oversized response**.
The unbounded reads become a real exposure the day any caller (dashboard,
a future `tasks/list` method, an MCP tool) wires up `ListTasks`, which is
why the per-key cap in §3 also bounds those reads (see §3, last paragraph).

### Disk math (estimates, not measurements — no task-size figure was measured)

Row sizes, from the schema (`store.go:36`–`50`):

| Piece | Size |
|---|---|
| Task row fixed fields (`id` 32 hex chars, `owner` 64 hex chars, `state`, two RFC3339Nano timestamps, `error`) | ~200 B + SQLite row overhead |
| `message` (params verbatim; typical small `messages[]` call) | ~0.2–1 KB |
| `result` (router's raw JSON reply; typical short completion) | ~1–5 KB |
| Events (2–4 rows × ~60–80 B: ids, two state strings, timestamp) | ~0.2–0.3 KB |
| **Typical task total** | **~2–6 KB** |
| Pessimistic planning figure (long reply, large params) | **~10 KB** |

Upper bound per task: the request body is capped at `maxRequestBody = 10 MB`
(`internal/server/server.go:92`), and the result is an engine reply with no
size cap of its own — so a single pathological task can be megabytes. The
planning figures above assume ordinary chat-sized traffic; retention alone
cannot bound a pathological single task, the per-key cap in §3 is what
bounds the aggregate.

Growth at the planning figures:

| Task rate | Per day (5 KB avg) | Per day (10 KB) | 7 days retained (5 KB) |
|---|---|---|---|
| 10/min (14,400/day, plausible busy local operator) | ~72 MB | ~144 MB | ~0.5 GB |
| 1/sec (86,400/day, sustained heavy use) | ~432 MB | ~864 MB | ~3.0 GB |
| 10/sec (864,000/day, abuse / runaway agent loop) | ~4.3 GB | ~8.6 GB | ~30 GB |

At a runaway-agent rate the 7-day window alone does not save the disk —
that is exactly why §3's cap exists alongside it. The two controls are
complementary: retention bounds the steady state, the cap bounds the worst
case. Neither control alone is sufficient.

### What already exists to reuse (and what does not)

- **No retention/pruning pattern exists for A2A or anywhere else in the
  router.** `grep -ri 'prune|retention' internal/` finds only the ledger's
  spend-reservation expiry (`internal/ledger/reservation.go:29`–`41`):
  `SpendReservationTTL = 10 * time.Minute`, expired rows pruned **lazily on
  read** (`reservedSpendLocked`, `reservation.go:158`–`164`, `DELETE ...
  WHERE expires_ms <= ?`). That pattern is NOT reusable here: it works
  because reservations are read on every budget check, so a lazy prune is
  guaranteed to run; A2A tasks are never read in bulk (no list caller), so
  lazy-on-read would never fire. Recorded so the builder does not copy it.
- **No periodic background-job infrastructure exists in the server.**
  `grep 'NewTicker|go func()' internal/server/*.go` finds only per-request
  goroutines (streaming, relay, tunnel copies, MCP heartbeats). There is no
  `Serve`/`Shutdown` background loop and no job registry. The prune loop in
  §2 therefore has to be a small self-contained goroutine started from the
  existing lazy-open path, not a plug-in to a scheduler that does not exist.
- **Config pattern to follow:** pointer sub-structs with `omitempty` and
  `Effective*` accessors — `Cache *CacheConfig` (`internal/config/config.go:135`)
  with `EffectiveSemanticThreshold()` (`config.go:806`, nil receiver = default,
  `0` = unset = default), `Responses *ResponsesConfig` (`config.go:123`) with
  Validate rejecting negatives, never silently clamping (`config.go:1320`–`1324`).
  `A2AEnabled` itself is a flat bool on `Config` (`config.go:95`, default
  `false` in `DefaultConfig`, `config.go:1264`).
- **Single-writer SQLite pattern to preserve:** `Open` (`store.go:70`–`78`)
  uses `_txlock=immediate`, `SetMaxOpenConns(1)`, WAL + 5 s busy timeout,
  `PRAGMA foreign_keys=ON`. Any prune/delete MUST run inside one
  `BEGIN IMMEDIATE` transaction on this same connection, like `Create` and
  `transitionOnce` do — never a second connection, never autocommit deletes.
- **Foreign keys have NO `ON DELETE CASCADE` on `a2a_task_events`**
  (`store.go:43`: `task_id TEXT NOT NULL REFERENCES a2a_tasks(id)` — compare
  `internal/ledger/ledger.go:618`, which does declare `ON DELETE CASCADE`).
  With `foreign_keys=ON`, deleting a task row while its events exist FAILS.
  The prune MUST delete events first, then tasks, in one transaction (§6).

## 1. Retention window for terminal tasks: 7 days

**Decision: terminal tasks (`completed`, `failed`, `cancelled`) are pruned
when `updated_at` is older than 7 days. Non-terminal tasks (`submitted`,
`working`) are NEVER pruned by age, however old they are.**

Reasons:

- **Operator expectation.** A task is a short-lived agent work item, not an
  audit record: `tasks/get` exists so a caller can poll a task it just
  created (`internal/server/a2a.go:644`). Nobody polls a week-old task in
  normal operation. 7 days covers the realistic debugging window — a Friday
  failure is still retrievable on Monday — without pretending tasks are a
  durable log. The spend ledger is the durable audit trail, not A2A tasks.
- **Disk math (§0).** At the plausible busy rate (10/min) 7 days costs
  ~0.5 GB at 5 KB/task; at 1/sec sustained it costs ~3 GB. Both are
  survivable on a local disk. 30 days would quadruple those figures
  (~2 GB / ~12 GB) for debugging value nobody asked for; 1 day would delete
  a Friday failure before Monday. 7 days is the middle that survives both
  objections.
- **Cutoff column is `updated_at`, not `created_at`.** `updated_at` is set
  on every transition (`transitionOnce`, `store.go:304`) and therefore equals
  the time the task *became* terminal. A task created 30 days ago but
  completed yesterday survives; a task completed 8 days ago is pruned even
  if it was created recently. Pruning on `created_at` would delete
  long-running tasks the moment they finished, which is the wrong semantic.
- **Non-terminal tasks are exempt, deliberately.** A `submitted`/`working`
  task older than 7 days is evidence of something wrong (crashed process,
  orphaned stream goroutine — the processor context is bounded by the 120 s
  server timeout, `internal/server/a2a.go:38`, so a `working` task older than
  hours is already stuck, not slow). Silently deleting it would destroy the
  only evidence of the stuck task. Stuck-task reaping is a separate, undecided
  question — see §7, open item (c) — not smuggled into this retention job.

## 2. Prune trigger: periodic background job (hourly), started from the lazy A2A open

**Decision: a periodic background goroutine, first run immediately at
startup of the A2A store, then every 1 hour. The interval is a code
constant (`pruneInterval = time.Hour` in `internal/a2a`), NOT configurable.**

Why this trigger, and why the others were rejected:

| Trigger | Verdict | Reason |
|---|---|---|
| Periodic background job (hourly) | **Chosen** | Runs without operator action and without request traffic. The only trigger that fires on an idle-but-enabled endpoint. Cost is one small `DELETE` per hour; the single-writer lock is held briefly (see §6, chunking note). |
| Lazy prune on write (piggyback on `Create`) | Rejected as primary | Puts delete work on the request path: every `message/send` would pay a `DELETE` under the same `BEGIN IMMEDIATE` write lock the insert needs (`store.go:70`–`78`), adding latency and lock contention exactly when the agent is waiting for its task. Also fires only when traffic exists — an idle endpoint never prunes, which is the same failure mode as below. |
| Prune on read (`ListTasks`/`Events`/`tasks/get`) | Rejected | There is no bulk-read traffic to trigger it: `ListTasks` has no HTTP caller (§0). A `tasks/get` read must never write (it would turn a read into a contended write under the single-writer lock, and a wrong-owner `tasks/get` must not cause any side effect at all). The ledger's lazy-on-read pattern (§0) works only because reservations are read on every request; A2A tasks are not. |
| Operator endpoint (manual prune button/API) | Rejected as sole trigger | Local-first single binary, no cron guarantee: an operator-triggered prune only runs if the operator remembers. Retention that depends on human memory is not retention. Visibility is provided another way — see §5 (status counts + log line). |

**Failure mode if the trigger never runs.** If the goroutine fails to start
or dies, disk growth is bounded by §3's cap, not by retention: worst case
`(#keys) × 1,000 tasks × ~10 KB ≈ 10 MB per key`. Retention is the
steady-state control; the cap is the safety net. The job must also be
fail-loud: every run logs one line (`a2a: prune removed N tasks (M events)
in D`, or the error on failure) via the server logger, and a failed run
must never crash the server or block the endpoint — the next hourly tick
retries. A prune error is never surfaced to a gateway-key caller (callers
must not learn operator maintenance state).

**Lifecycle (no scheduler exists — §0).** The store is opened lazily on the
first enabled `/a2a` call (`a2aService`, `internal/server/a2a.go:83`–`105`;
while disabled, no `tasks.db` is ever created). The prune goroutine starts
in that same lazy-open path, guarded so it starts exactly once (a
`sync.Once` / boolean on `Server`, next to `a2aMu`/`a2aSvc`/`a2aCancels` in
`internal/server/server.go:716`–`723`). It takes a `context.Context` so
tests can stop it; in production the context is process-lifetime — there is
no graceful-stop hook for background work in this codebase (existing
goroutines in `server.go`/`tunnel.go` likewise run to process exit), and
process exit ends the ticker. That is a deliberate decision, not an
oversight: do not invent a shutdown-registry in this slice. The first run
happens immediately after open (catches the backlog from a previous run),
then hourly.

## 3. Per-key task cap: 1,000 tasks per owner — evict oldest terminal first, never reject a live task

**Decision: at most 1,000 tasks per owner (gateway key). When a new task
would exceed the cap, the store evicts the oldest *terminal* tasks for that
owner first (oldest `updated_at`), inside the same transaction as the
insert, until the count is back under the cap. Only if the owner has zero
evictable terminal tasks (all 1,000 are `submitted`/`working`) is the create
rejected, with a new typed error `ErrTaskLimitExceeded`.**

Why evict, not reject:

- **Rejecting deadlocks the key.** There is no task-delete API (§5) and no
  operator delete path, so a key at the cap with reject-behaviour could only
  free space by waiting out the 7-day retention window — up to 7 days of
  refused `message/send` calls for an agent loop that has no way to recover.
  Eviction is self-healing: the oldest finished work is dropped to make room
  for new work, which is the correct trade for a short-lived task store.
- **Eviction is bounded and honest.** Only terminal tasks are evicted, so no
  in-flight work is ever destroyed. Eviction is oldest-first by `updated_at`,
  so the task most likely to be polled (the newest) is the last to go.
  A caller that `tasks/get`s an evicted task receives `ErrNotFound`
  ("task not found", indistinguishable from absence — the existing owner
  rule, `store.go:216`), which is the pre-existing semantic for a missing
  task, not a new error shape.
- **Why 1,000.** Disk bound: 1,000 × ~10 KB pessimistic ≈ 10 MB per key
  (≈ 2–6 MB at typical sizes) — negligible per key, and the aggregate stays
  bounded by the number of keys. Usability: an agent polling recent tasks
  never needs 1,000 tasks of history; `tasks/get` is per-task, and no list
  endpoint exists to page through them. Rate interplay: at 1 task/sec a key
  hits the cap in ~17 minutes, so under heavy/abusive load the cap — not the
  7-day window — is the binding constraint, which is the intended division
  of labour (§0). A smaller cap (e.g. 100) would evict tasks an operator
  might still be debugging within the same day; a larger cap (e.g. 10,000)
  would let one key hold ~100 MB and weaken the safety net for no benefit.
- **The reject corner case is real but narrow.** All-1,000-non-terminal
  requires 1,000 concurrent in-flight tasks for one key (each `message/send`
  runs synchronously through the router, `internal/server/a2a.go:508`; each
  `message/stream` holds a goroutine). That is a runaway-client signature,
  and rejecting it with a named error is the correct signal — evicting live
  work to admit more live work would be worse. The error must name the cap:
  `a2a: task limit exceeded for this key (1000 tasks, all non-terminal)`.
  HTTP maps it to JSON-RPC `-32603` (the existing code for create failures,
  `internal/server/a2a.go:514`–`518`), because `-32602` is invalid-params
  and there is no standard resource-exhausted code in this endpoint's
  existing mapping — do not invent a new code.

**Enforcement point — the gate-bypass interaction (hard requirement).**
The cap MUST be enforced in `Store.Create` (`internal/a2a/store.go:180`),
in the same `BEGIN IMMEDIATE` transaction as the insert — NOT only in the
HTTP handler. Today the only callers of `Service.CreateTask` are the two
HTTP handlers (`internal/server/a2a.go:514` and `:558`, verified by grep:
no other `CreateTask` caller exists in `internal/` or `cmd/`), but the
bug-hunt gate-bypass finding is exactly this pattern: a check that lives
only in one caller path is bypassed the moment a second caller path appears
(a future MCP tool, batch caller, or test harness calling `Service`
directly). The store is the single choke point — every creation flows
through `Store.Create` — so the count-check + evict + insert must be atomic
there: `SELECT COUNT(*) ... WHERE owner = ?`, evict oldest terminal, then
insert, all in one transaction under the existing `_txlock=immediate`
single-writer lock, so two racing creates cannot both pass the count.
`Service.CreateTask` (`service.go:37`) stays a pass-through; it adds no
second enforcement (one enforcement point, in the store). The HTTP layer
only translates `ErrTaskLimitExceeded` into the `-32603` error — it enforces
nothing.

**Side effect on the unbounded reads (§0).** Once the cap is enforced, no
owner can ever hold more than 1,000 tasks, so `ListByOwner` returns at most
1,000 rows and `Events` is bounded per task by its transition count (≤ 4
events: creation + ≤ 3 transitions — the transition graph in `a2a.go`
permits at most submitted → working → terminal). The reads stay
`LIMIT`-less in this slice (adding `LIMIT`/`OFFSET` is a separate API
decision), but their worst case is now bounded by construction. Say this in
the builder's test: create 1,001 tasks, assert `ListTasks` returns ≤ 1,000.

## 4. Config surface

**Decision: keep `A2AEnabled` as the master switch; add one pointer
sub-struct `A2A *A2AConfig` following the `Cache`/`Responses` pattern.
Retention and pruning are ON by default whenever A2A is enabled.**

```jsonc
// config.json — new keys, all optional, all under "a2a":
{
  "a2a_enabled": true,              // existing master switch, default false
  "a2a": {
    "retention_days": 7,            // default 7 (0 = default 7)
    "max_tasks_per_key": 1000,      // default 1000 (0 = default 1000)
    "prune_enabled": true           // default true (false = no retention prune)
  }
}
```

Exact keys and defaults:

| JSON key | Go field | Type | Default | Meaning of 0 / absent |
|---|---|---|---|---|
| `a2a.retention_days` | `A2AConfig.RetentionDays int` | int | **7** | 0 / absent = default 7 (the `Cache.SemanticThreshold` 0-means-default pattern, `config.go:788`–`798`). Negative = `Validate` error, never clamped. |
| `a2a.max_tasks_per_key` | `A2AConfig.MaxTasksPerKey int` | int | **1000** | 0 / absent = default 1000. Negative = `Validate` error. Any positive value is accepted (an operator who sets 1 gets exactly the reject-corner behaviour of §3 — their choice, documented). |
| `a2a.prune_enabled` | `A2AConfig.PruneEnabled *bool` | *bool | **true** | Pointer so "absent" (nil) is distinguishable from explicit `false` — a plain `bool` cannot tell "operator disabled it" from "operator said nothing", and the default must be ON. Absent/nil = true. |

Go shape (in `internal/config/config.go`, next to `Cache`/`Responses`):

```go
// A2AConfig tunes A2A task retention (M9.8 follow-up). Nil means
// defaults: 7-day retention, 1,000 tasks per key, pruning on.
// A pointer so omitempty drops the key entirely when unset.
A2A *A2AConfig `json:"a2a,omitempty"`

type A2AConfig struct {
    RetentionDays   int   `json:"retention_days,omitempty"`
    MaxTasksPerKey  int   `json:"max_tasks_per_key,omitempty"`
    PruneEnabled    *bool `json:"prune_enabled,omitempty"`
}
```

Accessors following `CacheConfig.EffectiveSemanticThreshold`
(`config.go:806`): `EffectiveRetentionDays()` and
`EffectiveMaxTasksPerKey()` on `*A2AConfig` with nil-receiver = default,
and `EffectivePruneEnabled()` returning true for nil receiver or nil
`PruneEnabled`. `Validate()` gains the two negativity checks with the
field named, in the style of `responses.session_token_cap`
(`config.go:1320`–`1324`): e.g.
`config: a2a.retention_days must not be negative (got %d)`.
`DefaultConfig` does NOT set `A2A` (nil = defaults), exactly as it does
not set `Cache`/`Responses` — the defaults live in the accessors, not in
the literal.

**Prune interval is NOT a config key** (§2): fixed `1h` constant. The
operator knob is the retention window, not the sweep frequency; an hourly
sweep bounds overshoot to ≤ 1 hour of tasks, which is negligible against a
7-day window.

**On by default? Yes — whenever A2A is enabled.** `A2AEnabled` (default
`false`) is already the opt-in that gates the whole endpoint and the
creation of `tasks.db`. Once an operator has deliberately enabled A2A,
retention ON is the safe default: pruning OFF by default would silently
grow the disk for every enabled deployment. The operator can still turn
pruning off explicitly (`prune_enabled: false`) — e.g. for a short forensic
investigation — but that is an explicit, visible choice in `config.json`,
not a silent default.

**What the cap's off-switch is NOT.** There is no `max_tasks_per_key: 0 =
unlimited` escape hatch and no way to disable cap eviction: 0 means the
1,000 default, and eviction is the safety net that makes the prune job's
failure mode bounded (§2). An operator who wants a bigger cap sets a bigger
number; nobody gets to re-open unbounded growth by omission.

## 5. Deliberately NOT built

### No task-delete API

No `tasks/delete` JSON-RPC method, no `DELETE` endpoint, no per-task delete
in the store beyond what the prune itself does internally. Reasons:

- **No product need.** Retention (§1) + cap eviction (§3) already remove
  every task that needs removing, automatically. The only caller-visible
  operations are create, poll (`tasks/get`), and cancel — the exact method
  set the sector baseline ships (`message/send`, `message/stream`, `tasks/get`,
  `tasks/cancel`; see `internal/server/a2a.go:1`–`20`). A delete method
  would be new protocol surface with no the sector baseline counterpart and no caller
  asking for it.
- **A delete API would weaken the owner-isolation story.** Every existing
  method is owner-scoped with "wrong owner = not found"
  (`store.go:216`–`218`). A delete would need the same treatment, plus a
  decision about deleting non-terminal tasks (aborting in-flight router
  work — the cancel path already owns that, `internal/server/a2a.go:665`).
  That is a product decision, not a retention prerequisite.
- **The reject-corner in §3 does not need it.** Eviction, not manual
  deletion, is the recovery path for a capped key.

### No operator list-all endpoint

No `tasks/list` method, no `GET /api/a2a/tasks` operator endpoint, no
list-all in the store. Reasons:

- **It would violate the package's central invariant.** `a2a.go` package
  doc: "there is NO ownerless access … there is no list-all"; `store.go:234`:
  "There is no list-all: every list is scoped to exactly one owner."
  An operator list-all would be the first cross-owner read of task content
  in the codebase, and pruning does not need it — the prune is a SQL
  predicate over `state` + `updated_at`, not a read-then-delete loop.
- **Pruning visibility does not need it.** What the operator needs to know
  is (a) how many tasks exist per state and (b) that pruning is running and
  removing rows. (a) already exists: `GET /api/a2a/status`
  (`internal/server/a2a_status.go`) returns `tasks_by_state` via the
  operator-only aggregate `CountsByState` — which deliberately contains no
  task content, ids, or owners. (b) is a log line per prune run (§2).
  **The status response shape is NOT changed in this slice** — its header
  warns "The dashboard codes against this exact response shape — do not
  rename fields" (`a2a_status.go:1`–`12`), and an additive field is still a
  dashboard contract change. If operators later ask for prune counters in
  the dashboard, that is a follow-up slice with its own dashboard work.
- **If a list endpoint is ever wanted, it is a separate design** (owner
  scoping, pagination — `ListByOwner` has no `LIMIT`/`OFFSET`, §0 — and a
  product decision about whether gateway-key callers may list their own
  tasks at all). This slice must not smuggle it in as "pruning visibility".

### Also not built

- No `VACUUM` / `auto_vacuum` change: SQLite `DELETE` frees pages that new
  inserts reuse, so file size plateaus at the steady state instead of
  growing; a `VACUUM` would hold the single-writer lock for a full rewrite
  and is not worth it. If the file is observed to stay large after pruning
  (e.g. after a one-off abuse spike), a manual `VACUUM` by the operator on
  a stopped router is the documented recourse — not an automatic job.
- No retention for non-terminal tasks (§1), no stuck-task reaping (§7c).
- No per-owner retention overrides and no retention for the ledger, cache,
  or logs — this slice is A2A tasks only. (The ledger's reservation TTL is
  a different mechanism for a different lifetime; §0.)

## 6. Builder slice (execute in order, no re-deciding)

### Step 1 — Config (`internal/config/config.go` + test)

- Add `A2A *A2AConfig` field to `Config` (next to `Cache`/`Responses`,
  `config.go:119`–`135`) and the `A2AConfig` struct with the three fields
  and `json` tags from §4.
- Add `EffectiveRetentionDays()`, `EffectiveMaxTasksPerKey()`,
  `EffectivePruneEnabled()` (nil-receiver = defaults; 0 = default).
- Add `Validate()` checks: negative `retention_days` / `max_tasks_per_key`
  rejected with the field named; no clamping.
- `DefaultConfig` unchanged (leaves `A2A` nil).
- Test: `internal/config/a2a_config_test.go` — nil config gives 7/1000/true;
  explicit values round-trip through JSON; negative values fail `Validate`;
  explicit `prune_enabled: false` parses and is honoured.

### Step 2 — Store (`internal/a2a/store.go` + test)

- Add to the `schema` const a prune index:
  `CREATE INDEX IF NOT EXISTS idx_a2a_tasks_state_updated ON
  a2a_tasks(state, updated_at)` — the prune predicate is cross-owner on
  `(state, updated_at)`; the existing index is `(owner, created_at)`
  (`store.go:40`) and does not serve it.
- Add `type Limits struct { RetentionDays int; MaxTasksPerKey int }` (or
  equivalent) in `internal/a2a`, plus `OpenWithLimits(dataDir string,
  lim Limits) (*Store, error)`; `Open` delegates with defaults
  (7 days, 1,000). Store the resolved limits on `Store`. Zero values in
  `Limits` mean defaults (same 0-means-default rule as §4).
- Add `func (s *Store) PruneExpired(now time.Time) (tasks, events int,
  error)`: one `BEGIN IMMEDIATE` transaction; cutoff =
  `now.AddDate(0, 0, -RetentionDays)`; delete events first, then tasks,
  both with `state IN ('completed','failed','cancelled') AND updated_at <
  ?` (cutoff formatted RFC3339Nano, the stored format — `store.go:180`).
  Events-first is mandatory: no `ON DELETE CASCADE` (§0). Return both
  counts. Never touch `submitted`/`working` rows.
- Add `var ErrTaskLimitExceeded = errors.New("a2a: task limit exceeded")`.
- Modify `Create` (`store.go:180`): in the same transaction, after the
  insert-statement preparation and before/around the insert — order:
  (a) `SELECT COUNT(*) FROM a2a_tasks WHERE owner = ?`; (b) if count >=
  `MaxTasksPerKey`, evict oldest terminal tasks for that owner
  (`DELETE ... WHERE id IN (SELECT id FROM a2a_tasks WHERE owner = ? AND
  state IN (terminal) ORDER BY updated_at ASC, id ASC LIMIT ?)`, events
  first, same transaction) until count < cap; (c) if no terminal task
  exists to evict, roll back and return `ErrTaskLimitExceeded` (wrapped
  with the cap number in the message — see §3); (d) otherwise insert the
  task + creation event as today and commit. The count, evict, and insert
  MUST be one transaction under the existing `_txlock=immediate` lock so
  concurrent creates serialise (§3).
- Tests (`internal/a2a/a2a_test.go`, following the existing
  `Open(t.TempDir())` pattern, `a2a_test.go:25`):
  1. **Prune test (required):** create tasks, drive them to terminal
     states via `Transition`, backdate a subset's `updated_at` (test-only
     SQL `UPDATE` — the store has no backdate API and must not gain one),
     call `PruneExpired`, assert: old terminal tasks AND their events are
     gone (`Get` → `ErrNotFound`, `Events` → `ErrNotFound`), recent
     terminal tasks survive, all `submitted`/`working` tasks survive
     however old, counts returned match.
  2. **Cap-eviction test (required):** open a store with a small test cap
     (e.g. `MaxTasksPerKey: 3` via `OpenWithLimits`), create 3 terminal
     tasks + 1 more, assert the oldest terminal task is gone
     (`Get` → `ErrNotFound`), the 3 newest survive, and `ListTasks`
     returns ≤ 3 rows.
  3. **Cap-reject test (required):** with cap 2, create 2 tasks and leave
     both non-terminal, assert the 3rd `Create` returns
     `ErrTaskLimitExceeded` and the first two are untouched.
  4. **Race test (recommended):** N concurrent `Create`s against a small
     cap assert the final per-owner count never exceeds the cap (mirrors
     `TestTransitionConcurrentRace`, `a2a_test.go:721`).

### Step 3 — Prune loop (`internal/server/a2a_prune.go`, new file, + wiring)

- New file `internal/server/a2a_prune.go`: `runA2APruneLoop(ctx
  context.Context, store *a2a.Store, pruneEnabled bool, log *log.Logger)`
  (or the server's existing logger type — re-verify at build time):
  run `PruneExpired(time.Now().UTC())` immediately, then on a
  `time.Ticker` every `time.Hour`; log one line per run with tasks/events
  removed and duration, or the error; never panic, never return the error
  to a caller; stop when `ctx` is done. Skip entirely when
  `prune_enabled` is false (log once at startup: pruning disabled by
  config — so the operator can see the choice).
- Wiring in `a2aService` (`internal/server/a2a.go:83`): after the first
  successful open, open via `a2a.OpenWithLimits` with the effective config
  values (`cfg.A2A.Effective*()`), and start the loop exactly once
  (guard field next to `a2aMu`/`a2aSvc`/`a2aCancels`,
  `internal/server/server.go:716`–`723`). The loop is NOT started when
  A2A is disabled — a disabled endpoint still never creates `tasks.db`.
- Test: `internal/server/a2a_prune_test.go` — loop with a cancellable
  context and a short test interval prunes an old terminal task and stops
  cleanly on cancel; `a2aService` called twice starts exactly one loop
  (assert via a counter or by log inspection — builder's choice, but the
  exactly-once property must have a test).

### Step 4 — HTTP error mapping (`internal/server/a2a.go`, no new methods)

- In `handleA2ASend` and `handleA2AStream` (the two `CreateTask` call
  sites, `:514` and `:558`), map `errors.Is(err, a2a.ErrTaskLimitExceeded)`
  to `writeA2AError(..., -32603, <message naming the cap>, ...)` — the
  existing create-failure code (§3). No other handler changes; no new
  JSON-RPC method; `tasks/get` and `tasks/cancel` are untouched.
- Test: HTTP-level test (existing `a2a` server-test pattern) asserting a
  capped key's `message/send` returns `-32603` with the cap named when
  all tasks are non-terminal.

### Step 5 — Docs (no code)

- Update the A2A section of `docs/API_REFERENCE.md` (the doc that
  documents the `/a2a` methods — re-verify the section at build time) with:
  the 7-day terminal retention, the 1,000-per-key cap with oldest-terminal
  eviction, the three `a2a.*` config keys, and the fact that an evicted
  task answers `tasks/get` as "not found". One paragraph + the config
  snippet from §4; do not write a new doc file.
- Do NOT touch `internal plan`, `internal parity tracker`, or `CHECKLIST.md` — the
  coordinator flips rows after reviewing evidence.

### Definition-of-Done (all must hold; no partial credit)

1. `go test ./internal/a2a/... ./internal/config/... ./internal/server/...`
   green (run the full packages, not just the new tests).
2. The §6 Step-2 prune test proves old terminal tasks + their events are
   pruned, recent terminal and all non-terminal tasks survive, and the
   returned counts are exact.
3. The §6 Step-2 cap tests prove oldest-terminal eviction at the cap AND
   the all-non-terminal reject with `ErrTaskLimitExceeded`.
4. The Step-3 test proves the loop starts exactly once from `a2aService`
   and stops on context cancel.
5. A manual check (builder's own run, recorded in the handoff): enable
   A2A, create tasks, confirm `tasks.db` shrinks-or-plateaus across two
   prune runs and `GET /api/a2a/status` counts drop accordingly; confirm
   a disabled endpoint still creates no `tasks.db`.
6. No new HTTP method, no new endpoint, no `LIMIT`/`OFFSET` change, no
   `VACUUM`, no delete API, no list-all — verified by `git diff --stat`
   touching only the files in §6 Steps 1–4 + the one docs paragraph.
7. Every new error message names the limit/field involved; no error
   message, log line, or doc claims a measured task size — the §0 figures
   are estimates and stay labelled as such.

## 7. What this design could not decide (needs a Raunak product call)

(a) **Is 7 days the right retention for Raunak's debugging/audit needs?**
    The design recommends 7 days on operator-expectation + disk-math
    grounds (§1), but if Raunak wants tasks kept for a month (e.g. to
    debug a customer's agent run weeks later), the only change is the
    default `retention_days` — the mechanism is identical. Needs his call
    before the builder hard-codes the default; 30 days at 1/sec would be
    ~13 GB, so the cap (§3) would do most of the work and the window
    would mostly matter at low rates.
(b) **Is silent eviction of a terminal task acceptable?** Under §3, a
    `tasks/get` for an evicted task answers "not found" with no indication
    it ever existed. That is the pre-existing semantic for missing tasks,
    but an agent that polls a task it created an hour ago could see it
    vanish if its key is churning fast. If Raunak wants a distinct signal
    (e.g. a `410`-style "task pruned" answer), that is a protocol change
    needing his call — the design deliberately does not invent one.
(c) **Stuck non-terminal tasks: reap, fail, or leave forever?** A `working`
    task older than hours is stuck (§1), and a crashed process can orphan
    `submitted`/`working` rows permanently — they are exempt from pruning
    and they occupy cap slots (up to the §3 reject corner). What "stuck"
    means (age threshold, whether to mark `failed` with an honest error or
    leave the row as evidence) is a product decision, not a retention
    decision. Left for a future slice; the builder must NOT add reaping
    to this one.
(d) **How many gateway keys will exist in practice?** The cap bounds disk
    per key (~10 MB at the pessimistic figure), so the aggregate worst case
    is `keys × 10 MB`. With a handful of keys this is trivial; with
    thousands of keys it is not. The design could not verify the expected
    key count from the code (no deployment inventory exists in the repo),
    so the aggregate bound is stated per key and the operator is expected
    to know their key count. If Raunak expects thousands of keys, the cap
    default (1,000) should be revisited downward.
