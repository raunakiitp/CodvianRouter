// Live model intelligence: Arena ELO + models.dev tier signals.
//
// This file closes parity tracker B2-14 ("Live intelligence (Arena ELO +
// models.dev)"): the models.dev sync already existed, but there was no
// Arena ELO, no tier derivation, and no ELO/tier signal for scoring to
// read. What lives here:
//
//   - Arena ELO sync: fetches the public LMArena leaderboards
//     (text + code) from the official lmarena-ai Hugging Face
//     leaderboard dataset every 24 h, normalises ELO to a task-fit
//     score in [0.4, 0.98] per leaderboard, and stores it with a
//     7-day TTL. The wulong.dev mirror serves as fallback. Sources
//     are never blended in one refresh. (Live probe 2026-11-20:
//     HF 200, 426 overall / 421 coding rows; wulong 404 "Leaderboard
//     not found" on text, code, overall and coding — the mirror is
//     dead as configured, so HF is primary. models.dev api.json
//     200, 6.78 MB.)
//   - models.dev tier derivation: aggregates each bare model id across
//     providers (max context window, reasoning/tool-call capability) and
//     derives a tier (premium/standard/fast/budget) mapped to per-category
//     task-fit scores. Tier rows never expire; they are rebuilt on the
//     next models.dev sync.
//   - The Intelligence store: the priority-ordered lookup the scorer
//     reads — user_override > arena_elo > models_dev_tier — with
//     variant→base resolution (effort/free suffixes) and expiry filtering.
//
// What is deliberately NOT here (follow-ups for other builders):
//   - The scoring hook: internal/scoring must read TaskFit and use it as
//     a factor. This package only exposes the lookup.
//   - The config surface (an arena_elo block next to models_dev) and the
//     main.go wiring that constructs the ArenaSyncer. ArenaSyncerConfig
//     is ready for that wiring; until then nothing here runs on its own
//     and no network call is made.
//   - The HTTP routes (POST /api/intelligence/sync, overrides): the
//     store methods they need (SetOverride/DeleteOverride/Health) exist.
//
// Parity notes (behaviour, not copied code — this is a clean-room Go
// implementation of the documented behaviour): ELO normalisation,
// confidence thresholds, vendor-prefix list, tier rules and the
// tier→score table follow the published MODEL_INTELLIGENCE behaviour.
// No score is ever invented: a model with no row scores nothing, an
// empty or truncated leaderboard fetch is refused and the last good
// data stays, and a failed sync never clears stored intelligence.
package modelsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─── Sources, categories, confidence ─────────────────────────

// Intelligence sources, in lookup priority order (highest first).
const (
	SourceUserOverride  = "user_override"
	SourceArenaELO      = "arena_elo"
	SourceModelsDevTier = "models_dev_tier"
)

// Task categories an intelligence row covers.
const (
	CategoryCoding        = "coding"
	CategoryReview        = "review"
	CategoryPlanning      = "planning"
	CategoryAnalysis      = "analysis"
	CategoryDebugging     = "debugging"
	CategoryDocumentation = "documentation"
	CategoryDefault       = "default"
)

// AllCategories is every task category, in a stable order.
var AllCategories = []string{
	CategoryCoding, CategoryReview, CategoryPlanning, CategoryAnalysis,
	CategoryDebugging, CategoryDocumentation, CategoryDefault,
}

// Confidence labels, from vote counts.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Vote thresholds for confidence: >=5000 high, >=1000 medium, else low.
const (
	highConfidenceVotes   = 5000
	mediumConfidenceVotes = 1000
)

// ─── Arena defaults ──────────────────────────────────────────

// DefaultArenaURL is the public Arena AI leaderboard API (wulong.dev
// mirror). It is the default for an enabled sync with no URL configured
// — since 2026-11-20 it is the FALLBACK mirror: the wulong API 404s on
// every leaderboard name ("Leaderboard not found"), so the Hugging Face
// dataset is fetched first and this URL is only tried when HF yields
// nothing usable.
const DefaultArenaURL = "https://api.wulong.dev/arena-ai-leaderboards/v1/leaderboard"

// DefaultArenaInterval is the Arena sync cadence: 24 h.
const DefaultArenaInterval = 24 * time.Hour

// ArenaEntryTTL is how long one Arena sync stays live: 7 days.
const ArenaEntryTTL = 7 * 24 * time.Hour

// DefaultArenaTimeoutSeconds bounds one Arena fetch when the caller
// configures no timeout (0).
const DefaultArenaTimeoutSeconds = 30

// MaxArenaBodyBytes bounds one leaderboard fetch (boards are a few
// hundred KB; 16 MiB leaves ample headroom while refusing an
// unbounded read).
const MaxArenaBodyBytes = 16 << 20

// IntelligenceStateFileName is the persisted intelligence state in
// the data dir.
const IntelligenceStateFileName = "model-intelligence.json"

// arenaLeaderboards are the leaderboard categories fetched from the
// Arena API. "vision" is intentionally skipped: it is not relevant
// for text task fitness.
var arenaLeaderboards = []string{"text", "code"}

// arenaCategoryMap maps an Arena leaderboard to the task categories
// its scores apply to.
var arenaCategoryMap = map[string][]string{
	"text": {CategoryDefault, CategoryReview, CategoryDocumentation, CategoryDebugging},
	"code": {CategoryCoding},
}

// ─── Model name normalisation ────────────────────────────────

// vendorPrefixes are the vendor prefixes stripped from Arena model
// names ("anthropic/claude-opus-4" -> "claude-opus-4"). The list is
// the set the leaderboard itself uses; an unknown prefix is left
// alone, never guessed away.
var vendorPrefixes = []string{
	"anthropic/", "openai/", "google/", "meta/", "mistral/",
	"deepseek/", "xai/", "cohere/", "qwen/", "alibaba/",
	"nvidia/", "01-ai/", "zerox/", "together/", "fireworks/",
	"perplexity/", "ai21/",
}

// harnessAnnotationRe matches a trailing harness annotation the
// Arena leaderboard appends to some entries, e.g.
// "gpt-5.6-sol-xhigh (codex-harness)". It describes the scaffold
// the model was measured under, not the model id, so it never
// belongs in a stored key.
var harnessAnnotationRe = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

// NormalizeModelName lowercases a model name, drops a trailing
// harness annotation and strips a known vendor prefix.
func NormalizeModelName(rawName string) string {
	name := strings.ToLower(harnessAnnotationRe.ReplaceAllString(rawName, ""))
	for _, p := range vendorPrefixes {
		if strings.HasPrefix(name, p) {
			return name[len(p):]
		}
	}
	return name
}

