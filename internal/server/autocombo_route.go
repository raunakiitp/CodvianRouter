// Live pool supply for GET /v1/auto-combo/{channel}/candidates
// (Phase D, builder D1). The pure listing lives in
// autocombo_handler.go; this file only assembles its input from
// real server/engine state:
//
//   - Channel's candidate pool: the autorouter's live tier models
//     (s.autoRouter.ModelsForTier — config entries plus models
//     registered live via RegisterTierModels).
//   - Breaker states: the engine's live BreakerStates() (via the
//     breakerReporter interface already used by cooldown.go).
//     The engine reports only open/closed, so BreakerState here is
//     "OPEN" or "CLOSED" — never an invented HALF_OPEN/DEGRADED.
//   - Per-key connections: the provider's key pool, live registry
//     first (ProviderKeyManager.Get + EffectiveKeys, which also
//     synthesises the legacy single-key id), config fallback
//     second (s.cfg.Providers[].Keys). A provider with no known
//     key yields no rows for its models — the handler's own rule
//     for a candidate with neither a connection nor an allowlist
//     — never an invented connection id.
//   - Connection cooldowns: the engine's live CooldownTargets()
//     (per-(provider, key) cooling pairs, via cooldownReporter).
//   - Model lockouts: the engine's live ModelLockouts() (via the
//     lockoutReporter interface below). A row is locked when its
//     (provider, key, model) triple is live-locked.
//
// Exclusions: the caller's per-API-key exclusion set comes from the
// operator-managed overrides file (autoCandidateOverrideStore, B2-15):
// (key_id, channel, connection_id) rows in
// <data-dir>/auto-candidate-overrides.json, reloaded when the file
// changes. The coordinator (server.go) resolves the caller key and
// passes the set in; an empty set means no exclusions, never an
// invented one.
//
// FreeAccessExclusion is price-derived (see
// autoComboFreeAccessExclusion): known-$0 by the price book, else
// "not-in-catalog". The reference's deeper verdicts (hard-stop
// guarantee, live allowance exhausted/unknown) have no data source
// in this repo.
//
// Model lockouts: the engine's live ModelLockouts() (via the
// lockoutReporter interface below, mirroring breakerReporter). A
// row is locked when its (provider, key, model) triple is
// live-locked — the lockout table keys on the request model as the
// engine saw it (the bare id the client sent), while the pool
// carries "provider/model" ids, so both sides are matched on the
// bare model id (see autoComboModelLockouts).
//
// NOTE (degrade lane): the production serving engine is
// degrade.StaleEngine (or its savingsEngine decorator), and it
// does not yet forward ModelLockouts — it forwards BreakerStates
// and CooldownTargets only (internal/degrade/degrade.go). Until
// the degrade lane adds a ModelLockouts forwarder mirroring
// BreakerStates, the assertion below fails closed in serve mode
// and ModelLocked reads false there; it is live wherever the
// server holds the bare *router.Engine (tests, non-degrade paths).
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
	"github.com/raunakiitp/codvian-router/internal/sla"
)

// errAutoComboNoRouter is returned by buildAutoComboPool when no
// auto-router is wired; the handler maps it to 503.
var errAutoComboNoRouter = errors.New("auto-router is not configured")

// autoComboVirtualTiers maps the six built-in virtual models to
// their candidate tiers. It mirrors the autorouter's virtualModels
// table (internal/autorouter/autorouter.go:205-210), which is
// unexported; the mapping is pinned here with its source so a
// drift is visible next to the citation, not hidden.
var autoComboVirtualTiers = map[string][]autorouter.Tier{
	"auto":           autorouter.AllTiers,
	"auto/fast":      {autorouter.TierQuickQA, autorouter.TierChat},
	"auto/cheap":     {autorouter.TierQuickQA, autorouter.TierChat},
	"auto/coding":    {autorouter.TierCoding, autorouter.TierReasoning},
	"auto/code":      {autorouter.TierCoding, autorouter.TierReasoning},
	"auto/reasoning": {autorouter.TierReasoning, autorouter.TierCoding},
}

