// Key creation from templates, key import, and the per-key
// policy enforcement point (G15 Area 4).
//
//   - GET /admin/key-templates — the built-in key policy
//     templates (read-only, standard, admin). Templates are
//     built-in presets, not stored records: a template is a
//     secret-free snapshot of a complete key policy, and
//     selecting one prefills a draft key (OmniRoute's
//     keyTemplates contract). Editing a template never changes
//     keys created from it, because the policy is copied onto
//     the key at creation. Custom templates need a store on the
//     Server struct, which Wave 3 owns (server.go) — see the
//     report.
//   - POST /admin/keys/import — import a key definition: the
//     exported policy shape (the same fields GatewayKey
//     serialises to) PLUS the raw key ("key"), because our
//     exports are secret-free by design (dbexport.go redacts
//     gateway keys) and a key cannot be imported without its
//     secret — we store only the SHA-256 hash. The response
//     never echoes the raw key: the caller supplied it, so
//     there is nothing to show once. A duplicate ID with the
//     same key hash is an idempotent retry (200, created=false,
//     OmniRoute's rule); a duplicate ID with a different key,
//     or a duplicate key hash under another ID, is a 409.
//
// Route registration + guard-table rows for these two routes
// (and for POST /admin/keys, whose handler already exists in
// keystatus.go) land with the coordinator — server.go and the
// guard table are Wave 3 territory, and adding a guard row
// without a registration breaks TestScopeEnforcedTableDriven
// (it drives every table row through the live mux) and the
// M8.21 doc-coverage gate. The exact lines are in the report.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// ---------------------------------------------------------------------------
// Key policy templates.
// ---------------------------------------------------------------------------

// keyTemplate is one built-in key policy template: a named,
// secret-free preset. Name is the template selector sent as
// "template" to POST /admin/keys.
type keyTemplate struct {
	Name              string           `json:"name"`
	Description       string           `json:"description"`
	Scopes            []string         `json:"scopes"`
	BudgetUSD         float64          `json:"budget_usd"`
	RequestsPerMinute int              `json:"requests_per_minute"`
	TokensPerMinute   int              `json:"tokens_per_minute"`
	RequestsPerDay    int              `json:"requests_per_day"`
	Guardrails        []string         `json:"guardrails,omitempty"`
	ThrottleDelayMs   int              `json:"throttle_delay_ms,omitempty"`
	MaxSessions       int              `json:"max_sessions,omitempty"`
	Policy            config.KeyPolicy `json:"policy"`
}

// keyTemplates are the built-in presets. read-only is the
// least-privilege key: it may list models and introspect itself,
// and its endpoint allowlist refuses every spend path (chat,
// embeddings, audio, images, responses) with a 403 — so a
// read-only key can never spend, even if it is leaked. standard
// is the working default (the same default POST /admin/keys
// uses without a template). admin is the operator key.
var keyTemplates = []keyTemplate{
	{
		Name: "read-only", Description: "List models and introspect the key itself; cannot call any spend endpoint.",
		Scopes: []string{"models"}, RequestsPerMinute: 60,
		Policy: config.KeyPolicy{AllowedEndpoints: []string{"/v1/models", "/v1/me/status", "/v1/me/sessions", "/v1/me/sessions/*"}},
	},
	{
		Name: "standard", Description: "Chat + models with a 60 RPM / 60k TPM working limit.",
		Scopes: []string{"chat", "models"}, RequestsPerMinute: 60, TokensPerMinute: 60000, RequestsPerDay: 1000,
		Policy: config.KeyPolicy{},
	},
	{
		Name: "admin", Description: "Full operator key: chat, models and admin scope, no traffic limits.",
		Scopes: []string{"chat", "models", "admin"},
		Policy: config.KeyPolicy{},
	},
}

// findKeyTemplate returns the built-in template with name, if any.
func findKeyTemplate(name string) (keyTemplate, bool) {
	for _, t := range keyTemplates {
		if t.Name == name {
			return t, true
		}
	}
	return keyTemplate{}, false
}

// handleKeyTemplates serves GET /admin/key-templates: the
// built-in templates, secret-free by construction (a template
// holds no key material at all).
func (s *Server) handleKeyTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	type templatesResponse struct {
		Templates []keyTemplate `json:"templates"`
		// NotImplemented surfaces the deliberate-omission list
		// (G15 re-review nit 7): the OmniRoute policy fields
		// this router does not carry, with the reason each one
		// is absent — so an operator reading the templates
		// endpoint sees what a template can never set.
		NotImplemented []keyPolicyOmission `json:"not_implemented_policy_fields"`
	}
	writeJSON(w, http.StatusOK, templatesResponse{Templates: append([]keyTemplate(nil), keyTemplates...), NotImplemented: append([]keyPolicyOmission(nil), keyPolicyNotImplemented...)})
}

