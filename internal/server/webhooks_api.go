// Webhooks management API (M8.19): the /api/webhooks endpoints.
//
// The outbound delivery machinery itself is M5.9
// (internal/webhooks — best-effort, HMAC-signed, no retries, one
// event "quota.exceeded"); this file adds the management surface
// around it and the second event kind, "budget.warning":
//
//   - POST   /api/webhooks       register a URL + event list
//   - GET    /api/webhooks       list registrations
//   - GET    /api/webhooks/{id}  one registration
//   - PUT    /api/webhooks/{id}  update a registration (M8.19
//     merge follow-up — partial update, OmniRoute semantics:
//     every field optional, only provided fields change)
//   - DELETE /api/webhooks/{id}  remove a registration
//
// Storage is the config file's webhooks block (config.Webhooks),
// the same durable store main.go builds the startup dispatcher
// from; mutations persist through s.configPersist with rollback
// on failure, exactly like the quota-schedule and key-budget
// handlers. After every mutation the live dispatcher is rebuilt
// from the stored list so a registration takes effect without a
// restart. Secrets are accepted on create but never returned:
// GET responses carry has_secret only.
//
// URL validation (SSRF posture): this codebase had no SSRF rule
// for webhook targets before M8.19, so this file sets the
// default: http/https only, no credentials in the URL, and no
// loopback / private / link-local / multicast target — literal
// IP or resolved hostname. There is deliberately no operator
// override yet: a local receiver (e.g. a dev relay on
// localhost) cannot be registered through this API today, only
// through the config file (which main.go trusts as operator
// input). If operators need localhost targets via the API, a
// config flag (e.g. webhooks_allow_private_targets) is the open
// follow-up — do not weaken this default silently.
//
// Events come from a fixed list: "quota.exceeded" (M5.9) and
// "budget.warning" (M8.19). An unknown event is a 400 naming it.
// No other event kind exists — do not invent one here.
//
// Registration: these handlers ARE wired into the mux —
// server.go:1464 calls registerWebhooksRoutes (below,
// which panics on a missing guard-table entry, like
// s.register) against the guard-table rows in guard.go:213-217
// (POST/DELETE TierAlwaysProtected, GETs TierManagement,
// scope "admin" throughout). The handlers themselves do no
// auth — the tier wrappers applied by Server.register do that,
// as for every other management route.
//
// Events: config validation accepts BOTH event kinds —
// "quota.exceeded" and "budget.warning" (internal/config/
// config.go:1964, the known-event check) — so a persisted
// "budget.warning" subscription survives the next startup's
// validation. An earlier BLOCKER comment here claimed
// validation only accepted "quota.exceeded"; the config fix
// landed in the same merge, so that claim was already false
// and is corrected here.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// BudgetWarningEvent fires when an internal budget crosses its
// warning threshold (M8.19). The payload carries the budget
// identity and the consumption that tripped it; see
// notifyBudgetWarning. It is the second event kind after
// webhooks.QuotaExceededEvent ("quota.exceeded"); no other event
// kind exists.
const BudgetWarningEvent = "budget.warning"

// webhookKnownEvents is the fixed event list a registration may
// subscribe to. Unknown events are a 400 naming the event.
var webhookKnownEvents = []string{webhooks.QuotaExceededEvent, BudgetWarningEvent}

// webhookMaxBody caps a registration request body (1 MiB, the
// same cap the quota-schedule handler uses).
const webhookMaxBody = 1 << 20

// webhookView is one registration as the API returns it. The
// secret is never returned — HasSecret only says one is set.
type webhookView struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	HasSecret bool     `json:"has_secret"`
}

// webhooksResponse is the GET /api/webhooks payload.
type webhooksResponse struct {
	Webhooks []webhookView `json:"webhooks"`
}

// webhookCreateRequest is the POST /api/webhooks body. The id is
// server-generated; a client-supplied id is rejected as an
// unknown field.
type webhookCreateRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
	Secret string   `json:"secret,omitempty"`
}

// webhookViewOf maps a stored registration to its API view.
func webhookViewOf(w config.WebhookConfig) webhookView {
	events := append([]string(nil), w.Events...)
	if events == nil {
		events = []string{}
	}
	return webhookView{ID: w.ID, URL: w.URL, Events: events, HasSecret: w.Secret != ""}
}

