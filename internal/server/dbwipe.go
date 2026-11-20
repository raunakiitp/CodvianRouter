// DB wipe (POST /admin/db/wipe/arm + POST /admin/db/wipe, Tier 2
// ALWAYS_PROTECTED).
//
// The wipe is the one genuinely irreversible destructive route:
// it deletes recorded history that no export can bring back, so
// it carries the strongest credential boundary in the router —
// three checks, not one:
//
//  1. Gateway-key auth (Tier 2 wrapper, always required even when
//     Tier 3 auth is relaxed).
//  2. The typed confirmation phrase "WIPE DATABASE" in the JSON
//     body, checked server-side (a stray click or scripted POST
//     without the phrase cannot trigger it).
//  3. A single-use confirmation token, issued only by
//     POST /admin/db/wipe/arm — itself Tier 2, itself requiring
//     the key AND the phrase — and consumed by exactly one wipe
//     call. The token is 32 random bytes (crypto/rand), returned
//     once in the arm response, stored only as its SHA-256 hash,
//     valid 5 minutes, single-use, at most 8 outstanding, and
//     never logged. A captured wipe request body is useless after
//     the token is spent or expires, and a single POST — however
//     it was produced — can never wipe: the arm and the fire are
//     two distinct requests.
//
// Honest limit of this boundary (stated here and in the docs,
// not hidden): the token is a second STEP, not a second factor.
// This system has exactly one credential type — gateway keys —
// so anyone holding a live gateway key can arm and then fire.
// What the token stops is the single-request attack: a leaked or
// replayed wipe body, a CSRF-style one-shot POST, an accidental
// click, and a script that only knows how to make one call. It
// does not stop an attacker who holds a key and can make two
// calls; no second factor exists to stop them with. The phrase
// is likewise a confirmation, not a secret — it is documented
// in the open. The defence against a live-key attacker is key
// hygiene (revoke-all) and the loopback-only bind, not this
// route's checks, and nothing here claims otherwise.
//
// Wipe scope — what is wiped, and what is preserved (the response
// lists both, so a caller never has to guess):
//
//	WIPED:
//	  - ledger usage_events: every row (ledger.Wipe).
//	  - ledger budgets: running spend reset to 0 for every key.
//	  - feedback store: every decision and every signal
//	    (feedback.ClearAll).
//	  - probe history: the whole in-memory ring plus
//	    health-history.json, its .bak backup AND the journal
//	    (health.ClearHistory).
//	PRESERVED (enforced by the code — the wipe touches only the
//	stores above):
//	  - the config file entirely: providers and provider keys,
//	    gateway keys, prices, routing, guardrail and auto-router
//	    settings, budget caps and periods.
//	  - the ledger.db / feedback.db files and schemas (emptied,
//	    not deleted), so the router keeps working with no restart.
//	  - benchmark.json (a measurement record, not usage history).
//	  - in-memory metrics, caches and the last routing decision:
//	    process-lifetime state, reset on restart, not database
//	    history — left alone and disclosed as such.
//
// Ordering and failure: the async ledger writer is flushed first
// (WithLedgerFlushHook, wired by main.go) so queued rows cannot
// resurrect after the delete; then ledger, feedback, history in
// that order, serialised against the other danger actions by
// dangerMu. A step failure answers 500 with the partial counts
// of the steps that already ran — a failed wipe never claims
// success, and never claims "nothing was wiped" when earlier
// steps already deleted rows.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// dbWipeConfirm is the exact phrase the caller must type (and
// send) to arm and to fire a database wipe. It is deliberately
// distinct from the other danger phrases (EXPORT, EXPORT
// SETTINGS, REVOKE ALL, RESET CONFIG, DELETE <id>), so a body
// captured for one destructive action cannot authorise another.
const dbWipeConfirm = "WIPE DATABASE"

// dbWipeTokenTTL is how long an issued confirmation token lives.
const dbWipeTokenTTL = 5 * time.Minute

// dbWipeMaxTokens caps outstanding (unexpired, unspent) tokens;
// issuing past the cap evicts the oldest one.
const dbWipeMaxTokens = 8

// HealthHistoryClearer clears the recorded probe history. The
// health.Checker satisfies it (see internal/health/history.go).
type HealthHistoryClearer interface {
	// ClearHistory drops every probe sample from the ring and
	// deletes the persisted history file and its backup.
	// It returns the samples cleared and whether any file was
	// actually removed.
	ClearHistory() (samples int, filesRemoved bool, err error)
}

