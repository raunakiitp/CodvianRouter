package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

// M5.6: GET /v1/me/sessions — the calling key's agent sessions.
//
// Two sources, one list (AK-3): Responses chains AND the
// ledger-backed coding-agent rollup (Ledger.AgentSessions).
// A session is a Responses chain rooted at a response with no
// previous_response_id, plus ALL of its descendants (a root that
// branches counts as one session; turns/tokens/cost sum the whole
// tree, never a single branch). A ledger session is one row of
// the agent_sessions rollup: one coding-agent session for the
// key — per client session id when the usage row carries one,
// else per key + project inside a 30-minute idle window —
// written in the same transaction as its usage row
// (RecordUsage / AsyncWriter.commitBatch), so live chat traffic
// populates it. Only the caller's own sessions are ever listed
// in either source — records carry the gateway key that opened
// them (KeyID, stored since M5.6); pre-M5.6 records have no key
// and are listed for nobody, rather than attributed to the wrong
// caller. The two sources are merged newest-first and capped at
// 100 rows total; Row.Source says which source a row came from
// ("responses" or "ledger").
//
// Costs differ by source, honestly: a Responses row's
// ListPriceUSD is the list-price cost (each turn repriced at the
// catalog's current list prices, so it differs from me/status
// when prices changed in between — the same rule the reference router
// documents for its /v1/me/sessions costUsd). A ledger row
// reports CostUSD instead: the cost as recorded on its usage
// rows (record-time cost, not a reprice), with ListPriceUSD left
// nil (unknown, never $0) and ListPriceComplete false — a ledger
// row is never repriced. UnpricedCount counts the turns (or
// requests) that used tokens but had no known catalog price.
// A ledger row's Status is "failed" only when every request
// errored, else "completed"; its per-request error count is in
// Errors. Turns on a ledger row is its request count.
//
// Attribution remainder (AK-3, honest): no request field
// carries a client session id, project, or git branch today —
// the live recording path (ledgerAdapter in
// cmd/codvian-router/main.go) records usage rows with no
// attribution, so explicit client-session rows never form and
// project/branch stay empty until a slice wires a carrier
// through (see internal gap tracker §14 AK-3). The idle-window
// grouping still works (one session per key per 30-minute
// window), and the moment a carrier exists the same rollup
// groups by it with no handler change.

const meSessionsMaxRows = 100

// meSessionRow is one session of GET /v1/me/sessions.
// Source is "responses" (a Responses chain) or "ledger" (one
// row of the agent_sessions rollup). The ledger-only fields
// (Requests, Errors, CostUSD, ClientSessionID, ProjectID,
// GitBranch) are omitted on Responses rows. On a ledger row,
// Turns is the request count, ListPriceUSD is nil (no reprice
// is performed — see the file header), and CostUSD is the
// record-time cost.
type meSessionRow struct {
	ID                string    `json:"id"`
	CreatedAt         time.Time `json:"created_at"`
	Model             string    `json:"model"`
	Provider          string    `json:"provider,omitempty"`
	Status            string    `json:"status"`
	Turns             int       `json:"turns"`
	InputTokens       int       `json:"input_tokens"`
	OutputTokens      int       `json:"output_tokens"`
	ListPriceUSD      *float64  `json:"list_price_usd,omitempty"`
	ListPriceComplete bool      `json:"list_price_complete"`
	UnpricedCount     int       `json:"unpriced_count"`
	Source            string    `json:"source"`
	Requests          int       `json:"requests,omitempty"`
	Errors            int       `json:"errors,omitempty"`
	CostUSD           *float64  `json:"cost_usd,omitempty"`
	ClientSessionID   string    `json:"client_session_id,omitempty"`
	ProjectID         string    `json:"project_id,omitempty"`
	GitBranch         string    `json:"git_branch,omitempty"`
}

// meSessionsResponse is the GET /v1/me/sessions payload (OpenAI
// list shape).
type meSessionsResponse struct {
	Object string         `json:"object"`
	Data   []meSessionRow `json:"data"`
	Total  int            `json:"total"`
}

