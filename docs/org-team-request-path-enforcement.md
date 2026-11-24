# Organization/Team Request-Path Enforcement (P1.6)

## Why this block exists

Organization and team management APIs already existed. P1.6's requirement
is that a declared policy **changes actual requests**: a team's allowlist
must refuse a request before provider dispatch, on every dialect, with no
alternate API as a bypass. Management endpoints alone do not satisfy it.

## Audit: what already existed (evidence: source, not reports)

| Piece | Where | Status |
| --- | --- | --- |
| Org/team/key-binding storage, roles, audit log | `internal/org/org.go` | IMPLEMENTED |
| Org management API (`/api/org`, `/api/org/teams`, bind/unbind, audit, analytics) | `internal/server/org_api.go`, `teams_api.go` | IMPLEMENTED |
| Ledger team cost centers + rolling budget windows + per-team usage attribution | `internal/ledger/team.go` | IMPLEMENTED |
| Team budget gate before dispatch | `internal/server/teams.go` `checkTeamBudget` | IMPLEMENTED |
| Team model/strategy allowlist | `internal/server/teams.go` `checkTeamPolicy` | PARTIALLY IMPLEMENTED — called on chat completions only |
| Key → team resolution on the request path | `Store.TeamForKey` + `Ledger.ActiveTeamForKey` | IMPLEMENTED |
| Denial audit logging | — | NOT IMPLEMENTED (audit covered management ops only) |
| Organization budget enforcement | `Organization.DefaultBudgetUSD` existed but was never read on a request | NOT IMPLEMENTED |

## Confirmed gaps and what was implemented

1. **Single policy gate, every dialect.** `Server.enforceTeamPolicy` /
   `evaluateTeamPolicy` is now the one decision path. It is called before
   dispatch on: `POST /v1/chat/completions`, `POST /v1/messages`,
   `POST /v1/responses`, `POST /v1/completions`,
   `POST /v1beta/models/{rest...}` (Gemini native), `POST /v1/embeddings`,
   `POST /v1/images/generations|edits`, `POST /v1/audio/*`,
   the media verification surfaces, **A2A task admission**, and **each
   item of a batch** (via the key-addressed core, so a batch worker with
   no `http.Request` enforces the same rule; a denied item becomes its own
   error line and the rest of the batch still runs).
2. **Strategy allowlist** read from the same field the engine honours
   (`routing.sort`), through one helper (`routingStrategy`).
3. **Organization budget** enforced: the org's `DefaultBudgetUSD`
   compared against the estimated LIST cost of all its teams' usage over
   the org's budget window (current calendar month, UTC). Unpriced usage
   in the window fails closed, exactly like the team gate. Refusal is
   `400 org_budget`, distinct from the team gate's `400 team_budget`.
4. **Denial auditing.** Every refusal appends `request.denied` to the
   store's append-only audit log with the key, the team and the reason,
   and persists it — a restart keeps the record.
5. **Org resolution helpers** on the store: `OrgOf(teamID)`,
   `OrgByID(id)`, `TeamsForOrg(orgID)` (all return copies; no shared
   mutable state).

## Guarantees and their tests

| Guarantee | Test |
| --- | --- |
| A forbidden model is refused on chat, Messages, Responses, legacy completions, Gemini, embeddings, images and audio — with **zero provider calls** | `TestP16TeamModelAllowlistRefusesEveryDialect` |
| The same team still serves the model it allows | `TestP16TeamModelAllowlistAllowsListedModel` |
| A forbidden strategy is refused before dispatch | `TestP16TeamStrategyAllowlistRefusesChat` |
| Streaming and non-streaming agree (the gate precedes the branch) | `TestP16StreamingAndNonStreamingAgree` |
| Denials are audited and survive reopening the store | `TestP16DenialIsAuditedAndPersists`, `TestRecordDenialIsAuditedAndPersisted` |
| Cross-team isolation: another team's allowlist cannot restrict this key; unbinding restores unrestricted behaviour | `TestP16OtherTeamAndUnboundKeyAreUnaffected` |
| Org budget refuses before dispatch (`org_budget`) | `TestP16OrgBudgetRefused` |
| Unpriced usage cannot prove an org is under budget (fail closed) | `TestP16OrgBudgetUnpricedFailsClosed` |
| The batch worker path enforces the same rule | `TestP16BatchItemPolicyDenied` |
| Org resolution is scoped and sorted; a team cannot exist under an unknown org | `TestOrgOfResolvesTeamToOrg`, `TestTeamsForOrgIsScopedAndSorted` |

## Deliberate limits (not silently claimed as done)

- **Team budget** remains the ledger-based, windowed gate (`400
  team_budget`, estimated list cost, unpriced fails closed); it runs
  after the policy gate. Both gates are pre-dispatch.
- **Org budget window** is the current calendar month in UTC, because an
  organization budget is a monthly cap; it deliberately does not borrow
  the rolling per-team windows, which would mix periods into one figure.
- **Management APIs** are guarded by the gateway-key scopes in the route
  table (`admin`), not per-member RBAC: the store records member roles
  (`RoleOf`) and role-gated mutators, but there is no per-request member
  identity on the dashboard surface today. This is a limitation, not a
  claim of member-level isolation.
- **No fabricated enforcement**: a key with no team binding, or a
  deployment with no org store, is unrestricted — no policy was declared,
  so none is invented.
