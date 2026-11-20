// Coding-agent session rollup (GAP-TRACK §14, AK-3, research-log
// entry 523): GET /v1/me/sessions used to list only Responses
// chains. A coding agent driving /v1/chat/completions has no
// Responses chain, so its traffic never appeared as a session at
// all — no client/project/branch attribution, no grouping.
//
// The rollup lives here, in the ledger, not in the Responses
// store: the usage row and the session counters move together in
// the same transaction (RecordUsage, and the async batch writer's
// commitBatch — the live request path writes through the batch
// writer, so both write paths must upsert, or live traffic would
// silently skip the rollup). The table is SQLite, in the same
// ledger.db file as usage_events, so a restart keeps the sessions
// exactly like responses.json keeps the Responses chains — no
// restart-loss gap, no separate persistence file to drift.
//
// Keying (mirrors OmniRoute's agent_sessions, migration 229):
// one row per gateway key + client session id when the usage row
// carries one (Usage.SessionID, set from request metadata with
// Usage.WithAttribution); otherwise one row per gateway key +
// project inside a 30-minute idle window — a request that lands
// within 30 minutes of the session's last request joins that
// session, a later one starts a new session. The row id is a
// key-scoped fingerprint, "as_" + sha256(key + session), so a
// client session id always maps to the same row id, across
// restarts. An idle-window row's id folds in the session's first
// request time, so a new idle window is a new row id.
//
// A row counts requests and errors (a failed attempt is a
// request and an error; a cancelled hedge loser is neither —
// the same rule Summary.Errors follows), the token split, the
// cost as recorded on the usage row (record-time cost, not a
// reprice — GET /v1/me/sessions says so), and how many requests
// spent tokens with no known price (unpriced_count). Client,
// project and git branch are recorded where the usage row
// carries them; a later request with a non-empty value overwrites
// an empty one, never the reverse. Branch has no producer yet:
// no request field carries it today, so the column stays empty
// until a slice wires a carrier through (see the remainder in
// docs/GAP-TRACK.md §14 AK-3). Nothing here invents one.
//
// Rows with no gateway key are never rolled up: an unkeyed row
// belongs to no caller, and GET /v1/me/sessions lists only the
// caller's own sessions (the same rule the Responses chains
// follow for pre-M5.6 keyless records). Non-chat usage rows
// (embeddings, rerank, ...) are skipped too: this is the
// chat/completions rollup, and the endpoint column ("\"\" =
// chat) is what distinguishes them.
package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// agentSessionIdleWindow is the idle gap that closes a session
// for traffic with no client session id: a request more than
// this long after the session's last request starts a new
// session. 30 minutes, matching OmniRoute's agent_sessions
// idle window.
const agentSessionIdleWindow = 30 * time.Minute