// buildAutoComboPool builds the live candidate pool for one
// channel segment (the part after "auto/", or "auto" itself). An
// unknown channel returns a nil pool and nil error — the caller
// (ListAutoComboCandidates) maps it to 404. A non-nil error means
// the pool cannot honestly be built for a KNOWN channel (today:
// auto/subscription and auto/lkgp, whose resolutions are
// fail-closed and need live quota / last-good signals this
// builder does not have); the caller maps it to 503.
func (s *Server) buildAutoComboPool(channel string) ([]AutoComboCandidateInput, error) {
	if s.autoRouter == nil {
		return nil, errAutoComboNoRouter
	}
	fullID := channel
	if channel != "auto" {
		fullID = "auto/" + channel
	}
	if !autorouter.IsKnownAutoID(fullID) {
		return nil, nil
	}
	modelIDs, err := s.autoComboChannelModels(fullID)
	if err != nil {
		return nil, err
	}
	breakers := s.autoComboBreakerStates()
	cooling := s.autoComboCoolingKeys()
	locked := s.autoComboModelLockouts()
	pool := []AutoComboCandidateInput{}
	for _, model := range modelIDs {
		prov, ok := splitAutoComboModel(model)
		if !ok {
			// A tier model with no "provider/" prefix names
			// no provider, so no connection can serve it;
			// skipping it is the handler's no-row rule, not
			// a silent invention.
			continue
		}
		// Removed providers never appear in the candidate
		// pool (G19 Area 1, CT-2).
		if s.autoComboProviderRemoved(prov) {
			continue
		}
		_, bare, _ := strings.Cut(model, "/")
		for _, keyID := range s.autoComboKeyIDs(prov) {
			pool = append(pool, AutoComboCandidateInput{
				Provider:            prov,
				ConnectionID:        keyID,
				Model:               model,
				BreakerState:        breakers[prov],
				ConnectionCooldown:  cooling[prov+"\x00"+keyID],
				ModelLocked:         locked[prov+"\x00"+keyID+"\x00"+bare],
				FreeAccessExclusion: s.autoComboFreeAccessExclusion(model),
			})
		}
	}
	return pool, nil
}

// autoComboChannelModels returns the channel's candidate model
// ids, deduped in first-seen order.
func (s *Server) autoComboChannelModels(fullID string) ([]string, error) {
	if tiers, ok := autoComboVirtualTiers[fullID]; ok {
		return s.autoComboTierUnion(tiers), nil
	}
	// Any other known channel: resolve it against the live tier
	// union (every tier candidate, as CatalogModels carrying
	// only Provider+Model — the resolver's own filters are
	// fail-open on such a catalog, so the free / reasoning /
	// premium / vision filters are re-applied below from live
	// signals instead of trusting the resolver's pick alone).
	catalog := []autorouter.CatalogModel{}
	for _, model := range s.autoComboTierUnion(autorouter.AllTiers) {
		prov, ok := splitAutoComboModel(model)
		if !ok {
			continue
		}
		catalog = append(catalog, autorouter.CatalogModel{Provider: prov, Model: model})
	}
	if len(catalog) == 0 {
		return nil, nil
	}
	res, err := autorouter.ResolveAutoVariantWithPolicyAndRanker(fullID, catalog, "", sla.DefaultPolicy(), s.autoRouter.RankModels())
	if err != nil {
		// Fail-closed ids (auto/subscription: no verified
		// subscription-quota signal; auto/lkgp: no last-good
		// recorded) — an error, never a billable fallback pool.
		return nil, err
	}
	if len(res.Panel) > 0 {
		// auto/chaos (and best-chaos): the panel IS the pool.
		return res.Panel, nil
	}
	var models []string
	switch {
	case res.Tier != "":
		// Single-tier ids (auto/chat, best-/pro-coding,
		// best-/pro-reasoning, best-/pro-chat, and
		// <category>:<tier> compositions): that tier's models.
		models = s.autoRouter.ModelsForTier(res.Tier)
	case strings.HasSuffix(fullID, "/thrifty") || strings.HasSuffix(fullID, ":thrifty"):
		// auto/thrifty (and <category>:thrifty): the rung
		// order is the pool; unknown quota is admitted by
		// the resolver, so every catalog model survives.
		return res.Ordered, nil
	default:
		// Everything else (auto/smart, auto/offline, best-/pro-fast,
		// best-free, vision ids): the whole tier union — the id
		// selects a scoring strategy, not a narrower pool, and no
		// narrower pool can honestly be derived from the signals
		// available here.
		models = make([]string, 0, len(catalog))
		for _, m := range catalog {
			models = append(models, m.Model)
		}
	}
	// Narrowings mirror the resolver's own filters (resolveFlat /
	// resolveComposed in internal/autorouter/autovariants.go) and
	// are fail-open exactly like them: a narrowing that matches
	// nothing keeps the full pool, because dispatch would do the
	// same. "free" keeps known-$0 models (the production
	// derivation: priceBook.BlendedUSDPer1M, ok && == 0 —
	// cmd/codvian-router/main.go SetAutoCatalog; an unknown price is
	// never $0). "reasoning" keeps models whose production-style
	// category is reasoning (first-wins tier membership in
	// AllTiers order — main.go SetAutoCatalog). "pro" keeps models
	// whose economic tier classifies premium from the real price
	// (scoring.ClassifyPriceTier, input leg when the registry
	// model carries one, else the blended price — the same
	// derivation SetAutoCatalog uses). "vision" keeps models
	// the registry reports as vision-capable (models.dev sync).
	if res.TierFilter == "free" {
		if narrowed := autoComboNarrowFree(s.priceBook, models); len(narrowed) > 0 {
			models = narrowed
		}
	}
	if res.Category == "reasoning" {
		if narrowed := autoComboNarrowReasoning(s.autoComboCategoryOf(), models); len(narrowed) > 0 {
			models = narrowed
		}
	}
	if res.TierFilter == "pro" {
		if narrowed := s.autoComboNarrowPremium(models); len(narrowed) > 0 {
			models = narrowed
		}
	}
	if res.Category == "vision" {
		if narrowed := s.autoComboNarrowVision(models); len(narrowed) > 0 {
			models = narrowed
		}
	}
	return models, nil
}

