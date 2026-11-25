// DB export (POST /admin/db/export, Tier 2 ALWAYS_PROTECTED).
//
// Exports the config plus the ledger usage history as one
// downloadable JSON file. The export is read-only: it reads the
// ledger, it never resets it — usage history is preserved exactly,
// and the response says how many events were included so a capped
// export can never be mistaken for the whole history.
//
// Credential boundary: gateway-key auth (always required, even
// when Tier 3 auth is relaxed) AND the typed confirmation phrase
// "EXPORT" in the JSON body — a key alone is not enough, exactly
// like the other Tier 2 destructive routes. The handler reuses
// readDangerConfirm (danger.go) for that check.
//
// Redaction: the file contains no provider keys, no gateway-key
// hashes, no base URLs (a base URL can carry embedded credentials),
// and no key material of any kind — only counts, names, and the
// operator-visible settings. Provider rows reuse
// settingsExportProvider (guard.go). Gateway keys appear only as a
// count. With no config loaded the route answers 503 (honest
// "unavailable", not an empty export); with no ledger wired the
// usage section is honestly empty with a note, never invented.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// dbExportConfirm is the exact phrase the caller must type (and
// send) to export the database.
const dbExportConfirm = "EXPORT"

// dbExportMaxEvents caps the usage events in one export file: the
// ledger's RecentEvents cap (500). The response reports the total
// so a reader can tell a capped file from a complete one.
const dbExportMaxEvents = 500

// dbExportConfig is the redacted config section of the export.
type dbExportConfig struct {
	Version          int                          `json:"version"`
	Listen           string                       `json:"listen"`
	BudgetUSD        float64                      `json:"budget_usd"`
	FailClosed       bool                         `json:"fail_closed"`
	TelemetryEnabled bool                         `json:"telemetry_enabled"`
	GatewayKeyCount  int                          `json:"gateway_key_count"`
	Providers        []settingsExportProvider     `json:"providers"`
	AutoRouter       config.AutoRouterConfig      `json:"auto_router,omitempty"`
	Guardrails       *config.GuardrailsConfig     `json:"guardrails,omitempty"`
	Routing          *config.RoutingConfig        `json:"routing,omitempty"`
	Prices           map[string]config.ModelPrice `json:"prices,omitempty"`
	// ProviderPrices is the per-(provider, model) price block;
	// like Prices it carries operator-configured prices only,
	// never keys or secrets.
	ProviderPrices map[string]map[string]config.ModelPrice `json:"provider_prices,omitempty"`
}

// dbExportUsage is the usage-history section of the export: the
// all-time summary plus the individual events (newest first,
// capped — see dbExportMaxEvents). Events carry metadata only
// (provider, model, tokens, cost, latency, outcome); the ledger
// stores no prompt or response content, so none can leak here.
type dbExportUsage struct {
	Summary        ledger.UsageSummary `json:"summary"`
	Events         []ledger.UsageEvent `json:"events"`
	EventsTotal    int                 `json:"events_total"`
	EventsIncluded int                 `json:"events_included"`
	Truncated      bool                `json:"truncated"`
	Note           string              `json:"note,omitempty"`
}

// dbExport is the whole exported file.
type dbExport struct {
	Format     string         `json:"format"`
	ExportedAt time.Time      `json:"exported_at"`
	Config     dbExportConfig `json:"config"`
	Usage      dbExportUsage  `json:"usage"`
	Secrets    string         `json:"secrets"`
	Note       string         `json:"note"`
}

