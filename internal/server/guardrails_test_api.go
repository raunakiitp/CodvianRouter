// POST /api/guardrails/test (M8.16, management promotion B).
//
// A dry-run probe of the LOCAL guardrails package: the caller posts
// a sample text and learns what would happen to it — which patterns
// match (kinds + pattern labels only, never the matched values),
// the redacted text, and whether the live blocking config would
// refuse it — WITHOUT sending anything upstream and WITHOUT
// recording the sample anywhere:
//
//   - No provider is dialled: the verdict comes from the pure
//     guardrails.Findings / guardrails.Redact scan (local
//     regex/substring scan, no network, no LLM call).
//   - No ledger entry: the probe never touches the ledger (a test
//     probe is not usage), and the sample text is never stored.
//   - No guardrail-store recording: the live store's Check is NOT
//     used, because Check increments the checked/redacted/blocked
//     counters and appends an audit event — a probe must not pollute
//     the live counts. The blocking verdict is instead derived from
//     the live store's config with the same precedence Check uses
//     (PII, then secrets, then injection), so the answer matches
//     what a real request with this text would get.
//   - The original sample is NEVER echoed back: the response carries
//     only the redacted text and finding labels. A sample containing
//     PII or a secret therefore cannot leak back through this
//     endpoint.
//   - External (async) guardrails are NOT run: they call a remote
//     classifier endpoint, which would send the sample upstream.
//     The probe covers the built-in local guardrails only; the
//     response says so.
//
// Tier/scope are proposed in the M8.16 slice report; the coordinator
// assigns them at registration (guard.go + register()).
package server

import (
	"encoding/json"
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// guardrailsTestRequest is the POST /api/guardrails/test body. Text
// is a pointer so a missing field (400) is distinct from an empty
// sample (a valid probe: nothing matches, action "allowed").
type guardrailsTestRequest struct {
	Text *string `json:"text"`
}

// guardrailsTestFindingJSON is one matched pattern in the response:
// kind + pattern label + match count. Labels only — never a matched
// value.
type guardrailsTestFindingJSON struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
}

// guardrailsTestResponse is the POST /api/guardrails/test payload.
// Action is "allowed" (no finding), "redacted" (a finding matched
// but the live config would not block it) or "blocked" (the live
// config would refuse a real request with this text). WouldBlock and
// BlockReason mirror the live blocking decision; Recorded and
// SentUpstream are always false — the probe records and sends
// nothing — and are carried in the payload so a client can verify
// that instead of trusting the docs.
type guardrailsTestResponse struct {
	Action       string                      `json:"action"`
	WouldBlock   bool                        `json:"would_block"`
	BlockReason  string                      `json:"block_reason,omitempty"`
	RedactedText string                      `json:"redacted_text"`
	Findings     []guardrailsTestFindingJSON `json:"findings"`
	Categories   []string                    `json:"categories"`
	Blocking     guardrails.Config           `json:"blocking_config"`
	Recorded     bool                        `json:"recorded"`
	SentUpstream bool                        `json:"sent_upstream"`
	ExternalRun  bool                        `json:"external_guardrails_run"`
	ExternalNote string                      `json:"external_guardrails_note"`
}

// guardrailsTestVerdict derives the would-block verdict from the
// live blocking config with the same precedence the store's Check
// uses (PII, then secrets, then injection). It reads the config only
// — it records nothing.
func guardrailsTestVerdict(findings []guardrails.Finding, cfg guardrails.Config) (blocked bool, reason string) {
	has := func(kind string) bool {
		for _, f := range findings {
			if f.Kind == kind {
				return true
			}
		}
		return false
	}
	switch {
	case cfg.BlockPII && has(guardrails.KindPII):
		return true, guardrails.KindPII
	case cfg.BlockSecrets && has(guardrails.KindSecret):
		return true, guardrails.KindSecret
	case cfg.BlockInjection && has(guardrails.KindInjection):
		return true, guardrails.KindInjection
	default:
		return false, ""
	}
}

// handleGuardrailsTest serves POST /api/guardrails/test.
func (s *Server) handleGuardrailsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req guardrailsTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid guardrails test: expected {\"text\": string}")
		return
	}
	if req.Text == nil {
		writeError(w, http.StatusBadRequest, "invalid guardrails test: \"text\" is required")
		return
	}
	// Pure local scan: no store Check (which would record counters
	// and an audit event), no ledger, no upstream.
	findings := guardrails.Findings(*req.Text)
	redacted := guardrails.Redact(*req.Text)
	cfg := guardrails.Config{}
	if s.guardrails != nil {
		cfg = s.guardrails.Config()
	}
	blocked, reason := guardrailsTestVerdict(findings, cfg)
	action := guardrails.ActionAllowed
	if len(findings) > 0 {
		if blocked {
			action = guardrails.ActionBlocked
		} else {
			action = guardrails.ActionRedacted
		}
	}
	out := guardrailsTestResponse{
		Action:       action,
		WouldBlock:   blocked,
		BlockReason:  reason,
		RedactedText: redacted,
		Findings:     []guardrailsTestFindingJSON{},
		Categories:   []string{},
		Blocking:     cfg,
		Recorded:     false,
		SentUpstream: false,
		ExternalRun:  false,
		ExternalNote: "external (async) guardrails are not run by a test probe: they call a remote classifier, which would send the sample upstream",
	}
	seenKinds := map[string]bool{}
	for _, f := range findings {
		out.Findings = append(out.Findings, guardrailsTestFindingJSON{Kind: f.Kind, Pattern: f.Pattern, Count: f.Count})
		if !seenKinds[f.Kind] {
			seenKinds[f.Kind] = true
			out.Categories = append(out.Categories, f.Kind)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