// autoComboNarrowFree keeps the models whose blended price is
// known to be $0 — the production "free" derivation
// (cmd/codvian-router/main.go, SetAutoCatalog). A nil book, or a model
// with no known price, is never free: unknown is not $0.
func autoComboNarrowFree(book *config.PriceBook, models []string) []string {
	if book == nil {
		return nil
	}
	out := []string{}
	for _, model := range models {
		prov, bare, ok := autoComboSplitPair(model)
		if !ok {
			continue
		}
		if blended, known := book.BlendedUSDPer1M(prov, bare); known && blended == 0 {
			out = append(out, model)
		}
	}
	return out
}

// autoComboSplitPair splits a "provider/model" id into its bare
// model id. False when there is no "/" (tier models without one
// name no provider and are skipped upstream).
func autoComboSplitPair(model string) (provider, bare string, ok bool) {
	prov, rest, found := strings.Cut(model, "/")
	if !found || prov == "" || rest == "" {
		return "", "", false
	}
	return prov, rest, true
}

// autoComboCategoryOf derives each live tier model's category
// exactly the way production does (cmd/codvian-router/main.go,
// SetAutoCatalog): first-wins tier membership in AllTiers order
// (Coding, then Chat, then Reasoning; QuickQA names no
// category). A model in both the coding and reasoning tiers is
// "coding", not "reasoning" — the same derivation the engine's
// catalog uses, so the endpoint and dispatch agree.
func (s *Server) autoComboCategoryOf() map[string]string {
	out := map[string]string{}
	for _, tier := range autorouter.AllTiers {
		var cat string
		switch tier {
		case autorouter.TierCoding:
			cat = "coding"
		case autorouter.TierChat:
			cat = "chat"
		case autorouter.TierReasoning:
			cat = "reasoning"
		default:
			continue
		}
		for _, m := range s.autoRouter.ModelsForTier(tier) {
			if _, ok := out[m]; !ok {
				out[m] = cat
			}
		}
	}
	return out
}

// autoComboNarrowReasoning keeps the models whose derived
// category is "reasoning".
func autoComboNarrowReasoning(categories map[string]string, models []string) []string {
	out := []string{}
	for _, model := range models {
		if categories[model] == "reasoning" {
			out = append(out, model)
		}
	}
	return out
}

// autoComboNarrowPremium keeps the models whose economic tier
// classifies premium from the real price (G19 Area 1): the
// input leg when the registry model carries one, else the
// blended price from the price book — the same derivation
// SetAutoCatalog uses (scoring.ClassifyPriceTier). An unknown
// price classifies unknown, never premium.
func (s *Server) autoComboNarrowPremium(models []string) []string {
	out := []string{}
	for _, model := range models {
		prov, bare, ok := autoComboSplitPair(model)
		if !ok {
			continue
		}
		var inputUSD float64
		hasInput := false
		if s.providerKeyManager != nil {
			if p, ok := s.providerKeyManager.Get(prov); ok && p != nil {
				for _, m := range p.Models {
					if m.ID == bare && m.InputPriceUSD > 0 {
						inputUSD = m.InputPriceUSD * 1e6
						hasInput = true
						break
					}
				}
			}
		}
		blended := -1.0
		if s.priceBook != nil {
			if b, known := s.priceBook.BlendedUSDPer1M(prov, bare); known {
				blended = b
			}
		}
		if scoring.ClassifyPriceTier(inputUSD, hasInput, blended) == scoring.ModelTierPremium {
			out = append(out, model)
		}
	}
	return out
}

