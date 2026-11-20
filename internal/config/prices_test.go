package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func fptr(v float64) *float64 { return &v }

func TestModelPriceValidate(t *testing.T) {
	// Valid: both sides set, cached set, zero (free) allowed.
	if err := (ModelPrice{InputUSDPer1M: fptr(2.5), OutputUSDPer1M: fptr(10), CachedInputUSDPer1M: fptr(0.25)}).Validate(); err != nil {
		t.Errorf("valid price rejected: %v", err)
	}
	if err := (ModelPrice{InputUSDPer1M: fptr(0), OutputUSDPer1M: fptr(0)}).Validate(); err != nil {
		t.Errorf("free price rejected: %v", err)
	}
	// All-nil is valid (explicitly unknown).
	if err := (ModelPrice{}).Validate(); err != nil {
		t.Errorf("unknown price rejected: %v", err)
	}
	for name, p := range map[string]ModelPrice{
		"negative input":  {InputUSDPer1M: fptr(-1), OutputUSDPer1M: fptr(10)},
		"negative output": {InputUSDPer1M: fptr(1), OutputUSDPer1M: fptr(-0.5)},
		"negative cached": {InputUSDPer1M: fptr(1), OutputUSDPer1M: fptr(2), CachedInputUSDPer1M: fptr(-1)},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: negative price accepted", name)
		}
	}
}

func TestModelPriceBlended(t *testing.T) {
	p := ModelPrice{InputUSDPer1M: fptr(2), OutputUSDPer1M: fptr(10)}
	if got, ok := p.BlendedUSDPer1M(); !ok || got != 6 {
		t.Errorf("blended = %v, %v (want 6, true)", got, ok)
	}
	// Half-known is unknown, never guessed from one side.
	half := ModelPrice{InputUSDPer1M: fptr(2)}
	if _, ok := half.BlendedUSDPer1M(); ok {
		t.Errorf("half-known price blended as known")
	}
	if _, ok := (ModelPrice{}).BlendedUSDPer1M(); ok {
		t.Errorf("unknown price blended as known")
	}
	// Free model blends to 0 and is known (distinct from unknown).
	free := ModelPrice{InputUSDPer1M: fptr(0), OutputUSDPer1M: fptr(0)}
	if got, ok := free.BlendedUSDPer1M(); !ok || got != 0 {
		t.Errorf("free blended = %v, %v (want 0, true)", got, ok)
	}
	if !free.Known() || (ModelPrice{}).Known() {
		t.Errorf("Known() wrong: free=%v unknown=%v", free.Known(), (ModelPrice{}).Known())
	}
}

func TestLoadPriceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(`{
		"model-a": {"input_usd_per_1m": 2.5, "output_usd_per_1m": 10.0, "cached_input_usd_per_1m": 0.25},
		"model-free": {"input_usd_per_1m": 0, "output_usd_per_1m": 0}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	book, err := LoadPriceFile(path)
	if err != nil {
		t.Fatalf("LoadPriceFile: %v", err)
	}
	if !book.HasFile || book.FileModels != 2 {
		t.Errorf("book = HasFile %v FileModels %d (want true, 2)", book.HasFile, book.FileModels)
	}
	if got, ok := book.BlendedUSDPer1M("", "model-a"); !ok || got != 6.25 {
		t.Errorf("model-a blended = %v, %v (want 6.25, true)", got, ok)
	}
	if book.SourceModel("model-a") != "price-file" {
		t.Errorf("model-a source = %q (want price-file)", book.SourceModel("model-a"))
	}
	if _, ok := book.BlendedUSDPer1M("", "model-missing"); ok {
		t.Errorf("missing model blended as known")
	}
	// Negative price in the file fails the whole load, naming the model.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"model-bad": {"input_usd_per_1m": -1, "output_usd_per_1m": 5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPriceFile(bad); err == nil {
		t.Errorf("negative price in file accepted")
	}
	// Missing file is an error (fail-closed, not silent).
	if _, err := LoadPriceFile(filepath.Join(dir, "nope.json")); err == nil {
		t.Errorf("missing price file accepted")
	}
	// Malformed JSON is an error.
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPriceFile(malformed); err == nil {
		t.Errorf("malformed price file accepted")
	}
}

func TestLoadPriceBookPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(`{
		"shared": {"input_usd_per_1m": 1, "output_usd_per_1m": 3},
		"file-only": {"input_usd_per_1m": 4, "output_usd_per_1m": 6}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(dir)
	cfg.PriceFile = path
	cfg.Prices = map[string]ModelPrice{
		"shared":      {InputUSDPer1M: fptr(10), OutputUSDPer1M: fptr(20)},
		"config-only": {InputUSDPer1M: fptr(5), OutputUSDPer1M: fptr(5)},
	}
	book, err := cfg.LoadPriceBook()
	if err != nil {
		t.Fatalf("LoadPriceBook: %v", err)
	}
	// Config-file entry wins over the price file for the same model.
	if got, ok := book.BlendedUSDPer1M("", "shared"); !ok || got != 15 {
		t.Errorf("shared blended = %v, %v (want 15, true: config wins)", got, ok)
	}
	if book.SourceModel("shared") != "config" {
		t.Errorf("shared source = %q (want config)", book.SourceModel("shared"))
	}
	if got, ok := book.BlendedUSDPer1M("", "file-only"); !ok || got != 5 {
		t.Errorf("file-only blended = %v, %v (want 5, true)", got, ok)
	}
	if got, ok := book.BlendedUSDPer1M("", "config-only"); !ok || got != 5 {
		t.Errorf("config-only blended = %v, %v (want 5, true)", got, ok)
	}
	if !book.HasFile || book.FileModels != 2 {
		t.Errorf("book = HasFile %v FileModels %d (want true, 2)", book.HasFile, book.FileModels)
	}
	// Neither source: empty book, no error, every model unknown.
	empty, err := DefaultConfig(dir).LoadPriceBook()
	if err != nil {
		t.Fatalf("empty LoadPriceBook: %v", err)
	}
	if _, ok := empty.BlendedUSDPer1M("", "anything"); ok {
		t.Errorf("empty book priced a model")
	}
	// Set but unreadable price file fails loudly.
	cfg2 := DefaultConfig(dir)
	cfg2.PriceFile = filepath.Join(dir, "missing.json")
	if _, err := cfg2.LoadPriceBook(); err == nil {
		t.Errorf("missing configured price file accepted")
	}
}

func TestConfigValidatePrices(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Prices = map[string]ModelPrice{
		"bad": {InputUSDPer1M: fptr(-2), OutputUSDPer1M: fptr(1)},
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("negative config price accepted")
	}
	cfg.Prices = map[string]ModelPrice{
		"ok": {InputUSDPer1M: fptr(1), OutputUSDPer1M: fptr(2)},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config price rejected: %v", err)
	}
	// Negative legacy per-token price on a provider model is rejected.
	cfg.Prices = nil
	cfg.Providers = []ProviderConfig{{
		ID: "p1", Name: "P1", BaseURL: "http://127.0.0.1:9",
		Models: []provider.Model{{ID: "m", InputPriceUSD: -1}},
	}}
	if err := cfg.Validate(); err == nil {
		t.Errorf("negative provider model price accepted")
	}
}

func TestConfigPricesRoundTripAndClone(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Prices = map[string]ModelPrice{
		"m1": {InputUSDPer1M: fptr(1.5), OutputUSDPer1M: fptr(3), CachedInputUSDPer1M: fptr(0.15)},
	}
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	book, err := loaded.LoadPriceBook()
	if err != nil {
		t.Fatalf("LoadPriceBook: %v", err)
	}
	if v, ok := book.BlendedUSDPer1M("", "m1"); !ok || v != 2.25 {
		t.Errorf("round-tripped blended = %v, %v (want 2.25, true)", v, ok)
	}
	// Clone deep-copies: mutating the clone's price must not move
	// the original.
	clone := cfg.Clone()
	*clone.Prices["m1"].InputUSDPer1M = 99
	if v, _ := cfg.Prices["m1"].BlendedUSDPer1M(); v != 2.25 {
		t.Errorf("clone mutated original: blended now %v", v)
	}
}

// TestPriceBookPairKey proves prices are keyed by (provider,
// model): the same model at two providers resolves to different
// prices, the ledger/strategy/autorouter pair price wins over the
// model-level fallback, and an unknown pair stays unknown.
func TestPriceBookPairKey(t *testing.T) {
	book := &PriceBook{
		Pairs: map[PriceKey]ModelPrice{
			{Provider: "cheap-provider", Model: "shared"}:     {InputUSDPer1M: fptr(1), OutputUSDPer1M: fptr(3)},
			{Provider: "expensive-provider", Model: "shared"}: {InputUSDPer1M: fptr(50), OutputUSDPer1M: fptr(70)},
			{Provider: "cheap-provider", Model: "free"}:       {InputUSDPer1M: fptr(0), OutputUSDPer1M: fptr(0)},
		},
		PairSources: map[PriceKey]string{
			{Provider: "cheap-provider", Model: "shared"}:     "config",
			{Provider: "expensive-provider", Model: "shared"}: "price-file",
		},
		Entries: map[string]ModelPrice{
			// Model-level fallback: applies only where no pair
			// price exists — it is a fallback, not a pair price.
			"shared": {InputUSDPer1M: fptr(10), OutputUSDPer1M: fptr(20)},
		},
		Sources: map[string]string{"shared": "config"},
	}
	// Same model, different price per provider.
	if got, ok := book.BlendedUSDPer1M("cheap-provider", "shared"); !ok || got != 2 {
		t.Errorf("cheap pair blended = %v, %v (want 2, true)", got, ok)
	}
	if got, ok := book.BlendedUSDPer1M("expensive-provider", "shared"); !ok || got != 60 {
		t.Errorf("expensive pair blended = %v, %v (want 60, true)", got, ok)
	}
	// LookupPair sees only the pair: the fallback provider has no
	// pair price of its own.
	if _, ok := book.LookupPair("other-provider", "shared"); ok {
		t.Errorf("LookupPair(other-provider, shared) found a pair price (want none: only the fallback exists)")
	}
	if got, ok := book.BlendedPair("other-provider", "shared"); ok {
		t.Errorf("BlendedPair(other-provider, shared) = %v (want unknown: pair-only lookup)", got)
	}
	// Lookup with the fallback provider still resolves — via the
	// MODEL-LEVEL FALLBACK, not a pair price.
	if got, ok := book.BlendedUSDPer1M("other-provider", "shared"); !ok || got != 15 {
		t.Errorf("fallback blended = %v, %v (want 15, true)", got, ok)
	}
	if got, ok := book.BlendedModel("shared"); !ok || got != 15 {
		t.Errorf("BlendedModel(shared) = %v, %v (want 15, true)", got, ok)
	}
	// Unknown pair stays unknown, never $0.
	if _, ok := book.BlendedUSDPer1M("nobody", "missing"); ok {
		t.Errorf("unknown pair blended as known")
	}
	if _, ok := book.Lookup("nobody", "missing"); ok {
		t.Errorf("unknown pair looked up as known")
	}
	// Sources distinguish the pair level from the fallback level.
	if s := book.SourcePair("cheap-provider", "shared"); s != "config" {
		t.Errorf("cheap pair source = %q (want config)", s)
	}
	if s := book.Source("other-provider", "shared"); s != "config" {
		t.Errorf("fallback source = %q (want config)", s)
	}
	// Free pair blends to 0 and is known (distinct from unknown).
	if got, ok := book.BlendedUSDPer1M("cheap-provider", "free"); !ok || got != 0 {
		t.Errorf("free pair blended = %v, %v (want 0, true)", got, ok)
	}
	// Cheapest pair for the model is the cheap provider's $2.
	if got, ok := book.CheapestBlendedUSDPer1M("shared"); !ok || got != 2 {
		t.Errorf("cheapest blended = %v, %v (want 2, true)", got, ok)
	}
	if _, ok := book.CheapestBlendedUSDPer1M("missing"); ok {
		t.Errorf("cheapest blended for unknown model = known")
	}
	// SortedPairs is ordered by provider, then model.
	pairs := book.SortedPairs()
	if len(pairs) != 3 || pairs[0].Provider != "cheap-provider" || pairs[0].Model != "free" {
		t.Errorf("SortedPairs = %v (want cheap-provider/free first)", pairs)
	}
	// Merge keeps the receiver's pair on conflict and adds the
	// other's pairs and model-level entries.
	other := NewPriceBook()
	other.Pairs[PriceKey{Provider: "cheap-provider", Model: "shared"}] = ModelPrice{InputUSDPer1M: fptr(100), OutputUSDPer1M: fptr(100)}
	other.PairSources[PriceKey{Provider: "cheap-provider", Model: "shared"}] = "price-file"
	other.Pairs[PriceKey{Provider: "new-provider", Model: "shared"}] = ModelPrice{InputUSDPer1M: fptr(4), OutputUSDPer1M: fptr(6)}
	other.Entries["extra"] = ModelPrice{InputUSDPer1M: fptr(1), OutputUSDPer1M: fptr(1)}
	book.Merge(other)
	if got, _ := book.BlendedUSDPer1M("cheap-provider", "shared"); got != 2 {
		t.Errorf("Merge overwrote receiver pair: blended = %v (want 2)", got)
	}
	if got, ok := book.BlendedUSDPer1M("new-provider", "shared"); !ok || got != 5 {
		t.Errorf("merged new pair blended = %v, %v (want 5, true)", got, ok)
	}
	if _, ok := book.LookupModel("extra"); !ok {
		t.Errorf("Merge dropped the other book's model-level entry")
	}
	// Clone deep-copies pairs: mutating the clone must not move
	// the original.
	clone := book.Clone()
	*clone.Pairs[PriceKey{Provider: "cheap-provider", Model: "shared"}].InputUSDPer1M = 99
	if v, _ := book.BlendedUSDPer1M("cheap-provider", "shared"); v != 2 {
		t.Errorf("clone mutated original pair: blended now %v", v)
	}
}

func TestLoadPriceFileNestedShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	// Mixed file: nested per-provider pairs plus a flat
	// model-level fallback entry.
	if err := os.WriteFile(path, []byte(`{
		"cheap-provider": {"shared": {"input_usd_per_1m": 1, "output_usd_per_1m": 3}},
		"expensive-provider": {"shared": {"input_usd_per_1m": 50, "output_usd_per_1m": 70}},
		"legacy-model": {"input_usd_per_1m": 4, "output_usd_per_1m": 6}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	book, err := LoadPriceFile(path)
	if err != nil {
		t.Fatalf("LoadPriceFile: %v", err)
	}
	if !book.HasFile || book.FileModels != 3 {
		t.Errorf("book = HasFile %v FileModels %d (want true, 3: two pairs + one fallback)", book.HasFile, book.FileModels)
	}
	if got, ok := book.BlendedUSDPer1M("cheap-provider", "shared"); !ok || got != 2 {
		t.Errorf("cheap pair blended = %v, %v (want 2, true)", got, ok)
	}
	if got, ok := book.BlendedUSDPer1M("expensive-provider", "shared"); !ok || got != 60 {
		t.Errorf("expensive pair blended = %v, %v (want 60, true)", got, ok)
	}
	if got, ok := book.BlendedModel("legacy-model"); !ok || got != 5 {
		t.Errorf("legacy fallback blended = %v, %v (want 5, true)", got, ok)
	}
	if book.SourcePair("cheap-provider", "shared") != "price-file" {
		t.Errorf("cheap pair source = %q (want price-file)", book.SourcePair("cheap-provider", "shared"))
	}
	// A bad nested price fails the whole load, naming provider
	// and model.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"prov": {"m": {"input_usd_per_1m": -1, "output_usd_per_1m": 5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPriceFile(bad); err == nil {
		t.Errorf("negative nested price accepted")
	} else if !contains(err.Error(), "prov") || !contains(err.Error(), `"m"`) {
		t.Errorf("nested price error %v does not name provider + model", err)
	}
}

func TestLoadPriceBookPairPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(`{
		"file-provider": {"shared": {"input_usd_per_1m": 1, "output_usd_per_1m": 3}},
		"shared": {"input_usd_per_1m": 10, "output_usd_per_1m": 20}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(dir)
	cfg.PriceFile = path
	cfg.ProviderPrices = map[string]map[string]ModelPrice{
		"file-provider": {"shared": {InputUSDPer1M: fptr(100), OutputUSDPer1M: fptr(100)}},
		"cfg-provider":  {"shared": {InputUSDPer1M: fptr(4), OutputUSDPer1M: fptr(6)}},
	}
	// A config model-level price must NOT beat the file's pair
	// price: the pair is the more specific claim (documented
	// specificity rule — config wins only within its own level).
	cfg.Prices = map[string]ModelPrice{
		"shared": {InputUSDPer1M: fptr(1000), OutputUSDPer1M: fptr(1000)},
	}
	book, err := cfg.LoadPriceBook()
	if err != nil {
		t.Fatalf("LoadPriceBook: %v", err)
	}
	// Config pair wins over the same file pair.
	if got, ok := book.BlendedUSDPer1M("file-provider", "shared"); !ok || got != 100 {
		t.Errorf("config pair blended = %v, %v (want 100, true: config wins within the pair level)", got, ok)
	}
	if book.SourcePair("file-provider", "shared") != "config" {
		t.Errorf("config pair source = %q (want config)", book.SourcePair("file-provider", "shared"))
	}
	// Config-only pair is visible.
	if got, ok := book.BlendedUSDPer1M("cfg-provider", "shared"); !ok || got != 5 {
		t.Errorf("config-only pair blended = %v, %v (want 5, true)", got, ok)
	}
	// No pair for this provider: the file's pair price is NOT
	// used (it belongs to another provider) — the config
	// model-level fallback applies instead.
	if got, ok := book.BlendedUSDPer1M("other-provider", "shared"); !ok || got != 1000 {
		t.Errorf("fallback blended = %v, %v (want 1000, true: config model-level fallback)", got, ok)
	}
}

func TestConfigProviderPricesRoundTripAndClone(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.ProviderPrices = map[string]map[string]ModelPrice{
		"openai": {"gpt-4o": {InputUSDPer1M: fptr(2.5), OutputUSDPer1M: fptr(10)}},
	}
	cfg.Prices = map[string]ModelPrice{
		"legacy": {InputUSDPer1M: fptr(1), OutputUSDPer1M: fptr(2)},
	}
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	book, err := loaded.LoadPriceBook()
	if err != nil {
		t.Fatalf("LoadPriceBook: %v", err)
	}
	if v, ok := book.BlendedUSDPer1M("openai", "gpt-4o"); !ok || v != 6.25 {
		t.Errorf("round-tripped pair blended = %v, %v (want 6.25, true)", v, ok)
	}
	if v, ok := book.BlendedUSDPer1M("", "legacy"); !ok || v != 1.5 {
		t.Errorf("round-tripped fallback blended = %v, %v (want 1.5, true)", v, ok)
	}
	// Clone deep-copies pair prices too.
	clone := cfg.Clone()
	*clone.ProviderPrices["openai"]["gpt-4o"].InputUSDPer1M = 99
	if v, _ := cfg.ProviderPrices["openai"]["gpt-4o"].BlendedUSDPer1M(); v != 6.25 {
		t.Errorf("clone mutated original pair price: blended now %v", v)
	}
	// Invalid pair prices are rejected at Validate.
	cfg.ProviderPrices = map[string]map[string]ModelPrice{
		"openai": {"bad": {InputUSDPer1M: fptr(-1), OutputUSDPer1M: fptr(1)}},
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("negative pair price accepted")
	}
	cfg.ProviderPrices = map[string]map[string]ModelPrice{
		"": {"m": {InputUSDPer1M: fptr(1), OutputUSDPer1M: fptr(1)}},
	}
	if err := cfg.Validate(); err == nil {
		t.Errorf("empty provider key in provider_prices accepted")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestPriceBookSyncedLayerPrecedence (CHECKLIST 179): the opt-in
// models.dev layer is consulted LAST — an operator price at either
// level always wins — and the source names it honestly.
func TestPriceBookSyncedLayerPrecedence(t *testing.T) {
	book := NewPriceBook()
	book.Pairs[PriceKey{Provider: "openai", Model: "gpt-4o"}] = ModelPrice{InputUSDPer1M: fptr(2), OutputUSDPer1M: fptr(10)}
	book.PairSources[PriceKey{Provider: "openai", Model: "gpt-4o"}] = "config"
	book.Entries["claude-sonnet"] = ModelPrice{InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(15)}
	book.Sources["claude-sonnet"] = "config"
	book.SetSyncedPrices(map[PriceKey]SyncedPrice{
		{Provider: "anthropic", Model: "claude-sonnet"}: {Price: ModelPrice{InputUSDPer1M: fptr(88), OutputUSDPer1M: fptr(88)}, SyncedAt: timeNow()},
		{Provider: "openai", Model: "gpt-4o-mini"}:      {Price: ModelPrice{InputUSDPer1M: fptr(0.15), OutputUSDPer1M: fptr(0.6)}, SyncedAt: timeNow()},
	})
	// Operator pair price beats the synced price for the same pair.
	if p, ok := book.Lookup("openai", "gpt-4o"); !ok || *p.InputUSDPer1M != 2 {
		t.Errorf("operator pair price lost to synced price: %+v", p)
	}
	if src := book.Source("openai", "gpt-4o"); src != "config" {
		t.Errorf("Source(openai/gpt-4o) = %q, want config", src)
	}
	// Operator MODEL-LEVEL price beats the synced PAIR price for
	// the same model at another provider — the pair-wins rule must
	// not let a synced pair shadow an operator fallback.
	if p, ok := book.Lookup("anthropic", "claude-sonnet"); !ok || *p.InputUSDPer1M != 3 {
		t.Errorf("operator model-level price lost to synced pair: %+v", p)
	}
	// No operator price anywhere: the synced price applies, and
	// the source says so.
	if p, ok := book.Lookup("openai", "gpt-4o-mini"); !ok || *p.InputUSDPer1M != 0.15 {
		t.Errorf("synced price did not apply: %+v", p)
	}
	if src := book.SourcePair("openai", "gpt-4o-mini"); src != "models.dev" {
		t.Errorf("SourcePair(synced) = %q, want models.dev", src)
	}
	if _, ok := book.SyncedAtPair("openai", "gpt-4o-mini"); !ok {
		t.Error("SyncedAtPair(synced) reports no sync time")
	}
	if _, ok := book.SyncedAtPair("openai", "gpt-4o"); ok {
		t.Error("SyncedAtPair(operator pair) reports a sync time for an operator price")
	}
	// OperatorHasPrice is the sync's oracle: true for operator
	// prices at either level, false for synced-only pairs.
	if !book.OperatorHasPrice("openai", "gpt-4o") || !book.OperatorHasPrice("anthropic", "claude-sonnet") {
		t.Error("OperatorHasPrice false for an operator-priced pair")
	}
	if book.OperatorHasPrice("openai", "gpt-4o-mini") {
		t.Error("OperatorHasPrice true for a synced-only pair — the sync would never fill it in")
	}
	// CheapestBlended + HasPriceForModel see synced prices too.
	if v, ok := book.CheapestBlendedUSDPer1M("gpt-4o-mini"); !ok || v != 0.375 {
		t.Errorf("cheapest synced blended = %v, want 0.375", v)
	}
	if !book.HasPriceForModel("gpt-4o-mini") {
		t.Error("HasPriceForModel false for a synced-only model")
	}
	if got := book.SortedSyncedPairs(); len(got) != 2 {
		t.Errorf("synced pairs = %d, want 2", len(got))
	}
	// Replacing the layer wholesale drops pairs (copy-on-write).
	book.SetSyncedPrices(nil)
	if _, ok := book.Lookup("openai", "gpt-4o-mini"); ok {
		t.Error("cleared synced layer still serves a price")
	}
	// Clone copies the synced layer independently: a later
	// SetSyncedPrices on the clone must not move the original.
	book.SetSyncedPrices(map[PriceKey]SyncedPrice{
		{Provider: "openai", Model: "gpt-4o-mini"}: {Price: ModelPrice{InputUSDPer1M: fptr(0.15), OutputUSDPer1M: fptr(0.6)}, SyncedAt: timeNow()},
	})
	clone := book.Clone()
	clone.SetSyncedPrices(nil)
	if _, ok := book.Lookup("openai", "gpt-4o-mini"); !ok {
		t.Error("clone's SetSyncedPrices moved the original's synced layer")
	}
	if _, ok := clone.Lookup("openai", "gpt-4o-mini"); ok {
		t.Error("clone still serves a price after clearing its own layer")
	}
}

func timeNow() time.Time { return time.Now().UTC() }
