// Package webhooks delivers outbound webhook events to
// operator-configured endpoints (M5.9). Today only the
// "quota.exceeded" event is emitted (OmniRoute webhookDispatcher
// notifyWebhookEvent): it fires, best-effort, whenever a quota
// schedule blocks a request. Delivery never blocks the request
// that triggered it: Notify hands the POST to a goroutine, drops
// failures on the floor (logged by the caller-supplied logger)
// after the bounded retry policy below, and never queues work
// beyond that one goroutine — an endpoint that is down must not
// queue up work in the gateway. When a secret is configured the
// body is signed with HMAC-SHA256 (header
// X-Codvian-Signature-256: sha256=<hex>).
//
// RETRY POLICY (M8.19 merge follow-up — matches OmniRoute's
// deliverWebhook, src/lib/webhookDispatcher.ts:83-140, in
// capability, not copied code): one delivery is retried up to
// webhookMaxRetries times (3 retries, 4 attempts total) with
// exponential backoff (1s, 2s, 4s) and a per-attempt timeout
// (10s). Only 5xx responses and transport errors are retried:
// a 2xx is success and anything below 500 is terminal (a 4xx
// means the endpoint rejected the payload — retrying the same
// body would fail the same way). Retries never queue work
// beyond the one in-flight Notify goroutine, and the attempt
// count bounds the whole sequence — there is no unbounded
// redelivery, no persistent queue, and no retry across process
// restarts (edge-trigger state is in-memory; see
// internal/server/webhooks_api.go).
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// QuotaExceededEvent is the event name for a quota threshold
// being reached (OmniRoute "quota.exceeded").
const QuotaExceededEvent = "quota.exceeded"

// Endpoint is one subscribed webhook endpoint.
type Endpoint struct {
	ID     string
	URL    string
	Events []string
	Secret string
}

// Dispatcher delivers events to the configured endpoints.
type Dispatcher struct {
	endpoints []Endpoint
	client    *http.Client
	log       *log.Logger
}

// webhookMaxRetries is the retry budget for one delivery: 3
// retries after the first attempt (4 attempts total),
// matching OmniRoute's deliverWebhook default (maxRetries = 3,
// src/lib/webhookDispatcher.ts:83).
const webhookMaxRetries = 3

// webhookAttemptTimeout bounds ONE delivery attempt (10s,
// matching OmniRoute's per-attempt AbortController timeout,
// webhookDispatcher.ts:101). The budget belongs to each
// attempt, not to the whole sequence: the attempt count bounds
// the sequence instead.
const webhookAttemptTimeout = 10 * time.Second

// webhookRetryBackoff is the delay before the next attempt
// after failedAttempts failed attempts: 2^(n-1) seconds —
// 1s, 2s, 4s — matching OmniRoute (Math.pow(2, attempt) *
// 1000, webhookDispatcher.ts:125,134). A variable, not a
// const, so tests can shrink it to zero; production never
// reassigns it.
var webhookRetryBackoff = func(failedAttempts int) time.Duration {
	return time.Duration(1<<(failedAttempts-1)) * time.Second
}

// New builds a dispatcher. A nil logger disables delivery logging
// (deliveries still happen); a nil client uses no client-level
// timeout — each attempt is bounded by its own 10s context
// (webhookAttemptTimeout), which is the only timeout that
// applies.
func New(endpoints []Endpoint, client *http.Client, logger *log.Logger) *Dispatcher {
	if client == nil {
		client = &http.Client{}
	}
	return &Dispatcher{endpoints: endpoints, client: client, log: logger}
}

// Notify fires event with payload to every endpoint subscribed
// to it, asynchronously. It never blocks and never returns an
// error: delivery failures are logged and dropped.
func (d *Dispatcher) Notify(event string, payload map[string]any) {
	if d == nil {
		return
	}
	body, err := json.Marshal(map[string]any{
		"event":     event,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"data":      payload,
	})
	if err != nil {
		return
	}
	for _, ep := range d.endpoints {
		if !ep.subscribed(event) {
			continue
		}
		ep := ep
		go d.deliver(ep, event, body)
	}
}

func (e Endpoint) subscribed(event string) bool {
	for _, ev := range e.Events {
		if ev == event {
			return true
		}
	}
	return false
}

func (d *Dispatcher) deliver(ep Endpoint, event string, body []byte) {
	// The HMAC is over the body, which never changes across
	// attempts, so the signature is computed once and sent
	// on every attempt.
	sig := ""
	if ep.Secret != "" {
		mac := hmac.New(sha256.New, []byte(ep.Secret))
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	for attempt := 0; attempt <= webhookMaxRetries; attempt++ {
		// One fresh context per attempt: a slow attempt
		// consumes its own 10s, never the next attempt's.
		ctx, cancel := context.WithTimeout(context.Background(), webhookAttemptTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
		if err != nil {
			cancel()
			d.logf("webhook %s: build request: %v", ep.ID, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "codvian-router-webhook/1")
		req.Header.Set("X-Codvian-Event", event)
		if sig != "" {
			req.Header.Set("X-Codvian-Signature-256", sig)
		}
		resp, err := d.client.Do(req)
		cancel()
		if err != nil {
			// Transport error: retryable (a refused
			// connection or a reset may clear on the next
			// attempt). The URL itself was validated at
			// registration, so there is no permanently
			// blocked target to fail closed on here —
			// unlike OmniRoute, which never retries its
			// OutboundUrlGuardError.
			if attempt == webhookMaxRetries {
				d.logf("webhook %s: delivery failed after %d attempts: %v", ep.ID, webhookMaxRetries+1, err)
				return
			}
			d.logf("webhook %s: delivery failed (attempt %d/%d): %v; retrying", ep.ID, attempt+1, webhookMaxRetries+1, err)
			time.Sleep(webhookRetryBackoff(attempt + 1))
			continue
		}
		if resp.StatusCode < 500 {
			// 2xx is success; 3xx/4xx is terminal — the
			// endpoint rejected this payload, and the same
			// body would fail the same way again.
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				d.logf("webhook %s: endpoint returned %s", ep.ID, resp.Status)
			}
			resp.Body.Close()
			return
		}
		// 5xx: retryable — the endpoint is failing, not
		// refusing this payload.
		resp.Body.Close()
		if attempt == webhookMaxRetries {
			d.logf("webhook %s: endpoint returned %s after %d attempts", ep.ID, resp.Status, webhookMaxRetries+1)
			return
		}
		d.logf("webhook %s: endpoint returned %s (attempt %d/%d); retrying", ep.ID, resp.Status, attempt+1, webhookMaxRetries+1)
		time.Sleep(webhookRetryBackoff(attempt + 1))
	}
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.log != nil {
		d.log.Printf(format, args...)
	}
}
