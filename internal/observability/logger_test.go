// Acceptance tests for M2.3 — structured JSON logs.
// Written FIRST (failing) per the Wave 1 plan; implementation lands after.
package observability

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONLogRequestEmitsOneDecodableLine(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf)
	log.LogRequest(RequestLogEntry{
		RequestID: "req-123", Provider: "openai", Model: "gpt-4o",
		Outcome: "ok", LatencyMs: 42.5, UpstreamLatencyMs: 40.1,
		TokensIn: 100, TokensOut: 50, TokensCached: 20, CostUSD: 0.0015,
		Cache: "hit", CacheLayer: "exact", FallbackAttempts: 1,
		FallbackPath: "openai->anthropic",
	})
	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, "\n") {
		t.Fatalf("log output is not a single line: %q", buf.String())
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%q)", err, line)
	}
	checks := map[string]any{
		"request_id": "req-123", "provider": "openai", "model": "gpt-4o",
		"outcome": "ok", "cache": "hit", "cache_layer": "exact",
		"fallback_path": "openai->anthropic",
	}
	for k, want := range checks {
		if row[k] != want {
			t.Fatalf("field %s = %v, want %v (row: %s)", k, row[k], want, line)
		}
	}
	if row["latency_ms"] != 42.5 || row["tokens_in"] != float64(100) || row["cost_usd"] != 0.0015 {
		t.Fatalf("numeric fields wrong: %s", line)
	}
	if row["fallback_attempts"] != float64(1) {
		t.Fatalf("fallback_attempts wrong: %s", line)
	}
	// The log must never carry prompt/response content or secrets: there is
	// no field for them, and the entry type must not accept one.
	if _, ok := row["prompt"]; ok {
		t.Fatalf("log row carries prompt content: %s", line)
	}
	if _, ok := row["api_key"]; ok {
		t.Fatalf("log row carries a key: %s", line)
	}
}

func TestJSONLogRequestErrorClass(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf)
	log.LogRequest(RequestLogEntry{
		RequestID: "req-err", Provider: "anthropic", Model: "claude-sonnet",
		Outcome: "error", ErrorClass: "rate_limited", LatencyMs: 12,
	})
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if row["error_class"] != "rate_limited" || row["outcome"] != "error" {
		t.Fatalf("error row wrong: %s", buf.String())
	}
}

func TestLoggerNilWriterIsNoop(t *testing.T) {
	log := NewLogger(nil)
	// Must not panic: a logger with nowhere to write drops rows silently.
	log.LogRequest(RequestLogEntry{RequestID: "x", Outcome: "ok"})
}