// AgentSession is one row of the agent_sessions rollup: one
// coding-agent session for one gateway key. Counts only, never
// content. ClientSessionID is "" for an idle-window session
// (grouped by key + project, no client id carried); CreatedAt is
// the session's first request, LastSeen its most recent one.
type AgentSession struct {
	ID              string    `json:"id"`
	ClientSessionID string    `json:"client_session_id,omitempty"`
	ClientID        string    `json:"client_id,omitempty"`
	ProjectID       string    `json:"project_id,omitempty"`
	GitBranch       string    `json:"git_branch,omitempty"`
	Model           string    `json:"model"`
	Provider        string    `json:"provider,omitempty"`
	Requests        int       `json:"requests"`
	Errors          int       `json:"errors"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	TotalTokens     int       `json:"total_tokens"`
	CostUSD         float64   `json:"cost_usd"`
	UnpricedCount   int       `json:"unpriced_count"`
	CreatedAt       time.Time `json:"created_at"`
	LastSeen        time.Time `json:"last_seen"`
}

// agentSessionIDForClient is the stable fingerprint id for an
// explicit client session: the same key + client session id
// always maps to the same row, across restarts.
func agentSessionIDForClient(keyID, clientSessionID string) string {
	sum := sha256.Sum256([]byte(keyID + "\x00" + clientSessionID))
	return "as_" + hex.EncodeToString(sum[:])
}

// agentSessionIDForIdle is the fingerprint id for an idle-window
// session: the window's first request time is folded in, so a
// new idle window after a gap is a new row id.
func agentSessionIDForIdle(keyID, projectID string, firstSeen time.Time) string {
	sum := sha256.Sum256([]byte(keyID + "\x00idle\x00" + projectID + "\x00" + fmt.Sprint(firstSeen.UnixMilli())))
	return "as_" + hex.EncodeToString(sum[:])
}

// agentSessionError reports whether one normalized usage row is
// an error for the rollup: a failed attempt counts, a cancelled
// one (hedge loser) does not — the same rule Summary.Errors
// follows.
func agentSessionError(r normalizedRow) int {
	if r.ok == 0 && r.cancelled == 0 {
		return 1
	}
	return 0
}

// agentSessionUnpriced reports whether the row spent tokens
// with no known price: tokens recorded but price_known is 0.
// A zero-token row (a failed attempt with no usage) is not
// unpriced — there was nothing to price.
func agentSessionUnpriced(r normalizedRow) int {
	if r.priceKnown == 0 && r.promptTokens+r.completionTokens+r.totalTokens > 0 {
		return 1
	}
	return 0
}

// upsertAgentSessionTx folds one normalized usage row into the
// agent_sessions rollup, in the caller's transaction — the usage
// row's own transaction, so the two move together or neither
// does. The caller holds the tx; a nil row set (unkeyed row,
// non-chat endpoint) is a silent no-op, never an error.
func upsertAgentSessionTx(ctx context.Context, tx *sql.Tx, r normalizedRow, now time.Time) error {
	if tx == nil || r.gatewayKeyID == "" {
		return nil
	}
	// Chat only: "" is chat completions (every row recorded
	// before the endpoint column existed was chat, and the
	// adapter still leaves it empty for chat); the explicit
	// "chat_completions" kind is the same surface spelled out.
	if r.endpoint != "" && r.endpoint != "chat_completions" {
		return nil
	}
	errs := agentSessionError(r)
	unpriced := agentSessionUnpriced(r)
	nowMs := now.UnixMilli()
	if r.sessionID != "" {
		// Explicit client session: one UPSERT, no SELECT —
		// the fingerprint id is the conflict target, and the
		// counters increment in place. Latest non-empty
		// client/project/branch wins; an empty value never
		// clobbers a recorded one.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO agent_sessions
			  (id, gateway_key_id, client_session_id, client_id, project_id, git_branch,
			   model, provider, requests, errors, input_tokens, output_tokens, total_tokens,
			   cost_usd, unpriced_count, created_at_ms, last_seen_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			  client_id = COALESCE(NULLIF(excluded.client_id, ''), agent_sessions.client_id),
			  project_id = COALESCE(NULLIF(excluded.project_id, ''), agent_sessions.project_id),
			  git_branch = COALESCE(NULLIF(excluded.git_branch, ''), agent_sessions.git_branch),
			  model = excluded.model,
			  provider = excluded.provider,
			  requests = agent_sessions.requests + 1,
			  errors = agent_sessions.errors + excluded.errors,
			  input_tokens = agent_sessions.input_tokens + excluded.input_tokens,
			  output_tokens = agent_sessions.output_tokens + excluded.output_tokens,
			  total_tokens = agent_sessions.total_tokens + excluded.total_tokens,
			  cost_usd = agent_sessions.cost_usd + excluded.cost_usd,
			  unpriced_count = agent_sessions.unpriced_count + excluded.unpriced_count,
			  last_seen_ms = excluded.last_seen_ms`,
			agentSessionIDForClient(r.gatewayKeyID, r.sessionID),
			r.gatewayKeyID, r.sessionID, r.clientID, r.projectID, r.gitBranch,
			r.model, r.provider, errs, r.promptTokens, r.completionTokens, r.totalTokens,
			r.cost, unpriced, nowMs, nowMs)
		if err != nil {
			return fmt.Errorf("ledger: upsert agent session: %w", err)
		}
		return nil
	}
	// Idle window (no client session id): find the key +
	// project's latest session and join it when its last
	// request is within the window, else start a new one.
	var id string
	var lastSeenMs int64
	err := tx.QueryRowContext(ctx, `
		SELECT id, last_seen_ms FROM agent_sessions
		WHERE gateway_key_id = ? AND client_session_id = '' AND project_id = ?
		ORDER BY last_seen_ms DESC LIMIT 1`,
		r.gatewayKeyID, r.projectID).Scan(&id, &lastSeenMs)
	switch {
	case err == sql.ErrNoRows:
		// No session yet for this key + project: insert.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO agent_sessions
			  (id, gateway_key_id, client_session_id, client_id, project_id, git_branch,
			   model, provider, requests, errors, input_tokens, output_tokens, total_tokens,
			   cost_usd, unpriced_count, created_at_ms, last_seen_ms)
			VALUES (?, ?, '', ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
			agentSessionIDForIdle(r.gatewayKeyID, r.projectID, now),
			r.gatewayKeyID, r.clientID, r.projectID, r.gitBranch,
			r.model, r.provider, errs, r.promptTokens, r.completionTokens, r.totalTokens,
			r.cost, unpriced, nowMs, nowMs)
		if err != nil {
			return fmt.Errorf("ledger: insert agent session: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("ledger: find agent session: %w", err)
	}
	if now.Sub(time.UnixMilli(lastSeenMs)) <= agentSessionIdleWindow {
		_, err := tx.ExecContext(ctx, `
			UPDATE agent_sessions SET
			  client_id = COALESCE(NULLIF(?, ''), client_id),
			  git_branch = COALESCE(NULLIF(?, ''), git_branch),
			  model = ?, provider = ?,
			  requests = requests + 1,
			  errors = errors + ?,
			  input_tokens = input_tokens + ?,
			  output_tokens = output_tokens + ?,
			  total_tokens = total_tokens + ?,
			  cost_usd = cost_usd + ?,
			  unpriced_count = unpriced_count + ?,
			  last_seen_ms = ?
			WHERE id = ?`,
			r.clientID, r.gitBranch, r.model, r.provider,
			errs, r.promptTokens, r.completionTokens, r.totalTokens,
			r.cost, unpriced, nowMs, id)
		if err != nil {
			return fmt.Errorf("ledger: update agent session: %w", err)
		}
		return nil
	}
	// Idle window expired: a new session for this key + project.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO agent_sessions
		  (id, gateway_key_id, client_session_id, client_id, project_id, git_branch,
		   model, provider, requests, errors, input_tokens, output_tokens, total_tokens,
		   cost_usd, unpriced_count, created_at_ms, last_seen_ms)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agentSessionIDForIdle(r.gatewayKeyID, r.projectID, now),
		r.gatewayKeyID, r.clientID, r.projectID, r.gitBranch,
		r.model, r.provider, errs, r.promptTokens, r.completionTokens, r.totalTokens,
		r.cost, unpriced, nowMs, nowMs)
	if err != nil {
		return fmt.Errorf("ledger: insert agent session: %w", err)
	}
	return nil
}