// ---------------------------------------------------------------------------
// Per-key policy request parsing (create + update share it).
// ---------------------------------------------------------------------------

// keyPolicyRequest is the policy section of a key create /
// update body. Every field is a pointer so "absent" stays
// distinct from "present but empty": a present empty list clears
// that list, an absent one leaves the template / stored value
// alone. Template is creation-only: update rejects it.
type keyPolicyRequest struct {
	Template         *string   `json:"template"`
	IPAllowlist      *[]string `json:"ip_allowlist"`
	IPBlocklist      *[]string `json:"ip_blocklist"`
	ModelAccessMode  *string   `json:"model_access_mode"`
	AllowedModels    *[]string `json:"allowed_models"`
	BlockedModels    *[]string `json:"blocked_models"`
	AllowedEndpoints *[]string `json:"allowed_endpoints"`
	ForcedModel      *string   `json:"forced_model"`
}

// hasPolicy reports whether the request carries any policy field
// at all (template included).
func (p *keyPolicyRequest) hasPolicy() bool {
	return p != nil && (p.Template != nil || p.IPAllowlist != nil || p.IPBlocklist != nil ||
		p.ModelAccessMode != nil || p.AllowedModels != nil || p.BlockedModels != nil ||
		p.AllowedEndpoints != nil || p.ForcedModel != nil)
}

// resolve returns the effective policy: the named template's
// policy first, then every explicit field overriding it. An
// unknown template name is an error; the resolved policy is
// validated before it is returned. allowTemplate=false (update)
// rejects any template field.
func (p *keyPolicyRequest) resolve(allowTemplate bool) (config.KeyPolicy, error) {
	return p.resolveOnto(config.KeyPolicy{}, allowTemplate)
}

// resolveOnto is resolve starting from a stored policy instead
// of an empty one: an update merges explicit fields onto the
// key's stored policy, so an update naming only ip_allowlist
// leaves the stored model policy untouched. A present empty list
// still clears that list; absent still means unchanged.
func (p *keyPolicyRequest) resolveOnto(base config.KeyPolicy, allowTemplate bool) (config.KeyPolicy, error) {
	out := base
	if p == nil {
		return out, nil
	}
	if p.Template != nil {
		if !allowTemplate {
			return out, fmt.Errorf("template is a creation-time preset and cannot be applied by an update")
		}
		tmpl, ok := findKeyTemplate(strings.TrimSpace(*p.Template))
		if !ok {
			return out, fmt.Errorf("unknown key template %q", *p.Template)
		}
		out = tmpl.Policy
	}
	if p.IPAllowlist != nil {
		out.IPAllowlist = append([]string(nil), *p.IPAllowlist...)
	}
	if p.IPBlocklist != nil {
		out.IPBlocklist = append([]string(nil), *p.IPBlocklist...)
	}
	if p.ModelAccessMode != nil {
		out.ModelAccessMode = strings.TrimSpace(*p.ModelAccessMode)
	}
	if p.AllowedModels != nil {
		out.AllowedModels = append([]string(nil), *p.AllowedModels...)
	}
	if p.BlockedModels != nil {
		out.BlockedModels = append([]string(nil), *p.BlockedModels...)
	}
	if p.AllowedEndpoints != nil {
		out.AllowedEndpoints = append([]string(nil), *p.AllowedEndpoints...)
	}
	if p.ForcedModel != nil {
		out.ForcedModel = strings.TrimSpace(*p.ForcedModel)
	}
	if err := out.Validate(); err != nil {
		return out, err
	}
	return out, nil
}

