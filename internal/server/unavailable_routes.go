package server

import (
	"net/http"
	"sort"
)

// unavailable_routes.go: the closed list of routes that answer
// 501 feature_unavailable ON PURPOSE.
//
// Each of these routes exists so the dashboard and API reference
// can name the feature, but the capability behind it is not built.
// Before Phase 0 most of them accepted settings into a process-local
// map that nothing in the router ever read (budget limits that were
// never enforced, an IP filter that filtered nothing, management
// tokens that authenticated nothing, a "pricing sync" that synced
// nothing, log feeds that nothing wrote to). Answering 200 to those
// told the operator a control was in force when it was not.
//
// Now every such route answers 501 with code feature_unavailable,
// the reason, and, where one exists, the real route to use instead.
// scripts/audit-fake.sh and TestUnavailableRoutesHonest both read
// this list: a route registered with unavailableRoute must have an
// entry here, and every StatusNotImplemented answer in production
// code must come from this handler.

// unavailableRoute is one documented unavailable feature.
type unavailableRoute struct {
	Reason  string // why it is unavailable
	UseInst string // the real route or command to use instead ("" = none yet)
}

var budgetUse = "POST /api/usage/budget (calendar budgets) or POST /dashboard/key-budgets (rolling 5-hour budget); both are enforced"

