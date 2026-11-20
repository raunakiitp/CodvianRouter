// Pricing management API (M8.15 — GET / PATCH /api/pricing).
//
// the reference router parity, Area 14 §14b "Pricing" (PARTIAL): the reference router has a
// pricing/sync surface incl. PATCH /api/pricing; we had only the
// GET /dashboard/prices feed. This file adds the management API for
// the same price table — the effective table the ledger actually
// charges with — plus operator overrides. The dashboard page is
// M10's job and is NOT built here.
//
// SOURCES — read, never invented:
//   - The ledger price map (Server.getLedgerPrices, set by
//     SetLedgerPrices): the per-token table the ledger charges
//     with. GET lists it verbatim (converted back to USD per 1M
//     tokens); a pair absent from it at both the pair and the
//     model-level fallback is UNKNOWN (priced:false), never $0.
//   - The operator price book (Server.priceBook, config.PriceBook):
//     where each rate came from ("config" / "price-file" /
//     "models.dev") and the half-known entries the ledger cannot
//     charge (input without output, or vice versa). A pair the
//     ledger does not know but the book half-knows is reported
//     with its configured side(s) and priced:false — the known
//     side is shown, the missing side is never guessed.
//   - Provider-reported prices: a ledger entry the book does not
//     know came from the registry's provider-reported prices
//     (buildLedgerPrices in cmd/codvian-router/main.go); its
//     source reports "provider-reported".
//   - When no ledger price map is wired (book-only server, e.g.
//     tests), GET falls back to the book itself — the same table
//     buildLedgerPrices would derive from it.
//
// PATCH persists through the EXISTING config/store mechanism and
// nothing else: the override lands in s.cfg.ProviderPrices (the
// config file's provider_prices block, the operator's per-pair
// price source) and is saved by s.configPersist (production:
// cfg.Save(path)); the in-memory book and ledger map are updated
// in the same step so GET reflects the override immediately.
// There is no second store. If the save fails, the book, the
// config block and the ledger map are all rolled back and the
// handler answers 500 — runtime and file must never disagree
// about a money rate.
//
// VALIDATION: every rate must be a finite non-negative number
// (config.ModelPrice.Validate, which names the bad field); at
// least one rate field must be present; provider and model must
// be known ids (the registry offers the pair, or the book/ledger
// already knows it — so an already-priced pair can be re-priced
// even after its provider was removed — or the book holds a
// model-level fallback for the model and the provider is known,
// so a pair GET reports as priced via fallback can be PATCHed).
// A bad rate or an unknown id is a 400 naming the field. Overrides merge field-wise into
// any existing pair price: absent fields keep their stored
// values, so a PATCH that names only the input rate does not
// silently drop the output rate.
//
// CONCURRENCY: PATCH mutates the price book's maps, which the
// dashboard feed (handlePrices) reads without a lock. pricingMu
// serialises this file's own GET snapshots against PATCH
// validation AND applies — validation reads the book maps
// (pricingPairKnown -> PriceBook.LookupPair), so it runs inside
// the same critical section as the apply; a PATCH-vs-PATCH race
// between unlocked validation and a locked apply would be a
// fatal concurrent map read/write. The two endpoints in this
// file therefore never race each other; full race-freedom for
// the book needs a server-level mutex, which lives in server.go
// and is outside this slice — see the report.
//
// Route registration and auth live in guard.go + server.go New()
// (coordinator wiring, NOT this file — this slice owns new files
// only). Proposed classification, see the M8.15 report:
//
//	GET   /api/pricing  Tier 3 MANAGEMENT, AuthExempt (read-only,
//	                    no secrets — same class as GET
//	                    /api/providers and GET /api/cache/stats)
//	PATCH /api/pricing  Tier 2 ALWAYS_PROTECTED, Scopes ["admin"]
//	                    (mutates operator money config)
//	s.register(mux, "GET", "/api/pricing", s.handleAPIPricing)
//	s.register(mux, "PATCH", "/api/pricing", s.handleAPIPricing)
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// pricingMu serialises this file's GET snapshots against PATCH
// validation and applies (see the file header: it does not cover
// handlePrices, which reads the book without a lock).
var pricingMu sync.Mutex

