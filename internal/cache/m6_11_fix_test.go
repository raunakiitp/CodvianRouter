// M6.11-fix — tenant + temperature scoping for the semantic layer.
//
// These tests prove the fix for the two BLOCKED M6.11 findings
// (see internal/degrade/m6_11_property_test.go):
//   - BLOCKED-1: a cross-tenant semantic paraphrase was served.
//   - BLOCKED-2: the semantic layer had no temperature dimension.
//
// Scope rule (cache.go): an entry stored through a Scoped API
// records tenant (gateway key id, "" = anonymous — a real tenant)
// and TemperatureBucket(temperature), the exact layer's 2-decimal
// bucketing. A scoped lookup serves only an entry recorded with
// the same tenant AND the same temperature bucket. An entry
// stored through a legacy (unscoped) API records neither and is
// NEVER served by a scoped lookup — it fails closed. The legacy
// lookup APIs stay unscoped by design (raw/diagnostic path,
// pinned by TestM611SemanticLayerIsTenantBlind): they serve any
// entry naming the model, scoped or not.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const m611FixModel = "test-model"
const m611FixStored = "What is the capital of France?"
const m611FixParaphrase = "What is the capital of France today?"

// m611FixScopedStore stores content under (tenant, temp) through
// the scoped API and fails the test on error (there is none —
// the in-memory store never errors; the helper just reads
// cleanly).
func m611FixScopedStore(t *testing.T, sem *LocalSemanticCache, tenant string, temp float64, text, content string) {
	t.Helper()
	sem.SetTextForModelScoped(context.Background(), m611FixModel, text, tenant, temp,
		SemanticEntry{Response: CachedResponse{Content: content, Model: m611FixModel}}, time.Hour)
}

// TestM611FixScopedTenantIsolation proves BLOCKED-1 at the layer:
// an entry stored scoped for tenant A is never served to tenant
// B — identical text or paraphrase — while tenant A keeps
// hitting it, including its paraphrase. Parameterised over
// tenant pairs (including anonymous "" vs named, both
// directions), with distinct contents per tenant so a
// cross-serve is visible, not just a hit/miss. Same-model
// sharing across providers still works: there is no provider
// dimension, so the same model id shares; a different model
// still misses.
func TestM611FixScopedTenantIsolation(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-b"},
		{"gk-1", "gk-2"},
		{"", "gk-named"},
		{"gk-named", ""},
	}
	ctx := context.Background()
	for _, pair := range pairs {
		sem := NewLocalSemanticCache()
		m611FixScopedStore(t, sem, pair[0], 0, m611FixStored, "Paris-A")
		m611FixScopedStore(t, sem, pair[1], 0, m611FixStored, "Paris-B")
		// Each tenant hits its OWN entry (same text and
		// paraphrase), never the other's.
		for _, tc := range []struct {
			tenant, text, want string
		}{
			{pair[0], m611FixStored, "Paris-A"},
			{pair[0], m611FixParaphrase, "Paris-A"},
			{pair[1], m611FixStored, "Paris-B"},
			{pair[1], m611FixParaphrase, "Paris-B"},
		} {
			got, ok := sem.GetTextForModelScopedWithThreshold(ctx, m611FixModel, tc.text, tc.tenant, 0, 0)
			if !ok || got.Response.Content != tc.want {
				t.Fatalf("tenants %q/%q: scoped lookup tenant %q text %q = (%q, %v), want (%q, true)",
					pair[0], pair[1], tc.tenant, tc.text, got.Response.Content, ok, tc.want)
			}
		}
		// The two scopes coexist as two slots: neither store
		// replaced the other's entry.
		if n := sem.Size(); n != 2 {
			t.Fatalf("tenants %q/%q: cache size = %d, want 2 (one slot per tenant scope)", pair[0], pair[1], n)
		}
		// Re-storing under the same scope replaces in place,
		// not a third slot.
		m611FixScopedStore(t, sem, pair[0], 0, m611FixStored, "Paris-A2")
		if n := sem.Size(); n != 2 {
			t.Fatalf("tenants %q/%q: cache size after same-scope re-store = %d, want 2", pair[0], pair[1], n)
		}
		if got, ok := sem.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, pair[0], 0, 0); !ok || got.Response.Content != "Paris-A2" {
			t.Fatalf("tenants %q/%q: same-scope re-store lookup = (%q, %v), want (Paris-A2, true)", pair[0], pair[1], got.Response.Content, ok)
		}
	}
	// Cross-provider sharing (M6.12 divergence): the same model
	// id shares — the lookup below names the same model the
	// entry was stored for, and there is no provider dimension
	// to vary — while a different model still misses.
	sem := NewLocalSemanticCache()
	m611FixScopedStore(t, sem, "tenant-a", 0, m611FixStored, "Paris")
	if got, ok := sem.GetTextForModelScopedWithThreshold(ctx, "test-model", m611FixStored, "tenant-a", 0, 0); !ok || got.Response.Content != "Paris" {
		t.Fatalf("same-model scoped lookup = (%q, %v), want (Paris, true)", got.Response.Content, ok)
	}
	if _, ok := sem.GetTextForModelScopedWithThreshold(ctx, "other-model", m611FixStored, "tenant-a", 0, 0); ok {
		t.Fatal("scoped lookup served a test-model entry to other-model; want miss (model scope)")
	}
}