// unavailableRoutes is keyed "METHOD path" exactly as registered.
var unavailableRoutes = map[string]unavailableRoute{
	// Tier 1 system features.
	"POST /admin/service/install": {Reason: "system service installation is not supported in this build", UseInst: "`codvian-router restart` starts a supervised background gateway"},
	"POST /admin/plugin/load":     {Reason: "dynamic plugin loading is unsupported by design: plugins are compiled-in and enabled from config at startup (docs/plugins.md)"},
	"POST /admin/provider/login":  {Reason: "provider sign-in (OAuth / device-code login) is not supported in this build", UseInst: "POST /api/providers with an API key"},

	// Key extensions whose settings were never enforced.
	"PATCH /admin/keys/{id}/budget":           {Reason: "this route stored limits in memory and nothing enforced them", UseInst: budgetUse},
	"PATCH /admin/keys/{id}/budget-warn":      {Reason: "this route stored a threshold in memory and nothing read it", UseInst: "POST /api/usage/budget (warning threshold field)"},
	"PATCH /admin/keys/{id}/budget-reset":     {Reason: "this route stored a reset window in memory and nothing read it", UseInst: "POST /api/usage/budget (reset interval field)"},
	"GET /admin/budget-templates":             {Reason: "budget templates are not implemented", UseInst: budgetUse},
	"POST /admin/budget-templates":            {Reason: "budget templates are not implemented", UseInst: budgetUse},
	"POST /admin/budget-templates/{id}/apply": {Reason: "budget templates are not implemented", UseInst: budgetUse},
	"POST /admin/keys/{id}/lease":             {Reason: "exclusive key leases are not implemented: nothing enforced the stored flag"},
	"GET /admin/management-tokens":            {Reason: "management tokens are not implemented: issued tokens never authenticated", UseInst: "POST /admin/keys with the admin scope"},
	"POST /admin/management-tokens":           {Reason: "management tokens are not implemented: issued tokens never authenticated", UseInst: "POST /admin/keys with the admin scope"},
	"GET /admin/management-tokens/{id}":       {Reason: "management tokens are not implemented", UseInst: "POST /admin/keys with the admin scope"},
	"DELETE /admin/management-tokens/{id}":    {Reason: "management tokens are not implemented", UseInst: "DELETE /admin/keys/{id}"},
	"GET /admin/key-pools":                    {Reason: "key fair-share pools are not implemented: stored pools never affected routing", UseInst: "GET /api/quota-pools"},
	"POST /admin/key-pools":                   {Reason: "key fair-share pools are not implemented", UseInst: "POST /api/quota-pools"},
	"GET /admin/key-pools/{id}":               {Reason: "key fair-share pools are not implemented", UseInst: "GET /api/quota-pools"},
	"PATCH /admin/key-pools/{id}":             {Reason: "key fair-share pools are not implemented", UseInst: "POST /api/quota-pools"},
	"DELETE /admin/key-pools/{id}":            {Reason: "key fair-share pools are not implemented", UseInst: "POST /api/quota-pools"},
	"GET /api/settings/ip-filter":             {Reason: "a global IP filter is not implemented: the stored lists filtered nothing", UseInst: "per-key ip_allowlist / ip_blocklist on the gateway key (enforced at auth)"},
	"PATCH /api/settings/ip-filter":           {Reason: "a global IP filter is not implemented: the stored lists filtered nothing", UseInst: "per-key ip_allowlist / ip_blocklist on the gateway key (enforced at auth)"},

	// Combo settings that were never read by the router.
	"GET /api/combos/{id}/capabilities":    {Reason: "capability-specific combo behaviour is not implemented"},
	"PUT /api/combos/{id}/capabilities":    {Reason: "capability-specific combo behaviour is not implemented"},
	"GET /api/settings/combo-tuning":       {Reason: "combo tuning settings are not implemented: stored values never reached the router"},
	"PATCH /api/settings/combo-tuning":     {Reason: "combo tuning settings are not implemented: stored values never reached the router"},
	"GET /api/settings/account-fallback":   {Reason: "account fallback strategy selection is not implemented"},
	"PATCH /api/settings/account-fallback": {Reason: "account fallback strategy selection is not implemented"},

	// Log feeds nothing ever wrote to.
	"GET /api/logs/proxy":                {Reason: "no proxy log is recorded", UseInst: "GET /dashboard/logs (ledger-backed request log)"},
	"GET /api/logs/console":              {Reason: "no console feed is recorded here", UseInst: "GET /api/logs (captured server log lines)"},
	"GET /api/logs/timeline/{id}":        {Reason: "per-request step timelines are not recorded", UseInst: "GET /dashboard/trace (route trace of the last request)"},
	"GET /api/logs/activity":             {Reason: "no activity feed is recorded", UseInst: "GET /dashboard/logs"},
	"GET /api/logs/conversations":        {Reason: "conversation transcripts are not recorded here", UseInst: "GET /v1/responses (stored Responses API conversations)"},
	"GET /api/logs/compression":          {Reason: "no per-request compression log is recorded", UseInst: "GET /api/analytics/compression"},
	"GET /api/logs/audit":                {Reason: "this audit feed was never written", UseInst: "GET /dashboard/audit"},
	"GET /api/logs/destinations":         {Reason: "log export destinations are not implemented: nothing shipped logs to them", UseInst: "GET /api/logs/export"},
	"POST /api/logs/destinations":        {Reason: "log export destinations are not implemented", UseInst: "GET /api/logs/export"},
	"GET /api/logs/destinations/{id}":    {Reason: "log export destinations are not implemented", UseInst: "GET /api/logs/export"},
	"PATCH /api/logs/destinations/{id}":  {Reason: "log export destinations are not implemented", UseInst: "GET /api/logs/export"},
	"DELETE /api/logs/destinations/{id}": {Reason: "log export destinations are not implemented", UseInst: "GET /api/logs/export"},

	// Settings stores that nothing in the router read.
	"GET /api/features":                     {Reason: "these feature flags were not wired to the features they name", UseInst: "the config file (each feature's own setting)"},
	"PATCH /api/features":                   {Reason: "these feature flags were not wired to the features they name", UseInst: "the config file (each feature's own setting)"},
	"GET /api/settings/cors":                {Reason: "CORS origin settings are not implemented: the stored list was never applied"},
	"PATCH /api/settings/cors":              {Reason: "CORS origin settings are not implemented: the stored list was never applied"},
	"GET /api/settings/blocked-providers":   {Reason: "the stored blocked-provider list was never applied", UseInst: "POST /api/providers/{id}/enabled (disable a provider)"},
	"PATCH /api/settings/blocked-providers": {Reason: "the stored blocked-provider list was never applied", UseInst: "POST /api/providers/{id}/enabled (disable a provider)"},
	"GET /api/settings/ai":                  {Reason: "these AI defaults were never applied to requests"},
	"PATCH /api/settings/ai":                {Reason: "these AI defaults were never applied to requests"},
	"GET /api/settings/modality":            {Reason: "modality bridge settings are not implemented"},
	"PATCH /api/settings/modality":          {Reason: "modality bridge settings are not implemented"},
	"POST /api/pricing/sync":                {Reason: "this route reported a sync without fetching any prices", UseInst: "PATCH /api/pricing (set rates) or GET /api/pricing (current book)"},
	"GET /api/pricing/sync/status":          {Reason: "no pricing sync runs, so there is no status", UseInst: "GET /api/pricing"},
	"GET /api/settings/transform":           {Reason: "the transform pipeline is not implemented: stored operations were never applied"},
	"PUT /api/settings/transform":           {Reason: "the transform pipeline is not implemented: stored operations were never applied"},
	"GET /api/settings/sidebar":             {Reason: "sidebar customisation is not implemented: the dashboard never read it"},
	"PUT /api/settings/sidebar":             {Reason: "sidebar customisation is not implemented: the dashboard never read it"},
	"GET /api/settings/advanced":            {Reason: "these advanced settings (body cap, lockout, aliases) were never applied"},
	"PATCH /api/settings/advanced":          {Reason: "these advanced settings (body cap, lockout, aliases) were never applied"},
	"GET /api/settings/stickiness":          {Reason: "session stickiness settings are not implemented: stored values were never applied"},
	"PATCH /api/settings/stickiness":        {Reason: "session stickiness settings are not implemented: stored values were never applied"},
	"GET /api/settings/switches":            {Reason: "these global switches were not wired to anything"},
	"PATCH /api/settings/switches":          {Reason: "these global switches were not wired to anything"},
	"GET /api/settings/response-validation": {Reason: "response validation rules are not implemented: stored rules were never evaluated"},
	"PUT /api/settings/response-validation": {Reason: "response validation rules are not implemented: stored rules were never evaluated"},

	// Analytics with no data source.
	"GET /api/analytics/cost-by-account": {Reason: "spend is not partitioned by provider account in the ledger", UseInst: "GET /api/analytics/cost-by-key"},
}

// unavailableRoute returns the handler for a route in
// unavailableRoutes. It panics at registration for an undocumented
// route, so an unlisted 501 cannot be added by accident.
func (s *Server) unavailableRoute(method, path string) http.HandlerFunc {
	key := method + " " + path
	u, ok := unavailableRoutes[key]
	if !ok {
		panic("server: unavailableRoute(" + key + ") has no entry in unavailableRoutes")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		body := map[string]any{
			"error": map[string]any{
				"message": u.Reason,
				"type":    "feature_unavailable",
				"code":    "feature_unavailable",
			},
			"available": false,
		}
		if u.UseInst != "" {
			body["use_instead"] = u.UseInst
		}
		writeJSON(w, http.StatusNotImplemented, body)
	}
}

// registerUnavailable registers every unavailableRoutes entry.
func (s *Server) registerUnavailable(mux *http.ServeMux) {
	keys := make([]string, 0, len(unavailableRoutes))
	for k := range unavailableRoutes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var method, path string
		for i := 0; i < len(k); i++ {
			if k[i] == ' ' {
				method, path = k[:i], k[i+1:]
				break
			}
		}
		s.register(mux, method, path, s.unavailableRoute(method, path))
	}
}