// effortTokens are trailing reasoning-effort tokens that mark a
// variant of a base model ("gpt-5.6-sol-xhigh" is "gpt-5.6-sol" at
// xhigh effort). Longest first, so "xhigh" is tried before "high".
// "thinking" covers the Claude "-thinking" variant. The list is the
// union of the effort vocabularies the leaderboard and the dispatch
// layer use; stripping is always catalog-anchored (see
// ResolveBaseModel), so a token that is part of a real model name
// never resolves to a phantom base.
var effortTokens = []string{"thinking", "xhigh", "ultra", "high", "medium", "max", "low", "none"}

// ResolveBaseModel resolves a model id to the id whose quality
// scores it inherits, or returns the id unchanged when it does
// not resolve. Tiers, in order: a trailing "-free" tier marker,
// then a trailing effort token. A stripped base is accepted ONLY
// when known[base] is true — the base must be a model the router
// actually knows, so "qwen3.7-max" never inherits from a phantom
// "qwen3.7". One hop only: a resolved base is not re-resolved.
// known == nil disables resolution entirely.
func ResolveBaseModel(modelID string, known map[string]bool) string {
	if known == nil || modelID == "" {
		return modelID
	}
	if strings.HasSuffix(modelID, "-free") {
		if base := strings.TrimSuffix(modelID, "-free"); base != "" && known[base] {
			return base
		}
	}
	for _, tok := range effortTokens {
		suffix := "-" + tok
		if strings.HasSuffix(modelID, suffix) {
			if base := strings.TrimSuffix(modelID, suffix); base != "" && known[base] {
				return base
			}
		}
	}
	return modelID
}

// ─── Intelligence entries and the store ──────────────────────