// dbWipePreserved is the preserved-scope list in every wipe
// response: what the wipe deliberately does not touch.
var dbWipePreserved = []string{
	"providers and provider keys (config untouched)",
	"gateway keys (config untouched)",
	"prices, routing, guardrail and auto-router settings (config untouched)",
	"budget caps and periods (only running spend was reset to zero)",
	"ledger.db and feedback.db files and schemas (emptied, not deleted)",
	"benchmark.json (a measurement record, not usage history)",
	"in-memory metrics, caches and last routing decision (process-lifetime; reset on restart)",
}

// dbWipeArmResponse is the POST /admin/db/wipe/arm payload. The
// token is shown exactly once, here — it is never logged and
// never returned again.
type dbWipeArmResponse struct {
	ConfirmationToken string `json:"confirmation_token"`
	ExpiresInSeconds  int    `json:"expires_in_seconds"`
	Note              string `json:"note"`
}

// dbWipeResult is the per-store wipe counts in the response.
type dbWipeResult struct {
	UsageEvents               int64 `json:"usage_events"`
	BudgetsSpentReset         int64 `json:"budgets_spent_reset"`
	FeedbackDecisions         int64 `json:"feedback_decisions"`
	FeedbackSignals           int64 `json:"feedback_signals"`
	HealthHistorySamples      int64 `json:"health_history_samples"`
	HealthHistoryFilesRemoved bool  `json:"health_history_files_removed"`
}

// dbWipeResponse is the POST /admin/db/wipe success payload.
type dbWipeResponse struct {
	Wiped        dbWipeResult `json:"wiped"`
	WipedAt      time.Time    `json:"wiped_at"`
	Irreversible bool         `json:"irreversible"`
	Preserved    []string     `json:"preserved"`
	Notes        []string     `json:"notes,omitempty"`
	Note         string       `json:"note"`
}

// dbWipeErrorResponse is the POST /admin/db/wipe failure payload:
// the step that failed plus the partial counts of the steps that
// already ran, so a half-finished wipe is reported exactly.
type dbWipeErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Wiped      dbWipeResult `json:"wiped"`
	FailedStep string       `json:"failed_step"`
	Note       string       `json:"note"`
}

// dbWipeBody is the POST /admin/db/wipe JSON body: the typed
// phrase AND the single-use confirmation token from the arm step.
type dbWipeBody struct {
	Confirm           string `json:"confirm"`
	ConfirmationToken string `json:"confirmation_token"`
}

// issueDBWipeToken mints one single-use confirmation token: 32
// random bytes, base64url-encoded for transport, stored only as
// its SHA-256 hash with a 5-minute expiry. Expired tokens are
// pruned on issue; past the cap the oldest outstanding token is
// evicted.
func (s *Server) issueDBWipeToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(tok))
	s.wipeMu.Lock()
	defer s.wipeMu.Unlock()
	if s.wipeTokens == nil {
		s.wipeTokens = map[[32]byte]time.Time{}
	}
	now := time.Now()
	for h, exp := range s.wipeTokens {
		if now.After(exp) {
			delete(s.wipeTokens, h)
		}
	}
	if len(s.wipeTokens) >= dbWipeMaxTokens {
		var oldest [32]byte
		var oldestExp time.Time
		first := true
		for h, exp := range s.wipeTokens {
			if first || exp.Before(oldestExp) {
				oldest, oldestExp, first = h, exp, false
			}
		}
		delete(s.wipeTokens, oldest)
	}
	s.wipeTokens[sum] = now.Add(dbWipeTokenTTL)
	return tok, nil
}

// consumeDBWipeToken verifies a presented token and spends it: a
// token that verifies is deleted whether or not it has expired,
// so an expired token cannot be retried into validity and a valid
// token cannot be replayed. It returns false for a malformed,
// unknown, expired, or already-spent token — the caller must not
// distinguish those cases in the response.
func (s *Server) consumeDBWipeToken(tok string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	s.wipeMu.Lock()
	defer s.wipeMu.Unlock()
	exp, ok := s.wipeTokens[sum]
	if !ok {
		return false
	}
	delete(s.wipeTokens, sum)
	return time.Now().Before(exp)
}

// handleDBWipeArm serves POST /admin/db/wipe/arm (Tier 2): after
// the gateway-key auth (wrapper) and the typed phrase, it issues
// a single-use confirmation token for one later wipe call. The
// token is returned once and never logged.
func (s *Server) handleDBWipeArm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.readDangerConfirm(w, r, dbWipeConfirm); !ok {
		return
	}
	tok, err := s.issueDBWipeToken()
	if err != nil {
		s.logger.Printf("db wipe: could not issue confirmation token: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not issue confirmation token")
		return
	}
	writeJSON(w, http.StatusCreated, dbWipeArmResponse{
		ConfirmationToken: tok,
		ExpiresInSeconds:  int(dbWipeTokenTTL / time.Second),
		Note:              "Single-use token for one POST /admin/db/wipe, valid 5 minutes. It is shown once, stored hashed, and never logged — keep it for the wipe call and do not share it. The wipe is irreversible.",
	})
}

