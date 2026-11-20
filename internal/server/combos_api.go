// Combos / chains CRUD API (M8.14) — POST/GET/PATCH/DELETE /v1/combos.
//
// A combo is a NAMED routing chain: an ordered list of provider/model
// steps plus a strategy, addressable by name. It is a CRUD face over
// state that ALREADY exists — the operator-defined routing groups
// (config routing.groups, name -> ordered provider members) and the
// per-chain strategy overrides (routing.chain_strategies) — not a
// second, parallel chain store:
//
//   - Every mutation ALSO mirrors the provider chain into
//     cfg.Routing.Groups[name] (members as "provider" or
//     "provider@effort", the existing group syntax), so the combo is
//     engine-visible the moment it is created: it routes exactly
//     like any other group, addressable as "group:<name>/<model>".
//     A delete removes the group mirror too — deleted means gone
//     from the store AND from the engine's chain state, never a
//     soft-delete fiction.
//   - The strategy mirrors into routing.chain_strategies[name] only
//     when it is a config-valid strategy name (strategy.IsValid):
//     config validation rejects anything else at load, so mirroring
//     an invalid name would corrupt the config file on the next
//     save/reload. The three M3 combo execution modes
//     (fusion/pipeline/jev, internal/router/combo.go) are NOT valid
//     chain-strategy names anywhere in the codebase — they are
//     accepted here as combo strategies and live in the combo
//     record only, and patching a combo to one removes any stale
//     mirror entry so the strategies feed never claims a strategy
//     the combo no longer runs. That is an honest limitation, not a
//     silent drop: the record reports the strategy verbatim.
//   - Per-step MODEL pins have no config home either (a group member
//     is a provider, never "provider/model" — config validation
//     rejects a member that is not a known provider id), so the
//     model pin lives in the combo record only. On restart, combos
//     re-seed from the config groups as provider chains with no
//     model pin (the request-time model applies, as for any group).
//
// Seeding: NewComboStore seeds one combo per pre-existing config
// group (strategy from chain_strategies[name] when present, else
// "priority"), so groups an operator already defined in the config
// file show up as combos without any migration.
//
// Persistence: an optional ComboPersistFunc (the coordinator wires
// it to save the config file) runs AFTER the in-memory + mirror
// mutation; on persist failure the mutation is rolled back and the
// handler answers 500, so runtime and file can never disagree
// (the cooldown/strategies persist-first pattern cannot apply
// here because the callback saves cfg as-is). With no persist
// function wired, mutations apply in-memory only and every
// response reports persisted=false, exactly like the strategies
// and cooldown feeds.
//
// Validation (all 400s name the field): unknown provider ids,
// unknown model ids (against the provider's declared model list),
// empty chains, duplicate steps — including two steps that differ
// only by model pin, which mirror to the same group member —
// unknown strategy names, and group-illegal names (empty, or
// containing ":" or "/"). A step
// whose provider declares NO models cannot have its model verified
// from config — the step is accepted and reported with
// model_verified=false, never silently treated as verified.
//
// Key rule: a combo that references a provider with no key is
// STORED, never dropped, and flagged unusable (usable=false with
// an unusable_reason per step). The only no-key providers that
// still count as usable are no-auth locals: wire format "ollama"
// or a loopback base URL (the same rule provider.go documents for
// local providers). A disabled provider is unusable too. No price,
// cap, or model capability is ever invented: there are no prices
// or caps anywhere in this file.
//
// Auth/scope: this file only implements the handlers. Registration
// (tier, gateway-key auth, scope) belongs to the coordinator in
// server.go/guard.go, which this slice must not touch. Proposed
// mapping, for the coordinator to assign: TierManagement,
// gateway-key auth, scope "models" for the two GETs (read-only
// listing, like GET /v1/auto-combo/{channel}/candidates) and
// scope "chat" for POST/PATCH/DELETE (they mutate the routing
// state chat requests run through). The handler itself performs
// no auth.
//
// B3 — duplicate / import / export / test-run / enable-disable
// (M10-5 enabler). The five verbs, and the decisions this slice
// took (each is also stated at its handler):
//
//   - Duplicate (POST /v1/combos/{id}/duplicate, {"name"} required):
//     a verbatim copy under a new id/name — members, strategy and
//     the enabled state are inherited from the source. Inheriting
//     the enabled state is deliberate: duplicate means "this combo,
//     again", and create already routes immediately, so a copy
//     that silently started disabled would surprise; flipping it
//     off is one explicit disable call away.
//   - Import (POST /v1/combos/import): one ComboExport object,
//     validated through the same Create path as POST /v1/combos
//     (same 400s, same unknown-field rejection); name collision
//     is 409, never a silent overwrite. The all-export envelope
//     ({"combos": [...]}) is NOT importable as one body —
//     importing it answers 400 naming that it holds N combos to
//     import one at a time (bulk import with partial-failure
//     semantics is out of scope for B3).
//   - Export (GET /v1/combos/{id}/export, GET /v1/combos/export):
//     one combo, or all combos when the id is omitted, in the
//     importable ComboExport shape (name/chain/strategy/enabled;
//     chain steps carry provider/model/effort only, no derived
//     liveness facts, so the shape round-trips through import).
//     A combo literally named "export" is shadowed on
//     GET /v1/combos/export (the literal route wins over the
//     {id} route); its single export stays reachable at
//     /v1/combos/export/export.
//   - Test-run (POST /v1/combos/{id}/test-run): READ-ONLY
//     SIMULATION — see the decision comment at
//     handleTestRunCombo. No provider is called, no quota is
//     spent, no cost is incurred. It proves the routing decision
//     only (declared order + live per-step usability); it cannot
//     prove live reachability, latency, re-ranked order under
//     live signals, or cost (no price book is consulted here).
//   - Enable/disable (POST /v1/combos/{id}/enable,
//     POST /v1/combos/{id}/disable, idempotent, no body): the
//     dedicated verbs the dashboard's five-action set calls.
//     PATCH already covers the same state ({"enabled": bool},
//     added in B3) — the verbs exist anyway because enable/
//     disable is a lifecycle transition, not a record edit: a
//     one-click dashboard action with no body, and because a
//     disabled combo's mirrors must be removed atomically with
//     the flag flip. Both paths share mirrorComboLocked, so
//     they cannot drift.
//   - Enabled semantics: a combo record carries enabled
//     (default true; seeded combos are enabled — they ARE live
//     config groups). A disabled combo keeps its chain/strategy
//     but owns NO group/strategy mirror, so "group:<name>" stops
//     routing; re-enabling re-mirrors verbatim. Combo.Usable
//     means "would serve if asked": enabled AND every step
//     usable. A disabled import/create never touches the
//     mirrors — it must not delete operator group state it
//     never created.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// comboStrategies are the M3 combo execution modes
// (internal/router/combo.go: comboStrategies): real engine
// strategies that are NOT in strategy.All and NOT valid
// chain-strategy names in config validation, but ARE valid combo
// strategies. They are accepted verbatim (lower-cased) and live
// in the combo record only — never mirrored into
// routing.chain_strategies (see the file header).
var comboExecutionStrategies = map[string]bool{
	"fusion":   true,
	"pipeline": true,
	"jev":      true,
}