// IntelligenceEntry is one (model, source, category) quality row:
// a task-fit score in [0, 1], the raw ELO when the source is
// arena_elo, a confidence label, and when it was synced / expires.
// ExpiresAt == nil means the row never expires (tier + override
// rows); arena_elo rows always carry now+7d.
type IntelligenceEntry struct {
	Model      string     `json:"model"`
	Source     string     `json:"source"`
	Category   string     `json:"category"`
	Score      float64    `json:"score"`
	ELORaw     *float64   `json:"elo_raw,omitempty"`
	Confidence string     `json:"confidence,omitempty"`
	SyncedAt   time.Time  `json:"synced_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Votes      int        `json:"votes,omitempty"`
}

// entryKey is the store map key for one row.
func entryKey(model, source, category string) string {
	return model + "\x00" + source + "\x00" + category
}

// SourceHealth is the per-source aggregate for the dashboard:
// rows held, last sync time, and the oldest expiry (nil = the
// source never expires rows). A source with no rows reports
// count 0 and nil times, never an absent key.
type SourceHealth struct {
	Count           int        `json:"count"`
	LastSync        *time.Time `json:"last_sync,omitempty"`
	OldestExpiresAt *time.Time `json:"oldest_expires_at,omitempty"`
}

// ApplyResult is the honest accounting of one store refresh.
type ApplyResult struct {
	Upserted       int `json:"upserted"`
	Pruned         int `json:"pruned"`
	SkippedInvalid int `json:"skipped_invalid"`
}

// Intelligence is the in-memory model-intelligence store: one row
// per (model, source, category), persisted to a state file (0600,
// atomic write) so a restart keeps the last good data without
// re-fetching. It is safe for concurrent use.
type Intelligence struct {
	mu        sync.RWMutex
	entries   map[string]IntelligenceEntry // keyed by entryKey
	lastSync  map[string]time.Time         // per source
	lastError map[string]string            // per source
	statePath string
}

// NewIntelligence returns an empty store. statePath "" means
// in-memory only (no persistence); otherwise Save/Load use it.
func NewIntelligence(statePath string) *Intelligence {
	return &Intelligence{
		entries:   make(map[string]IntelligenceEntry),
		lastSync:  make(map[string]time.Time),
		lastError: make(map[string]string),
		statePath: statePath,
	}
}

// confidenceForVotes maps a human-preference vote count to a
// confidence label: >=5000 high, >=1000 medium, else low.
func confidenceForVotes(votes int) string {
	switch {
	case votes >= highConfidenceVotes:
		return ConfidenceHigh
	case votes >= mediumConfidenceVotes:
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// round4 rounds a task-fit score to 4 decimals.
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// validScore reports whether a score is a real number in [0, 1].
func validScore(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

// lookupLocked is TaskFit's core: exact match on the normalized
// model, priority override > arena_elo > tier, expired rows
// ignored. Caller holds at least a read lock. now is the instant
// expiry is judged at.
func (in *Intelligence) lookupLocked(model, category string, now time.Time) (float64, string, bool) {
	if category == "" {
		category = CategoryDefault
	}
	for _, source := range []string{SourceUserOverride, SourceArenaELO, SourceModelsDevTier} {
		e, ok := in.entries[entryKey(model, source, category)]
		if !ok {
			continue
		}
		if e.ExpiresAt != nil && !now.Before(*e.ExpiresAt) {
			continue // expired: as if the row were not there
		}
		return e.Score, source, true
	}
	return 0, "", false
}

// TaskFit is the priority-ordered lookup the scorer reads:
// user_override > arena_elo > models_dev_tier, per (model,
// category). The model id is normalised (lowercase, harness
// annotation and vendor prefix stripped). When the exact id has
// no row, one variant hop is tried: a trailing "-free" or effort
// token is stripped and the base id is looked up — the strip is
// accepted only when the base actually has a row, so a phantom
// base never scores. An unknown category reads as "default".
// ok == false means "no signal": the caller must fall back to its
// own default, never to an invented score.
func (in *Intelligence) TaskFit(model, category string) (score float64, source string, ok bool) {
	if in == nil {
		return 0, "", false
	}
	now := time.Now().UTC()
	norm := NormalizeModelName(model)
	if norm == "" {
		return 0, "", false
	}
	if category == "" {
		category = CategoryDefault
	}
	in.mu.RLock()
	defer in.mu.RUnlock()
	if s, src, found := in.lookupLocked(norm, category, now); found {
		return s, src, true
	}
	// One variant hop, anchored by the row itself: strip "-free"
	// or one effort token and retry the base id.
	base := norm
	if strings.HasSuffix(base, "-free") {
		base = strings.TrimSuffix(base, "-free")
	} else {
		for _, tok := range effortTokens {
			if suffix := "-" + tok; strings.HasSuffix(base, suffix) {
				base = strings.TrimSuffix(base, suffix)
				break
			}
		}
	}
	if base != "" && base != norm {
		if s, src, found := in.lookupLocked(base, category, now); found {
			return s, src, true
		}
	}
	return 0, "", false
}

// SetOverride writes a user_override row: the operator's explicit
// score for (model, category). The score is clamped to [0, 1]
// (an out-of-range operator value is a typo, not a score). The
// row never expires until DeleteOverride removes it. The model id
// is normalised on write, so "GPT-4o" and "gpt-4o" are one row.
func (in *Intelligence) SetOverride(model, category string, score float64) {
	if in == nil {
		return
	}
	norm := NormalizeModelName(model)
	if norm == "" {
		return
	}
	if category == "" {
		category = CategoryDefault
	}
	score = math.Max(0, math.Min(1, score))
	in.mu.Lock()
	defer in.mu.Unlock()
	in.entries[entryKey(norm, SourceUserOverride, category)] = IntelligenceEntry{
		Model: norm, Source: SourceUserOverride, Category: category,
		Score: round4(score), Confidence: ConfidenceHigh, SyncedAt: time.Now().UTC(),
	}
	in.lastSync[SourceUserOverride] = time.Now().UTC()
}

// DeleteOverride removes a user_override row. It reports whether
// a row was there to remove.
func (in *Intelligence) DeleteOverride(model, category string) bool {
	if in == nil {
		return false
	}
	norm := NormalizeModelName(model)
	if category == "" {
		category = CategoryDefault
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	key := entryKey(norm, SourceUserOverride, category)
	if _, ok := in.entries[key]; !ok {
		return false
	}
	delete(in.entries, key)
	return true
}

// ApplySource atomically replaces one source's dataset: every
// entry in entries is upserted, and rows of that source that are
// no longer in the set are pruned, in one lock hold. Entries with
// an empty model/category, a non-finite score, or a score outside
// [0, 1] are skipped (counted, never stored). SyncedAt is stamped
// by the caller on each entry; entries without one get now.
func (in *Intelligence) ApplySource(source string, entries []IntelligenceEntry) ApplyResult {
	if in == nil {
		return ApplyResult{}
	}
	now := time.Now().UTC()
	in.mu.Lock()
	defer in.mu.Unlock()
	var res ApplyResult
	keep := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Model == "" || e.Category == "" || !validScore(e.Score) {
			res.SkippedInvalid++
			continue
		}
		e.Source = source
		if e.SyncedAt.IsZero() {
			e.SyncedAt = now
		}
		key := entryKey(e.Model, e.Source, e.Category)
		keep[key] = true
		in.entries[key] = e
		res.Upserted++
	}
	for key, e := range in.entries {
		if e.Source != source || keep[key] {
			continue
		}
		delete(in.entries, key)
		res.Pruned++
	}
	in.lastSync[source] = now
	in.lastError[source] = ""
	return res
}

// ClearSource deletes every row of one source and reports how
// many were removed.
func (in *Intelligence) ClearSource(source string) int {
	if in == nil {
		return 0
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for key, e := range in.entries {
		if e.Source == source {
			delete(in.entries, key)
			n++
		}
	}
	return n
}

// PruneExpired drops arena_elo rows whose TTL has passed and
// reports how many were removed. Tier and override rows never
// expire and are untouched.
func (in *Intelligence) PruneExpired(now time.Time) int {
	if in == nil {
		return 0
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for key, e := range in.entries {
		if e.ExpiresAt != nil && !now.Before(*e.ExpiresAt) {
			delete(in.entries, key)
			n++
		}
	}
	return n
}

// Health returns the per-source aggregate for the dashboard. Every
// declared source is present, even with zero rows.
func (in *Intelligence) Health() map[string]SourceHealth {
	if in == nil {
		return map[string]SourceHealth{}
	}
	in.mu.RLock()
	defer in.mu.RUnlock()
	out := map[string]SourceHealth{
		SourceArenaELO:      {},
		SourceModelsDevTier: {},
		SourceUserOverride:  {},
	}
	for _, e := range in.entries {
		h := out[e.Source]
		h.Count++
		if e.ExpiresAt != nil && (h.OldestExpiresAt == nil || e.ExpiresAt.Before(*h.OldestExpiresAt)) {
			exp := *e.ExpiresAt
			h.OldestExpiresAt = &exp
		}
		out[e.Source] = h
	}
	for src, t := range in.lastSync {
		h := out[src]
		ts := t
		h.LastSync = &ts
		out[src] = h
	}
	return out
}

// intelligenceStateFile is the on-disk shape of the persisted
// intelligence state.
type intelligenceStateFile struct {
	Version   int                  `json:"version"`
	LastSync  map[string]time.Time `json:"last_sync,omitempty"`
	LastError map[string]string    `json:"last_error,omitempty"`
	Entries   []IntelligenceEntry  `json:"entries,omitempty"`
}

// Save persists the store atomically (temp file + rename, 0600).
// With no state path it is a no-op returning nil.
func (in *Intelligence) Save() error {
	if in == nil || in.statePath == "" {
		return nil
	}
	in.mu.RLock()
	sf := intelligenceStateFile{Version: 1}
	sf.LastSync = make(map[string]time.Time, len(in.lastSync))
	for k, v := range in.lastSync {
		sf.LastSync[k] = v
	}
	sf.LastError = make(map[string]string, len(in.lastError))
	for k, v := range in.lastError {
		sf.LastError[k] = v
	}
	keys := make([]string, 0, len(in.entries))
	for k := range in.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sf.Entries = append(sf.Entries, in.entries[k])
	}
	in.mu.RUnlock()
	data, err := json.Marshal(sf)
	if err != nil {
		return fmt.Errorf("modelsync: cannot encode intelligence state: %w", err)
	}
	dir := filepath.Dir(in.statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("modelsync: cannot create state dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".model-intelligence-*.tmp")
	if err != nil {
		return fmt.Errorf("modelsync: cannot create intelligence state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("modelsync: cannot write intelligence state temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("modelsync: cannot chmod intelligence state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("modelsync: cannot fsync intelligence state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("modelsync: cannot close intelligence state temp file: %w", err)
	}
	if err := os.Rename(tmpName, in.statePath); err != nil {
		return fmt.Errorf("modelsync: cannot rename intelligence state file: %w", err)
	}
	return nil
}

// Load reads the persisted state file into the store. A missing
// file means "never synced" and is not an error. A corrupt file
// returns an error and leaves the store empty — the caller logs
// the warning and carries on with no intelligence (startup must
// never fail on a corrupt cache), and the next successful sync
// overwrites the file.
func (in *Intelligence) Load() error {
	if in == nil || in.statePath == "" {
		return nil
	}
	raw, err := os.ReadFile(in.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("modelsync: cannot read intelligence state file %q: %w", in.statePath, err)
	}
	var sf intelligenceStateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return fmt.Errorf("modelsync: invalid intelligence state file %q: %w", in.statePath, err)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.entries = make(map[string]IntelligenceEntry, len(sf.Entries))
	for _, e := range sf.Entries {
		if e.Model == "" || e.Source == "" || e.Category == "" || !validScore(e.Score) {
			continue
		}
		in.entries[entryKey(e.Model, e.Source, e.Category)] = e
	}
	in.lastSync = make(map[string]time.Time, len(sf.LastSync))
	for k, v := range sf.LastSync {
		in.lastSync[k] = v
	}
	in.lastError = make(map[string]string, len(sf.LastError))
	for k, v := range sf.LastError {
		in.lastError[k] = v
	}
	return nil
}

// RecordError records a per-source sync failure honestly (the
// dashboard can show it; the last good entries stay in place).
func (in *Intelligence) RecordError(source, msg string) {
	if in == nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.lastError[source] = msg
}

// LastError returns the last recorded error for a source ("" =
// none, or the last attempt succeeded).
func (in *Intelligence) LastError(source string) string {
	if in == nil {
		return ""
	}
	in.mu.RLock()
	defer in.mu.RUnlock()
	return in.lastError[source]
}

// ─── Arena leaderboard fetch + transform ─────────────────────

// arenaModelEntry is one model in an Arena leaderboard response.
type arenaModelEntry struct {
	Rank    int     `json:"rank"`
	Model   string  `json:"model"`
	Vendor  string  `json:"vendor"`
	Score   float64 `json:"score"`
	CI      float64 `json:"ci"`
	Votes   int     `json:"votes"`
	License string  `json:"license"`
}

// arenaLeaderboard is one leaderboard category from the Arena API:
// metadata plus the ranked models.
type arenaLeaderboard struct {
	Meta struct {
		Leaderboard string `json:"leaderboard"`
		ModelCount  int    `json:"model_count"`
		DatasetDate string `json:"dataset_date"`
	} `json:"meta"`
	Models []arenaModelEntry `json:"models"`
}

// ParseArenaLeaderboard parses one Arena leaderboard document. The
// top level carries "meta" and "models"; a model entry with an
// empty name or a non-finite score fails the whole parse — a
// half-parsed leaderboard must never sync.
func ParseArenaLeaderboard(data []byte) (*arenaLeaderboard, error) {
	var board arenaLeaderboard
	if err := json.Unmarshal(data, &board); err != nil {
		return nil, fmt.Errorf("modelsync: invalid Arena leaderboard JSON: %w", err)
	}
	for _, m := range board.Models {
		if strings.TrimSpace(m.Model) == "" {
			return nil, fmt.Errorf("modelsync: Arena leaderboard entry with empty model name")
		}
		if math.IsNaN(m.Score) || math.IsInf(m.Score, 0) {
			return nil, fmt.Errorf("modelsync: Arena leaderboard entry %q has non-finite score", m.Model)
		}
	}
	return &board, nil
}

// TransformArenaLeaderboards turns fetched leaderboards into
// intelligence entries: per leaderboard, ELO is normalised to a
// task-fit score in [0.4, 0.98] — taskFit = 0.4 + 0.58 *
// ((elo - minElo) / (maxElo - minElo || 1)) — rounded to 4
// decimals, so scores never reach 0 or 1 and leave headroom for
// user overrides. Each model yields one entry per task category
// its leaderboard maps to, expiring now+7d. Boards outside
// {text, code} are ignored (vision is not a text-fitness signal).
// withSynthesizedBaseRows then adds one base row per (base,
// category) for every leaderboard entry that is an effort/free
// variant of a known model id, carrying the BEST task fit among
// that base's variants; a directly measured base row is only
// replaced when a variant scores strictly higher. known == nil
// disables synthesis.
func TransformArenaLeaderboards(boards map[string]*arenaLeaderboard, known map[string]bool, now time.Time) []IntelligenceEntry {
	expires := now.Add(ArenaEntryTTL)
	var entries []IntelligenceEntry
	for _, boardName := range arenaLeaderboards {
		board := boards[boardName]
		if board == nil {
			continue
		}
		taskCategories := arenaCategoryMap[boardName]
		if len(taskCategories) == 0 || len(board.Models) == 0 {
			continue
		}
		minElo, maxElo := board.Models[0].Score, board.Models[0].Score
		for _, m := range board.Models[1:] {
			minElo = math.Min(minElo, m.Score)
			maxElo = math.Max(maxElo, m.Score)
		}
		eloRange := maxElo - minElo
		if eloRange == 0 {
			eloRange = 1
		}
		for _, m := range board.Models {
			norm := NormalizeModelName(m.Model)
			if norm == "" {
				continue
			}
			taskFit := round4(0.4 + 0.58*((m.Score-minElo)/eloRange))
			elo := m.Score
			conf := confidenceForVotes(m.Votes)
			for _, cat := range taskCategories {
				entries = append(entries, IntelligenceEntry{
					Model: norm, Source: SourceArenaELO, Category: cat,
					Score: taskFit, ELORaw: &elo, Confidence: conf,
					SyncedAt: now, ExpiresAt: &expires, Votes: m.Votes,
				})
			}
		}
	}
	return withSynthesizedBaseRows(entries, known)
}

// withSynthesizedBaseRows adds the base rows described in
// TransformArenaLeaderboards. A variant that ResolveBaseModel
// cannot anchor to a known id contributes nothing.
func withSynthesizedBaseRows(entries []IntelligenceEntry, known map[string]bool) []IntelligenceEntry {
	if known == nil {
		return entries
	}
	indexByKey := make(map[string]int, len(entries))
	for i, e := range entries {
		indexByKey[entryKey(e.Model, e.Source, e.Category)] = i
	}
	best := make(map[string]IntelligenceEntry)
	for _, e := range entries {
		base := ResolveBaseModel(e.Model, known)
		if base == e.Model {
			continue
		}
		key := entryKey(base, e.Source, e.Category)
		if cur, ok := best[key]; !ok || e.Score > cur.Score {
			cand := e
			cand.Model = base
			best[key] = cand
		}
	}
	out := entries
	for key, cand := range best {
		if i, ok := indexByKey[key]; ok {
			// The leaderboard measured the base itself — only a
			// strictly better variant wins.
			if cand.Score > out[i].Score {
				out[i] = cand
			}
			continue
		}
		out = append(out, cand)
	}
	return out
}

// ─── Hugging Face fallback source ────────────────────────────

// The Hugging Face source is PRIMARY since 2026-11-20: the wulong.dev
// mirror 404s on every leaderboard name (live probe: text, code,
// overall, coding all "Leaderboard not found"), while the official
// lmarena-ai leaderboard dataset answers 200 on both categories
// (426 overall / 421 coding rows). It is fetched through the Hugging
// Face datasets-server, mapped to the same leaderboard shape the sync
// engine consumes ("overall" -> text, "coding" -> code). Sources are
// never blended in one refresh.
const (
	hfDataset     = "lmarena-ai/leaderboard-dataset"
	hfPageLength  = 100
	hfMaxPages    = 10
	hfMaxAttempts = 3
)

// hfFilterURL is the datasets-server filter endpoint. A var, not a
// const, so tests can point the HF fetch at a local stand-in; the
// production value is the public endpoint above.
var hfFilterURL = "https://datasets-server.huggingface.co/filter"

var hfCategoryMap = map[string]string{"overall": "text", "coding": "code"}

// hfEffortWords are the effort words a trailing parenthetical in
// an Arena display name may carry ("Claude Opus 5.5 (High)" ->
// "claude-opus-5.5-high"). Any other parenthetical is a harness
// annotation and is dropped.
var hfEffortWords = map[string]bool{
	"high": true, "low": true, "medium": true,
	"xhigh": true, "max": true, "thinking": true,
}

var hfTrailingParenRe = regexp.MustCompile(`\s*\(([^)]*)\)\s*$`)

// slugifyArenaDisplayName turns an Arena display name into a model
// id: a recognised effort word in the trailing parentheses is
// folded into the id, any other parenthetical is dropped, the rest
// is lowercased with spaces as "-".
func slugifyArenaDisplayName(displayName string) string {
	name := strings.TrimSpace(displayName)
	effort := ""
	if m := hfTrailingParenRe.FindStringSubmatch(name); m != nil {
		if hfEffortWords[strings.ToLower(strings.TrimSpace(m[1]))] {
			effort = strings.ToLower(strings.TrimSpace(m[1]))
		}
		name = strings.TrimSpace(name[:hfTrailingParenRe.FindStringIndex(name)[0]])
	}
	base := strings.ToLower(strings.Join(strings.Fields(name), "-"))
	if effort != "" {
		return base + "-" + effort
	}
	return base
}

type hfRow struct {
	Row struct {
		ModelName    string   `json:"model_name"`
		Organization *string  `json:"organization"`
		License      *string  `json:"license"`
		Rating       float64  `json:"rating"`
		RatingLower  *float64 `json:"rating_lower"`
		RatingUpper  *float64 `json:"rating_upper"`
		VoteCount    *int     `json:"vote_count"`
		Rank         *int     `json:"rank"`
		DatasetDate  *string  `json:"leaderboard_publish_date"`
	} `json:"row"`
}

type hfPage struct {
	Rows         []hfRow `json:"rows"`
	NumRowsTotal int     `json:"num_rows_total"`
}

// fetchHFCategory fetches one HF leaderboard category (paged, with
// a small retry on 5xx / transport errors only).
func fetchHFCategory(ctx context.Context, client *http.Client, hfCategory, leaderboard string) (*arenaLeaderboard, error) {
	board := &arenaLeaderboard{}
	board.Meta.Leaderboard = leaderboard
	var total = -1
	for page := 0; page < hfMaxPages && (total < 0 || len(board.Models) < total); page++ {
		where := url.QueryEscape(`"category"='` + hfCategory + `'`)
		u := hfFilterURL + "?dataset=" + url.QueryEscape(hfDataset) +
			"&config=text&split=latest&where=" + where +
			fmt.Sprintf("&offset=%d&length=%d", page*hfPageLength, hfPageLength)
		var pg hfPage
		var lastErr error
		for attempt := 1; attempt <= hfMaxAttempts; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return nil, fmt.Errorf("modelsync: cannot build HF request: %w", err)
			}
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "codvian-router arena-elo sync")
			resp, err := client.Do(req)
			if err != nil {
				lastErr = fmt.Errorf("modelsync: HF fetch %q failed: %w", u, err)
				break // transport errors are not retried blindly; the sync loop retries on schedule
			}
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxArenaBodyBytes+1))
			resp.Body.Close()
			if rerr != nil {
				return nil, fmt.Errorf("modelsync: HF fetch %q: cannot read body: %w", u, rerr)
			}
			if resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("modelsync: HF fetch %q: upstream HTTP %d", u, resp.StatusCode)
				time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("modelsync: HF fetch %q: unexpected HTTP status %d", u, resp.StatusCode)
			}
			if len(body) > MaxArenaBodyBytes {
				return nil, fmt.Errorf("modelsync: HF fetch %q: body exceeds %d bytes", u, MaxArenaBodyBytes)
			}
			if err := json.Unmarshal(body, &pg); err != nil {
				return nil, fmt.Errorf("modelsync: invalid HF response JSON: %w", err)
			}
			lastErr = nil
			break
		}
		if lastErr != nil {
			return nil, lastErr
		}
		total = pg.NumRowsTotal
		if len(pg.Rows) == 0 {
			break
		}
		for _, r := range pg.Rows {
			if board.Meta.DatasetDate == "" && r.Row.DatasetDate != nil {
				board.Meta.DatasetDate = *r.Row.DatasetDate
			}
			lower := r.Row.Rating
			if r.Row.RatingLower != nil {
				lower = *r.Row.RatingLower
			}
			upper := r.Row.Rating
			if r.Row.RatingUpper != nil {
				upper = *r.Row.RatingUpper
			}
			votes := 0
			if r.Row.VoteCount != nil {
				votes = *r.Row.VoteCount
			}
			rank := len(board.Models) + 1
			if r.Row.Rank != nil {
				rank = *r.Row.Rank
			}
			vendor := ""
			if r.Row.Organization != nil {
				vendor = *r.Row.Organization
			}
			license := ""
			if r.Row.License != nil {
				license = *r.Row.License
			}
			board.Models = append(board.Models, arenaModelEntry{
				Rank: rank, Model: slugifyArenaDisplayName(r.Row.ModelName),
				Vendor: vendor, Score: r.Row.Rating, CI: (upper - lower) / 2,
				Votes: votes, License: license,
			})
		}
	}
	board.Meta.ModelCount = len(board.Models)
	return board, nil
}

// ─── models.dev tier derivation ──────────────────────────────

// TierCapability is the capability aggregate one bare model id is
// tiered from: the max context window across providers, and
// whether any provider's feed entry reports reasoning / tool-call
// support. A capability the feed never reported reads false — the
// tier then falls to budget, honestly: no capability data means
// no premium claim.
type TierCapability struct {
	Reasoning     bool
	ToolCall      bool
	ContextWindow int
}

// Tier names, derived from capabilities.
const (
	TierPremium  = "premium"
	TierStandard = "standard"
	TierFast     = "fast"
	TierBudget   = "budget"
)

// TierTaskFit is the tier -> per-category task-fit table: the same
// values the live resolver and the persisted rebuild share, so the
// two cannot drift. Scores are fixed per tier, not measured —
// they are the tier's standing, and the arena_elo layer (real
// measured ELO) always outranks them at lookup time.
var TierTaskFit = map[string]map[string]float64{
	TierPremium: {
		CategoryCoding: 0.92, CategoryReview: 0.93, CategoryPlanning: 0.94,
		CategoryAnalysis: 0.95, CategoryDebugging: 0.90,
		CategoryDocumentation: 0.88, CategoryDefault: 0.85,
	},
	TierStandard: {
		CategoryCoding: 0.85, CategoryReview: 0.84, CategoryPlanning: 0.85,
		CategoryAnalysis: 0.85, CategoryDebugging: 0.82,
		CategoryDocumentation: 0.85, CategoryDefault: 0.78,
	},
	TierFast: {
		CategoryCoding: 0.78, CategoryReview: 0.72, CategoryPlanning: 0.70,
		CategoryAnalysis: 0.72, CategoryDebugging: 0.75,
		CategoryDocumentation: 0.80, CategoryDefault: 0.72,
	},
	TierBudget: {
		CategoryCoding: 0.65, CategoryReview: 0.60, CategoryPlanning: 0.55,
		CategoryAnalysis: 0.58, CategoryDebugging: 0.60,
		CategoryDocumentation: 0.70, CategoryDefault: 0.55,
	},
}

// tierConfidence maps a tier to its confidence label.
func tierConfidence(tier string) string {
	switch tier {
	case TierPremium:
		return ConfidenceHigh
	case TierStandard:
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// DeriveTier assigns a tier from capabilities:
//   - reasoning == true -> premium
//   - tool_call == true && context >= 128000 -> standard
//   - tool_call == true -> fast
//   - everything else -> budget
func DeriveTier(cap TierCapability) string {
	if cap.Reasoning {
		return TierPremium
	}
	if cap.ToolCall && cap.ContextWindow >= 128000 {
		return TierStandard
	}
	if cap.ToolCall {
		return TierFast
	}
	return TierBudget
}

// AggregateCapabilities aggregates a models.dev catalog into one
// capability row per bare model id: the max context window across
// providers, and whether ANY provider's entry reports reasoning /
// tool-call support. Deprecated models are skipped entirely. Model
// ids are normalised (lowercase, harness/vendor stripped) so the
// same model served by two providers aggregates into one row.
func AggregateCapabilities(cat *Catalog) map[string]TierCapability {
	out := make(map[string]TierCapability)
	if cat == nil {
		return out
	}
	for _, p := range cat.Providers {
		for _, m := range p.Models {
			if m.Deprecated {
				continue
			}
			id := NormalizeModelName(m.ID)
			if id == "" {
				continue
			}
			cur := out[id]
			if m.ContextWindow > cur.ContextWindow {
				cur.ContextWindow = m.ContextWindow
			}
			if m.Reasoning != nil && *m.Reasoning {
				cur.Reasoning = true
			}
			if m.ToolCall != nil && *m.ToolCall {
				cur.ToolCall = true
			}
			out[id] = cur
		}
	}
	return out
}

// BuildTierEntries derives one models_dev_tier row per (model,
// category) from a models.dev catalog. Tier rows never expire —
// they are rebuilt on the next capabilities sync. An empty
// catalog builds nothing (never an error: there is simply no
// capability data to tier from).
func BuildTierEntries(cat *Catalog, now time.Time) []IntelligenceEntry {
	caps := AggregateCapabilities(cat)
	if len(caps) == 0 {
		return nil
	}
	ids := make([]string, 0, len(caps))
	for id := range caps {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic output order
	var entries []IntelligenceEntry
	for _, id := range ids {
		tier := DeriveTier(caps[id])
		scores := TierTaskFit[tier]
		if scores == nil {
			continue
		}
		conf := tierConfidence(tier)
		for _, catName := range AllCategories {
			score, ok := scores[catName]
			if !ok {
				score = scores[CategoryDefault]
			}
			entries = append(entries, IntelligenceEntry{
				Model: id, Source: SourceModelsDevTier, Category: catName,
				Score: score, Confidence: conf, SyncedAt: now,
			})
		}
	}
	return entries
}

// RebuildTier rebuilds the models_dev_tier dataset from a fresh
// models.dev catalog: derive + atomically replace the tier source
// (stale tier rows prune with it). Call it after a successful
// models.dev sync — never on a failed one, so a failed fetch
// cannot wipe the last good tiers. The caller persists the store
// (Save) afterwards; a nil catalog clears nothing and applies
// nothing.
func (in *Intelligence) RebuildTier(cat *Catalog) ApplyResult {
	if in == nil || cat == nil {
		return ApplyResult{}
	}
	return in.ApplySource(SourceModelsDevTier, BuildTierEntries(cat, time.Now().UTC()))
}

// KnownModels returns the bare, normalised model ids a catalog
// knows, for anchoring Arena variant resolution (ResolveBaseModel
// accepts a base only when it is in this set).
func KnownModels(cat *Catalog) map[string]bool {
	out := make(map[string]bool)
	if cat == nil {
		return out
	}
	for _, p := range cat.Providers {
		for _, m := range p.Models {
			if m.Deprecated {
				continue
			}
			if id := NormalizeModelName(m.ID); id != "" {
				out[id] = true
			}
		}
	}
	return out
}

// ─── Arena syncer ────────────────────────────────────────────

// ArenaSyncerConfig is what an ArenaSyncer needs: the operator's
// opt-in and fetch bounds. HTTPClient is injectable (a client
// pointed at a local stand-in); nil builds a default client with
// the configured timeout. The sync is OFF unless Enabled is true:
// with it off Sync is a no-op that never touches the network.
type ArenaSyncerConfig struct {
	Enabled  bool
	URL      string
	Interval time.Duration
	Timeout  time.Duration
	// HTTPClient, when set, is used as-is. It must already carry
	// a timeout when the caller cares about one.
	HTTPClient *http.Client
}

// ArenaSyncResult is the outcome of one Arena sync run.
type ArenaSyncResult struct {
	Entries int    // intelligence entries stored (all categories)
	Models  int    // distinct models stored
	Skipped bool   // true when the fetch was skipped (fresh/backoff)
	Reason  string // why skipped, when Skipped
	Source  string // upstream that served the sync ("wulong" / "huggingface")
}

// ArenaStatus is a snapshot of the Arena syncer's state for the
// dashboard.
type ArenaStatus struct {
	Enabled       bool
	URL           string
	Interval      time.Duration
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	LastFailedAt  *time.Time
	LastError     string
	LastCount     int
	NextSync      *time.Time
	ActiveSource  string
}

// ArenaSyncer runs the opt-in Arena ELO sync: fetch, transform,
// apply to the Intelligence store, persist. It is safe for
// concurrent use; Sync serialises on a mutex so a ticker and a
// manual run can never overlap.
type ArenaSyncer struct {
	cfg    ArenaSyncerConfig
	client *http.Client
	intel  *Intelligence

	mu           sync.Mutex
	running      bool
	stopCh       chan struct{}
	inProgress   bool
	lastAttempt  *time.Time
	lastSuccess  *time.Time
	lastFailedAt *time.Time
	lastError    string
	lastCount    int
	activeSource string
}

// NewArenaSyncer returns an ArenaSyncer. A disabled config yields
// a syncer whose Sync is a no-op that never touches the network;
// Status still reports enabled=false so the dashboard can say so
// honestly. The default URL is the public Arena leaderboard API;
// the default interval is 24 h; the default timeout is 30 s.
func NewArenaSyncer(cfg ArenaSyncerConfig, intel *Intelligence) *ArenaSyncer {
	if cfg.URL == "" {
		cfg.URL = DefaultArenaURL
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultArenaInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultArenaTimeoutSeconds * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	if intel == nil {
		intel = NewIntelligence("")
	}
	return &ArenaSyncer{cfg: cfg, client: client, intel: intel, stopCh: make(chan struct{})}
}

// Enabled reports whether the operator turned the sync on.
func (s *ArenaSyncer) Enabled() bool { return s.cfg.Enabled }

// Interval returns the configured re-sync interval.
func (s *ArenaSyncer) Interval() time.Duration { return s.cfg.Interval }

// URL returns the leaderboard URL in force.
func (s *ArenaSyncer) URL() string { return s.cfg.URL }

// Status returns a copy of the current sync state.
func (s *ArenaSyncer) Status() ArenaStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ArenaStatus{
		Enabled: s.cfg.Enabled, URL: s.cfg.URL, Interval: s.cfg.Interval,
		LastError: s.lastError, LastCount: s.lastCount, ActiveSource: s.activeSource,
	}
	if s.lastAttempt != nil {
		t := *s.lastAttempt
		st.LastAttemptAt = &t
	}
	if s.lastSuccess != nil {
		t := *s.lastSuccess
		st.LastSuccessAt = &t
		if s.running {
			nx := t.Add(s.cfg.Interval)
			st.NextSync = &nx
		}
	}
	if s.lastFailedAt != nil {
		t := *s.lastFailedAt
		st.LastFailedAt = &t
	}
	return st
}

// fetchArenaBoard fetches and parses one leaderboard category from
// the wulong.dev mirror (the fallback source since 2026-11-20).
func (s *ArenaSyncer) fetchArenaBoard(ctx context.Context, category string) (*arenaLeaderboard, error) {
	u := s.cfg.URL + "?name=" + url.QueryEscape(category)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("modelsync: cannot build Arena request for %q: %w", u, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codvian-router arena-elo sync")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modelsync: Arena fetch %q failed: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modelsync: Arena fetch %q: unexpected HTTP status %d", u, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxArenaBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("modelsync: Arena fetch %q: cannot read body: %w", u, err)
	}
	if len(data) > MaxArenaBodyBytes {
		return nil, fmt.Errorf("modelsync: Arena fetch %q: body exceeds %d bytes", u, MaxArenaBodyBytes)
	}
	return ParseArenaLeaderboard(data)
}

// fetchLeaderboards fetches the text + code boards, Hugging Face
// first (primary since 2026-11-20 — the wulong.dev mirror 404s on
// every leaderboard name), then the wulong.dev mirror as fallback.
// Sources are never blended in one refresh: a fallback refresh is
// entirely the fallback's data. An error is returned only when no
// source yields a usable board; a single-category failure is logged
// and the other board still syncs.
func (s *ArenaSyncer) fetchLeaderboards(ctx context.Context) (map[string]*arenaLeaderboard, string, error) {
	hfBoards, hfErrs := s.fetchHFBoards(ctx)
	if usableBoards(hfBoards) {
		return hfBoards, "huggingface", nil
	}
	slog.Warn("modelsync: Hugging Face Arena source unusable, trying wulong.dev mirror fallback",
		"errors", strings.Join(hfErrs, "; "))
	boards, wulongErrs := s.fetchWulongBoards(ctx)
	if usableBoards(boards) {
		return boards, "wulong", nil
	}
	return nil, "", fmt.Errorf("modelsync: no usable Arena leaderboard: huggingface: %s; wulong: %s",
		strings.Join(hfErrs, "; "), strings.Join(wulongErrs, "; "))
}

// fetchHFBoards fetches the text + code boards from the Hugging Face
// dataset, one category at a time (its pages are small; sequential
// keeps the primary fetch simple and bounded).
func (s *ArenaSyncer) fetchHFBoards(ctx context.Context) (map[string]*arenaLeaderboard, []string) {
	hfBoards := make(map[string]*arenaLeaderboard)
	var hfErrs []string
	for hfCat, boardName := range hfCategoryMap {
		b, err := fetchHFCategory(ctx, s.client, hfCat, boardName)
		if err != nil {
			slog.Warn("modelsync: HF fetch failed, skipping category",
				"category", hfCat, "error", err)
			hfErrs = append(hfErrs, err.Error())
			continue
		}
		hfBoards[boardName] = b
	}
	return hfBoards, hfErrs
}

// fetchWulongBoards fetches the text + code boards concurrently
// from the wulong.dev mirror.
func (s *ArenaSyncer) fetchWulongBoards(ctx context.Context) (map[string]*arenaLeaderboard, []string) {
	type boardRes struct {
		cat   string
		board *arenaLeaderboard
		err   error
	}
	results := make(chan boardRes, len(arenaLeaderboards))
	var wg sync.WaitGroup
	for _, cat := range arenaLeaderboards {
		wg.Add(1)
		go func(cat string) {
			defer wg.Done()
			b, err := s.fetchArenaBoard(ctx, cat)
			results <- boardRes{cat: cat, board: b, err: err}
		}(cat)
	}
	wg.Wait()
	close(results)
	boards := make(map[string]*arenaLeaderboard)
	var errs []string
	for r := range results {
		if r.err != nil {
			slog.Warn("modelsync: Arena leaderboard fetch failed, skipping category",
				"category", r.cat, "error", r.err)
			errs = append(errs, r.err.Error())
			continue
		}
		boards[r.cat] = r.board
	}
	return boards, errs
}

// usableBoards reports whether any board in the set has models.
func usableBoards(boards map[string]*arenaLeaderboard) bool {
	for _, b := range boards {
		if len(b.Models) > 0 {
			return true
		}
	}
	return false
}

// Sync runs one Arena sync: freshness guard, failure backoff,
// fetch, transform, apply, persist. When disabled it is a no-op
// that returns a zero result and never touches the network. On a
// fetch/transform failure it records the error + failure time
// (backoff), keeps the last good entries in place, and returns
// the error. A persist failure is recorded but does not fail the
// sync itself (the in-memory state is correct; only restart
// durability is lost). known anchors variant resolution (see
// ResolveBaseModel); nil disables base-row synthesis.
func (s *ArenaSyncer) Sync(ctx context.Context, known map[string]bool) (ArenaSyncResult, error) {
	if !s.cfg.Enabled {
		return ArenaSyncResult{}, nil
	}
	now := time.Now().UTC()
	s.mu.Lock()
	if s.inProgress {
		s.mu.Unlock()
		return ArenaSyncResult{}, fmt.Errorf("modelsync: Arena sync already in progress")
	}
	s.inProgress = true
	s.lastAttempt = &now
	// Freshness guard: a non-empty dataset synced within the
	// interval is still good — skip the fetch entirely.
	if s.lastSuccess != nil && s.lastCount > 0 && !now.Before(*s.lastSuccess) &&
		now.Sub(*s.lastSuccess) < s.cfg.Interval {
		res := ArenaSyncResult{Entries: s.lastCount, Skipped: true, Reason: "fresh", Source: s.activeSource}
		s.inProgress = false
		s.mu.Unlock()
		return res, nil
	}
	// Failure backoff: a recent failed sync means the upstream is
	// probably still rate-limited or down — skip instead of
	// hammering it. Backoff is interval/4 (6 h at the 24 h default).
	if s.lastFailedAt != nil && now.Sub(*s.lastFailedAt) < s.cfg.Interval/4 {
		res := ArenaSyncResult{Skipped: true, Reason: "backoff", Source: s.activeSource}
		s.inProgress = false
		s.mu.Unlock()
		return res, nil
	}
	s.mu.Unlock()

	fail := func(err error) (ArenaSyncResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastFailedAt = &now
		s.lastError = err.Error()
		if s.intel != nil {
			s.intel.RecordError(SourceArenaELO, err.Error())
		}
		return ArenaSyncResult{}, err
	}

	boards, source, err := s.fetchLeaderboards(ctx)
	if err != nil {
		return fail(err)
	}
	entries := TransformArenaLeaderboards(boards, known, now)
	if len(entries) == 0 {
		return fail(fmt.Errorf("modelsync: Arena leaderboards yielded 0 entries — refusing empty sync, keeping last good data"))
	}
	models := make(map[string]bool, len(entries))
	for _, e := range entries {
		models[e.Model] = true
	}
	s.intel.PruneExpired(now)
	res := s.intel.ApplySource(SourceArenaELO, entries)
	_ = res // counts are visible in Health; the sync result reports stored rows
	if err := s.intel.Save(); err != nil {
		// The sync itself succeeded; only restart durability is
		// lost. Record it, but do not report the sync as failed.
		s.mu.Lock()
		s.lastError = fmt.Sprintf("sync applied, but intelligence state could not be persisted: %v", err)
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.lastSuccess = &now
	s.lastError = ""
	s.lastCount = len(entries)
	s.activeSource = source
	s.inProgress = false
	s.mu.Unlock()
	return ArenaSyncResult{Entries: len(entries), Models: len(models), Source: source}, nil
}

// DryRun fetches and transforms without writing anything: it
// reports how many entries a sync would store. Disabled syncers
// return 0 entries and no error.
func (s *ArenaSyncer) DryRun(ctx context.Context, known map[string]bool) ([]IntelligenceEntry, error) {
	if !s.cfg.Enabled {
		return nil, nil
	}
	boards, _, err := s.fetchLeaderboards(ctx)
	if err != nil {
		return nil, err
	}
	return TransformArenaLeaderboards(boards, known, time.Now().UTC()), nil
}

// Start begins the periodic sync in the background: one immediate
// sync, then a tick every interval. It returns immediately and
// never blocks the caller; sync errors are logged, never fatal.
// A second Start is a no-op. known supplies the variant-resolution
// anchor set on each tick (nil supplier means no synthesis).
func (s *ArenaSyncer) Start(ctx context.Context, known func() map[string]bool) {
	s.mu.Lock()
	if s.running || !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}
	s.running = true
	stopCh := s.stopCh
	s.mu.Unlock()
	go func() {
		syncOnce := func() {
			var k map[string]bool
			if known != nil {
				k = known()
			}
			if _, err := s.Sync(ctx, k); err != nil {
				slog.Warn("modelsync: Arena ELO sync failed", "error", err)
			}
		}
		syncOnce()
		ticker := time.NewTicker(s.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				syncOnce()
			}
		}
	}()
}

// Stop halts the periodic sync started by Start. It does not
// interrupt a sync already in flight.
func (s *ArenaSyncer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.running = false
	close(s.stopCh)
	s.stopCh = make(chan struct{})
}

// Intelligence returns the store this syncer writes to.
func (s *ArenaSyncer) Intelligence() *Intelligence { return s.intel }