// handleDBWipe serves POST /admin/db/wipe (Tier 2): gateway-key
// auth (wrapper) AND the typed phrase AND the single-use
// confirmation token, then the irreversible wipe of usage
// history. A missing token is 403 (the second credential is
// absent); an unknown/expired/spent token is also 403, without
// distinguishing the cases. A wrong phrase is 400, like the
// other danger routes.
func (s *Server) handleDBWipe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var body dbWipeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: expected {\"confirm\": \""+dbWipeConfirm+"\", \"confirmation_token\": \"...\"}")
		return
	}
	if body.Confirm != dbWipeConfirm {
		writeError(w, http.StatusBadRequest, "confirmation phrase does not match: type "+dbWipeConfirm+" to confirm")
		return
	}
	if body.ConfirmationToken == "" {
		writeError(w, http.StatusForbidden, "confirmation token is required: arm the wipe first with POST /admin/db/wipe/arm")
		return
	}
	if !s.consumeDBWipeToken(body.ConfirmationToken) {
		writeError(w, http.StatusForbidden, "confirmation token is invalid, expired, or already used: arm the wipe again with POST /admin/db/wipe/arm")
		return
	}

	s.dangerMu.Lock()
	defer s.dangerMu.Unlock()
	// Flush queued ledger rows first: a row flushed after the
	// delete would resurrect history the wipe just removed.
	if s.ledgerFlushHook != nil {
		s.ledgerFlushHook()
	}
	var wiped dbWipeResult
	var notes []string
	fail := func(step string, err error, wiped dbWipeResult) {
		s.logger.Printf("db wipe: failed at step %s: %v (partial wipe: %+v)", step, guardrails.RedactLogLine(err.Error()), wiped)
		resp := dbWipeErrorResponse{Wiped: wiped, FailedStep: step, Note: "The wipe stopped part-way: steps before " + step + " already deleted their data and cannot be undone. Fix the failed step and wipe again if a complete wipe is still wanted — already-wiped stores report zero on the retry."}
		resp.Error.Message = "database wipe failed at step " + step
		writeJSON(w, http.StatusInternalServerError, resp)
	}

	if s.ledger == nil {
		notes = append(notes, "no ledger configured — no usage history to wipe")
	} else {
		res, err := s.ledger.Wipe(r.Context())
		if err != nil {
			fail("ledger", err, wiped)
			return
		}
		wiped.UsageEvents = res.UsageEvents
		wiped.BudgetsSpentReset = res.BudgetsReset
	}
	if s.feedback == nil {
		notes = append(notes, "no feedback store configured — no feedback history to wipe")
	} else {
		decisions, signals, err := s.feedback.ClearAll(r.Context())
		if err != nil {
			fail("feedback", err, wiped)
			return
		}
		wiped.FeedbackDecisions = decisions
		wiped.FeedbackSignals = signals
	}
	if s.healthHistory == nil {
		notes = append(notes, "no probe-history store configured — no probe history to wipe")
	} else {
		samples, filesRemoved, err := s.healthHistory.ClearHistory()
		if err != nil {
			fail("health-history", err, wiped)
			return
		}
		wiped.HealthHistorySamples = int64(samples)
		wiped.HealthHistoryFilesRemoved = filesRemoved
	}
	s.logger.Printf("db wipe: wiped %d usage event(s), %d budget(s) spend reset, %d feedback decision(s), %d signal(s), %d probe sample(s)", wiped.UsageEvents, wiped.BudgetsSpentReset, wiped.FeedbackDecisions, wiped.FeedbackSignals, wiped.HealthHistorySamples)
	writeJSON(w, http.StatusOK, dbWipeResponse{
		Wiped:        wiped,
		WipedAt:      time.Now().UTC(),
		Irreversible: true,
		Preserved:    dbWipePreserved,
		Notes:        notes,
		Note:         "Irreversible: the wiped usage history, feedback signals and probe history are gone and cannot be recovered. Providers, provider keys, gateway keys, prices and all other config were preserved — use POST /admin/config/reset for a config reset and POST /admin/keys/revoke-all for key revocation, each with its own confirmation.",
	})
}