// TestM611FixScopedTemperatureIsolation proves BLOCKED-2 at the
// layer: an entry stored scoped at temp 0.7 is never served to a
// temp-0 lookup — identical text or paraphrase — while the same
// temperature keeps hitting it. Bucket semantics mirror the
// exact layer: 0.704 shares 0.700's bucket, 0.71 does not share
// 0.70's.
func TestM611FixScopedTemperatureIsolation(t *testing.T) {
	ctx := context.Background()
	sem := NewLocalSemanticCache()
	m611FixScopedStore(t, sem, "tenant-a", 0.7, m611FixStored, "hot-Paris")
	// Control: same temperature hits, identical and paraphrase.
	for _, text := range []string{m611FixStored, m611FixParaphrase} {
		if got, ok := sem.GetTextForModelScopedWithThreshold(ctx, m611FixModel, text, "tenant-a", 0.7, 0); !ok || got.Response.Content != "hot-Paris" {
			t.Fatalf("same-temp lookup text %q = (%q, %v), want (hot-Paris, true)", text, got.Response.Content, ok)
		}
	}
	// Property: temp-0 misses, identical and paraphrase alike.
	for _, text := range []string{m611FixStored, m611FixParaphrase} {
		if got, ok := sem.GetTextForModelScopedWithThreshold(ctx, m611FixModel, text, "tenant-a", 0, 0); ok {
			t.Fatalf("temp-0 lookup text %q served the temp-0.7 entry %q; want miss", text, got.Response.Content)
		}
	}
	// Bucket semantics: 0.704 shares 0.700, 0.71 does not.
	sem2 := NewLocalSemanticCache()
	m611FixScopedStore(t, sem2, "tenant-a", 0.700, m611FixStored, "bucket-answer")
	if got, ok := sem2.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0.704, 0); !ok || got.Response.Content != "bucket-answer" {
		t.Fatalf("0.704 lookup = (%q, %v), want (bucket-answer, true): same bucket must share", got.Response.Content, ok)
	}
	if got, ok := sem2.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0.71, 0); ok {
		t.Fatalf("0.71 lookup served the 0.70 entry %q; want miss (different bucket)", got.Response.Content)
	}
	// Reverse direction: a temp-0 entry is never served to a
	// temp-0.7 lookup.
	sem3 := NewLocalSemanticCache()
	m611FixScopedStore(t, sem3, "tenant-a", 0, m611FixStored, "cold-Paris")
	if got, ok := sem3.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0.7, 0); ok {
		t.Fatalf("temp-0.7 lookup served the temp-0 entry %q; want miss", got.Response.Content)
	}
	if got, ok := sem3.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0, 0); !ok || got.Response.Content != "cold-Paris" {
		t.Fatalf("temp-0 lookup = (%q, %v), want (cold-Paris, true)", got.Response.Content, ok)
	}
}