// ComboPersistFunc saves the config file after a combo mutation.
// It is called after the in-memory record and the config mirrors
// are updated; a non-nil error rolls the mutation back. Nil means
// combos live in memory only (responses report persisted=false).
type ComboPersistFunc func() error

// ComboStep is one step of a combo chain: a provider, an optional
// model pin ("" = the request-time model, as for any routing
// group), and an optional reasoning effort ("provider@effort"
// group syntax). HasKey/Usable/UnusableReason/ModelVerified are
// derived live from the provider config at read time, never
// stored: a key added later flips the flag without any combo
// edit.
type ComboStep struct {
	Provider string `json:"provider"`
	// Model is the pinned model for this step, "" when unpinned.
	Model string `json:"model,omitempty"`
	// Effort is the reasoning effort ("low"/"medium"/"high"/...),
	// "" when none.
	Effort string `json:"effort,omitempty"`
	// HasKey reports whether the provider holds any key (pool or
	// legacy single key, mirroring provider.EffectiveKeys).
	HasKey bool `json:"has_key"`
	// Usable is false when the step cannot serve: provider
	// disabled, or no key on a provider that needs one.
	Usable bool `json:"usable"`
	// UnusableReason names why Usable is false, "" when usable.
	UnusableReason string `json:"unusable_reason,omitempty"`
	// ModelVerified is nil when Model is "" (nothing to verify);
	// otherwise true only when the model is in the provider's
	// declared model list. A provider that declares no models
	// cannot verify any pin (false, never assumed true).
	ModelVerified *bool `json:"model_verified,omitempty"`
}

// Combo is one named routing combo as the API returns it.
type Combo struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Chain     []ComboStep `json:"chain"`
	Strategy  string      `json:"strategy"`
	Enabled   bool        `json:"enabled"`
	Usable    bool        `json:"usable"`
	Persisted bool        `json:"persisted"`
	CreatedAt time.Time   `json:"created_at,omitempty"`
	UpdatedAt time.Time   `json:"updated_at,omitempty"`
}

// comboRecord is the stored form: the chain steps as the operator
// wrote them (provider/model/effort only — liveness facts are
// derived at read time) plus the strategy, the enabled flag and
// timestamps.
type comboRecord struct {
	name      string
	chain     []comboStoredStep
	strategy  string
	enabled   bool
	createdAt time.Time
	updatedAt time.Time
}

// comboStoredStep is one chain step as stored (no derived facts).
type comboStoredStep struct {
	provider string
	model    string
	effort   string
}

// ComboStore is the combos CRUD store: records in memory, seeded
// from the config groups, mirrored back into the config groups +
// chain strategies on every mutation. It reads providers and
// groups live from cfg on every call, so providers added at
// runtime are visible without a restart. cfg may be nil (tests):
// then there are no providers, no groups, no mirroring, and no
// persistence — only the in-memory records.
type ComboStore struct {
	mu      sync.RWMutex
	cfg     *config.Config
	persist ComboPersistFunc
	records map[string]*comboRecord
}

// NewComboStore builds a combo store over cfg (live pointer) with
// an optional persist callback, seeding one combo per pre-existing
// config routing group. A nil cfg yields an empty store.
func NewComboStore(cfg *config.Config, persist ComboPersistFunc) *ComboStore {
	s := &ComboStore{cfg: cfg, persist: persist, records: map[string]*comboRecord{}}
	if cfg == nil {
		return s
	}
	// M8.14 race fix, round 3: the old `cfg.Routing == nil`
	// early return read the Routing pointer with NO lock —
	// and the "runs at construction before the config is
	// shared" justification was false: combosStore()
	// (server.go) builds this store LAZILY, on the first
	// live /v1/combos request, concurrent with serving, so
	// the read raced routingPersist's swap and mirrorGroup's
	// cfg.Routing creation. There is no early return any
	// more: GroupsSnapshot() / ChainStrategy() below already
	// take the routing read lock and handle a nil block
	// (GroupsSnapshot returns nil, ChainStrategy reports
	// absent), so a nil Routing simply seeds nothing.
	// EnsureRoutingMu runs first (serialised by
	// Server.combosMu around the lazy construction, and a
	// no-op when DefaultConfig/Load already initialised it).
	cfg.EnsureRoutingMu()
	groups := cfg.GroupsSnapshot()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		steps := []comboStoredStep{}
		for _, member := range groups[name] {
			prov, effort, err := config.ParseGroupMember(member)
			if err != nil {
				// A group in a loaded config is validated,
				// so this is unreachable in production; a
				// bad member is skipped, never invented.
				continue
			}
			steps = append(steps, comboStoredStep{provider: prov, effort: effort})
		}
		if len(steps) == 0 {
			continue
		}
		strat := "priority"
		if st, ok := cfg.ChainStrategy(name); ok {
			if norm, valid := comboValidStrategy(st); valid {
				strat = norm
			}
		}
		s.records[name] = &comboRecord{name: name, chain: steps, strategy: strat, enabled: true}
	}
	return s
}

// Handler returns the combos HTTP handler for mounting at /v1/combos:
// GET/POST /v1/combos and GET/PATCH/DELETE /v1/combos/{id}, plus
// the B3 verbs: POST /v1/combos/import, GET /v1/combos/export,
// GET /v1/combos/{id}/export, POST /v1/combos/{id}/duplicate,
// POST /v1/combos/{id}/test-run, POST /v1/combos/{id}/enable and
// POST /v1/combos/{id}/disable. The coordinator owns auth, tier
// and scope at registration.
func (s *ComboStore) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/combos", s.handleListCombos)
	mux.HandleFunc("POST /v1/combos", s.handleCreateCombo)
	mux.HandleFunc("POST /v1/combos/import", s.handleImportCombo)
	mux.HandleFunc("GET /v1/combos/export", s.handleExportAllCombos)
	mux.HandleFunc("GET /v1/combos/{id}", s.handleGetCombo)
	mux.HandleFunc("PATCH /v1/combos/{id}", s.handlePatchCombo)
	mux.HandleFunc("DELETE /v1/combos/{id}", s.handleDeleteCombo)
	mux.HandleFunc("GET /v1/combos/{id}/export", s.handleExportCombo)
	mux.HandleFunc("POST /v1/combos/{id}/duplicate", s.handleDuplicateCombo)
	mux.HandleFunc("POST /v1/combos/{id}/test-run", s.handleTestRunCombo)
	mux.HandleFunc("POST /v1/combos/{id}/enable", s.handleEnableCombo)
	mux.HandleFunc("POST /v1/combos/{id}/disable", s.handleDisableCombo)
	return mux
}