// keyPolicyView is the wire shape of a key's enforceable policy
// (create / update / import responses, GET /v1/me/status).
// It carries policy only — never key material.
type keyPolicyView struct {
	IPAllowlist      []string `json:"ip_allowlist,omitempty"`
	IPBlocklist      []string `json:"ip_blocklist,omitempty"`
	ModelAccessMode  string   `json:"model_access_mode,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
	BlockedModels    []string `json:"blocked_models,omitempty"`
	AllowedEndpoints []string `json:"allowed_endpoints,omitempty"`
	ForcedModel      string   `json:"forced_model,omitempty"`
	KeyGroups        []string `json:"key_groups,omitempty"`
}

// keyPolicyViewOf renders a stored key's policy for responses.
func keyPolicyViewOf(k config.GatewayKey) keyPolicyView {
	p := k.PolicyOf()
	return keyPolicyView{
		IPAllowlist: p.IPAllowlist, IPBlocklist: p.IPBlocklist,
		ModelAccessMode: p.ModelAccessMode, AllowedModels: p.AllowedModels,
		BlockedModels: p.BlockedModels, AllowedEndpoints: p.AllowedEndpoints,
		ForcedModel: p.ForcedModel, KeyGroups: p.KeyGroups,
	}
}

// keyPolicyOmission is one OmniRoute ApiKeyMetadata field we
// deliberately do NOT carry as a per-key policy field, with the
// reason. The rule is: no field without an honest enforcement
// point in a local single-binary router — a stored field that
// is never enforced would be fake configuration.
type keyPolicyOmission struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// keyPolicyNotImplemented is the full omission list (OmniRoute
// apiKeys.ts:108-157 fields not in config.KeyPolicy). Fields
// OmniRoute has that we already enforce elsewhere — budgets
// (Area 5, ledger), RPM/TPM/RPD + rateLimits (keytraffic.go),
// accessSchedule / throttleDelayMs / maxSessions (keyaccess.go,
// keysessions.go), scopes, Disabled — are not omissions.
var keyPolicyNotImplemented = []keyPolicyOmission{
	{Field: "machineId / proxyId", Reason: "no machine-identity or proxy concept in a local single-binary router; nothing to bind a key to"},
	{Field: "allowedConnections / preferredConnections", Reason: "provider connections are not per-key scoped in the engine — there is no connection id to allow or prefer, so the field could not be enforced"},
	{Field: "allowedCombos / allowAutoCombos", Reason: "combos are routing groups, not per-key scoped; no per-key combo gate exists to enforce against"},
	{Field: "allowedQuotas", Reason: "quota-pool membership (M5.4) already is the per-key pool gate; a second allowlist would duplicate it"},
	{Field: "noLog", Reason: "logging is global with redaction; no per-key request-log plumbing exists to toggle"},
	{Field: "autoResolve", Reason: "no per-key model-resolution behaviour exists to toggle"},
	{Field: "streamDefaultMode / cacheDefaultMode / codexServiceMode", Reason: "stream/cache/codex defaults are global config, not per-key request defaults; no per-key plumbing exists"},
	{Field: "disableNonPublicModels / catalogScope", Reason: "listing-only filters on GET /v1/models, whose handler lives in server.go (Wave 3 owns it); recorded as a follow-up, not faked here"},
	{Field: "chaosModeEnabled / compressionEnabled", Reason: "global features, not per-key behaviour; no per-key gate exists"},
	{Field: "allowCcDiscoveryAliases", Reason: "Codex discovery aliases have no equivalent surface in this router"},
}

// ---------------------------------------------------------------------------
// Per-request policy enforcement (the check itself).
// ---------------------------------------------------------------------------

// checkKeyPolicy enforces one key's policy on one request. It
// writes the 403 and returns true when the request must stop
// here, before any traffic is counted or any provider is called.
// Order: IP first (the cheapest, most decisive refusal), then
// endpoint, then model / forced-model, then the key-group layer.
// A key with no policy fields passes untouched, exactly as before.
// The group layer (AK-1) runs only when the request names a model
// (same convention as ModelAllowed) and only when the key belongs
// to at least one group; it is intersected with the per-key
// policy — a request must pass BOTH.
func (s *Server) checkKeyPolicy(w http.ResponseWriter, r *http.Request, key config.GatewayKey, model string) bool {
	if !key.AllowedIP(peerIP(r)) {
		writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
			Message: "client IP is not allowed for this API key",
			Type:    "key_ip_not_allowed",
		}})
		return true
	}
	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}
	if !key.EndpointAllowed(path) {
		writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
			Message: "endpoint " + path + " is not allowed for this API key",
			Type:    "key_endpoint_not_allowed",
		}})
		return true
	}
	if model != "" {
		if key.ForcedModel != "" && model != key.ForcedModel {
			writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
				Message: "this API key is pinned to model " + key.ForcedModel,
				Type:    "key_forced_model",
			}})
			return true
		}
		if !key.ModelAllowed(model) {
			writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
				Message: "model " + model + " is not allowed for this API key",
				Type:    "key_model_not_allowed",
			}})
			return true
		}
		if !s.keyGroupModelAllowed(w, key, model) {
			return true
		}
	}
	return false
}

// keyGroupModelAllowed enforces the key-group layer (AK-1) for one
// named model on the request path, where the provider is unknown
// (so provider-pinned rules never match here — see keygroups.go).
// It writes the 403 and returns false when the request must stop.
// A key in no groups passes untouched. A fail-closed refusal
// (groups missing / all inactive / unreadable) gets its own error
// type naming the cause, so a typo'd membership is diagnosable
// instead of looking like a policy denial.
func (s *Server) keyGroupModelAllowed(w http.ResponseWriter, key config.GatewayKey, model string) bool {
	if len(key.KeyGroups) == 0 {
		return true
	}
	if s.cfg == nil {
		// No config to resolve groups against: a key carrying
		// memberships must never silently become unrestricted.
		s.logger.Printf("key groups: refusing key %s for model %s: no config loaded to resolve groups", key.ID, model)
		writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
			Message: "this API key belongs to key groups that cannot be resolved (no config loaded)",
			Type:    "key_group_missing",
		}})
		return false
	}
	decision := s.cfg.GroupModelDecision(key, model, "")
	if decision.Allowed {
		return true
	}
	if decision.FailClosed {
		s.logger.Printf("key groups: refusing key %s for model %s: %s", key.ID, model, decision.Reason)
		writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
			Message: "this API key belongs to key groups that cannot be resolved (" + string(decision.Reason) + "); model access is refused, never unrestricted",
			Type:    "key_group_missing",
		}})
		return false
	}
	writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
		Message: "model " + model + " is not allowed for this API key by its key groups",
		Type:    "key_group_model_not_allowed",
	}})
	return false
}

// filterModelsByKeyGroups applies the key-group layer (AK-1) to a
// /v1/models list: a key in groups lists only models its groups
// allow (deny-wins, inactive groups grant nothing but still deny,
// provider-pinned rules matched against each row's provider). A
// key in no groups lists everything (unchanged). A fail-closed key
// (groups missing / all inactive / unreadable, or no config to
// resolve against) lists NOTHING — a broken group setup must never
// silently list the whole catalog.
//
// CALL-SITE REMAINDER (Wave 3 owns server.go / models.go): this
// helper is built and tested here, but handleModels
// (server.go:4207) and the provider-scoped list
// (provider_v1.go:390) do not call it yet — each needs one line
// after the key is resolved and before the response is written:
// models = s.filterModelsByKeyGroups(models, key). Until that
// lands, /v1/models lists are unfiltered by groups; the request
// path (checkKeyPolicy above) IS enforced.
func (s *Server) filterModelsByKeyGroups(models []Model, key config.GatewayKey) []Model {
	if len(key.KeyGroups) == 0 {
		return models
	}
	if s.cfg == nil {
		return []Model{}
	}
	out := make([]Model, 0, len(models))
	for _, m := range models {
		provider := m.ProviderID
		if provider == "" {
			provider = m.OwnedBy
		}
		if s.cfg.GroupModelDecision(key, m.ID, provider).Allowed {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// POST /admin/keys/import — import a key definition.
// ---------------------------------------------------------------------------

// keyImportRequest is one POST /admin/keys/import body: the
// exported policy shape (the same fields GatewayKey serialises
// to) plus the raw key. ID is optional (a fresh gk_ id is minted
// when absent); CreatedAt is optional (now when absent).
type keyImportRequest struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Key               string            `json:"key"`
	Scopes            []string          `json:"scopes"`
	BudgetUSD         float64           `json:"budget_usd"`
	ExpiresAt         *time.Time        `json:"expires_at"`
	CreatedAt         *time.Time        `json:"created_at"`
	RequestsPerMinute int               `json:"requests_per_minute"`
	TokensPerMinute   int               `json:"tokens_per_minute"`
	RequestsPerDay    int               `json:"requests_per_day"`
	Guardrails        []string          `json:"guardrails"`
	Disabled          bool              `json:"disabled"`
	Policy            *config.KeyPolicy `json:"policy"`
}

// keyImportResponse is the POST /admin/keys/import reply. It
// never contains the raw key — the caller supplied it — only the
// fingerprint and the masked value, like every other key view.
type keyImportResponse struct {
	KeyID             string        `json:"key_id"`
	Name              string        `json:"name"`
	Created           bool          `json:"created"`
	Fingerprint       string        `json:"fingerprint"`
	Masked            string        `json:"masked"`
	Policy            keyPolicyView `json:"policy"`
	PersistedToConfig bool          `json:"persisted_to_config"`
	Note              string        `json:"note"`
}

// validRawGatewayKey reports whether raw looks like an issued
// gateway key: "crk_" + 32 [A-Za-z0-9] chars. The shape check
// runs before any hash comparison, so a malformed import is a
// 400, never a stored record.
func validRawGatewayKey(raw string) bool {
	if len(raw) != len(config.GatewayKeyPrefix)+32 || !strings.HasPrefix(raw, config.GatewayKeyPrefix) {
		return false
	}
	for _, c := range raw[len(config.GatewayKeyPrefix):] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// handleKeyImport serves POST /admin/keys/import (Tier 2, admin
// scope — enforced by the guard table at registration, like
// rotate/revoke): it stores an externally-issued key definition
// under its hash. The raw key is never logged and never returned.
// Without a persist function it answers 503 rather than storing
// a key that would vanish on restart while the caller believes
// it imported.
// sameStringSet reports whether two string lists hold the same
// values ignoring order (duplicates counted, so ["a","a"] != ["a"]).
func sameStringSet(a, b []string) bool {
	counts := map[string]int{}
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		counts[v]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// keyPolicyEqual reports whether two policies agree on every field.
func keyPolicyEqual(a, b config.KeyPolicy) bool {
	return sameStringSet(a.IPAllowlist, b.IPAllowlist) &&
		sameStringSet(a.IPBlocklist, b.IPBlocklist) &&
		a.ModelAccessMode == b.ModelAccessMode &&
		sameStringSet(a.AllowedModels, b.AllowedModels) &&
		sameStringSet(a.BlockedModels, b.BlockedModels) &&
		sameStringSet(a.AllowedEndpoints, b.AllowedEndpoints) &&
		a.ForcedModel == b.ForcedModel &&
		sameStringSet(a.KeyGroups, b.KeyGroups)
}

// importRetryDiffers reports the first field on which an idempotent
// import retry disagrees with the stored key (G15 re-review nit
// 11): same id + same key hash is a no-op 200 only when the retry
// body agrees with what is stored. Fields the retry body does not
// carry (no policy, no guardrails, no expiry, zero traffic
// limits) are not compared — a bare retry must stay idempotent.
// Budget is never compared: 0 is indistinguishable from "not
// carried". Returns "" when the retry agrees.
func importRetryDiffers(req keyImportRequest, existing config.GatewayKey) string {
	if strings.TrimSpace(req.Name) != existing.Name {
		return "name"
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"chat", "models"}
	}
	if !sameStringSet(scopes, existing.Scopes) {
		return "scopes"
	}
	if req.Policy != nil && !keyPolicyEqual(*req.Policy, existing.PolicyOf()) {
		return "policy"
	}
	if len(req.Guardrails) > 0 && !sameStringSet(req.Guardrails, existing.Guardrails) {
		return "guardrails"
	}
	if req.ExpiresAt != nil {
		if existing.ExpiresAt == nil || !req.ExpiresAt.Equal(*existing.ExpiresAt) {
			return "expires_at"
		}
	}
	if req.RequestsPerMinute != 0 && req.RequestsPerMinute != existing.RequestsPerMinute {
		return "requests_per_minute"
	}
	if req.TokensPerMinute != 0 && req.TokensPerMinute != existing.TokensPerMinute {
		return "tokens_per_minute"
	}
	if req.RequestsPerDay != 0 && req.RequestsPerDay != existing.RequestsPerDay {
		return "requests_per_day"
	}
	return ""
}

func (s *Server) handleKeyImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cfg == nil || s.configPersist == nil {
		writeError(w, http.StatusServiceUnavailable, "key import unavailable: config persistence is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req keyImportRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid key import: expected {\"name\": string, \"key\": \"crk_...\" (32 chars), \"id\": string (optional), \"scopes\": [string] (optional), \"budget_usd\": number (optional), \"expires_at\": \"RFC3339\" (optional), \"policy\": {\"ip_allowlist\": [...], \"model_access_mode\": \"all|restricted\", ...} (optional)}")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !validRawGatewayKey(req.Key) {
		writeError(w, http.StatusBadRequest, "key must be a gateway key (\"crk_\" + 32 alphanumeric characters)")
		return
	}
	if req.BudgetUSD < 0 {
		writeError(w, http.StatusBadRequest, "budget_usd must not be negative")
		return
	}
	if req.RequestsPerMinute < 0 || req.TokensPerMinute < 0 || req.RequestsPerDay < 0 {
		writeError(w, http.StatusBadRequest, "traffic limits must not be negative")
		return
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"chat", "models"}
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		if sc == "" {
			writeError(w, http.StatusBadRequest, "scopes must not contain an empty scope")
			return
		}
		if _, dup := seen[sc]; dup {
			writeError(w, http.StatusBadRequest, "duplicate scope "+sc)
			return
		}
		seen[sc] = struct{}{}
		if !validGatewayScope(sc) {
			writeError(w, http.StatusBadRequest, "unknown scope "+sc)
			return
		}
	}
	if req.Policy != nil {
		if err := req.Policy.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid key import: "+err.Error())
			return
		}
	}
	if len(req.Guardrails) > 0 {
		if err := s.guardrails.Config().ValidateGuardrailNames(req.Guardrails); err != nil {
			s.guardrails.RecordSecurityRefusal(guardrails.SecurityRefusalUnknownGuardrail)
			writeError(w, http.StatusBadRequest, "invalid key import: "+err.Error())
			return
		}
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		var err error
		if id, err = config.NewKeyID(); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not issue a key id for the import")
			return
		}
	}
	hash := config.HashKey(req.Key)

	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	// Idempotent retry (OmniRoute's rule): the same ID with the
	// same key hash is a no-op 200, never a duplicate row — but
	// only when the retry agrees with what is stored (G15
	// re-review nit 11). A retry that disagrees on any carried
	// field is a 409: silently accepting it would pretend
	// nothing changed while the operator's intent differed.
	if existing, ok := s.cfg.FindGatewayKey(id); ok {
		if existing.KeyHash == hash {
			if field := importRetryDiffers(req, existing); field != "" {
				writeError(w, http.StatusConflict, "key import conflicts: same id and key but different "+field+" than the stored key")
				return
			}
			s.logger.Printf("key import: key %s already present (idempotent retry)", id)
			writeJSON(w, http.StatusOK, keyImportResponse{
				KeyID: existing.ID, Name: existing.Name, Created: false,
				Fingerprint: keyFingerprint(existing), Masked: "crk_••••••••",
				Policy: keyPolicyViewOf(existing), PersistedToConfig: true,
				Note: "The key was already imported; nothing changed.",
			})
			return
		}
		writeError(w, http.StatusConflict, "key import conflicts with an existing key id "+id)
		return
	}
	for _, k := range s.cfg.GatewayKeys {
		if k.KeyHash == hash {
			writeError(w, http.StatusConflict, "key import conflicts with an existing credential (the same key is already stored under "+k.ID+")")
			return
		}
	}
	createdAt := time.Now().UTC().Truncate(time.Second)
	if req.CreatedAt != nil {
		createdAt = *req.CreatedAt
	}
	key := config.GatewayKey{
		ID: id, Name: name, KeyHash: hash,
		Scopes: append([]string(nil), scopes...), BudgetUSD: req.BudgetUSD,
		CreatedAt: createdAt, ExpiresAt: req.ExpiresAt,
		RequestsPerMinute: req.RequestsPerMinute, TokensPerMinute: req.TokensPerMinute,
		RequestsPerDay: req.RequestsPerDay,
		Guardrails:     append([]string(nil), req.Guardrails...),
		Disabled:       req.Disabled,
	}
	if req.Policy != nil {
		key.ApplyPolicy(*req.Policy)
	}
	snapshot := s.cfg.Clone()
	if err := s.cfg.AddGatewayKey(key); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not store imported key")
		return
	}
	if err := s.configPersist(); err != nil {
		s.cfg.RestoreFrom(snapshot)
		s.logger.Printf("key import: config persist failed, config rolled back: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not save imported key to the config file")
		return
	}
	// Log the id only — never the raw key.
	s.logger.Printf("key import: key %s imported", key.ID)
	writeJSON(w, http.StatusCreated, keyImportResponse{
		KeyID: key.ID, Name: key.Name, Created: true,
		Fingerprint: keyFingerprint(key), Masked: "crk_••••••••",
		Policy: keyPolicyViewOf(key), PersistedToConfig: true,
		Note: "Imported key stored by hash. The raw key is not returned: you supplied it, and it is never echoed back.",
	})
}