// TestM611FixLegacyEntryFailsClosedScoped proves the migration
// rule: an entry stored through a legacy (unscoped) API records
// no tenant/temperature, so NO scoped lookup ever serves it —
// any tenant, any temperature, identical text or paraphrase.
// The legacy lookup still serves it (pinned compat: the raw
// path is unscoped by design), and a scoped entry is still
// visible to the legacy lookup (the probe
// TestM611NoCrossTenantSemanticExactText relies on).
func TestM611FixLegacyEntryFailsClosedScoped(t *testing.T) {
	ctx := context.Background()
	sem := NewLocalSemanticCache()
	sem.SetTextForModel(ctx, m611FixModel, m611FixStored,
		SemanticEntry{Response: CachedResponse{Content: "Paris", Model: m611FixModel}}, time.Hour)
	// Fail closed: no scoped lookup serves the legacy entry.
	for _, tenant := range []string{"tenant-a", "tenant-b", ""} {
		for _, temp := range []float64{0, 0.7} {
			for _, text := range []string{m611FixStored, m611FixParaphrase} {
				if got, ok := sem.GetTextForModelScopedWithThreshold(ctx, m611FixModel, text, tenant, temp, 0); ok {
					t.Fatalf("scoped lookup tenant %q temp %v text %q served legacy entry %q; want miss (fail closed)",
						tenant, temp, text, got.Response.Content)
				}
			}
		}
	}
	// Pinned compat: the legacy lookup still serves it.
	if got, ok := sem.GetTextForModel(ctx, m611FixModel, m611FixStored); !ok || got.Response.Content != "Paris" {
		t.Fatalf("legacy lookup = (%q, %v), want (Paris, true)", got.Response.Content, ok)
	}
	// And a scoped entry is still visible to the legacy lookup
	// (the engine test's "layer still holds the entry" probe).
	sem2 := NewLocalSemanticCache()
	m611FixScopedStore(t, sem2, "tenant-a", 0, m611FixStored, "Paris")
	if got, ok := sem2.GetTextForModel(ctx, m611FixModel, m611FixStored); !ok || got.Response.Content != "Paris" {
		t.Fatalf("legacy lookup of scoped entry = (%q, %v), want (Paris, true)", got.Response.Content, ok)
	}
}

// TestM611FixScopedWideVector proves the scope filter is
// enforced on the legacy wide-vector path too (vectors longer
// than SemanticVectorDims, scored by the full scan in
// getLegacyScopedLocked): a wide entry stored scoped for tenant
// A is served to A and to nobody else, at no other temperature.
// While a wide entry lives, every lookup takes the legacy scan,
// so the filter there is the only thing standing between
// tenants — a filter applied only on the indexed path would
// leak exactly here.
func TestM611FixScopedWideVector(t *testing.T) {
	ctx := context.Background()
	// A 300-dimensional unit vector (dims 0 and 1 only).
	vec := make([]float32, 300)
	vec[0], vec[1] = 0.6, 0.8
	entry := SemanticEntry{Response: CachedResponse{Content: "wide-answer", Model: m611FixModel}}
	sem := NewLocalSemanticCache()
	sem.SetForModelScoped(ctx, m611FixModel, vec, "tenant-a", 0, entry)
	// Control: same tenant, same temperature, same vector hits.
	if got, ok := sem.GetForModelScopedWithThreshold(ctx, m611FixModel, vec, "tenant-a", 0, 0); !ok || got.Response.Content != "wide-answer" {
		t.Fatalf("wide same-scope lookup = (%q, %v), want (wide-answer, true)", got.Response.Content, ok)
	}
	// Property: other tenant misses, other temperature misses.
	if got, ok := sem.GetForModelScopedWithThreshold(ctx, m611FixModel, vec, "tenant-b", 0, 0); ok {
		t.Fatalf("wide cross-tenant lookup served %q; want miss", got.Response.Content)
	}
	if got, ok := sem.GetForModelScopedWithThreshold(ctx, m611FixModel, vec, "tenant-a", 0.7, 0); ok {
		t.Fatalf("wide cross-temp lookup served %q; want miss", got.Response.Content)
	}
	// With the wide entry live (legacy scan for every lookup),
	// a normal 256-dim entry is still scoped correctly.
	sem.SetTextForModelScoped(ctx, m611FixModel, m611FixStored, "tenant-a", 0,
		SemanticEntry{Response: CachedResponse{Content: "Paris", Model: m611FixModel}}, time.Hour)
	if got, ok := sem.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-b", 0, 0); ok {
		t.Fatalf("normal entry under wide-scan lookup served cross-tenant %q; want miss", got.Response.Content)
	}
	if got, ok := sem.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0, 0); !ok || got.Response.Content != "Paris" {
		t.Fatalf("normal entry under wide-scan lookup = (%q, %v), want (Paris, true)", got.Response.Content, ok)
	}
}