// apiPricingRow is one row of the GET / PATCH /api/pricing
// payload: the effective price for one (provider, model) pair, in
// USD per 1M tokens. Pointer fields are nil when that rate is
// unknown; a present 0 means genuinely free. Priced is false when
// no complete price is known for the pair (input and output must
// both be known for the ledger to charge it) — an unpriced pair
// reports unknown, never $0. ModelLevel rows carry no provider:
// they are the fallback price for every provider serving that
// model unless a pair row overrides it. Source is where the rate
// came from: "config", "price-file", "models.dev",
// "provider-reported", or "" when the pair is unpriced.
type apiPricingRow struct {
	Provider             string   `json:"provider,omitempty"`
	Model                string   `json:"model"`
	ModelLevel           bool     `json:"model_level,omitempty"`
	InputUSDPer1M        *float64 `json:"input_usd_per_1m,omitempty"`
	OutputUSDPer1M       *float64 `json:"output_usd_per_1m,omitempty"`
	CachedInputUSDPer1M  *float64 `json:"cached_input_usd_per_1m,omitempty"`
	CacheWrite5mUSDPer1M *float64 `json:"cache_write_5m_usd_per_1m,omitempty"`
	CacheWrite1hUSDPer1M *float64 `json:"cache_write_1h_usd_per_1m,omitempty"`
	BlendedUSDPer1M      *float64 `json:"blended_usd_per_1m,omitempty"`
	Priced               bool     `json:"priced"`
	Source               string   `json:"source,omitempty"`
}

// apiPricingResponse is the GET /api/pricing payload: the
// effective price table (or, with ?model=, the single requested
// pair). Prices is never nil: an empty table is [], never null.
type apiPricingResponse struct {
	Prices []apiPricingRow `json:"prices"`
}

// apiPricingPatchResponse is the PATCH /api/pricing payload: the
// rows as applied, in request order.
type apiPricingPatchResponse struct {
	Prices  []apiPricingRow `json:"prices"`
	Updated int             `json:"updated"`
}

// apiPricingOverride is one PATCH override: the (provider, model)
// pair plus the rates to set. Every rate is a pointer so an
// absent field (nil) means "keep the stored value", distinct from
// an explicit 0 (genuinely free).
type apiPricingOverride struct {
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	InputUSDPer1M        *float64 `json:"input_usd_per_1m,omitempty"`
	OutputUSDPer1M       *float64 `json:"output_usd_per_1m,omitempty"`
	CachedInputUSDPer1M  *float64 `json:"cached_input_usd_per_1m,omitempty"`
	CacheWrite5mUSDPer1M *float64 `json:"cache_write_5m_usd_per_1m,omitempty"`
	CacheWrite1hUSDPer1M *float64 `json:"cache_write_1h_usd_per_1m,omitempty"`
}