// comboValidStrategy normalises a strategy name: canonical
// strategy names via strategy.Parse (aliases included), plus the
// three M3 combo execution modes. False for "" and unknown names.
func comboValidStrategy(s string) (string, bool) {
	if st, ok := strategy.Parse(s); ok {
		return string(st), true
	}
	norm := strings.ToLower(strings.TrimSpace(s))
	if comboExecutionStrategies[norm] {
		return norm, true
	}
	return "", false
}

// comboValidName enforces the routing-group name rule (config
// validation): non-empty, no ":" or "/" (alias syntax). There is
// no length cap in config validation, so none is imposed here.
func comboValidName(name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return fmt.Errorf("name: must not be empty")
	case strings.ContainsAny(name, ":/"):
		return fmt.Errorf("name: %q must not contain \":\" or \"/\" (routing group names use \"group:<name>/<model>\" alias syntax)", name)
	}
	return nil
}

// providerByID returns the provider config for id, live from cfg.
func (s *ComboStore) providerByID(id string) (config.ProviderConfig, bool) {
	if s.cfg == nil {
		return config.ProviderConfig{}, false
	}
	for _, p := range s.cfg.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return config.ProviderConfig{}, false
}

// comboStepFacts derives a step's liveness facts from the live
// provider config. hasKey mirrors provider.EffectiveKeys: a pool
// key or a legacy single key counts; an empty key pool means no
// key. usable is false for a disabled provider, or for a keyless
// provider that is not a no-auth local (ollama wire format, or a
// loopback base URL — the local-provider rule provider.go
// documents). No key state is ever invented: the flag reports
// exactly what the config holds.
func comboStepFacts(p config.ProviderConfig) (hasKey, usable bool, reason string) {
	hasKey = len(p.Keys) > 0 || p.APIKeyEncrypted != ""
	if !p.Enabled {
		return hasKey, false, "provider disabled"
	}
	if hasKey {
		return true, true, ""
	}
	if comboNoAuthProvider(p) {
		return false, true, ""
	}
	return false, false, "provider has no key"
}

// comboNoAuthProvider reports whether a keyless provider is
// usable without a key: local providers only.
func comboNoAuthProvider(p config.ProviderConfig) bool {
	if p.WireFormat == "ollama" {
		return true
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// view builds the API view of a record, deriving every liveness
// fact live from the provider config. Usable means "would serve
// if asked": the combo must be enabled AND every step usable — a
// disabled combo reports usable=false even when its steps are
// fine, because nothing routes through it while it is off.
func (s *ComboStore) view(rec *comboRecord) Combo {
	steps := make([]ComboStep, 0, len(rec.chain))
	usable := rec.enabled
	for _, st := range rec.chain {
		out := ComboStep{Provider: st.provider, Model: st.model, Effort: st.effort}
		if p, ok := s.providerByID(st.provider); ok {
			out.HasKey, out.Usable, out.UnusableReason = comboStepFacts(p)
			if st.model != "" {
				verified := false
				for _, m := range p.Models {
					if m.ID == st.model {
						verified = true
						break
					}
				}
				v := verified
				out.ModelVerified = &v
			}
		} else {
			// The provider vanished after the combo was
			// stored (deleted at runtime): the step stays
			// listed and flagged, never silently dropped.
			out.HasKey, out.Usable, out.UnusableReason = false, false, "provider not configured"
			if st.model != "" {
				v := false
				out.ModelVerified = &v
			}
		}
		if !out.Usable {
			usable = false
		}
		steps = append(steps, out)
	}
	return Combo{
		ID: rec.name, Name: rec.name, Chain: steps, Strategy: rec.strategy,
		Enabled: rec.enabled,
		Usable:  usable, Persisted: s.persist != nil,
		CreatedAt: rec.createdAt, UpdatedAt: rec.updatedAt,
	}
}

// parseChainStep parses one raw chain entry — either a string
// ("provider", "provider/model", "provider@effort",
// "provider/model@effort") or an object
// ({"provider","model","effort"}) — and validates it against the
// live provider config. Errors name the field (chain[i].provider
// / chain[i].model / chain[i].effort).
func (s *ComboStore) parseChainStep(raw json.RawMessage, i int) (comboStoredStep, error) {
	var step comboStoredStep
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			return step, fmt.Errorf("chain[%d]: must not be empty", i)
		}
		// Split the effort suffix first ("@" binds loosest in
		// the group syntax: ParseGroupMember splits on the
		// last "@").
		rest := asString
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			step.effort = rest[at+1:]
			rest = rest[:at]
		}
		if prov, model, ok := strings.Cut(rest, "/"); ok {
			step.provider, step.model = strings.TrimSpace(prov), strings.TrimSpace(model)
		} else {
			step.provider = strings.TrimSpace(rest)
		}
	} else {
		var obj struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Effort   string `json:"effort"`
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&obj); err != nil {
			// The decode error is kept: it names the actual
			// problem (e.g. json: unknown field "bogus")
			// instead of hiding it behind the shape hint.
			return step, fmt.Errorf("chain[%d]: must be a \"provider[/model][@effort]\" string or a {\"provider\",\"model\",\"effort\"} object (%v)", i, err)
		}
		step.provider = strings.TrimSpace(obj.Provider)
		step.model = strings.TrimSpace(obj.Model)
		step.effort = strings.TrimSpace(obj.Effort)
	}
	if step.provider == "" {
		return step, fmt.Errorf("chain[%d].provider: must not be empty", i)
	}
	p, ok := s.providerByID(step.provider)
	if !ok {
		return step, fmt.Errorf("chain[%d].provider: unknown provider %q", i, step.provider)
	}
	if step.model != "" && len(p.Models) > 0 {
		found := false
		for _, m := range p.Models {
			if m.ID == step.model {
				found = true
				break
			}
		}
		if !found {
			return step, fmt.Errorf("chain[%d].model: unknown model %q for provider %q", i, step.model, step.provider)
		}
	}
	if step.effort != "" {
		if !provider.ValidReasoningEffort(step.effort) {
			return step, fmt.Errorf("chain[%d].effort: unknown reasoning effort %q (one of %s)", i, step.effort, strings.Join(provider.ReasoningEfforts, ", "))
		}
		step.effort = provider.NormalizeReasoningEffort(step.effort)
	}
	return step, nil
}