// validateWebhookURL enforces the SSRF default for a new target:
// http/https only, no embedded credentials, no fragment, and no
// loopback / private / link-local / multicast host — checked for
// literal IPs directly and for hostnames via DNS resolution. A
// resolution failure is a rejection, not a pass: an unresolvable
// target cannot be delivered to.
func validateWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return fmt.Errorf("url %q is not a parseable URL", raw)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("url %q must use http or https (got scheme %q)", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q has no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("url %q must not embed credentials", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("url %q must not carry a fragment", raw)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url %q has no host", raw)
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("url %q targets localhost, which is not an allowed webhook target (loopback targets are rejected by default; register one through the config file instead)", raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := checkWebhookIP(ip); err != nil {
			return fmt.Errorf("url %q targets %s: %v", raw, ip, err)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("url %q: host %q does not resolve: %v", raw, host, err)
	}
	for _, ip := range ips {
		if err := checkWebhookIP(ip); err != nil {
			return fmt.Errorf("url %q resolves to %s: %v", raw, ip, err)
		}
	}
	return nil
}

// checkWebhookIP rejects the addresses a webhook must never be
// pointed at by default: loopback, private (RFC 1918), link-local
// and multicast. The caller names the URL; this names the reason.
func checkWebhookIP(ip net.IP) error {
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("loopback addresses are not allowed webhook targets")
	case ip.IsPrivate():
		return fmt.Errorf("private-network addresses are not allowed webhook targets")
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("link-local addresses are not allowed webhook targets")
	case ip.IsMulticast():
		return fmt.Errorf("multicast addresses are not allowed webhook targets")
	case ip.IsUnspecified():
		return fmt.Errorf("unspecified addresses are not allowed webhook targets")
	default:
		return nil
	}
}

// validateWebhookEvents requires at least one event and rejects
// any name outside the fixed list, naming the first unknown one.
func validateWebhookEvents(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("events is required: subscribe to at least one event (%s)", strings.Join(webhookKnownEvents, ", "))
	}
	for _, ev := range events {
		known := false
		for _, k := range webhookKnownEvents {
			if ev == k {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("event %q is not a known webhook event (known: %s)", ev, strings.Join(webhookKnownEvents, ", "))
		}
	}
	return nil
}

// dedupeWebhookEvents drops repeat subscriptions, keeping the
// first occurrence order.
func dedupeWebhookEvents(events []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if !seen[ev] {
			seen[ev] = true
			out = append(out, ev)
		}
	}
	return out
}

// newWebhookID mints a registration id: "wh_" + 16 random hex
// chars. The caller checks it against the stored ids.
func newWebhookID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("could not generate a webhook id: %v", err)
	}
	return "wh_" + hex.EncodeToString(b[:]), nil
}

// webhookConfigs returns the stored registrations (a copy; an
// empty list when no config is wired, never nil).
func (s *Server) webhookConfigs() []config.WebhookConfig {
	if s.cfg == nil {
		return []config.WebhookConfig{}
	}
	return append([]config.WebhookConfig(nil), s.cfg.Webhooks...)
}

// findWebhook returns the stored registration with id, if any.
func (s *Server) findWebhook(id string) (config.WebhookConfig, bool) {
	for _, w := range s.webhookConfigs() {
		if w.ID == id {
			return w, true
		}
	}
	return config.WebhookConfig{}, false
}

// rebuildWebhookDispatcher rebuilds the live outbound dispatcher
// from the stored registrations, so a create/delete takes effect
// without a restart. No registrations means a nil dispatcher —
// the server treats that as "no delivery", exactly as main.go's
// startup wiring does.
func (s *Server) rebuildWebhookDispatcher() {
	if s.cfg == nil || len(s.cfg.Webhooks) == 0 {
		s.webhooks = nil
		return
	}
	endpoints := make([]webhooks.Endpoint, 0, len(s.cfg.Webhooks))
	for _, w := range s.cfg.Webhooks {
		endpoints = append(endpoints, webhooks.Endpoint{ID: w.ID, URL: w.URL, Events: w.Events, Secret: w.Secret})
	}
	s.webhooks = webhooks.New(endpoints, nil, s.logger)
}

// persistWebhooks saves the config file after a mutation, or
// reports why it cannot. With no persist function wired the
// in-memory change stands and the caller proceeds (tests and
// config-less servers); a persist failure is the caller's cue to
// roll the in-memory change back.
func (s *Server) persistWebhooks() error {
	if s.configPersist == nil {
		return nil
	}
	return s.configPersist()
}