// handleAPIPricing serves GET and PATCH /api/pricing (M8.15).
// Auth and tier live in the guard table (coordinator wiring).
func (s *Server) handleAPIPricing(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAPIPricingGet(w, r)
	case http.MethodPatch:
		s.handleAPIPricingPatch(w, r)
	default:
		w.Header().Set("Allow", "GET, PATCH")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAPIPricingGet serves GET /api/pricing: the effective
// price table the ledger actually uses. With ?model= (and
// optionally ?provider=) it returns just that pair, reporting
// priced:false when the pair is unpriced; without ?model= it
// lists the whole table, optionally filtered by ?provider=.
func (s *Server) handleAPIPricingGet(w http.ResponseWriter, r *http.Request) {
	pricingMu.Lock()
	defer pricingMu.Unlock()
	providerID := strings.TrimSpace(r.URL.Query().Get("provider"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	resp := apiPricingResponse{Prices: []apiPricingRow{}}
	if model != "" {
		resp.Prices = append(resp.Prices, s.apiPricingLookup(providerID, model))
	} else {
		resp.Prices = s.apiPricingTable(providerID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// apiPricingTable lists the effective price table: the ledger
// price map when one is wired, else the price book. List mode is
// the ledger-effective table: the ledger rows, plus any book
// pair / synced pair / model-level entry the ledger does not
// already cover — a half-known book pair is shown with its known
// side(s) and priced:false, exactly as single-pair lookup shows
// it, never omitted and never completed by guessing. Rows are
// sorted by (provider, model); model-level fallback rows (empty
// provider) sort first within their model. A ?provider= filter
// keeps pair rows for that provider AND the model-level fallback
// rows, which apply to that provider; it never drops a fallback
// row. The same rule applies on both the ledger and the
// book-only path.
func (s *Server) apiPricingTable(providerFilter string) []apiPricingRow {
	rows := []apiPricingRow{}
	if lp := s.getLedgerPrices(); lp != nil {
		seen := map[ledger.PriceKey]bool{}
		keys := make([]ledger.PriceKey, 0, len(lp))
		for k := range lp {
			if providerFilter != "" && k.Provider != "" && k.Provider != providerFilter {
				continue
			}
			keys = append(keys, k)
			seen[k] = true
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Provider != keys[j].Provider {
				return keys[i].Provider < keys[j].Provider
			}
			return keys[i].Model < keys[j].Model
		})
		for _, k := range keys {
			rows = append(rows, s.apiLedgerRow(k, lp[k]))
		}
		// Book entries the ledger does not cover (half-known
		// pairs, synced pairs, model-level fallbacks): list
		// them as book rows so list mode agrees with the
		// single-pair lookup.
		if s.priceBook != nil {
			for _, k := range s.priceBook.SortedPairs() {
				if providerFilter != "" && k.Provider != providerFilter {
					continue
				}
				lk := ledger.PriceKey{Provider: k.Provider, Model: k.Model}
				if seen[lk] {
					continue
				}
				p, _ := s.priceBook.LookupPair(k.Provider, k.Model)
				rows = append(rows, s.apiBookRow(k.Provider, k.Model, false, p, s.priceBook.SourcePair(k.Provider, k.Model)))
			}
			for _, k := range s.priceBook.SortedSyncedPairs() {
				if providerFilter != "" && k.Provider != providerFilter {
					continue
				}
				lk := ledger.PriceKey{Provider: k.Provider, Model: k.Model}
				if seen[lk] {
					continue
				}
				if _, ok := s.priceBook.LookupPair(k.Provider, k.Model); ok {
					continue // pair row above already covers it
				}
				sp, _ := s.priceBook.SyncedPair(k.Provider, k.Model)
				rows = append(rows, s.apiBookRow(k.Provider, k.Model, false, sp.Price, "models.dev"))
			}
			for _, model := range s.priceBook.SortedModels() {
				lk := ledger.PriceKey{Model: model}
				if seen[lk] {
					continue
				}
				p, _ := s.priceBook.LookupModel(model)
				rows = append(rows, s.apiBookRow("", model, true, p, s.priceBook.SourceModel(model)))
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Provider != rows[j].Provider {
				return rows[i].Provider < rows[j].Provider
			}
			return rows[i].Model < rows[j].Model
		})
		return rows
	}
	// No ledger map wired: the book is the table
	// buildLedgerPrices would derive, so list it instead.
	if s.priceBook == nil {
		return rows
	}
	for _, k := range s.priceBook.SortedPairs() {
		if providerFilter != "" && k.Provider != providerFilter {
			continue
		}
		p, _ := s.priceBook.LookupPair(k.Provider, k.Model)
		rows = append(rows, s.apiBookRow(k.Provider, k.Model, false, p, s.priceBook.SourcePair(k.Provider, k.Model)))
	}
	for _, k := range s.priceBook.SortedSyncedPairs() {
		if providerFilter != "" && k.Provider != providerFilter {
			continue
		}
		sp, _ := s.priceBook.SyncedPair(k.Provider, k.Model)
		rows = append(rows, s.apiBookRow(k.Provider, k.Model, false, sp.Price, "models.dev"))
	}
	// Model-level fallback rows are included even under a
	// ?provider= filter: the fallback applies to that provider.
	for _, model := range s.priceBook.SortedModels() {
		p, _ := s.priceBook.LookupModel(model)
		rows = append(rows, s.apiBookRow("", model, true, p, s.priceBook.SourceModel(model)))
	}
	return rows
}

// apiPricingLookup resolves one (provider, model) pair to its
// effective row: the ledger's pair price, else its model-level
// fallback, else the book's pair / fallback / synced price
// (reported with priced:false when incomplete), else an
// explicitly unpriced row — never $0, never invented.
func (s *Server) apiPricingLookup(providerID, model string) apiPricingRow {
	if lp := s.getLedgerPrices(); lp != nil {
		if p, ok := lp[ledger.PriceKey{Provider: providerID, Model: model}]; ok {
			return s.apiLedgerRow(ledger.PriceKey{Provider: providerID, Model: model}, p)
		}
		if providerID != "" {
			if p, ok := lp[ledger.PriceKey{Model: model}]; ok {
				return s.apiLedgerRow(ledger.PriceKey{Model: model}, p)
			}
		}
	}
	if s.priceBook != nil {
		if p, ok := s.priceBook.LookupPair(providerID, model); ok {
			return s.apiBookRow(providerID, model, false, p, s.priceBook.SourcePair(providerID, model))
		}
		if p, ok := s.priceBook.LookupSyncedPair(providerID, model); ok {
			return s.apiBookRow(providerID, model, false, p, "models.dev")
		}
		if p, ok := s.priceBook.LookupModel(model); ok {
			return s.apiBookRow("", model, true, p, s.priceBook.SourceModel(model))
		}
	}
	return apiPricingRow{Provider: providerID, Model: model, Priced: false}
}

// apiLedgerRow converts one ledger price-map entry (per-token
// rates) to a per-1M row. The ledger only holds complete pairs
// (buildLedgerPrices skips half-known prices), so a ledger row
// is always priced:true. Cached / cache-write tier rates appear
// only when their Known flag is set — an unset tier is unknown,
// never derived from the input rate here.
func (s *Server) apiLedgerRow(key ledger.PriceKey, p ledger.ModelPrice) apiPricingRow {
	per1M := func(perToken float64) *float64 { v := perToken * 1e6; return &v }
	row := apiPricingRow{
		Provider:       key.Provider,
		Model:          key.Model,
		ModelLevel:     key.Provider == "",
		InputUSDPer1M:  per1M(p.PromptPerToken),
		OutputUSDPer1M: per1M(p.CompletionPerToken),
		Priced:         true,
		Source:         s.apiLedgerRowSource(key),
	}
	if p.CachedKnown {
		row.CachedInputUSDPer1M = per1M(p.CachedPromptPerToken)
	}
	if p.CacheWrite5mKnown {
		row.CacheWrite5mUSDPer1M = per1M(p.CacheWrite5mPerToken)
	}
	if p.CacheWrite1hKnown {
		row.CacheWrite1hUSDPer1M = per1M(p.CacheWrite1hPerToken)
	}
	if blended, ok := row.blended(); ok {
		row.BlendedUSDPer1M = &blended
	}
	return row
}

// apiLedgerRowSource attributes one ledger entry: the book's
// source when the book knows the pair (or the model-level
// fallback), else "provider-reported" — the entry came from the
// registry's provider-reported prices.
func (s *Server) apiLedgerRowSource(key ledger.PriceKey) string {
	if s.priceBook == nil {
		return "provider-reported"
	}
	if key.Provider != "" {
		if src := s.priceBook.SourcePair(key.Provider, key.Model); src != "" {
			return src
		}
		return "provider-reported"
	}
	if src := s.priceBook.SourceModel(key.Model); src != "" {
		return src
	}
	return "provider-reported"
}

// apiBookRow converts one price-book entry (per-1M rates) to a
// row. Priced is true only when input and output are both
// configured: a half-known book price is shown with its known
// side(s) and priced:false, never completed by guessing.
func (s *Server) apiBookRow(providerID, model string, modelLevel bool, p config.ModelPrice, source string) apiPricingRow {
	row := apiPricingRow{
		Provider:             providerID,
		Model:                model,
		ModelLevel:           modelLevel,
		InputUSDPer1M:        p.InputUSDPer1M,
		OutputUSDPer1M:       p.OutputUSDPer1M,
		CachedInputUSDPer1M:  p.CachedInputUSDPer1M,
		CacheWrite5mUSDPer1M: p.CacheWrite5mUSDPer1M,
		CacheWrite1hUSDPer1M: p.CacheWrite1hUSDPer1M,
		Priced:               p.InputUSDPer1M != nil && p.OutputUSDPer1M != nil,
		Source:               source,
	}
	if blended, ok := row.blended(); ok {
		row.BlendedUSDPer1M = &blended
	}
	return row
}

// blended returns the mean of the input and output rates, when
// both are known.
func (r apiPricingRow) blended() (float64, bool) {
	if r.InputUSDPer1M == nil || r.OutputUSDPer1M == nil {
		return 0, false
	}
	return (*r.InputUSDPer1M + *r.OutputUSDPer1M) / 2, true
}

// handleAPIPricingPatch serves PATCH /api/pricing: operator price
// overrides. The body is either one override object or
// {"overrides": [...]}. Every override is validated before any
// is applied (all-or-nothing); the apply writes the config
// block, the in-memory book and the ledger map together, then
// persists via configPersist, rolling all three back when the
// save fails.
func (s *Server) handleAPIPricingPatch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	overrides, err := decodePricingPatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(overrides) == 0 {
		writeError(w, http.StatusBadRequest, "at least one price override is required")
		return
	}
	// Deliberate precedence: the 503 (no persist wired) is
	// checked before validation, so an invalid PATCH on a
	// persist-less server answers 503, not 400 — the endpoint
	// is unavailable regardless of body validity.
	if s.cfg == nil || s.configPersist == nil {
		writeServerError(w, http.StatusServiceUnavailable, "price overrides unavailable: no config persistence wired")
		return
	}

	// Validation runs INSIDE pricingMu: it reads the price-book
	// maps (pricingPairKnown -> PriceBook.LookupPair), and an
	// unlocked validation racing a concurrent PATCH apply's map
	// write is a fatal concurrent map read/write. The apply
	// below runs under the same lock, so validation and apply
	// are atomic against each other and against GET. The lock
	// is held across configPersist() below (disk I/O): a slow
	// disk stalls concurrent GET/PATCH, but the save must stay
	// inside the critical section or a concurrent PATCH could
	// interleave between apply and persist and leave runtime
	// and file disagreeing about a money rate.
	pricingMu.Lock()
	defer pricingMu.Unlock()
	for i, o := range overrides {
		if err := validatePricingOverride(s, o); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("overrides[%d]: %v", i, err))
			return
		}
	}
	if s.priceBook == nil {
		s.priceBook = config.NewPriceBook()
	}
	// Snapshots for rollback when the save fails. The
	// ProviderPrices snapshot + apply below run under the
	// config prices write lock (M8.14 race fix, round 3):
	// Clone and Save range ProviderPrices under the prices
	// read lock, so an unlocked apply here would race them.
	// The write lock is released BEFORE configPersist below:
	// Save takes the prices READ lock, so holding the write
	// lock across the persist would deadlock. That release
	// is safe: the only ProviderPrices writer is this PATCH
	// path, and PATCHes are serialised by pricingMu (still
	// held), so no writer can interleave between apply and
	// persist — only readers (Clone/Save), which is exactly
	// what the read lock serialises against. Lock order on
	// this path: pricingMu -> pricesMu -> routingMu (inside
	// Save), one-directional, never reversed.
	s.cfg.LockPrices()
	prevCfg := map[string]map[string]config.ModelPrice{}
	prevBook := map[config.PriceKey]struct {
		price  config.ModelPrice
		ok     bool
		source string
	}{}
	for _, o := range overrides {
		if _, done := prevCfg[o.Provider]; !done {
			if existing, ok := s.cfg.ProviderPrices[o.Provider]; ok {
				cp := make(map[string]config.ModelPrice, len(existing))
				for k, v := range existing {
					cp[k] = v
				}
				prevCfg[o.Provider] = cp
			} else {
				prevCfg[o.Provider] = nil
			}
		}
		key := config.PriceKey{Provider: o.Provider, Model: o.Model}
		if _, done := prevBook[key]; !done {
			p, ok := s.priceBook.Pairs[key]
			prevBook[key] = struct {
				price  config.ModelPrice
				ok     bool
				source string
			}{price: p, ok: ok, source: s.priceBook.PairSources[key]}
		}
	}
	prevLedger := s.getLedgerPrices()
	var prevLedgerCopy ledger.Prices
	if prevLedger != nil {
		prevLedgerCopy = make(ledger.Prices, len(prevLedger))
		for k, v := range prevLedger {
			prevLedgerCopy[k] = v
		}
	}

	applied := make([]apiPricingRow, 0, len(overrides))
	for _, o := range overrides {
		key := config.PriceKey{Provider: o.Provider, Model: o.Model}
		merged := mergePricingOverride(s.priceBook.Pairs[key], o)
		if s.priceBook.Pairs == nil {
			s.priceBook.Pairs = map[config.PriceKey]config.ModelPrice{}
		}
		s.priceBook.Pairs[key] = merged
		if s.priceBook.PairSources == nil {
			s.priceBook.PairSources = map[config.PriceKey]string{}
		}
		s.priceBook.PairSources[key] = "config"
		if s.cfg.ProviderPrices == nil {
			s.cfg.ProviderPrices = map[string]map[string]config.ModelPrice{}
		}
		if s.cfg.ProviderPrices[o.Provider] == nil {
			s.cfg.ProviderPrices[o.Provider] = map[string]config.ModelPrice{}
		}
		s.cfg.ProviderPrices[o.Provider][o.Model] = merged
		applied = append(applied, s.apiBookRow(o.Provider, o.Model, false, merged, "config"))
	}
	// Release the prices write lock before the persist (see
	// above): Save takes the prices read lock.
	s.cfg.UnlockPrices()
	s.refreshLedgerPricesFromBookLocked()
	if err := s.configPersist(); err != nil {
		// Roll everything back: runtime and file must never
		// disagree about a money rate. Re-take the prices
		// write lock for the ProviderPrices rollback.
		s.cfg.LockPrices()
		for providerID, prev := range prevCfg {
			if prev == nil {
				delete(s.cfg.ProviderPrices, providerID)
			} else {
				s.cfg.ProviderPrices[providerID] = prev
			}
		}
		s.cfg.UnlockPrices()
		for key, prev := range prevBook {
			if prev.ok {
				s.priceBook.Pairs[key] = prev.price
			} else {
				delete(s.priceBook.Pairs, key)
			}
			if prev.source == "" {
				delete(s.priceBook.PairSources, key)
			} else {
				s.priceBook.PairSources[key] = prev.source
			}
		}
		s.SetLedgerPrices(prevLedgerCopy)
		writeServerError(w, http.StatusInternalServerError, "could not save price overrides")
		return
	}
	writeJSON(w, http.StatusOK, apiPricingPatchResponse{Prices: applied, Updated: len(applied)})
}

// decodePricingPatch decodes a PATCH body: one override object,
// or {"overrides": [...]}. Unknown fields are rejected, so a
// typo can never silently drop a rate.
func decodePricingPatch(r *http.Request) ([]apiPricingOverride, error) {
	raw, err := readPricingPatchBody(r)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %v", err)
	}
	if _, batch := top["overrides"]; batch {
		var wrapper struct {
			Overrides []apiPricingOverride `json:"overrides"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wrapper); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %v", err)
		}
		return wrapper.Overrides, nil
	}
	var single apiPricingOverride
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&single); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %v", err)
	}
	return []apiPricingOverride{single}, nil
}

// readPricingPatchBody reads the PATCH body, rejecting an empty
// one (MaxBytesReader is already applied by the caller).
func readPricingPatchBody(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %v", err)
	}
	if len(bytes.TrimSpace(buf.Bytes())) == 0 {
		return nil, fmt.Errorf("empty request body: a price override is required")
	}
	return buf.Bytes(), nil
}

// validatePricingOverride checks one override: non-empty ids, at
// least one rate, every rate finite and non-negative (naming the
// field), and known provider/model ids.
func validatePricingOverride(s *Server, o apiPricingOverride) error {
	if strings.TrimSpace(o.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(o.Model) == "" {
		return fmt.Errorf("model is required")
	}
	p := config.ModelPrice{
		InputUSDPer1M:        o.InputUSDPer1M,
		OutputUSDPer1M:       o.OutputUSDPer1M,
		CachedInputUSDPer1M:  o.CachedInputUSDPer1M,
		CacheWrite5mUSDPer1M: o.CacheWrite5mUSDPer1M,
		CacheWrite1hUSDPer1M: o.CacheWrite1hUSDPer1M,
	}
	if !p.Known() {
		return fmt.Errorf("at least one rate field is required for provider %q model %q", o.Provider, o.Model)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	providerKnown, pairKnown := s.pricingPairKnown(o.Provider, o.Model)
	if !providerKnown {
		return fmt.Errorf("unknown provider %q", o.Provider)
	}
	if !pairKnown {
		return fmt.Errorf("unknown model %q for provider %q", o.Model, o.Provider)
	}
	return nil
}

// pricingPairKnown reports whether the provider is known at all
// and whether the registry (or the already-priced book/ledger)
// knows this exact (provider, model) pair. The registry is the
// authority when wired; the book/ledger fallback lets an
// already-priced pair be re-priced after its provider was
// removed, and lets a book-only server validate against the
// prices it already holds. A model-level fallback in the book
// also counts as pair-known when the provider itself is known:
// GET reports that pair as priced via the fallback, so PATCH
// must accept it too. Caller holds pricingMu (PATCH) or the
// read path that already holds it.
func (s *Server) pricingPairKnown(providerID, model string) (providerKnown, pairKnown bool) {
	if ml, ok := s.registry.(dashboardModelLister); ok {
		for _, m := range ml.DashboardModels() {
			if m.ProviderID == providerID {
				providerKnown = true
				if m.ID == model {
					pairKnown = true
				}
			}
		}
	} else if s.registry != nil {
		if models, err := s.registry.Models(context.Background()); err == nil {
			for _, m := range models {
				if m.ProviderID == providerID {
					providerKnown = true
					if m.ID == model {
						pairKnown = true
					}
				}
			}
		}
	}
	if s.priceBook != nil {
		if _, ok := s.priceBook.LookupPair(providerID, model); ok {
			pairKnown, providerKnown = true, true
		}
		if _, ok := s.priceBook.LookupSyncedPair(providerID, model); ok {
			pairKnown, providerKnown = true, true
		}
		for _, k := range s.priceBook.SortedPairs() {
			if k.Provider == providerID {
				providerKnown = true
			}
		}
	}
	if lp := s.getLedgerPrices(); lp != nil {
		if lp.Known(providerID, model) {
			pairKnown, providerKnown = true, true
		}
		for k := range lp {
			if k.Provider == providerID {
				providerKnown = true
			}
		}
	}
	// Model-level fallback: a pair GET reports as priced via
	// the fallback when the book holds one for the model, so a
	// PATCH for that pair must be allowed too when the provider
	// itself is known — otherwise a priced pair would be
	// un-PATCHable on a book-only server. The fallback alone
	// never makes the provider known.
	if s.priceBook != nil && providerKnown {
		if _, ok := s.priceBook.LookupModel(model); ok {
			pairKnown = true
		}
	}
	return providerKnown, pairKnown
}

// mergePricingOverride merges one override field-wise into the
// stored pair price: a rate the override names replaces the
// stored one, a rate it omits keeps the stored value.
func mergePricingOverride(stored config.ModelPrice, o apiPricingOverride) config.ModelPrice {
	if o.InputUSDPer1M != nil {
		stored.InputUSDPer1M = o.InputUSDPer1M
	}
	if o.OutputUSDPer1M != nil {
		stored.OutputUSDPer1M = o.OutputUSDPer1M
	}
	if o.CachedInputUSDPer1M != nil {
		stored.CachedInputUSDPer1M = o.CachedInputUSDPer1M
	}
	if o.CacheWrite5mUSDPer1M != nil {
		stored.CacheWrite5mUSDPer1M = o.CacheWrite5mUSDPer1M
	}
	if o.CacheWrite1hUSDPer1M != nil {
		stored.CacheWrite1hUSDPer1M = o.CacheWrite1hUSDPer1M
	}
	return stored
}

// refreshLedgerPricesFromBookLocked rebuilds the ledger price
// map from the price book, preserving entries the book does not
// know (provider-reported prices). It mirrors buildLedgerPrices
// in cmd/codvian-router/main.go for the book half: only pairs
// with both input and output known enter the map, with the
// cached-input and cache-write tier rates carried when set — a
// half-known price never becomes a ledger price. Caller holds
// pricingMu.
func (s *Server) refreshLedgerPricesFromBookLocked() {
	var next ledger.Prices
	if cur := s.getLedgerPrices(); cur != nil {
		next = make(ledger.Prices, len(cur))
		for k, v := range cur {
			next[k] = v
		}
	} else {
		next = ledger.Prices{}
	}
	if s.priceBook == nil {
		s.SetLedgerPrices(next)
		return
	}
	toLedger := func(p config.ModelPrice) (ledger.ModelPrice, bool) {
		if p.InputUSDPer1M == nil || p.OutputUSDPer1M == nil {
			return ledger.ModelPrice{}, false
		}
		mp := ledger.ModelPrice{
			PromptPerToken:     *p.InputUSDPer1M / 1e6,
			CompletionPerToken: *p.OutputUSDPer1M / 1e6,
		}
		if p.CachedInputUSDPer1M != nil {
			mp.CachedPromptPerToken = *p.CachedInputUSDPer1M / 1e6
			mp.CachedKnown = true
		}
		if p.CacheWrite5mUSDPer1M != nil {
			mp.CacheWrite5mPerToken = *p.CacheWrite5mUSDPer1M / 1e6
			mp.CacheWrite5mKnown = true
		}
		if p.CacheWrite1hUSDPer1M != nil {
			mp.CacheWrite1hPerToken = *p.CacheWrite1hUSDPer1M / 1e6
			mp.CacheWrite1hKnown = true
		}
		return mp, true
	}
	for _, key := range s.priceBook.SortedPairs() {
		p, _ := s.priceBook.LookupPair(key.Provider, key.Model)
		if mp, ok := toLedger(p); ok {
			next[ledger.PriceKey{Provider: key.Provider, Model: key.Model}] = mp
		}
	}
	for _, key := range s.priceBook.SortedSyncedPairs() {
		if _, ok := s.priceBook.LookupModel(key.Model); ok {
			continue // operator model-level price wins
		}
		sp, _ := s.priceBook.SyncedPair(key.Provider, key.Model)
		if mp, ok := toLedger(sp.Price); ok {
			next[ledger.PriceKey{Provider: key.Provider, Model: key.Model}] = mp
		}
	}
	for _, model := range s.priceBook.SortedModels() {
		p, _ := s.priceBook.LookupModel(model)
		if mp, ok := toLedger(p); ok {
			next[ledger.PriceKey{Model: model}] = mp
		}
	}
	s.SetLedgerPrices(next)
}
