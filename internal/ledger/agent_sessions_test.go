// AK-3 — coding-agent session rollup (GAP-TRACK §14): RecordUsage
// folds chat rows into agent_sessions in the same transaction as
// the usage row, so usage and session counters move together or
// neither does. One row per gateway key + client session id when
// the usage row carries one, otherwise one row per gateway key +
// project inside a 30-minute idle window. Non-chat endpoints are
// skipped; a failed attempt counts as a request and an error.
package ledger

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestAgentSessionsRollup(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	record := func(u Usage) {
		t.Helper()
		if _, err := l.RecordUsage(ctx, u, prices); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	// (a) Same client session id rolls up into one row: requests,
	// errors and tokens sum; cost is the recorded (priced) cost.
	record(Usage{GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 100, CompletionTokens: 50, SplitKnown: true, OK: true}.
		WithAttribution("sess-1", "proj-a", "cursor"))
	record(Usage{GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: false}.
		WithAttribution("sess-1", "proj-a", "cursor"))
	sessions, err := l.AgentSessions(ctx, "key-1", 100)
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("client-session rollup: %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.ClientSessionID != "sess-1" || s.ProjectID != "proj-a" || s.ClientID != "cursor" {
		t.Fatalf("session identity = %q/%q/%q, want sess-1/proj-a/cursor", s.ClientSessionID, s.ProjectID, s.ClientID)
	}
	if s.Requests != 2 || s.Errors != 1 || s.InputTokens != 110 || s.OutputTokens != 55 {
		t.Fatalf("session counters = req %d err %d in %d out %d, want 2/1/110/55", s.Requests, s.Errors, s.InputTokens, s.OutputTokens)
	}
	// (100*0.001 + 50*0.002) + (10*0.001 + 5*0.002) = 0.22.
	if math.Abs(s.CostUSD-0.22) > 1e-9 {
		t.Fatalf("session cost = %v, want 0.22", s.CostUSD)
	}
	if s.UnpricedCount != 0 {
		t.Fatalf("priced session unpriced_count = %d, want 0", s.UnpricedCount)
	}

	// (b) No client session id: same project inside the 30-minute
	// idle window joins one row.
	record(Usage{GatewayKeyID: "key-2", Provider: "p1", Model: "model-a",
		PromptTokens: 7, CompletionTokens: 3, SplitKnown: true, OK: true}.
		WithAttribution("", "proj-b", "cursor"))
	record(Usage{GatewayKeyID: "key-2", Provider: "p1", Model: "model-a",
		PromptTokens: 1, CompletionTokens: 1, SplitKnown: true, OK: true}.
		WithAttribution("", "proj-b", "cursor"))
	sessions, err = l.AgentSessions(ctx, "key-2", 100)
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Requests != 2 {
		t.Fatalf("idle-window rollup: %d sessions, want 1 with 2 requests", len(sessions))
	}
	if sessions[0].ClientSessionID != "" {
		t.Fatalf("idle-window session client_session_id = %q, want empty", sessions[0].ClientSessionID)
	}

	// (c) Idle window expiry: a request more than 30 minutes after
	// the session's last request starts a NEW session.
	if _, err := l.db.Exec(`UPDATE agent_sessions SET last_seen_ms = ? WHERE gateway_key_id = ?`,
		time.Now().Add(-31*time.Minute).UnixMilli(), "key-2"); err != nil {
		t.Fatalf("age session: %v", err)
	}
	record(Usage{GatewayKeyID: "key-2", Provider: "p1", Model: "model-a",
		PromptTokens: 2, CompletionTokens: 2, SplitKnown: true, OK: true}.
		WithAttribution("", "proj-b", "cursor"))
	sessions, err = l.AgentSessions(ctx, "key-2", 100)
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("after 31-min idle gap: %d sessions, want 2", len(sessions))
	}

	// (d) A non-chat endpoint is never rolled up.
	record(Usage{GatewayKeyID: "key-3", Provider: "p1", Model: "model-a",
		Endpoint: "embeddings", PromptTokens: 5, SplitKnown: true, OK: true})
	sessions, err = l.AgentSessions(ctx, "key-3", 100)
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("embeddings row: %d sessions, want 0", len(sessions))
	}

	// (e) Tokens spent with no known price count as unpriced, and
	// the session still records (a failed attempt with no tokens
	// would not — here the tokens exist, the price does not).
	record(Usage{GatewayKeyID: "key-4", Provider: "p1", Model: "unpriced-model",
		PromptTokens: 9, SplitKnown: true, OK: true}.
		WithAttribution("sess-9", "proj-c", "cursor"))
	sessions, err = l.AgentSessions(ctx, "key-4", 100)
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].UnpricedCount != 1 {
		t.Fatalf("unpriced session: %d sessions, unpriced_count %d, want 1/1",
			len(sessions), func() int {
				if len(sessions) == 1 {
					return sessions[0].UnpricedCount
				}
				return -1
			}())
	}

	// (f) Sessions are key-scoped: another key lists nothing.
	sessions, err = l.AgentSessions(ctx, "key-other", 100)
	if err != nil {
		t.Fatalf("AgentSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("other key: %d sessions, want 0", len(sessions))
	}
}