// TestM611FixPersistentScopedSurvivesRestart proves the
// persistent adapter carries the scope across a restart: scoped
// entries stored, closed with no Sync, reopened — same tenant /
// same temperature still hit, cross-tenant and cross-temperature
// still miss. A Sync + reopen keeps the same property.
func TestM611FixPersistentScopedSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sem.db")
	psc, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	if err := psc.SetTextForModelScoped(ctx, m611FixModel, m611FixStored, "tenant-a", 0.7,
		SemanticEntry{Response: CachedResponse{Content: "hot-Paris", Model: m611FixModel}}, time.Hour); err != nil {
		t.Fatalf("SetTextForModelScoped: %v", err)
	}
	if err := psc.SetTextForModelScoped(ctx, m611FixModel, m611FixStored, "tenant-a", 0,
		SemanticEntry{Response: CachedResponse{Content: "cold-Paris", Model: m611FixModel}}, time.Hour); err != nil {
		t.Fatalf("SetTextForModelScoped: %v", err)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer psc2.Close()
	for _, tc := range []struct {
		tenant string
		temp   float64
		want   string
	}{
		{"tenant-a", 0.7, "hot-Paris"},
		{"tenant-a", 0, "cold-Paris"},
	} {
		if got, ok := psc2.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, tc.tenant, tc.temp, 0); !ok || got.Response.Content != tc.want {
			t.Fatalf("reopened scoped lookup tenant %q temp %v = (%q, %v), want (%q, true)", tc.tenant, tc.temp, got.Response.Content, ok, tc.want)
		}
	}
	// Cross-tenant and cross-temperature still miss after restart.
	if got, ok := psc2.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixParaphrase, "tenant-b", 0.7, 0); ok {
		t.Fatalf("reopened cross-tenant lookup served %q; want miss", got.Response.Content)
	}
	if got, ok := psc2.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixParaphrase, "tenant-a", 0, 0); !ok || got.Response.Content != "cold-Paris" {
		// Paraphrase at temp 0 must hit the COLD entry, never the hot one.
		t.Fatalf("reopened temp-0 paraphrase = (%q, %v), want (cold-Paris, true)", got.Response.Content, ok)
	}
	// Sync + reopen keeps the scope too.
	if _, err := psc2.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := psc2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc3, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen after Sync: %v", err)
	}
	defer psc3.Close()
	if got, ok := psc3.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0.7, 0); !ok || got.Response.Content != "hot-Paris" {
		t.Fatalf("post-Sync reopen scoped lookup = (%q, %v), want (hot-Paris, true)", got.Response.Content, ok)
	}
	if _, ok := psc3.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-b", 0.7, 0); ok {
		t.Fatal("post-Sync reopen cross-tenant lookup hit; want miss")
	}
}