func (s *Server) handleMeSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		writeError(w, http.StatusUnauthorized, "a gateway key is required to identify the caller")
		return
	}
	out := meSessionsResponse{Object: "list", Data: []meSessionRow{}}
	// Responses chains (M5.6, unchanged).
	if s.responses != nil {
		prices := s.catalogListPrices(r.Context())
		recs := s.responses.Snapshot()
		var roots []responses.Record
		for _, rec := range recs {
			if rec.PreviousID == "" && rec.KeyID == keyID {
				roots = append(roots, rec)
			}
		}
		// Newest first.
		sort.Slice(roots, func(a, b int) bool {
			return roots[a].CreatedAt > roots[b].CreatedAt
		})
		for _, root := range roots {
			out.Data = append(out.Data, s.meSessionRow(root, prices))
		}
	}
	// Ledger agent sessions (AK-3): the coding-agent rollup,
	// alongside the Responses chains. A ledger read failure
	// logs and serves the Responses rows alone — the endpoint
	// never fails outright for a ledger read the caller cannot
	// fix (same fail-open rule me/status follows); the log line
	// is the signal, not a silent zero.
	if s.ledger != nil {
		if sessions, err := s.ledger.AgentSessions(r.Context(), keyID, meSessionsMaxRows); err != nil {
			s.logger.Printf("me/sessions: agent sessions failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		} else {
			for _, sess := range sessions {
				out.Data = append(out.Data, meSessionRowFromLedger(sess))
			}
		}
	}
	// Merged newest-first (CreatedAt, then id for a stable
	// order on ties), capped at meSessionsMaxRows total.
	sort.Slice(out.Data, func(a, b int) bool {
		if !out.Data[a].CreatedAt.Equal(out.Data[b].CreatedAt) {
			return out.Data[a].CreatedAt.After(out.Data[b].CreatedAt)
		}
		return out.Data[a].ID < out.Data[b].ID
	})
	if len(out.Data) > meSessionsMaxRows {
		out.Data = out.Data[:meSessionsMaxRows]
	}
	out.Total = len(out.Data)
	writeJSON(w, http.StatusOK, out)
}

// meSessionRowFromLedger maps one agent_sessions rollup row onto
// the list row shape. Turns is the request count; Status is
// "failed" only when every request errored, else "completed"
// (the per-request error count is in Errors). ListPriceUSD stays
// nil: the rollup cost is record-time cost, reported as CostUSD,
// never a list-price reprice — so ListPriceComplete is false and
// UnpricedCount is the rollup's own count of requests that spent
// tokens with no known price.
func meSessionRowFromLedger(s ledger.AgentSession) meSessionRow {
	status := responses.StatusCompleted
	if s.Requests > 0 && s.Errors >= s.Requests {
		status = responses.StatusFailed
	}
	cost := s.CostUSD
	return meSessionRow{
		ID:                s.ID,
		CreatedAt:         s.CreatedAt,
		Model:             s.Model,
		Provider:          s.Provider,
		Status:            status,
		Turns:             s.Requests,
		InputTokens:       s.InputTokens,
		OutputTokens:      s.OutputTokens,
		ListPriceComplete: false,
		UnpricedCount:     s.UnpricedCount,
		Source:            "ledger",
		Requests:          s.Requests,
		Errors:            s.Errors,
		CostUSD:           &cost,
		ClientSessionID:   s.ClientSessionID,
		ProjectID:         s.ProjectID,
		GitBranch:         s.GitBranch,
	}
}

// meSessionRow aggregates one session tree: the root plus every
// descendant reachable through Children (branches included).
// Status is the newest record's status; the list price sums each
// turn at the catalog's current prices.
func (s *Server) meSessionRow(root responses.Record, prices map[string][2]float64) meSessionRow {
	row := meSessionRow{
		ID:                root.ID,
		CreatedAt:         time.Unix(root.CreatedAt, 0).UTC(),
		Model:             root.Model,
		Provider:          root.ProviderID,
		Status:            root.Status,
		ListPriceComplete: true,
		Source:            "responses",
	}
	// Depth-first walk of the whole tree (a branched root's
	// children are all part of the session). The store is
	// append-only in practice and PreviousID always points at
	// an older record, so the walk terminates; the seen set is
	// a belt-and-braces guard against a corrupt cycle.
	seen := map[string]bool{}
	stack := []string{root.ID}
	var newest int64 = root.CreatedAt
	var list float64
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		rec, ok := s.responses.Get(id)
		if !ok {
			continue
		}
		row.Turns++
		row.InputTokens += rec.TurnInputTokens
		row.OutputTokens += rec.TurnOutputTokens
		if rec.CreatedAt > newest {
			newest = rec.CreatedAt
			row.Status = rec.Status
		}
		if p, ok := prices[rec.Model]; ok {
			list += (float64(rec.TurnInputTokens)*p[0] + float64(rec.TurnOutputTokens)*p[1]) / 1e6
		} else {
			row.UnpricedCount++
			row.ListPriceComplete = false
		}
		stack = append(stack, s.responses.Children(id)...)
	}
	if row.Turns > 0 {
		row.ListPriceUSD = &list
	}
	return row
}