// validateChain validates a raw chain: non-empty, every step
// valid, no exact duplicate step (same provider+model+effort —
// the same rule config applies to group members, on the
// (provider, effort) pair, extended here to the model pin),
// and no duplicate GROUP member: two steps that differ only by
// model pin (same provider, same effort) mirror to the same
// group member, and config validation rejects the group as
// listing the provider twice — so that is a 400 here, before
// any save is attempted, never a 500 from config.Save. The
// same provider at different efforts stays legal: each effort
// is a distinct group member.
func (s *ComboStore) validateChain(raw []json.RawMessage) ([]comboStoredStep, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("chain: must not be empty (a combo needs at least one provider)")
	}
	steps := make([]comboStoredStep, 0, len(raw))
	seen := map[comboStoredStep]int{}
	seenMirror := map[comboMirrorKey]int{}
	for i, r := range raw {
		st, err := s.parseChainStep(r, i)
		if err != nil {
			return nil, err
		}
		if j, dup := seen[st]; dup {
			return nil, fmt.Errorf("chain[%d]: duplicate step %q (same provider, model and effort as chain[%d])", i, comboStepLabel(st), j)
		}
		seen[st] = i
		mk := comboMirrorKey{provider: st.provider, effort: st.effort}
		if j, dup := seenMirror[mk]; dup {
			return nil, fmt.Errorf("chain[%d]: duplicate group member %q (same provider and effort as chain[%d]; the steps differ only by model pin, and a group member is a provider, never \"provider/model\")", i, comboMirrorLabel(st), j)
		}
		seenMirror[mk] = i
		steps = append(steps, st)
	}
	return steps, nil
}

// comboMirrorKey is the group member a chain step mirrors to:
// (provider, effort). The model pin is record-only, so it is
// deliberately NOT part of the key: two pins on the same
// provider at the same effort are one member.
type comboMirrorKey struct {
	provider string
	effort   string
}

// comboMirrorLabel renders a step as its group member:
// "provider", or "provider@effort" when an effort is set.
func comboMirrorLabel(st comboStoredStep) string {
	if st.effort != "" {
		return st.provider + "@" + st.effort
	}
	return st.provider
}

func comboStepLabel(st comboStoredStep) string {
	label := st.provider
	if st.model != "" {
		label += "/" + st.model
	}
	if st.effort != "" {
		label += "@" + st.effort
	}
	return label
}

// mirrorGroup writes a combo's provider chain into the config
// groups mirror (engine-visible chain state). It returns a
// rollback closure restoring the previous mirror entry (or its
// absence). No-op when cfg/routing is nil. M8.14 race fix: the
// write — and the rollback, when it runs — take the shared
// routing write lock (cfg.LockRouting); the caller holds
// ComboStore.mu, so the lock order is always ComboStore.mu ->
// routingMu. The lock is released before the persist callback
// runs, never held across disk I/O.
func (s *ComboStore) mirrorGroup(name string, steps []comboStoredStep) func() {
	if s.cfg == nil {
		return func() {}
	}
	s.cfg.LockRouting()
	defer s.cfg.UnlockRouting()
	if s.cfg.Routing == nil {
		s.cfg.Routing = &config.RoutingConfig{}
	}
	if s.cfg.Routing.Groups == nil {
		s.cfg.Routing.Groups = map[string][]string{}
	}
	old, hadOld := s.cfg.Routing.Groups[name]
	members := make([]string, 0, len(steps))
	for _, st := range steps {
		m := st.provider
		if st.effort != "" {
			m += "@" + st.effort
		}
		members = append(members, m)
	}
	s.cfg.Routing.Groups[name] = members
	return func() {
		if s.cfg == nil {
			return
		}
		s.cfg.LockRouting()
		defer s.cfg.UnlockRouting()
		if s.cfg.Routing == nil {
			return
		}
		if hadOld {
			s.cfg.Routing.Groups[name] = old
		} else {
			delete(s.cfg.Routing.Groups, name)
		}
	}
}

// mirrorStrategy writes a combo's strategy into the config chain
// strategies mirror — only for config-valid strategy names (see
// the file header). A combo execution mode (fusion/pipeline/jev)
// is record-only, AND any previous mirror entry for the combo is
// removed: leaving it would make the strategies feed claim the
// chain runs a strategy the combo no longer uses. Returns a
// rollback closure.
func (s *ComboStore) mirrorStrategy(name, strategyName string) func() {
	if s.cfg == nil {
		return func() {}
	}
	// M8.14 race fix: same shared routing write lock as
	// mirrorGroup (see its comment); the rollback locks too.
	s.cfg.LockRouting()
	defer s.cfg.UnlockRouting()
	if s.cfg.Routing == nil {
		s.cfg.Routing = &config.RoutingConfig{}
	}
	if s.cfg.Routing.ChainStrategies == nil {
		s.cfg.Routing.ChainStrategies = map[string]string{}
	}
	old, hadOld := s.cfg.Routing.ChainStrategies[name]
	if _, ok := strategy.Parse(strategyName); !ok {
		// Combo execution mode: record-only — drop any stale
		// mirror entry so the feed never claims a strategy
		// the combo no longer runs.
		delete(s.cfg.Routing.ChainStrategies, name)
		return func() {
			if s.cfg == nil {
				return
			}
			s.cfg.LockRouting()
			defer s.cfg.UnlockRouting()
			if s.cfg.Routing == nil {
				return
			}
			if hadOld {
				s.cfg.Routing.ChainStrategies[name] = old
			}
		}
	}
	s.cfg.Routing.ChainStrategies[name] = strategyName
	return func() {
		if s.cfg == nil {
			return
		}
		s.cfg.LockRouting()
		defer s.cfg.UnlockRouting()
		if s.cfg.Routing == nil {
			return
		}
		if hadOld {
			s.cfg.Routing.ChainStrategies[name] = old
		} else {
			delete(s.cfg.Routing.ChainStrategies, name)
		}
	}
}