// TestM611FixPersistentLegacyRowFailsClosed proves the
// migration rule for on-disk rows written before M6.11: a row
// with no tenant/temp recorded reloads as a legacy (unscoped)
// entry — a scoped lookup never serves it, while the legacy
// lookup still does (pinned compat). The old-schema table is
// migrated in place on open (tenant/temp_bucket columns +
// new primary key), and a scoped store written after the
// migration works and survives a reopen.
func TestM611FixPersistentLegacyRowFailsClosed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sem.db")
	// Write one row with the PRE-M6.11 schema, by hand.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE semantic_entries(
		model TEXT NOT NULL,
		vec_key TEXT NOT NULL,
		vector BLOB NOT NULL,
		response TEXT NOT NULL,
		expires_unix INTEGER NOT NULL,
		PRIMARY KEY(model, vec_key)
	)`); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	vec := EmbedText(m611FixStored)
	raw, err := json.Marshal(SemanticEntry{Response: CachedResponse{Content: "Paris", Model: m611FixModel}})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO semantic_entries(model, vec_key, vector, response, expires_unix) VALUES(?,?,?,?,?)`,
		m611FixModel, semanticVecKey(vec), encodeSemanticVector(vec), string(raw), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db Close: %v", err)
	}
	// Open: the migration must run, and the row must load legacy.
	psc, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache (migration): %v", err)
	}
	defer psc.Close()
	var schemaSQL string
	if err := psc.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='semantic_entries'`).Scan(&schemaSQL); err != nil {
		t.Fatalf("schema read: %v", err)
	}
	if !strings.Contains(schemaSQL, "temp_bucket") || !strings.Contains(schemaSQL, "tenant") {
		t.Fatalf("migrated schema = %q, want tenant + temp_bucket columns", schemaSQL)
	}
	// Fail closed: no scoped lookup serves the legacy row.
	for _, tenant := range []string{"tenant-a", ""} {
		for _, temp := range []float64{0, 0.7} {
			if got, ok := psc.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, tenant, temp, 0); ok {
				t.Fatalf("scoped lookup tenant %q temp %v served migrated legacy row %q; want miss", tenant, temp, got.Response.Content)
			}
		}
	}
	// Pinned compat: the legacy lookup still serves it.
	if got, ok := psc.GetTextForModel(ctx, m611FixModel, m611FixStored); !ok || got.Response.Content != "Paris" {
		t.Fatalf("legacy lookup of migrated row = (%q, %v), want (Paris, true)", got.Response.Content, ok)
	}
	// A scoped store after the migration writes a scoped row
	// (new primary key: same model+vector under a tenant does
	// not clobber the legacy row) and survives a reopen.
	if err := psc.SetTextForModelScoped(ctx, m611FixModel, m611FixStored, "tenant-a", 0,
		SemanticEntry{Response: CachedResponse{Content: "Paris-scoped", Model: m611FixModel}}, time.Hour); err != nil {
		t.Fatalf("SetTextForModelScoped after migration: %v", err)
	}
	if got, ok := psc.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0, 0); !ok || got.Response.Content != "Paris-scoped" {
		t.Fatalf("scoped lookup after migration = (%q, %v), want (Paris-scoped, true)", got.Response.Content, ok)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	psc2, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	defer psc2.Close()
	if got, ok := psc2.GetTextForModelScopedWithThreshold(ctx, m611FixModel, m611FixStored, "tenant-a", 0, 0); !ok || got.Response.Content != "Paris-scoped" {
		t.Fatalf("reopened scoped lookup = (%q, %v), want (Paris-scoped, true)", got.Response.Content, ok)
	}
	if got, ok := psc2.GetTextForModel(ctx, m611FixModel, m611FixStored); !ok {
		t.Fatalf("reopened legacy lookup missed (found=%v); want the legacy row still held", ok)
	} else if got.Response.Content != "Paris" && got.Response.Content != "Paris-scoped" {
		t.Fatalf("reopened legacy lookup = %q, want Paris or Paris-scoped", got.Response.Content)
	}
}