// AgentSessions returns the gateway key's agent sessions, most
// recently active first. An empty key or a non-positive limit
// returns no rows (limit <= 0 means the default 100); a key
// with no sessions returns an empty slice, never an error. The
// caller must already have authenticated the key — this method
// trusts keyID and never lists another key's sessions.
func (l *Ledger) AgentSessions(ctx context.Context, keyID string, limit int) ([]AgentSession, error) {
	if l == nil || keyID == "" {
		return []AgentSession{}, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT id, client_session_id, client_id, project_id, git_branch,
		       model, provider, requests, errors, input_tokens, output_tokens,
		       total_tokens, cost_usd, unpriced_count, created_at_ms, last_seen_ms
		FROM agent_sessions
		WHERE gateway_key_id = ?
		ORDER BY last_seen_ms DESC
		LIMIT ?`, keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: list agent sessions: %w", err)
	}
	defer rows.Close()
	out := []AgentSession{}
	for rows.Next() {
		var s AgentSession
		var createdMs, lastSeenMs int64
		if err := rows.Scan(
			&s.ID, &s.ClientSessionID, &s.ClientID, &s.ProjectID, &s.GitBranch,
			&s.Model, &s.Provider, &s.Requests, &s.Errors, &s.InputTokens, &s.OutputTokens,
			&s.TotalTokens, &s.CostUSD, &s.UnpricedCount, &createdMs, &lastSeenMs); err != nil {
			return nil, fmt.Errorf("ledger: scan agent session: %w", err)
		}
		s.CreatedAt = time.UnixMilli(createdMs).UTC()
		s.LastSeen = time.UnixMilli(lastSeenMs).UTC()
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list agent sessions: %w", err)
	}
	return out, nil
}
