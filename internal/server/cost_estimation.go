package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// CostEstimateRequest is the request body for POST /api/cost/estimate.
type CostEstimateRequest struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

// CostEstimateResponse is the response for POST /api/cost/estimate.
type CostEstimateResponse struct {
	Model                     string         `json:"model"`
	PromptTokens              int            `json:"prompt_tokens"`
	EstimatedCompletionTokens int            `json:"estimated_completion_tokens,omitempty"`
	EstimatedCostUSD          float64        `json:"estimated_cost_usd"`
	CostKnown                 bool           `json:"cost_known"`
	PriceSource               string         `json:"price_source"` // "catalog" | "estimate" | "unknown"
	Breakdown                 *CostBreakdown `json:"breakdown,omitempty"`
	Warning                   string         `json:"warning,omitempty"`
}

// CostBreakdown provides detailed cost breakdown.
type CostBreakdown struct {
	InputCostUSD     float64  `json:"input_cost_usd"`
	OutputCostUSD    float64  `json:"output_cost_usd,omitempty"`
	InputPricePer1M  *float64 `json:"input_price_per_1m,omitempty"`
	OutputPricePer1M *float64 `json:"output_price_per_1m,omitempty"`
}

// estimateCompletionTokens estimates completion tokens based on max_tokens
// or a heuristic (default 1/3 of max_tokens or 500).
func estimateCompletionTokens(req *CostEstimateRequest) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	// Heuristic: assume ~500 completion tokens if not specified
	return 500
}

// handleCostEstimate handles POST /api/cost/estimate.
// It estimates the cost of a chat completion request without calling any provider.
func (s *Server) handleCostEstimate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req CostEstimateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages is required")
		return
	}

	// Count prompt tokens using the tiktoken-based counter
	tc, err := tokencount.CountPrompt(req.Model, req.Messages)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token count failed: "+err.Error())
		return
	}
	promptTokens := tc.Tokens
	if promptTokens <= 0 {
		// Fallback: rough estimate ~4 chars per token
		totalChars := 0
		for _, m := range req.Messages {
			totalChars += len(m.Content)
		}
		promptTokens = totalChars / 4
		if promptTokens <= 0 {
			promptTokens = 1
		}
	}

	// Get model prices from catalog
	price, _ := s.pairPrice("", req.Model)
	var inputPrice, outputPrice *float64
	if price.Known() {
		inputPrice = price.InputUSDPer1M
		outputPrice = price.OutputUSDPer1M
	}

	// Estimate completion tokens
	estimatedCompletionTokens := estimateCompletionTokens(&req)

	// Calculate estimated cost
	var estimatedCostUSD float64
	costKnown := false
	priceSource := "unknown"
	var breakdown *CostBreakdown

	if inputPrice != nil {
		inputCost := float64(promptTokens) * (*inputPrice / 1e6)
		estimatedCostUSD = inputCost
		costKnown = true
		priceSource = "catalog"

		breakdown = &CostBreakdown{
			InputCostUSD:    inputCost,
			InputPricePer1M: inputPrice,
		}

		if outputPrice != nil {
			outputCost := float64(estimatedCompletionTokens) * (*outputPrice / 1e6)
			estimatedCostUSD += outputCost
			breakdown.OutputCostUSD = outputCost
			breakdown.OutputPricePer1M = outputPrice
		}
	} else if inputPrice == nil && outputPrice == nil {
		priceSource = "unknown"
	}

	// Add warning for unknown prices
	warning := ""
	if !costKnown {
		warning = fmt.Sprintf("model %q has no known pricing in catalog; cost cannot be estimated", req.Model)
	} else if outputPrice == nil {
		warning = fmt.Sprintf("model %q has input pricing but no output pricing; completion cost is estimated", req.Model)
	}

	resp := CostEstimateResponse{
		Model:                     req.Model,
		PromptTokens:              promptTokens,
		EstimatedCompletionTokens: estimatedCompletionTokens,
		EstimatedCostUSD:          estimatedCostUSD,
		CostKnown:                 costKnown,
		PriceSource:               priceSource,
		Breakdown:                 breakdown,
		Warning:                   warning,
	}
	writeJSON(w, http.StatusOK, resp)
}

// CostReconciliation tracks estimated vs actual costs for a request.
type CostReconciliation struct {
	RequestID          string    `json:"request_id"`
	Model              string    `json:"model"`
	EstimatedCostUSD   float64   `json:"estimated_cost_usd"`
	EstimatedCostKnown bool      `json:"estimated_cost_known"`
	ActualCostUSD      float64   `json:"actual_cost_usd,omitempty"`
	ActualCostKnown    bool      `json:"actual_cost_known"`
	VarianceUSD        float64   `json:"variance_usd,omitempty"`
	VariancePct        float64   `json:"variance_pct,omitempty"`
	PromptTokens       int       `json:"prompt_tokens"`
	CompletionTokens   int       `json:"completion_tokens"`
	Timestamp          time.Time `json:"timestamp"`
	Status             string    `json:"status"` // "estimated", "reconciled", "failed"
}

// reconcileCost compares estimated and actual costs for a completed request.
// Returns a CostReconciliation record.
func (s *Server) reconcileCost(ctx context.Context, req *provider.ChatRequest, resp *provider.ChatResponse, estimatedCostUSD float64, estimatedCostKnown bool, startTime time.Time) *CostReconciliation {
	rec := &CostReconciliation{
		Model:              req.Model,
		EstimatedCostUSD:   estimatedCostUSD,
		EstimatedCostKnown: estimatedCostKnown,
		PromptTokens:       resp.Usage.PromptTokens,
		CompletionTokens:   resp.Usage.CompletionTokens,
		Timestamp:          startTime,
		Status:             "estimated",
	}

	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		rec.Status = "reconciled"
		// The actual cost is recorded in the ledger by the engine
		// We can't easily get it here without querying the ledger
		// For now, mark as reconciled but without actual cost
		rec.ActualCostKnown = false
	}

	return rec
}

// registerCostRoutes wires the P3-C cost estimation API.
func (s *Server) registerCostRoutes(mux *http.ServeMux) {
	s.register(mux, "POST", "/api/cost/estimate", s.handleCostEstimate)
}

// callerKeyIDFromContext extracts the gateway key ID from context.
func (s *Server) callerKeyIDFromContext(ctx context.Context) string {
	// The key ID is stored in the request context by the handler
	// For now, return empty - the actual implementation would
	// retrieve it from the context value set during handling
	return ""
}
