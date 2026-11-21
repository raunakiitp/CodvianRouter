//go:build otel

package observability

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type collectedPayload struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []struct {
				Key   string `json:"key"`
				Value struct {
					StringValue string `json:"stringValue"`
				} `json:"value"`
			} `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID      string `json:"traceId"`
				SpanID       string `json:"spanId"`
				Name         string `json:"name"`
				Kind         int    `json:"kind"`
				Attributes   []struct {
					Key   string `json:"key"`
					Value struct {
						StringValue string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
				StartTimeUnixNano string `json:"startTimeUnixNano"`
				EndTimeUnixNano   string `json:"endTimeUnixNano"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

func waitForPayload(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("collector received no payload within 5s")
		return nil
	}
}

func parseInt64(t *testing.T, s string) int64 {
	t.Helper()
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// TestOTLPExporterShipsRecordedSpans drives the full export path with a
// LOCAL test collector: the gateway POSTs one OTLP/JSON batch, and the
// collector decodes ids as base64 of raw bytes (the OTLP/JSON mapping),
// so this is a real wire-format check, not a shape echo.
func TestOTLPExporterShipsRecordedSpans(t *testing.T) {
	payloads := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if h := r.Header.Get("X-Collector-Auth"); h != "token-123" {
			t.Errorf("auth header = %q, want token-123", h)
		}
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		payloads <- b
	}))
	defer srv.Close()

	tr := NewConfiguredTracer(TracerConfig{
		Endpoint:  srv.URL,
		Headers:   map[string]string{"X-Collector-Auth": "token-123"},
		TimeoutMs: 2000,
	})
	_, span := tr.StartRequestSpan(context.Background(), "req-1")
	span.SetAttribute("http.route", "/v1/chat/completions")
	span.End()

	var p collectedPayload
	if err := json.Unmarshal(waitForPayload(t, payloads), &p); err != nil {
		t.Fatalf("payload not valid OTLP/JSON: %v", err)
	}
	if len(p.ResourceSpans) != 1 || len(p.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("envelope = %+v, want one resourceSpans/scopeSpans", p)
	}
	spans := p.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	sp := spans[0]
	if sp.Name != "codvian.request" || sp.Kind != 2 {
		t.Errorf("span = %s kind %d, want codvian.request kind 2 (server)", sp.Name, sp.Kind)
	}
	if _, err := hex.DecodeString(sp.TraceID); err == nil {
		t.Error("traceId decoded as hex; OTLP/JSON wants base64 of raw bytes")
	}
	raw, err := base64.StdEncoding.DecodeString(sp.TraceID)
	if err != nil || len(raw) != 16 {
		t.Errorf("traceId base64 → %d bytes, err %v; want 16-byte trace id", len(raw), err)
	}
	rawSpan, err := base64.StdEncoding.DecodeString(sp.SpanID)
	if err != nil || len(rawSpan) != 8 {
		t.Errorf("spanId base64 → %d bytes, err %v; want 8-byte span id", len(rawSpan), err)
	}
	found := false
	for _, a := range sp.Attributes {
		if a.Key == "http.route" && a.Value.StringValue == "/v1/chat/completions" {
			found = true
		}
	}
	if !found {
		t.Errorf("attributes missing http.route: %+v", sp.Attributes)
	}
	startNS := parseInt64(t, sp.StartTimeUnixNano)
	endNS := parseInt64(t, sp.EndTimeUnixNano)
	if endNS < startNS {
		t.Errorf("times %d..%d, want end >= start", startNS, endNS)
	}
	// The local ring still records (export ADDS to, never replaces).
	if got := len(tr.Spans()); got != 1 {
		t.Errorf("local ring spans = %d, want 1 (export must not replace the ring)", got)
	}
}

// TestOTLPExporterDropsWhenCollectorDown proves the failure rule: a
// failed POST drops the batch (counted), never blocks, never grows the
// queue, and later spans are attempted again after the backoff.
func TestOTLPExporterDropsWhenCollectorDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	url := srv.URL
	srv.Close() // unreachable from here

	tr := NewConfiguredTracer(TracerConfig{Endpoint: url, TimeoutMs: 500})
	for i := 0; i < 3; i++ {
		_, span := tr.StartRequestSpan(context.Background(), "req")
		span.End()
	}
	time.Sleep(2 * otlpBatchWait)
	if d := tr.exporter.Drops(); d < 1 {
		t.Fatalf("drops = %d, want >= 1 for an unreachable collector", d)
	}
}

// TestOTLPExporterRejectsInvalidEndpoint: a non-URL or non-http(s)
// endpoint never starts exporting — silently sending traces to a junk
// address would be worse than recording locally only.
func TestOTLPExporterRejectsInvalidEndpoint(t *testing.T) {
	for _, bad := range []string{"://nope", "ftp://collector/v1/traces"} {
		tr := NewConfiguredTracer(TracerConfig{Endpoint: bad, TimeoutMs: 500})
		if tr.exporter != nil {
			t.Errorf("endpoint %q started an exporter, want none", bad)
		}
		if len(tr.Spans()) != 0 {
			t.Errorf("unexpected local spans for %q", bad)
		}
	}
}