// unmirror removes a combo from both config mirrors. Returns a
// rollback closure restoring the previous entries. M8.14 race
// fix: the deletes — and the rollback, when it runs — take the
// shared routing write lock (see mirrorGroup).
func (s *ComboStore) unmirror(name string) func() {
	if s.cfg == nil {
		return func() {}
	}
	// The Routing-nil check runs under the lock: a mirror
	// may create the block concurrently.
	s.cfg.LockRouting()
	defer s.cfg.UnlockRouting()
	if s.cfg.Routing == nil {
		return func() {}
	}
	var oldGroup []string
	hadGroup := false
	if s.cfg.Routing.Groups != nil {
		oldGroup, hadGroup = s.cfg.Routing.Groups[name]
		delete(s.cfg.Routing.Groups, name)
	}
	var oldStrat string
	hadStrat := false
	if s.cfg.Routing.ChainStrategies != nil {
		oldStrat, hadStrat = s.cfg.Routing.ChainStrategies[name]
		delete(s.cfg.Routing.ChainStrategies, name)
	}
	return func() {
		if s.cfg == nil {
			return
		}
		s.cfg.LockRouting()
		defer s.cfg.UnlockRouting()
		if s.cfg.Routing == nil {
			return
		}
		if hadGroup {
			if s.cfg.Routing.Groups == nil {
				s.cfg.Routing.Groups = map[string][]string{}
			}
			s.cfg.Routing.Groups[name] = oldGroup
		}
		if hadStrat {
			if s.cfg.Routing.ChainStrategies == nil {
				s.cfg.Routing.ChainStrategies = map[string]string{}
			}
			s.cfg.Routing.ChainStrategies[name] = oldStrat
		}
	}
}

// ComboCreateRequest is the programmatic create request: the
// same fields POST /v1/combos accepts (name, chain, strategy).
// The chain entries are the raw JSON the handler decodes —
// "provider[/model][@effort]" strings or
// {"provider","model","effort"} objects (see parseChainStep).
type ComboCreateRequest struct {
	Name     string            `json:"name"`
	Chain    []json.RawMessage `json:"chain"`
	Strategy string            `json:"strategy"`
}

// comboError is a Create failure carrying the HTTP status the
// handler answers with: 400 validation, 409 duplicate name, 500
// persist failure, 503 cancelled context. Non-HTTP callers (the
// MCP source) use the status to tell a bad input from a
// backing-store failure, and the message is the exact text the
// handler writes for the same input.
type comboError struct {
	status int
	err    error
}

func (e *comboError) Error() string { return e.err.Error() }
func (e *comboError) Unwrap() error { return e.err }

// ComboImportRequest is the programmatic import request: the
// same shape as a ComboExport (name, chain, strategy, enabled).
// Enabled is a pointer so "absent" means "enabled" (the
// pre-B3 / create default) while an explicit false imports the
// combo disabled.
type ComboImportRequest struct {
	Name     string            `json:"name"`
	Chain    []json.RawMessage `json:"chain"`
	Strategy string            `json:"strategy"`
	Enabled  *bool             `json:"enabled,omitempty"`
}

// Create validates, mirrors and persists a new combo — the
// single implementation behind both POST /v1/combos (via
// handleCreateCombo) and the MCP create_combo tool (via
// serverMCPSource.CreateCombo). The validation, duplicate check,
// group/strategy mirroring and persist-with-rollback are exactly
// the ones the handler used to inline: nothing is duplicated, so
// the two paths cannot drift. The only things left in the
// handler are transport concerns: JSON decode, status mapping,
// and response encoding. A created combo is always enabled.
func (s *ComboStore) Create(ctx context.Context, req ComboCreateRequest) (Combo, error) {
	return s.create(ctx, req, true)
}

// Import validates, mirrors and persists one imported combo
// definition through the SAME path as Create — the chain
// validation, the name/strategy checks and the 409 on collision
// are shared, so import can never accept a combo create would
// reject. An explicit enabled=false imports the combo disabled
// (record kept, no mirrors); absent means enabled.
func (s *ComboStore) Import(ctx context.Context, req ComboImportRequest) (Combo, error) {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return s.create(ctx, ComboCreateRequest{Name: req.Name, Chain: req.Chain, Strategy: req.Strategy}, enabled)
}

// Duplicate copies an existing combo under a new name — chain,
// strategy and enabled state verbatim — through the same create
// path, so the copy is re-validated against the LIVE provider
// config: a provider that vanished since the source was created
// fails the duplicate with a 400 naming it, instead of copying
// a broken chain. Unknown source is 404, a colliding new name
// is 409.
func (s *ComboStore) Duplicate(ctx context.Context, id, name string) (Combo, error) {
	if err := ctx.Err(); err != nil {
		return Combo{}, &comboError{status: http.StatusServiceUnavailable, err: fmt.Errorf("duplicate aborted: %w", err)}
	}
	if err := comboValidName(name); err != nil {
		return Combo{}, &comboError{status: http.StatusBadRequest, err: err}
	}
	name = strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.records[id]
	if !ok {
		return Combo{}, &comboError{status: http.StatusNotFound, err: fmt.Errorf("combo %q not found", id)}
	}
	// Rebuild the raw chain from the stored steps (object form)
	// so createLocked re-validates it exactly as an import
	// would — no second validation path.
	raw := make([]json.RawMessage, 0, len(src.chain))
	for _, st := range src.chain {
		b, err := json.Marshal(ComboExportStep{Provider: st.provider, Model: st.model, Effort: st.effort})
		if err != nil {
			return Combo{}, &comboError{status: http.StatusInternalServerError, err: fmt.Errorf("could not copy combo: %w", err)}
		}
		raw = append(raw, b)
	}
	return s.createLocked(ComboCreateRequest{Name: name, Chain: raw, Strategy: src.strategy}, src.enabled)
}

