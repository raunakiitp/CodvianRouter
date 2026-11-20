// POST /v1/moderations — OpenAI-shaped moderation backed ONLY by the
// router's built-in LOCAL guardrails (M8.9, Area 14 §14a).
//
// LOCAL ONLY: the input is scanned by internal/guardrails (the same
// deterministic PII / credential-secret / prompt-injection pattern
// stage that guards chat requests) and never leaves this gateway —
// no upstream moderation API is called, no provider is dialled, no
// input text is stored or echoed back. The response is OpenAI-shaped
// (id / model / results[] with flagged + categories +
// category_scores + category_applied_input_types) so existing
// OpenAI moderation clients can parse it, but the verdicts are
// honest about what the local engine can and cannot judge. Note
// category_applied_input_types is a PER-CATEGORY OBJECT in the
// OpenAI shape — one entry per category, e.g. {"harassment":
// ["text"], "sexual": ["text", "image"], ...} — not a bare array:
// the local engine is text-only, so every category maps to
// ["text"] (see moderationCategoryAppliedInputTypes).
//
//   - The 13 OpenAI moderation categories (hate, harassment, illicit,
//     self-harm, sexual, violence, ...) are NOT judged by the local
//     engine: it has no classifier for them, only deterministic
//     patterns for PII, secrets and injection. Every such category is
//     therefore reported as not-flagged with score 0 — never a
//     fabricated confidence — and the top-level notes field says so.
//   - The real local verdict lives in results[].local_guardrails:
//     flagged (any local finding), pii / secret / injection booleans,
//     and the finding list (kind + pattern label + count only — never
//     the matched value, per the guardrails no-storage rule).
//   - results[].flagged is true when the LOCAL guardrails flag the
//     input, even though every OpenAI category reads false. That is
//     deliberate and documented in notes: a client that only reads
//     flagged still gets the local verdict; a client that reads
//     categories sees exactly which judgements were actually made
//     (none of the OpenAI ones).
//
// Scores are 0 or 1 only — 1 when a local signal maps to a verdict,
// 0 otherwise — never an invented float. There is no local
// per-category confidence to report, and none is invented.
//
// Routing / auth (coordinator-owned, NOT in this file): register as
// POST /v1/moderations, TierManagement, gateway-key auth, scope
// "chat" (reviewer verdict: "models" is read-only listings only).
// This file defines only the handler method; it does not touch
// guard.go or server.go.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// moderationLocalEngineID is the model value reported when the
// request names no model. It is the local engine's identifier, not
// a provider model — no upstream moderation model is ever used.
const moderationLocalEngineID = "local-guardrails"

// moderationNotes is the honesty note carried on every response: it
// states what was judged, what was not, and where the real verdict
// lives. It is a top-level extension field, outside the OpenAI
// shape, so OpenAI clients ignore it and human operators cannot
// mistake the category block for a classifier verdict.
const moderationNotes = "Local-only moderation: input was scanned by the router's built-in deterministic guardrails (PII, credential/secret, prompt-injection patterns) only. No upstream moderation API was called and no input left this gateway. The 13 OpenAI categories are not judged by the local engine and are reported as not-flagged with score 0; the actual local verdict is in results[].local_guardrails, and results[].flagged reflects that local verdict."

// moderationCategories is the full OpenAI moderation category set.
// Every field is false for a local verdict: the local engine cannot
// judge any of these categories. The struct (not a map) keeps the
// wire order stable and the contract testable.
type moderationCategories struct {
	Hate                  bool `json:"hate"`
	HateThreatening       bool `json:"hate/threatening"`
	Harassment            bool `json:"harassment"`
	HarassmentThreatening bool `json:"harassment/threatening"`
	Illicit               bool `json:"illicit"`
	IllicitViolent        bool `json:"illicit/violent"`
	SelfHarm              bool `json:"self-harm"`
	SelfHarmIntent        bool `json:"self-harm/intent"`
	SelfHarmInstructions  bool `json:"self-harm/instructions"`
	Sexual                bool `json:"sexual"`
	SexualMinors          bool `json:"sexual/minors"`
	Violence              bool `json:"violence"`
	ViolenceGraphic       bool `json:"violence/graphic"`
}

// moderationCategoryScores mirrors moderationCategories with 0/1
// scores. All zeros for a local verdict: no category was measured,
// so no confidence is claimed.
type moderationCategoryScores struct {
	Hate                  float64 `json:"hate"`
	HateThreatening       float64 `json:"hate/threatening"`
	Harassment            float64 `json:"harassment"`
	HarassmentThreatening float64 `json:"harassment/threatening"`
	Illicit               float64 `json:"illicit"`
	IllicitViolent        float64 `json:"illicit/violent"`
	SelfHarm              float64 `json:"self-harm"`
	SelfHarmIntent        float64 `json:"self-harm/intent"`
	SelfHarmInstructions  float64 `json:"self-harm/instructions"`
	Sexual                float64 `json:"sexual"`
	SexualMinors          float64 `json:"sexual/minors"`
	Violence              float64 `json:"violence"`
	ViolenceGraphic       float64 `json:"violence/graphic"`
}