// autoComboNarrowVision keeps the models the registry reports
// as vision-capable (G19 Area 1): provider.Model.Vision, set
// only by the models.dev sync when the feed's [modalities]
// table reports image input. A model the registry does not
// know (or reports without vision) is not vision-capable —
// never guessed from the model id.
func (s *Server) autoComboNarrowVision(models []string) []string {
	out := []string{}
	if s.providerKeyManager == nil {
		return out
	}
	for _, model := range models {
		prov, bare, ok := autoComboSplitPair(model)
		if !ok {
			continue
		}
		p, ok := s.providerKeyManager.Get(prov)
		if !ok || p == nil {
			continue
		}
		for _, m := range p.Models {
			if m.ID == bare && m.Vision {
				out = append(out, model)
				break
			}
		}
	}
	return out
}

// autoComboProviderRemoved reports whether the provider is the
// removed variant (G19 Area 1, CT-2): removed providers never
// appear in the candidate pool. The live registry is
// authoritative (its IsRemovedVariant carries the keyless-only
// nuance); the config fallback judges the static entry — a
// whole-integration removal matches by id/alias, a keyless-only
// removal matches only a keyless entry.
func (s *Server) autoComboProviderRemoved(providerID string) bool {
	if s.providerKeyManager != nil {
		if p, ok := s.providerKeyManager.Get(providerID); ok && p != nil {
			_, removed := p.IsRemovedVariant()
			return removed
		}
	}
	if s.cfg != nil {
		for i := range s.cfg.Providers {
			if s.cfg.Providers[i].ID != providerID {
				continue
			}
			if rem, ok := catalog.IsRemoved(providerID); ok {
				if !rem.KeylessOnly || len(s.cfg.Providers[i].Keys) == 0 {
					return true
				}
			}
			return false
		}
	}
	return false
}