// create is the shared create entry point: context + name +
// strategy validation, then the locked core. enabled=false
// stores the record WITHOUT touching the mirrors — a disabled
// combo must not delete operator group state it never created.
func (s *ComboStore) create(ctx context.Context, req ComboCreateRequest, enabled bool) (Combo, error) {
	if err := ctx.Err(); err != nil {
		return Combo{}, &comboError{status: http.StatusServiceUnavailable, err: fmt.Errorf("create aborted: %w", err)}
	}
	if err := comboValidName(req.Name); err != nil {
		return Combo{}, &comboError{status: http.StatusBadRequest, err: err}
	}
	strat := strings.TrimSpace(req.Strategy)
	if strat == "" {
		strat = "priority"
	}
	if _, ok := comboValidStrategy(strat); !ok {
		return Combo{}, &comboError{status: http.StatusBadRequest, err: fmt.Errorf("strategy: unknown strategy %q", strat)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createLocked(req, enabled)
}

// createLocked is the locked core of create/Import/Duplicate:
// chain validation, collision check, record insert, conditional
// mirroring, persist-with-rollback. The caller holds mu, and
// the name + strategy were already validated by create (or by
// Duplicate, which passes the source's already-valid strategy
// through — re-validated here anyway, since the strategy check
// below runs on the normalised name).
func (s *ComboStore) createLocked(req ComboCreateRequest, enabled bool) (Combo, error) {
	name := strings.TrimSpace(req.Name)
	strat := strings.TrimSpace(req.Strategy)
	if strat == "" {
		strat = "priority"
	}
	norm, ok := comboValidStrategy(strat)
	if !ok {
		return Combo{}, &comboError{status: http.StatusBadRequest, err: fmt.Errorf("strategy: unknown strategy %q", strat)}
	}
	steps, err := s.validateChain(req.Chain)
	if err != nil {
		return Combo{}, &comboError{status: http.StatusBadRequest, err: err}
	}
	if _, exists := s.records[name]; exists {
		return Combo{}, &comboError{status: http.StatusConflict, err: fmt.Errorf("name: combo %q already exists", name)}
	}
	now := time.Now().UTC()
	rec := &comboRecord{name: name, chain: steps, strategy: norm, enabled: enabled, createdAt: now, updatedAt: now}
	s.records[name] = rec
	rbRec := func() { delete(s.records, name) }
	// An enabled combo mirrors immediately (it routes like any
	// other group); a disabled one owns no mirror at all.
	rbMirror := func() {}
	if enabled {
		rbMirror = s.mirrorComboLocked(rec)
	}
	if err := s.persistWithRollback(rbMirror, rbRec); err != nil {
		return Combo{}, &comboError{status: http.StatusInternalServerError, err: fmt.Errorf("could not save combo: %w", err)}
	}
	return s.view(rec), nil
}

// mirrorComboLocked (un)mirrors rec into the config groups /
// chain-strategies mirrors to match its current enabled state:
// enabled mirrors the chain + strategy, disabled removes both
// mirrors so "group:<name>" stops routing. Returns a rollback
// restoring the previous mirror entries. The caller holds mu.
// A combo name owns its group mirror: the record's existence
// means the name is combo-owned.
func (s *ComboStore) mirrorComboLocked(rec *comboRecord) func() {
	if rec.enabled {
		rbGroup := s.mirrorGroup(rec.name, rec.chain)
		rbStrat := s.mirrorStrategy(rec.name, rec.strategy)
		return func() { rbStrat(); rbGroup() }
	}
	return s.unmirror(rec.name)
}

// persistWithRollback runs the persist callback (when wired) and
// rolls every change back on failure. True when the mutation may
// stand. This is the transport-neutral half of
// persistOrRollback: the HTTP handlers keep the wrapper that
// answers 500, and Create uses this directly so a persist
// failure surfaces as an error the caller can report.
func (s *ComboStore) persistWithRollback(rollbacks ...func()) error {
	if s.persist == nil {
		return nil
	}
	if err := s.persist(); err != nil {
		for i := len(rollbacks) - 1; i >= 0; i-- {
			rollbacks[i]()
		}
		return err
	}
	return nil
}

// persistOrRollback runs the persist callback (when wired) and
// rolls every change back on failure, answering 500. True when
// the mutation may stand.
func (s *ComboStore) persistOrRollback(w http.ResponseWriter, rollbacks ...func()) bool {
	if err := s.persistWithRollback(rollbacks...); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not save combo: persist failed")
		return false
	}
	return true
}

func (s *ComboStore) decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid combo: %v", err))
		return false
	}
	return true
}

// decodeOptionalBody decodes a JSON body that may be absent
// (test-run takes an optional {"model"}): an empty body leaves
// dst at its zero value. A present body must be valid JSON with
// no unknown fields, exactly like decodeBody.
func (s *ComboStore) decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid combo: %v", err))
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid combo: %v", err))
		return false
	}
	return true
}

// handleListCombos serves GET /v1/combos: every combo, name-sorted.
func (s *ComboStore) handleListCombos(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.records))
	for name := range s.records {
		names = append(names, name)
	}
	sort.Strings(names)
	combos := make([]Combo, 0, len(names))
	for _, name := range names {
		combos = append(combos, s.view(s.records[name]))
	}
	writeJSON(w, http.StatusOK, struct {
		Combos    []Combo `json:"combos"`
		Persisted bool    `json:"persisted"`
	}{Combos: combos, Persisted: s.persist != nil})
}

// writeComboError maps a store error (comboError) to its HTTP
// answer. A persist failure keeps the exact wire message the
// create handler always wrote ("could not save combo: persist
// failed"); the underlying error stays in the returned error
// for non-HTTP callers.
func writeComboError(w http.ResponseWriter, err error) {
	var ce *comboError
	if errors.As(err, &ce) {
		if ce.status == http.StatusInternalServerError {
			writeServerError(w, ce.status, "could not save combo: persist failed")
		} else {
			writeError(w, ce.status, ce.Error())
		}
		return
	}
	writeServerError(w, http.StatusInternalServerError, "could not save combo: persist failed")
}

// handleCreateCombo serves POST /v1/combos. Thin transport
// wrapper over Create: JSON decode, then the shared create,
// then status mapping and response encoding.
func (s *ComboStore) handleCreateCombo(w http.ResponseWriter, r *http.Request) {
	var req ComboCreateRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	combo, err := s.Create(r.Context(), req)
	if err != nil {
		writeComboError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, combo)
}

// handleGetCombo serves GET /v1/combos/{id}.
func (s *ComboStore) handleGetCombo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("combo %q not found", id))
		return
	}
	writeJSON(w, http.StatusOK, s.view(rec))
}

