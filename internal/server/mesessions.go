package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/responses"
)

// M5.6: GET /v1/me/sessions — the calling key's agent sessions.
// A session is a Responses chain rooted at a response with no
// previous_response_id, plus ALL of its descendants (a root that
// branches counts as one session; turns/tokens/cost sum the whole
// tree, never a single branch). Only the caller's own sessions
// are ever listed — records carry the gateway key that opened
// them (KeyID, stored since M5.6); pre-M5.6 records have no key
// and are listed for nobody, rather than attributed to the wrong
// caller. Session cost is the list-price cost (see
// meStatusUsageWindow): each turn repriced at the catalog's
// current list prices, so it differs from me/status when prices
// changed in between — the same rule OmniRoute documents for its
// /v1/me/sessions costUsd. UnpricedCount counts the turns that
// used tokens but had no known catalog price.

const meSessionsMaxRows = 100

// meSessionRow is one session of GET /v1/me/sessions.
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
	if s.responses == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
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
	if len(roots) > meSessionsMaxRows {
		roots = roots[:meSessionsMaxRows]
	}
	for _, root := range roots {
		out.Data = append(out.Data, s.meSessionRow(root, prices))
	}
	out.Total = len(out.Data)
	writeJSON(w, http.StatusOK, out)
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