// handleDBExport serves POST /admin/db/export (Tier 2): gateway-key
// auth plus the typed confirmation phrase EXPORT, then the
// redacted config + usage history as a downloadable JSON file.
func (s *Server) handleDBExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.readDangerConfirm(w, r, dbExportConfirm); !ok {
		return
	}
	cfg := s.cfg
	if cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "db export unavailable: no config loaded")
		return
	}
	// M8.14 race fix, round 3: cfg.Providers is NOT
	// startup-only — configProviderStore.Save
	// (cmd/codvian-router/main.go) replaces the whole slice
	// at runtime on every live provider add/remove (via
	// internal/server/providers_add.go), so this range is
	// NOT safe by construction. There is no clean lock to
	// take here: the provider store has no mutex of its own
	// and cfg.Providers has no guarding mutex anywhere in
	// the tree (every other reader ranges it unlocked too),
	// so a snapshot lock would need a new mutex wired
	// through the store and all readers — a redesign, not a
	// one-line fix. Recorded as an open follow-up (see
	// docs/LOG.md, M8.14 race fix round 3); this comment
	// corrects the record so nobody re-assumes startup-only.
	providers := []settingsExportProvider{}
	for _, p := range cfg.Providers {
		models := []string{}
		for _, m := range p.Models {
			models = append(models, m.ID)
		}
		providers = append(providers, settingsExportProvider{
			ID: p.ID, Name: p.Name, WireFormat: p.WireFormat,
			CatalogID: p.CatalogID, Verification: p.Verification,
			Models: models, Enabled: p.Enabled,
		})
	}
	usage := dbExportUsage{
		Summary: ledger.UsageSummary{ByProvider: []ledger.ProviderUsage{}, ByModel: []ledger.ModelUsage{}, ByKey: []ledger.KeyUsage{}},
		Events:  []ledger.UsageEvent{},
	}
	if s.ledger == nil {
		usage.Note = "no ledger configured — usage history is empty, not reset"
	} else {
		sum, err := s.ledger.Summary(r.Context(), time.Time{}, time.Time{})
		if err != nil {
			s.logger.Printf("db export: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not read usage history")
			return
		}
		events, err := s.ledger.RecentEvents(r.Context(), dbExportMaxEvents, ledger.LogAll)
		if err != nil {
			s.logger.Printf("db export: ledger events failed: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not read usage history")
			return
		}
		usage.Summary = sum
		usage.Events = events
		usage.EventsTotal = sum.Requests
		usage.EventsIncluded = len(events)
		usage.Truncated = len(events) < sum.Requests
		if usage.Truncated {
			usage.Note = fmt.Sprintf("usage history capped at the %d most recent events of %d total — the summary totals cover all %d", dbExportMaxEvents, sum.Requests, sum.Requests)
		}
	}
	now := time.Now().UTC()
	// M8.14 race fix, round 2: the encoder below ranges the
	// routing maps (Groups, ChainStrategies, ...) while a combo
	// mirror may rewrite them — encode a deep copy taken under
	// the routing read lock (RoutingSnapshot) instead of the
	// live block. The copy is a true deep copy of every map
	// the encoder ranges inside Routing (Groups,
	// ChainStrategies, ChainWeights incl. inner maps,
	// ChainCooldownModes, RefusalChains, ModelEquivalents,
	// StrategyTrees, plus the ContextFallbackModels slice and
	// the WeightedWeights / ModelLockout / WaitBudgets
	// pointers), so no map the encoder touches is shared
	// with the live config.
	routing := cfg.RoutingSnapshot()
	// Prices / ProviderPrices are operator-configured maps
	// the encoder ranges too; snapshot them so the payload is
	// fully detached from the live config. Both snapshots are
	// taken under the prices read lock (Config.PricesSnapshot /
	// Config.ProviderPricesSnapshot): ProviderPrices is written
	// by the pricing PATCH under pricingMu + the prices write
	// lock (pricing_api.go) AND by RestoreFrom under the prices
	// write lock alone, and Prices is written by RestoreFrom
	// the same way — so the old copy here, taken under
	// pricingMu alone (and Prices ranged with no lock at all),
	// was unserialised against RestoreFrom. The prices read
	// lock serialises both snapshots against both writers.
	// (Guardrails is a pointer the encoder reads but nothing
	// mutates live, and the AutoRouter maps have no live writer
	// either — both verified by grep; they stay shared reads.)
	prices := cfg.PricesSnapshot()
	providerPrices := cfg.ProviderPricesSnapshot()
	// GatewayKeyCount reads len(cfg.GatewayKeys) unlocked
	// (M8.14 race fix, round 3 — same open follow-up as
	// cfg.Providers above): keystatus mutates the slice
	// under keyMu, danger revoke-all under dangerMu, and
	// the bootstrap issuer (cmd/codvian-router/main.go)
	// appends with no lock at all, so no single mutex
	// serialises the length read — snapshotting under
	// keyMu alone would still race revoke-all and
	// bootstrap. A real fix needs one guard for the
	// slice (or a count accessor on Config) wired through
	// all three writers; recorded as an open follow-up.
	payload := dbExport{
		Format:     ledger.ExportFormat,
		ExportedAt: now,
		Config: dbExportConfig{
			Version: cfg.Version, Listen: cfg.Listen,
			BudgetUSD: cfg.BudgetUSD, FailClosed: cfg.FailClosed,
			TelemetryEnabled: cfg.TelemetryEnabled,
			GatewayKeyCount:  len(cfg.GatewayKeys),
			Providers:        providers,
			AutoRouter:       cfg.AutoRouter,
			Guardrails:       cfg.Guardrails,
			Routing:          routing,
			Prices:           prices,
			ProviderPrices:   providerPrices,
		},
		Usage:   usage,
		Secrets: "redacted — provider keys, gateway-key hashes and base URLs are never exported",
		Note:    "Read-only export: usage history is preserved, never reset.",
	}
	filename := fmt.Sprintf("codvian-router-db-export-%s.json", now.Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// AUD-CODE-002: marshal before committing the status. The export can
	// be large, so it is encoded into a buffer rather than streamed after
	// a committed 200 — a failure then answers a clean 500 instead of a
	// half-written download.
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export could not be encoded")
		return
	}
	buf = append(buf, '\n')
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
	s.logger.Printf("db export: exported config (%d provider(s), %d gateway key(s)) + %d usage event(s)", len(providers), len(cfg.GatewayKeys), usage.EventsIncluded)
}