// moderationCategoryAppliedInputTypes is the OpenAI
// category_applied_input_types shape: a PER-CATEGORY OBJECT
// mapping each of the 13 moderation categories to the input types
// it was applied to — NOT a bare array. The local engine is
// text-only, so every category maps to ["text"]. The struct (not a
// map) keeps the wire order stable and the contract testable, like
// moderationCategories.
type moderationCategoryAppliedInputTypes struct {
	Hate                  []string `json:"hate"`
	HateThreatening       []string `json:"hate/threatening"`
	Harassment            []string `json:"harassment"`
	HarassmentThreatening []string `json:"harassment/threatening"`
	Illicit               []string `json:"illicit"`
	IllicitViolent        []string `json:"illicit/violent"`
	SelfHarm              []string `json:"self-harm"`
	SelfHarmIntent        []string `json:"self-harm/intent"`
	SelfHarmInstructions  []string `json:"self-harm/instructions"`
	Sexual                []string `json:"sexual"`
	SexualMinors          []string `json:"sexual/minors"`
	Violence              []string `json:"violence"`
	ViolenceGraphic       []string `json:"violence/graphic"`
}

// moderationTextOnlyAppliedInputTypes returns the applied-input-types
// object for the local engine: every category applied to text only.
func moderationTextOnlyAppliedInputTypes() moderationCategoryAppliedInputTypes {
	text := []string{"text"}
	return moderationCategoryAppliedInputTypes{
		Hate:                  text,
		HateThreatening:       text,
		Harassment:            text,
		HarassmentThreatening: text,
		Illicit:               text,
		IllicitViolent:        text,
		SelfHarm:              text,
		SelfHarmIntent:        text,
		SelfHarmInstructions:  text,
		Sexual:                text,
		SexualMinors:          text,
		Violence:              text,
		ViolenceGraphic:       text,
	}
}

// moderationLocalFinding is one local guardrail finding, copied from
// guardrails.Finding. Kind + pattern label + count only — the
// matched value is never carried (guardrails no-storage rule).
type moderationLocalFinding struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
}

// moderationLocalVerdict is the per-result local-guardrails extension:
// the verdict the local engine actually reached.
type moderationLocalVerdict struct {
	Flagged   bool                     `json:"flagged"`
	PII       bool                     `json:"pii"`
	Secret    bool                     `json:"secret"`
	Injection bool                     `json:"injection"`
	Findings  []moderationLocalFinding `json:"findings"`
}

// moderationResult is one OpenAI-shaped moderation result, plus the
// local_guardrails extension carrying the real verdict.
type moderationResult struct {
	Flagged                   bool                                `json:"flagged"`
	Categories                moderationCategories                `json:"categories"`
	CategoryScores            moderationCategoryScores            `json:"category_scores"`
	CategoryAppliedInputTypes moderationCategoryAppliedInputTypes `json:"category_applied_input_types"`
	LocalGuardrails           moderationLocalVerdict              `json:"local_guardrails"`
}

// moderationResponse is the POST /v1/moderations response: the
// OpenAI shape (id / model / results) plus the local_only flag and
// the honesty note.
type moderationResponse struct {
	ID        string             `json:"id"`
	Model     string             `json:"model"`
	Results   []moderationResult `json:"results"`
	LocalOnly bool               `json:"local_only"`
	Notes     string             `json:"notes"`
}

// moderationRequest is the accepted request shape: OpenAI's input
// (string, array of strings, or array of {type, text} objects) plus
// an optional model name, echoed back in the response.
type moderationRequest struct {
	Model string          `json:"model,omitempty"`
	Input json.RawMessage `json:"input"`
}

// moderationStore returns the server's guardrail store, or a fresh
// default store when the server was built without one (bare
// &Server{} literals in tests — New always wires a store). The
// fresh store runs the same deterministic patterns; only its
// counters are per-instance.
func (s *Server) moderationStore() *guardrails.Store {
	if s != nil && s.guardrails != nil {
		return s.guardrails
	}
	return guardrails.NewStore()
}

