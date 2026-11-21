//go:build otel

// OTLP/HTTP trace export (Phase 2 observability). The in-process tracer
// records finished spans to a bounded ring; when the operator sets an
// OTLP endpoint, a background batcher ALSO ships them to that collector
// as OTLP/JSON (one POST per batch to {endpoint}). Design constraints:
//
//   - Never blocks a request: enqueue is select-default, a full queue
//     drops the span and counts it.
//   - Never grows without bound: fixed queue, failed batches are dropped
//     (never requeued) with a drop counter.
//   - Bounded work per POST: timeout on the HTTP client.
//
// Encoding follows the OTLP/JSON mapping (protojson): ids are base64 of
// the raw bytes, nanos are decimal strings, kind maps internal=1,
// server=2.
package observability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	otlpQueueSize = 4096
	otlpBatchSize = 256
	otlpBatchWait = 500 * time.Millisecond
	otlpDefaultTO = 5 * time.Second
)

type otlpExporter struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
	queue    chan SpanData
	drops    atomic.Int64
	stop     chan struct{}
	wg       sync.WaitGroup
}

func newOTLPExporter(endpoint string, headers map[string]string, timeoutMs int) *otlpExporter {
	to := otlpDefaultTO
	if timeoutMs > 0 {
		to = time.Duration(timeoutMs) * time.Millisecond
	}
	e := &otlpExporter{
		endpoint: endpoint,
		headers:  headers,
		client:   &http.Client{Timeout: to},
		queue:    make(chan SpanData, otlpQueueSize),
		stop:     make(chan struct{}),
	}
	e.wg.Add(1)
	go e.loop()
	return e
}

// enqueue offers sd to the export queue without ever blocking the caller.
func (e *otlpExporter) enqueue(sd SpanData) {
	select {
	case e.queue <- sd:
	default:
		e.drops.Add(1)
	}
}

// Drops reports how many spans were dropped for a full queue or a failed
// POST (export pressure or an unreachable collector).
func (e *otlpExporter) Drops() int64 { return e.drops.Load() }

// Close stops the loop after the queue drains or the timeout passes.
func (e *otlpExporter) Close() {
	close(e.stop)
	e.wg.Wait()
}

func (e *otlpExporter) loop() {
	defer e.wg.Done()
	batch := make([]SpanData, 0, otlpBatchSize)
	timer := time.NewTimer(otlpBatchWait)
	defer timer.Stop()
	paused := time.Duration(0)
	for {
		if paused > 0 {
			// Back off after a failed POST; drain nothing meanwhile.
			t := time.NewTimer(paused)
			select {
			case <-t.C:
			case <-e.stop:
				t.Stop()
				return
			}
			paused = 0
			batch = batch[:0]
		}
		select {
		case sd := <-e.queue:
			batch = append(batch, sd)
			if len(batch) >= otlpBatchSize {
				e.flush(batch)
				batch = batch[:0]
				resetTimer(timer, otlpBatchWait)
			}
		case <-timer.C:
			if len(batch) > 0 {
				if !e.flush(batch) {
					// Drop the failed batch; the next wait is longer.
					paused = otlpBatchWait
				}
				batch = batch[:0]
			}
			resetTimer(timer, otlpBatchWait)
		case <-e.stop:
			// Best-effort drain, one bounded flush.
			for len(batch) > 0 {
				e.flush(batch)
				batch = batch[:0]
			}
			return
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// flush POSTs one batch. Success is a 2xx; anything else drops the batch
// and reports failure so the loop backs off.
func (e *otlpExporter) flush(batch []SpanData) bool {
	if len(batch) == 0 {
		return true
	}
	body, err := json.Marshal(otlpPayload(batch))
	if err != nil {
		e.drops.Add(int64(len(batch)))
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		e.drops.Add(int64(len(batch)))
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.drops.Add(int64(len(batch)))
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e.drops.Add(int64(len(batch)))
		return false
	}
	return true
}

// --- OTLP/JSON payload shaping ---

type otlpValue struct {
	StringValue string `json:"stringValue"`
}

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpAttr `json:"attributes,omitempty"`
}

type otlpScopeSpans struct {
	Scope struct {
		Name string `json:"name"`
	} `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpResourceSpans struct {
	Resource struct {
		Attributes []otlpAttr `json:"attributes"`
	} `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpEnvelope struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

func otlpPayload(spans []SpanData) otlpEnvelope {
	var env otlpEnvelope
	rs := otlpResourceSpans{}
	rs.Resource.Attributes = []otlpAttr{{Key: "service.name", Value: otlpValue{StringValue: "codvian-router"}}}
	ss := otlpScopeSpans{}
	ss.Scope.Name = "codvian-router"
	for _, s := range spans {
		sp := otlpSpan{
			TraceID:           b64Hex(s.TraceID),
			SpanID:            b64Hex(s.SpanID),
			ParentSpanID:      b64Hex(s.ParentSpanID),
			Name:              s.Name,
			Kind:              1,
			StartTimeUnixNano: fmt.Sprintf("%d", s.Start.UnixNano()),
			EndTimeUnixNano:   fmt.Sprintf("%d", s.End.UnixNano()),
		}
		if s.Kind == "server" {
			sp.Kind = 2
		}
		for k, v := range s.Attributes {
			sp.Attributes = append(sp.Attributes, otlpAttr{Key: k, Value: otlpValue{StringValue: v}})
		}
		ss.Spans = append(ss.Spans, sp)
	}
	rs.ScopeSpans = []otlpScopeSpans{ss}
	env.ResourceSpans = []otlpResourceSpans{rs}
	return env
}

// b64Hex renders a hex id as base64 of its raw bytes, per the OTLP/JSON
// mapping. An unparseable id becomes empty (the collector rejects it
// rather than the gateway inventing one).
func b64Hex(h string) string {
	b, err := hex.DecodeString(h)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// NewConfiguredTracer returns the otel-tagged tracer. When cfg.Endpoint
// is set AND a valid http(s) collector URL, the tracer keeps its
// in-process ring AND ships spans to the collector; anything else
// (empty or malformed endpoint) keeps spans local only — silently
// exporting to a junk address would be worse than not exporting.
func NewConfiguredTracer(cfg TracerConfig) *Tracer {
	t := NewTracer()
	if cfg.Endpoint == "" {
		return t
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return t
	}
	t.exporter = newOTLPExporter(cfg.Endpoint, cfg.Headers, cfg.TimeoutMs)
	return t
}