// autoComboTierUnion returns the deduped union of the given tiers'
// live candidate models, in first-seen order.
func (s *Server) autoComboTierUnion(tiers []autorouter.Tier) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, tier := range tiers {
		for _, model := range s.autoRouter.ModelsForTier(tier) {
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

// lockoutReporter is optionally implemented by engines that can
// report live per-model lockouts (router.Engine, mirroring
// breakerReporter). It is separate from breakerReporter so an
// engine that reports breakers but no lockouts still feeds the
// breaker half.
type lockoutReporter interface {
	ModelLockouts() []router.LockoutStatus
}

// autoComboModelLockouts returns the set of live locked
// (provider, key, model) triples, keyed
// "provider\x00keyID\x00bare-model", from the engine's
// ModelLockouts(). The lockout table keys on the request model as
// the engine saw it — the bare id the client sent — while the pool
// carries "provider/model" ids, so each status is filed under its
// bare model part (strings.Cut on the first "/"; an id with no "/"
// files as-is). Empty when the engine does not report lockouts.
func (s *Server) autoComboModelLockouts() map[string]bool {
	out := map[string]bool{}
	rep, ok := s.engine.(lockoutReporter)
	if !ok || rep == nil {
		return out
	}
	for _, st := range rep.ModelLockouts() {
		_, bare, found := strings.Cut(st.Model, "/")
		if !found || bare == "" {
			bare = st.Model
		}
		out[st.ProviderID+"\x00"+st.KeyID+"\x00"+bare] = true
	}
	return out
}

// autoComboBreakerStates maps provider id -> "OPEN" or "CLOSED"
// from the engine's live breaker report. A provider the engine
// does not report reads as "CLOSED" (the handler's default);
// no engine (or an engine that does not report) yields an empty
// map, so every row reads CLOSED rather than inventing a state.
func (s *Server) autoComboBreakerStates() map[string]string {
	out := map[string]string{}
	rep, ok := s.engine.(breakerReporter)
	if !ok || rep == nil {
		return out
	}
	for _, st := range rep.BreakerStates() {
		if st.Open {
			out[st.ProviderID] = "OPEN"
		} else {
			out[st.ProviderID] = "CLOSED"
		}
	}
	return out
}

// autoComboCoolingKeys returns the set of live cooling
// (provider, key) pairs, keyed "provider\x00keyID", from the
// engine's CooldownTargets(). Empty when the engine does not
// report cooldowns.
func (s *Server) autoComboCoolingKeys() map[string]bool {
	out := map[string]bool{}
	rep, ok := s.engine.(cooldownReporter)
	if !ok || rep == nil {
		return out
	}
	for _, t := range rep.CooldownTargets() {
		out[t.ProviderID+"\x00"+t.KeyID] = true
	}
	return out
}

// autoComboKeyIDs returns the provider's connection (key) ids in
// pool order. Live registry first: providers added or changed at
// runtime land there (and EffectiveKeys synthesises the legacy
// single-key id). Config fallback second: the startup snapshot
// (s.cfg is not updated by the live provider endpoints), explicit
// Keys only — a legacy provider with no stored Keys has no
// computable id on this path, so it yields no rows rather than an
// invented id.
func (s *Server) autoComboKeyIDs(providerID string) []string {
	if s.providerKeyManager != nil {
		if p, ok := s.providerKeyManager.Get(providerID); ok && p != nil {
			return autoComboKeyIDList(p.EffectiveKeys())
		}
	}
	if s.cfg != nil {
		for i := range s.cfg.Providers {
			if s.cfg.Providers[i].ID == providerID {
				return autoComboKeyIDList(s.cfg.Providers[i].Keys)
			}
		}
	}
	return nil
}

func autoComboKeyIDList(keys []provider.ProviderKey) []string {
	out := []string{}
	for _, k := range keys {
		if k.ID == "" {
			continue
		}
		out = append(out, k.ID)
	}
	return out
}

// splitAutoComboModel splits a "provider/model" id. Tier models
// without a "/" name no provider and cannot be served by any
// known connection.
func splitAutoComboModel(model string) (string, bool) {
	prov, _, ok := strings.Cut(model, "/")
	if !ok || prov == "" {
		return "", false
	}
	return prov, true
}

// ---------------------------------------------------------------------------
// Per-API-key candidate exclusions (B2-15).
// ---------------------------------------------------------------------------

// autoCandidateOverridesFileName is the operator-managed exclusion
// file in the data dir. It mirrors the reference's
// auto_candidate_overrides table (apiKeyId, autoChannel,
// connectionId, excluded) as a flat row list instead of DB rows:
//
//	{"overrides": [
//	  {"key_id": "gw_...", "channel": "auto/coding",
//	   "connection_id": "openai_key_1", "excluded": true}
//	]}
//
// Rows are keyed by (key_id, channel, connection_id); last row wins.
// A row with excluded:false clears an earlier exclusion. The channel
// is the full auto id ("auto" or "auto/<suffix>"). No new HTTP
// endpoint manages this file: routes need guard-table entries
// (guard.go — Server.register panics on an unclassified route), so
// the operator edits the file and the server picks it up without a
// restart (reloaded when its mtime/size changes). A missing file, or
// no data dir at all, means no exclusions — the default unconfigured
// path, never an invented one.
const autoCandidateOverridesFileName = "auto-candidate-overrides.json"

type autoCandidateOverride struct {
	KeyID        string `json:"key_id"`
	Channel      string `json:"channel"`
	ConnectionID string `json:"connection_id"`
	Excluded     bool   `json:"excluded"`
}

type autoCandidateOverrideFile struct {
	Overrides []autoCandidateOverride `json:"overrides"`
}

// autoCandidateOverrideStore is the loaded exclusion set: the
// operator's file parsed into byKey[keyID][channel][connectionID].
// The zero value is an empty store; use
// newAutoCandidateOverrideStore.
type autoCandidateOverrideStore struct {
	mu      sync.Mutex
	path    string // "" = in-memory only, never loads a file
	byKey   map[string]map[string]map[string]bool
	loaded  bool
	modTime time.Time
	size    int64
}

func newAutoCandidateOverrideStore(path string) *autoCandidateOverrideStore {
	return &autoCandidateOverrideStore{path: path, byKey: map[string]map[string]map[string]bool{}}
}

// excludedIDs returns the connection ids excluded for (keyID,
// channel) — the reference's getExcludedConnectionIds. Nil when no
// overrides exist for the pair. A corrupt overrides file errors:
// callers fail loudly (503) rather than listing rows with wrong
// `excluded` flags; the error clears itself the next time the file
// changes.
func (st *autoCandidateOverrideStore) excludedIDs(keyID, channel string) (map[string]bool, error) {
	if st == nil || keyID == "" || channel == "" {
		return nil, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.reloadLocked(); err != nil {
		return nil, err
	}
	byChannel, ok := st.byKey[keyID]
	if !ok {
		return nil, nil
	}
	excl, ok := byChannel[channel]
	if !ok {
		return nil, nil
	}
	out := make(map[string]bool, len(excl))
	for id, ex := range excl {
		if ex {
			out[id] = true
		}
	}
	return out, nil
}

// reloadLocked re-reads the overrides file when its mtime or size
// changed since the last load. A missing file means no overrides.
// A transient stat/read error keeps the last good state (and retries
// on the next call); a corrupt file errors loudly and is retried on
// the next call too (the load is not marked, so the next access
// re-reads). An empty file is an empty store.
func (st *autoCandidateOverrideStore) reloadLocked() error {
	if st.path == "" {
		return nil
	}
	fi, err := os.Stat(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			if st.loaded {
				st.byKey = map[string]map[string]map[string]bool{}
				st.loaded = false
			}
			return nil
		}
		return nil // transient stat error: keep last good state
	}
	if st.loaded && fi.ModTime().Equal(st.modTime) && fi.Size() == st.size {
		return nil
	}
	raw, err := os.ReadFile(st.path)
	if err != nil {
		return nil // transient read error: keep last good state
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		st.byKey = map[string]map[string]map[string]bool{}
		st.loaded = true
		st.modTime = fi.ModTime()
		st.size = fi.Size()
		return nil
	}
	var file autoCandidateOverrideFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return fmt.Errorf("auto-candidate-overrides %s is corrupt: %w", st.path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("auto-candidate-overrides %s: trailing data after JSON object", st.path)
	}
	next := map[string]map[string]map[string]bool{}
	for _, row := range file.Overrides {
		if row.KeyID == "" || row.Channel == "" || row.ConnectionID == "" {
			return fmt.Errorf("auto-candidate-overrides %s: row with empty key_id, channel or connection_id", st.path)
		}
		byChannel, ok := next[row.KeyID]
		if !ok {
			byChannel = map[string]map[string]bool{}
			next[row.KeyID] = byChannel
		}
		excl, ok := byChannel[row.Channel]
		if !ok {
			excl = map[string]bool{}
			byChannel[row.Channel] = excl
		}
		excl[row.ConnectionID] = row.Excluded
	}
	st.byKey = next
	st.loaded = true
	st.modTime = fi.ModTime()
	st.size = fi.Size()
	return nil
}

// autoComboExcludedIDs returns this server's exclusion set for
// (keyID, channel) — the reference's getExcludedConnectionIds.
// The store is created lazily from cfg.DataDir (in-memory empty
// when there is no config or no data dir), mirroring oauthIssuer.
func (s *Server) autoComboExcludedIDs(keyID, channel string) (map[string]bool, error) {
	s.autoOverridesMu.Lock()
	if s.autoOverrides == nil {
		dataDir := ""
		if s.cfg != nil {
			dataDir = s.cfg.DataDir
		}
		path := ""
		if dataDir != "" {
			path = filepath.Join(dataDir, autoCandidateOverridesFileName)
		}
		s.autoOverrides = newAutoCandidateOverrideStore(path)
	}
	st := s.autoOverrides
	s.autoOverridesMu.Unlock()
	return st.excludedIDs(keyID, channel)
}

// autoComboFreeAccessExclusion reports why STRICT_ZERO_COST would
// exclude this candidate — the reference's
// StrictZeroCostExclusionReason — derived from the live price book
// only: a model whose blended price is known to be $0 is not
// excluded (nil); anything else — known-paid or unknown price —
// reports "not-in-catalog", the reference's own first verdict
// ("paid, or genuinely unknown"). DOCUMENTED LIMIT: this is a
// price-only approximation of the reference's verdict. The
// reference also checks the free-model catalog's freeType, a
// hard-stop guarantee, and live per-connection allowance state
// (exhausted / state-unknown) — none of that data exists in this
// repo, so nil here means "known $0 by price", never "allowance
// verified". The field is reported, never enforced: the row is
// listed either way.
func (s *Server) autoComboFreeAccessExclusion(model string) *string {
	prov, bare, ok := autoComboSplitPair(model)
	if !ok || s.priceBook == nil {
		return autoComboStrPtr("not-in-catalog")
	}
	if blended, known := s.priceBook.BlendedUSDPer1M(prov, bare); known && blended == 0 {
		return nil
	}
	return autoComboStrPtr("not-in-catalog")
}

func autoComboStrPtr(v string) *string { return &v }
