package config

// Cache-write tier rates (M6.9b): config ModelPrice carries
// cache_write_5m_usd_per_1m / cache_write_1h_usd_per_1m through
// JSON parse, validation, Known and Clone, following the
// CachedInputUSDPer1M pattern exactly.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestModelPriceCacheWriteTierRatesRoundTrip(t *testing.T) {
	raw := `{"input_usd_per_1m":3.0,"output_usd_per_1m":15.0,"cached_input_usd_per_1m":0.3,"cache_write_5m_usd_per_1m":3.75,"cache_write_1h_usd_per_1m":6.0}`
	var p ModelPrice
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal ModelPrice: %v", err)
	}
	if p.CacheWrite5mUSDPer1M == nil || *p.CacheWrite5mUSDPer1M != 3.75 {
		t.Errorf("5m rate = %v, want 3.75", p.CacheWrite5mUSDPer1M)
	}
	if p.CacheWrite1hUSDPer1M == nil || *p.CacheWrite1hUSDPer1M != 6.0 {
		t.Errorf("1h rate = %v, want 6.0", p.CacheWrite1hUSDPer1M)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate tier rates: %v", err)
	}
	if !p.Known() {
		t.Errorf("Known = false, want true with tier rates set")
	}
	// Marshal round-trips the exact field names.
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal ModelPrice: %v", err)
	}
	var back ModelPrice
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal ModelPrice: %v", err)
	}
	if back.CacheWrite5mUSDPer1M == nil || *back.CacheWrite5mUSDPer1M != 3.75 || back.CacheWrite1hUSDPer1M == nil || *back.CacheWrite1hUSDPer1M != 6.0 {
		t.Errorf("round-tripped tier rates = %v/%v, want 3.75/6.0", back.CacheWrite5mUSDPer1M, back.CacheWrite1hUSDPer1M)
	}
	// Absent tier rates stay nil (unknown, never zero-filled).
	var absent ModelPrice
	if err := json.Unmarshal([]byte(`{"input_usd_per_1m":3.0,"output_usd_per_1m":15.0}`), &absent); err != nil {
		t.Fatalf("unmarshal without tiers: %v", err)
	}
	if absent.CacheWrite5mUSDPer1M != nil || absent.CacheWrite1hUSDPer1M != nil {
		t.Errorf("absent tier rates = %v/%v, want nil/nil", absent.CacheWrite5mUSDPer1M, absent.CacheWrite1hUSDPer1M)
	}
	// A configured $0 tier rate is a real price, distinct from absent.
	zero := ModelPrice{CacheWrite5mUSDPer1M: fptr(0)}
	if err := zero.Validate(); err != nil {
		t.Errorf("Validate $0 tier rate: %v", err)
	}
	if !zero.Known() {
		t.Errorf("Known = false, want true with a $0 tier rate configured")
	}
	// Negative tier rates are rejected, naming the field.
	neg := ModelPrice{InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(15), CacheWrite1hUSDPer1M: fptr(-1)}
	if err := neg.Validate(); err == nil {
		t.Errorf("Validate negative 1h tier rate: got nil, want an error")
	}
	// Clone deep-copies the tier rates: mutating the clone
	// never moves the original.
	orig := ModelPrice{InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(15), CacheWrite5mUSDPer1M: fptr(3.75), CacheWrite1hUSDPer1M: fptr(6)}
	clone := orig.clone()
	*clone.CacheWrite5mUSDPer1M = 99
	*clone.CacheWrite1hUSDPer1M = 99
	if *orig.CacheWrite5mUSDPer1M != 3.75 || *orig.CacheWrite1hUSDPer1M != 6 {
		t.Errorf("clone mutated original tier rates to %v/%v", *orig.CacheWrite5mUSDPer1M, *orig.CacheWrite1hUSDPer1M)
	}
}

func TestLoadPriceFileCacheWriteTierRates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	content := `{"anthropic": {"claude-sonnet-4-5": {"input_usd_per_1m": 3.0, "output_usd_per_1m": 15.0, "cache_write_5m_usd_per_1m": 3.75, "cache_write_1h_usd_per_1m": 6.0}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write price file: %v", err)
	}
	book, err := LoadPriceFile(path)
	if err != nil {
		t.Fatalf("LoadPriceFile: %v", err)
	}
	p, ok := book.LookupPair("anthropic", "claude-sonnet-4-5")
	if !ok {
		t.Fatalf("LookupPair anthropic/claude-sonnet-4-5: not found")
	}
	if p.CacheWrite5mUSDPer1M == nil || *p.CacheWrite5mUSDPer1M != 3.75 || p.CacheWrite1hUSDPer1M == nil || *p.CacheWrite1hUSDPer1M != 6.0 {
		t.Errorf("price-file tier rates = %v/%v, want 3.75/6.0", p.CacheWrite5mUSDPer1M, p.CacheWrite1hUSDPer1M)
	}
}