// handleWebhooks serves GET /api/webhooks (list) and POST
// /api/webhooks (register). Auth is applied by the tier wrapper
// at registration, not here.
func (s *Server) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out := webhooksResponse{Webhooks: []webhookView{}}
		for _, c := range s.webhookConfigs() {
			out.Webhooks = append(out.Webhooks, webhookViewOf(c))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		s.handleWebhookCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleWebhookCreate serves POST /api/webhooks: validate the URL
// and the event list, mint an id, store the registration, persist
// the config file (rolling back on failure), rebuild the live
// dispatcher, and answer 201 with the registration (secret never
// echoed). With no config wired it answers 503 rather than
// pretending to register.
func (s *Server) handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "webhooks unavailable: no config wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
	var req webhookCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook registration: expected {\"url\": string, \"events\": [\"quota.exceeded\"|\"budget.warning\"], \"secret\": string}")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	if err := validateWebhookURL(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateWebhookEvents(req.Events); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := newWebhookID()
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not register webhook")
		return
	}
	for _, existing := range s.cfg.Webhooks {
		if existing.ID == id {
			writeServerError(w, http.StatusInternalServerError, "could not register webhook")
			return
		}
	}
	next := config.WebhookConfig{ID: id, URL: req.URL, Events: dedupeWebhookEvents(req.Events), Secret: req.Secret}
	prev := append([]config.WebhookConfig(nil), s.cfg.Webhooks...)
	s.cfg.Webhooks = append(s.cfg.Webhooks, next)
	if err := s.persistWebhooks(); err != nil {
		s.cfg.Webhooks = prev
		s.logger.Printf("webhooks: persist failed after registering %s: %v", id, guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not persist webhook config")
		return
	}
	s.rebuildWebhookDispatcher()
	writeJSON(w, http.StatusCreated, webhookViewOf(next))
}

// webhookIDFromRequest returns the {id} path segment: from the
// mux when the request arrived through a registered route, or by
// trimming the /api/webhooks/ prefix when the handler is invoked
// directly (tests, apitest harness).
func webhookIDFromRequest(r *http.Request) string {
	if id := strings.TrimSpace(r.PathValue("id")); id != "" {
		return id
	}
	return strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/webhooks/"), "/")
}

// handleWebhookByID serves GET /api/webhooks/{id}, PUT
// /api/webhooks/{id} and DELETE /api/webhooks/{id}. Unknown ids
// are 404. A delete or update persists the config file (rolling
// the change back on failure) and rebuilds the live dispatcher.
func (s *Server) handleWebhookByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodDelete, http.MethodPut:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := webhookIDFromRequest(r)
	if id == "" {
		writeError(w, http.StatusBadRequest, "webhook id is required")
		return
	}
	stored, ok := s.findWebhook(id)
	if !ok {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, webhookViewOf(stored))
		return
	case http.MethodPut:
		s.handleWebhookUpdate(w, r, stored)
		return
	}
	prev := append([]config.WebhookConfig(nil), s.cfg.Webhooks...)
	kept := s.cfg.Webhooks[:0]
	for _, c := range s.cfg.Webhooks {
		if c.ID != id {
			kept = append(kept, c)
		}
	}
	s.cfg.Webhooks = kept
	if err := s.persistWebhooks(); err != nil {
		s.cfg.Webhooks = prev
		s.logger.Printf("webhooks: persist failed after deleting %s: %v", id, guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not persist webhook config")
		return
	}
	s.rebuildWebhookDispatcher()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// webhookUpdateRequest is the PUT /api/webhooks/{id} body.
// Partial update, OmniRoute's PUT semantics (src/app/api/
// webhooks/[id]/route.ts: updateWebhookSchema — every field
// optional, only provided fields change): a nil pointer means
// "keep the stored value". URL, when provided, must be a
// non-empty valid target (re-validated, like create); Events,
// when provided, must be a non-empty known-event list; Secret,
// when provided, replaces the stored secret — an explicitly
// empty string clears it. The id is never updatable (it is in
// the path, and a client-supplied id is rejected as an unknown
// field). OmniRoute also accepts kind / description / enabled
// / metadata on PUT; this gateway's WebhookConfig carries none
// of those fields, so they are not accepted here — adding them
// needs a config-model extension, not a handler change.
type webhookUpdateRequest struct {
	URL    *string  `json:"url,omitempty"`
	Events []string `json:"events,omitempty"`
	Secret *string  `json:"secret,omitempty"`
}

// handleWebhookUpdate serves PUT /api/webhooks/{id}: apply the
// provided fields to the stored registration (partial update),
// persist the config file (rolling back on failure), rebuild
// the live dispatcher, and answer 200 with the registration
// (secret never echoed). Unknown ids are 404 — the caller
// (handleWebhookByID) has already checked that.
func (s *Server) handleWebhookUpdate(w http.ResponseWriter, r *http.Request, stored config.WebhookConfig) {
	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
	var req webhookUpdateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook update: expected {\"url\": string (optional), \"events\": [\"quota.exceeded\"|\"budget.warning\"] (optional), \"secret\": string (optional)}")
		return
	}
	next := stored
	if req.URL != nil {
		u := strings.TrimSpace(*req.URL)
		if u == "" {
			writeError(w, http.StatusBadRequest, "url must not be empty")
			return
		}
		if err := validateWebhookURL(u); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		next.URL = u
	}
	if req.Events != nil {
		if err := validateWebhookEvents(req.Events); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		next.Events = dedupeWebhookEvents(req.Events)
	}
	if req.Secret != nil {
		next.Secret = *req.Secret
	}
	prev := append([]config.WebhookConfig(nil), s.cfg.Webhooks...)
	for i := range s.cfg.Webhooks {
		if s.cfg.Webhooks[i].ID == stored.ID {
			s.cfg.Webhooks[i] = next
			break
		}
	}
	if err := s.persistWebhooks(); err != nil {
		s.cfg.Webhooks = prev
		s.logger.Printf("webhooks: persist failed after updating %s: %v", stored.ID, guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not persist webhook config")
		return
	}
	s.rebuildWebhookDispatcher()
	writeJSON(w, http.StatusOK, webhookViewOf(next))
}

// registerWebhooksRoutes registers the five management routes.
// Classification (guard.go):
//
//	POST   /api/webhooks      Tier 2 ALWAYS_PROTECTED, scopes ["admin"]
//	GET    /api/webhooks      Tier 3 MANAGEMENT,      scopes ["admin"]
//	GET    /api/webhooks/{id} Tier 3 MANAGEMENT,      scopes ["admin"]
//	PUT    /api/webhooks/{id} Tier 2 ALWAYS_PROTECTED, scopes ["admin"]
//	DELETE /api/webhooks/{id} Tier 2 ALWAYS_PROTECTED, scopes ["admin"]
//
// register() panics on a missing table entry, so calling this
// before the entries land fails loudly at startup, never silently.
func (s *Server) registerWebhooksRoutes(mux *http.ServeMux) {
	s.register(mux, "POST", "/api/webhooks", s.handleWebhooks)
	s.register(mux, "GET", "/api/webhooks", s.handleWebhooks)
	s.register(mux, "GET", "/api/webhooks/{id}", s.handleWebhookByID)
	s.register(mux, "PUT", "/api/webhooks/{id}", s.handleWebhookByID)
	s.register(mux, "DELETE", "/api/webhooks/{id}", s.handleWebhookByID)
}

// ---------------------------------------------------------------------------
// budget.warning delivery.
// ---------------------------------------------------------------------------

// budgetWarningState remembers, per budget id, the spend at the
// last warning delivery, so budget.warning is edge-triggered: it
// fires when a budget crosses its warning threshold, not on every
// request served while the budget sits above it (the warn
// evaluation runs on every chat/embedding request). The record is
// updated on every warn evaluation, so when a new period starts
// and spend resets below the threshold, the next crossing fires
// again. Package-level because Server's struct lives in
// server.go, which this slice does not own; keyed by budget id,
// which the ledger guarantees unique.
var budgetWarningState = struct {
	sync.Mutex
	lastUsed map[string]float64
}{lastUsed: map[string]float64{}}

// notifyBudgetWarning fires the "budget.warning" event for a warn
// evaluation, best-effort through the live dispatcher (async,
// HMAC-signed when the endpoint has a secret, no retries — the
// M5.9 delivery contract). A nil dispatcher is a no-op. The
// edge-trigger rule lives here: a warn evaluation whose budget
// was already above its threshold at the last evaluation is a
// repeat, not a crossing, and is not delivered.
func (s *Server) notifyBudgetWarning(ev *ledger.InternalBudgetEvaluation) {
	if s.webhooks == nil || ev == nil || ev.Budget == nil {
		return
	}
	threshold := ev.Budget.WarningThreshold
	if threshold == 0 {
		threshold = 0.8 // the ledger's default when 0 is stored
	}
	pct := 0.0
	if ev.Budget.LimitValue > 0 {
		pct = ev.Used / ev.Budget.LimitValue * 100
		// Edge-trigger: only a crossing delivers.
		budgetWarningState.Lock()
		last, seen := budgetWarningState.lastUsed[ev.Budget.ID]
		budgetWarningState.lastUsed[ev.Budget.ID] = ev.Used
		budgetWarningState.Unlock()
		if seen && last >= threshold*ev.Budget.LimitValue {
			return
		}
	}
	s.webhooks.Notify(BudgetWarningEvent, map[string]any{
		"budget_id":  ev.Budget.ID,
		"scope":      ev.Budget.Scope,
		"limit_type": ev.Budget.LimitType,
		"limit":      ev.Budget.LimitValue,
		"used":       ev.Used,
		"remaining":  ev.Remaining,
		"pct":        pct,
		"threshold":  threshold,
		"decision":   string(ev.Decision),
	})
}
