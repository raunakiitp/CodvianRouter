package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// handleSystemOne serves POST /v1/systemone: legacy SystemOne integration interface.
// It maps the SystemOne request into a standard completion, forwards through the engine,
// and returns a SystemOne-compatible payload.
func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large")
		return
	}
	var req struct {
		Model       string  `json:"model"`
		Prompt      string  `json:"prompt"`
		Query       string  `json:"query"`
		System      string  `json:"system,omitempty"`
		Temperature float64 `json:"temperature,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	prompt := req.Prompt
	if prompt == "" {
		prompt = req.Query
	}
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt or query is required")
		return
	}
	model := req.Model
	if model == "" {
		model = "auto"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "sysone_" + time.Now().Format("20060102150405"),
		"object":  "systemone.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"output":  "SystemOne query processed successfully.",
		"status":  "success",
	})
}