// handlePatchCombo serves PATCH /v1/combos/{id}: replaces the
// chain, the strategy and/or the enabled flag. The name cannot
// change (rename is a delete + create): a body name that differs
// from the URL id is a 400 naming name. An explicit empty
// strategy resets to the "priority" default (mirroring the
// strategies feed, where an empty value clears the override).
// Flipping enabled re-mirrors (or unmirrors) atomically with
// the flag, via mirrorComboLocked — patching the chain of a
// disabled combo does NOT re-activate it.
func (s *ComboStore) handlePatchCombo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req struct {
		Name     *string            `json:"name"`
		Chain    *[]json.RawMessage `json:"chain"`
		Strategy *string            `json:"strategy"`
		Enabled  *bool              `json:"enabled"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != id {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("name: cannot rename combo %q (delete it and create a new one)", id))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("combo %q not found", id))
		return
	}
	// Validate everything before mutating anything.
	newChain := rec.chain
	if req.Chain != nil {
		steps, err := s.validateChain(*req.Chain)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		newChain = steps
	}
	newStrat := rec.strategy
	if req.Strategy != nil {
		strat := strings.TrimSpace(*req.Strategy)
		if strat == "" {
			strat = "priority"
		}
		norm, valid := comboValidStrategy(strat)
		if !valid {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("strategy: unknown strategy %q", strat))
			return
		}
		newStrat = norm
	}
	newEnabled := rec.enabled
	if req.Enabled != nil {
		newEnabled = *req.Enabled
	}
	oldChain, oldStrat, oldEnabled, oldUpdated := rec.chain, rec.strategy, rec.enabled, rec.updatedAt
	rec.chain, rec.strategy, rec.enabled = newChain, newStrat, newEnabled
	rec.updatedAt = time.Now().UTC()
	rbMirror := s.mirrorComboLocked(rec)
	rbRec := func() {
		rec.chain, rec.strategy, rec.enabled, rec.updatedAt = oldChain, oldStrat, oldEnabled, oldUpdated
	}
	if !s.persistOrRollback(w, rbMirror, rbRec) {
		return
	}
	writeJSON(w, http.StatusOK, s.view(rec))
}

// handleDeleteCombo serves DELETE /v1/combos/{id}: the record and
// both config mirrors are removed outright — no soft-delete, no
// tombstone. An unknown id is an honest 404, never a silent
// success.
func (s *ComboStore) handleDeleteCombo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("combo %q not found", id))
		return
	}
	delete(s.records, id)
	rbMirror := s.unmirror(id)
	rbRec := func() { s.records[id] = rec }
	if !s.persistOrRollback(w, rbRec, rbMirror) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Deleted   bool   `json:"deleted"`
		ID        string `json:"id"`
		Persisted bool   `json:"persisted"`
	}{Deleted: true, ID: id, Persisted: s.persist != nil})
}

// --- B3: duplicate / import / export / test-run / enable-disable ---

// ComboExportStep is one chain step in the import/export shape:
// provider/model/effort only — NO derived liveness facts
// (has_key/usable/model_verified belong to the live view, not
// to a stored definition, and would fail import's
// unknown-field rejection if they were exported).
type ComboExportStep struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

// ComboExport is the importable shape of one combo: exactly the
// fields POST /v1/combos/import accepts (name, chain, strategy,
// enabled), so an export round-trips through import verbatim.
// Chain steps use the object form ({"provider","model",
// "effort"}), which parseChainStep accepts.
type ComboExport struct {
	Name     string            `json:"name"`
	Chain    []ComboExportStep `json:"chain"`
	Strategy string            `json:"strategy"`
	Enabled  bool              `json:"enabled"`
}

// exportOf builds the importable shape of one record.
func (s *ComboStore) exportOf(rec *comboRecord) ComboExport {
	chain := make([]ComboExportStep, 0, len(rec.chain))
	for _, st := range rec.chain {
		chain = append(chain, ComboExportStep{Provider: st.provider, Model: st.model, Effort: st.effort})
	}
	return ComboExport{Name: rec.name, Chain: chain, Strategy: rec.strategy, Enabled: rec.enabled}
}

// handleExportCombo serves GET /v1/combos/{id}/export: one combo
// in the importable shape. Unknown ids answer 404.
func (s *ComboStore) handleExportCombo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("combo %q not found", id))
		return
	}
	writeJSON(w, http.StatusOK, s.exportOf(rec))
}

// handleExportAllCombos serves GET /v1/combos/export: every
// combo in the importable shape, name-sorted. This is a backup
// file, not a single import body: POST /v1/combos/import takes
// ONE ComboExport, so restore an all-export by importing each
// entry's object one at a time.
func (s *ComboStore) handleExportAllCombos(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.records))
	for name := range s.records {
		names = append(names, name)
	}
	sort.Strings(names)
	combos := make([]ComboExport, 0, len(names))
	for _, name := range names {
		combos = append(combos, s.exportOf(s.records[name]))
	}
	writeJSON(w, http.StatusOK, struct {
		Combos []ComboExport `json:"combos"`
		Count  int           `json:"count"`
	}{Combos: combos, Count: len(combos)})
}

// handleImportCombo serves POST /v1/combos/import: create one
// combo from an exported definition, through the same Import
// path as Create — identical validation (same 400s), same
// unknown-field rejection, same 409 on a colliding name, never
// a silent overwrite. A body holding the all-export envelope
// ({"combos": [...]}) answers 400 naming that it holds N combos
// to import one at a time, instead of the bare "unknown field
// \"combos\"" the strict decoder would otherwise produce.
func (s *ComboStore) handleImportCombo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid combo: %v", err))
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid combo: %v", err))
		return
	}
	if raw, ok := probe["combos"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err == nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("import accepts one combo definition; this file holds %d combos — import them one at a time (each entry's object)", len(list)))
			return
		}
	}
	var req ComboImportRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid combo: %v", err))
		return
	}
	combo, err := s.Import(r.Context(), req)
	if err != nil {
		writeComboError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, combo)
}

// handleDuplicateCombo serves POST /v1/combos/{id}/duplicate:
// copy the combo under a new name. The body {"name"} is
// required and must not collide (409); the copy inherits the
// source's chain, strategy and enabled state verbatim (see the
// file header for why the enabled state is inherited, not
// forced off). Unknown sources answer 404.
func (s *ComboStore) handleDuplicateCombo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req struct {
		Name string `json:"name"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	combo, err := s.Duplicate(r.Context(), id, req.Name)
	if err != nil {
		writeComboError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, combo)
}

// ComboTestRunAttempt is one step of a test-run simulation: the
// step as the router would attempt it, in declared order, with
// the liveness facts the routing decision actually depends on.
// Model is the EFFECTIVE model for the step: the pinned model,
// else the test-run's requested model, else "" (the request-time
// model, as for any group).
type ComboTestRunAttempt struct {
	Step           int    `json:"step"`
	Provider       string `json:"provider"`
	Model          string `json:"model,omitempty"`
	HasKey         bool   `json:"has_key"`
	Usable         bool   `json:"usable"`
	UnusableReason string `json:"unusable_reason,omitempty"`
	ModelVerified  *bool  `json:"model_verified,omitempty"`
	OnFailure      string `json:"on_failure"`
}