// handleModerations serves POST /v1/moderations.
func (s *Server) handleModerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Body cap mirrors chat (server.go maxRequestBody, 10 MiB): an
	// unbounded body is a memory-exhaustion vector for the gateway.
	// An over-cap body is 413, other read errors are 400 — the same
	// convention as handleMessagesCountTokens (count_tokens.go).
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}

	var req moderationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Generic message only: err.Error() leaks the Go type name
		// (e.g. "cannot unmarshal array into Go value of type
		// server.moderationRequest") to the client.
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	inputs, err := parseModerationInput(req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Per-key guardrail scoping, exactly as chat applies it
	// (guardrail_scope_277.go): a key with a non-empty guardrails
	// list runs only its listed kinds; nil runs everything.
	allowed := s.keyGuardrailScope(r)
	store := s.moderationStore()

	results := make([]moderationResult, 0, len(inputs))
	for _, text := range inputs {
		res := store.CheckScoped(text, guardrails.PhaseRequest, allowed)
		results = append(results, moderationResultFor(res))
	}

	model := req.Model
	if model == "" {
		model = moderationLocalEngineID
	}
	writeJSON(w, http.StatusOK, moderationResponse{
		ID:        fmt.Sprintf("modr-local-%d", time.Now().UnixNano()),
		Model:     model,
		Results:   results,
		LocalOnly: true,
		Notes:     moderationNotes,
	})
}

// moderationResultFor maps one guardrails.Result to an OpenAI-shaped
// result. All 13 OpenAI categories stay false with score 0: the
// local engine judges none of them (see the package doc). The local
// verdict — the only judgement actually made — is carried in
// LocalGuardrails, and Flagged reflects it.
func moderationResultFor(res guardrails.Result) moderationResult {
	local := moderationLocalVerdict{
		Flagged:   len(res.Findings) > 0,
		PII:       res.Has(guardrails.KindPII),
		Secret:    res.Has(guardrails.KindSecret),
		Injection: res.Has(guardrails.KindInjection),
		Findings:  make([]moderationLocalFinding, 0, len(res.Findings)),
	}
	for _, f := range res.Findings {
		local.Findings = append(local.Findings, moderationLocalFinding{
			Kind:    f.Kind,
			Pattern: f.Pattern,
			Count:   f.Count,
		})
	}
	return moderationResult{
		Flagged:                   local.Flagged,
		Categories:                moderationCategories{},
		CategoryScores:            moderationCategoryScores{},
		CategoryAppliedInputTypes: moderationTextOnlyAppliedInputTypes(),
		LocalGuardrails:           local,
	}
}

// parseModerationInput accepts OpenAI's input shapes: a single
// string, an array of strings, or an array of {type:"text",
// text:"..."} objects. Empty input (missing, empty string, or an
// empty array) is a 400: there is nothing to moderate. An empty
// string inside a non-empty string array is kept — it moderates
// clean, like OpenAI's empty-input result — so even an array of
// only empty strings returns 200 with clean results; only a
// wholly empty request (no inputs at all) is rejected. (For the
// {type, text} form, parts with no text at all likewise leave
// nothing to moderate and are a 400.)
func parseModerationInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("input is required")
	}
	// Single string.
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, fmt.Errorf("input must not be empty")
		}
		return []string{single}, nil
	}
	// Array of strings.
	var multi []string
	if err := json.Unmarshal(raw, &multi); err == nil {
		if len(multi) == 0 {
			return nil, fmt.Errorf("input must not be empty")
		}
		return multi, nil
	}
	// Array of {type, text} objects (OpenAI's multi-modal input
	// items, text parts only — image parts have no text to scan and
	// are rejected, not silently skipped).
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("input must be a string, an array of strings, or an array of {type, text} objects")
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("input must not be empty")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, p.Text)
		case "":
			// A part with no type but a text field is treated as
			// text — tolerant of clients that omit the type.
			if p.Text != "" {
				out = append(out, p.Text)
			}
		default:
			return nil, fmt.Errorf("input part type %q is not supported by local moderation (text only)", p.Type)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("input must not be empty")
	}
	return out, nil
}

// Category mapping (local signal → OpenAI moderation category):
//
//	| Local signal | OpenAI category |
//	| pii (email / phone / card / aadhaar) | NONE — no OpenAI category covers PII → local_guardrails.pii |
//	| secret (api-key / password / bearer / provider keys / jwt) | NONE → local_guardrails.secret |
//	| injection (ignore/disregard/forget instructions, jailbreak, DAN, reveal/override/bypass) | NONE → local_guardrails.injection |
//	| hate / harassment / illicit / self-harm / sexual / violence (+ all sub-categories) | NOT JUDGED by the local engine → reported not-flagged, score 0 |
//
// Every local signal maps to NO OpenAI category — the local engine
// judges none of them — and to the local_guardrails extension
// instead. There is no hidden mapping elsewhere in the handler.