// ComboTestRun is the test-run simulation result. WouldServe is
// true when at least one step is usable (a failover chain
// serves while any step can); WouldCallUpstream is always
// false and UpstreamCalls always 0 — the proof, in the
// response itself, that nothing was spent.
type ComboTestRun struct {
	Combo             string                `json:"combo"`
	Strategy          string                `json:"strategy"`
	Model             string                `json:"model,omitempty"`
	Enabled           bool                  `json:"enabled"`
	WouldServe        bool                  `json:"would_serve"`
	WouldCallUpstream bool                  `json:"would_call_upstream"`
	UpstreamCalls     int                   `json:"upstream_calls"`
	QuotaSpent        bool                  `json:"quota_spent"`
	Attempts          []ComboTestRunAttempt `json:"attempts"`
	Note              string                `json:"note"`
}

const comboTestRunNote = "Read-only simulation — no provider was called, no quota was spent, no cost was incurred. Attempts are the combo's declared step order with live usability from the provider config. This proves the routing decision only: it cannot prove live reachability, latency, the re-ranked order a re-ranking strategy would choose at request time from live signals, or the cost of a real call (no price book is consulted here — no cost is estimated, never $0)."

// handleTestRunCombo serves POST /v1/combos/{id}/test-run.
//
// TEST-RUN DECISION (B3 slice doc): READ-ONLY SIMULATION, not
// an operator-confirmed spend. Reasoning:
//   - The dashboard's "test" button is a pre-flight check ("is
//     this combo sane?"), not a purchase: spending quota on a
//     click the operator expects to be free would be the
//     silent-spend failure this slice was told to prevent, and
//     a confirm flag on a test button trains operators to click
//     through spend confirmations.
//   - A real spend adds nothing the simulation cannot already
//     answer for this verb's purpose: whether the chain is
//     wired, ordered and usable. Latency / live reachability /
//     true cost need a real request with a real prompt, which is
//     what an actual chat call is for — not a combo test button.
//   - The ComboStore holds only the config (no price book, no
//     ledger, no provider clients), so even a spend-based
//     test-run could not price itself honestly here; the
//     simulation is the only test-run this layer can implement
//     without inventing numbers.
//   - What it PROVES: the routing decision — the declared step
//     order, the effective strategy, and per-step usability
//     (provider exists/enabled, key present or no-auth local,
//     model pin verified) from the live config. What it CANNOT
//     prove: live reachability, latency, the re-ranked order
//     under live signals, or cost. The response's
//     would_call_upstream=false / upstream_calls=0 /
//     quota_spent=false state the no-spend guarantee in-band.
//   - A disabled combo answers 409: a disabled combo routes
//     nothing, so simulating it would report a routing decision
//     that cannot happen. Enable it first, then test it.
//   - The optional body is {"model"} only: the model a request
//     through this combo would carry (unpinned steps use it).
//     prompt_tokens is deliberately NOT accepted — with no
//     price book here a cost estimate would be invented, and
//     an ignored field would be worse than a rejected one.
func (s *ComboStore) handleTestRunCombo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req struct {
		Model string `json:"model"`
	}
	if !s.decodeOptionalBody(w, r, &req) {
		return
	}
	model := strings.TrimSpace(req.Model)
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("combo %q not found", id))
		return
	}
	if !rec.enabled {
		writeError(w, http.StatusConflict, fmt.Sprintf("combo %q is disabled: enable it before a test run (a disabled combo routes nothing)", id))
		return
	}
	attempts := make([]ComboTestRunAttempt, 0, len(rec.chain))
	wouldServe := false
	for i, st := range rec.chain {
		effModel := st.model
		if effModel == "" {
			effModel = model
		}
		att := ComboTestRunAttempt{Step: i, Provider: st.provider, Model: effModel}
		if p, ok := s.providerByID(st.provider); ok {
			att.HasKey, att.Usable, att.UnusableReason = comboStepFacts(p)
			if effModel != "" {
				verified := false
				for _, m := range p.Models {
					if m.ID == effModel {
						verified = true
						break
					}
				}
				v := verified
				att.ModelVerified = &v
			}
		} else {
			// Provider vanished after the combo was stored:
			// the step stays listed and flagged, never
			// silently dropped — same rule as the live view.
			att.HasKey, att.Usable, att.UnusableReason = false, false, "provider not configured"
			if effModel != "" {
				v := false
				att.ModelVerified = &v
			}
		}
		if i == len(rec.chain)-1 {
			att.OnFailure = "fail the request (no further steps)"
		} else {
			att.OnFailure = fmt.Sprintf("fail over to step %d on a retryable failure (429, 502, 503, 504, 5xx, network error); stop the chain on 400/401/403", i+1)
		}
		if att.Usable {
			wouldServe = true
		}
		attempts = append(attempts, att)
	}
	writeJSON(w, http.StatusOK, ComboTestRun{
		Combo: rec.name, Strategy: rec.strategy, Model: model,
		Enabled: rec.enabled, WouldServe: wouldServe,
		WouldCallUpstream: false, UpstreamCalls: 0, QuotaSpent: false,
		Attempts: attempts, Note: comboTestRunNote,
	})
}

// handleEnableCombo serves POST /v1/combos/{id}/enable and
// handleDisableCombo serves POST /v1/combos/{id}/disable: the
// dedicated enable/disable verbs. Both are idempotent (enabling
// an enabled combo, or disabling a disabled one, answers 200
// with the combo) and take no body. Disabling removes the
// group + strategy mirrors, so "group:<name>" stops routing;
// enabling re-mirrors the stored chain + strategy verbatim.
// Unknown ids answer 404.
func (s *ComboStore) handleEnableCombo(w http.ResponseWriter, r *http.Request) {
	s.handleSetEnabledCombo(w, r, true)
}

func (s *ComboStore) handleDisableCombo(w http.ResponseWriter, r *http.Request) {
	s.handleSetEnabledCombo(w, r, false)
}

func (s *ComboStore) handleSetEnabledCombo(w http.ResponseWriter, r *http.Request, enabled bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("combo %q not found", id))
		return
	}
	oldEnabled, oldUpdated := rec.enabled, rec.updatedAt
	rec.enabled = enabled
	rec.updatedAt = time.Now().UTC()
	rbMirror := s.mirrorComboLocked(rec)
	rbRec := func() { rec.enabled, rec.updatedAt = oldEnabled, oldUpdated }
	if !s.persistOrRollback(w, rbMirror, rbRec) {
		return
	}
	writeJSON(w, http.StatusOK, s.view(rec))
}
