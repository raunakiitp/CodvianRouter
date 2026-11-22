// Package webhooks delivers outbound webhook events to
// operator-configured endpoints (M5.9). Today only the
// "quota.exceeded" event is emitted (the reference router webhookDispatcher
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
// RETRY POLICY (M8.19 merge follow-up — matches the reference router's
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
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/egress"
)

// QuotaExceededEvent is the event name for a quota threshold
// being reached (the reference router "quota.exceeded").
const QuotaExceededEvent = "quota.exceeded"

// RequestCompletedEvent fires when a chat request completes
// successfully (a provider served a response).
const RequestCompletedEvent = "request.completed"

// RequestFailedEvent fires when a chat request fails
// (no provider could serve it, or every attempt errored).
const RequestFailedEvent = "request.failed"

// ProviderExhaustedEvent fires when every provider in the
// routing chain was exhausted (no attempt available).
const ProviderExhaustedEvent = "provider.exhausted"

// TestPingEvent is a synthetic event for delivery testing.
// Subscribers receive a minimal payload; it carries no
// request data and is safe to emit in tests.
const TestPingEvent = "test.ping"

// ProviderDegradedEvent fires when a provider's circuit breaker first
// trips (CLOSED -> OPEN), with only the provider id and the real
// consecutive-failure/trip counts and open duration in the payload.
// ProviderRecoveredEvent fires when that OPEN breaker later clears on
// a real success. Both are edge-triggered: steady failures while the
// breaker is already open, and ordinary successes by a healthy
// provider, never emit an event. Neither payload contains a key, a
// prompt or any provider credential.
const (
	ProviderDegradedEvent  = "provider.degraded"
	ProviderRecoveredEvent = "provider.recovered"
)

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
// matching the reference router's deliverWebhook default (maxRetries = 3,
// src/lib/webhookDispatcher.ts:83).
const webhookMaxRetries = 3

// webhookAttemptTimeout bounds ONE delivery attempt (10s,
// matching the reference router's per-attempt AbortController timeout,
// webhookDispatcher.ts:101). The budget belongs to each
// attempt, not to the whole sequence: the attempt count bounds
// the sequence instead.
const webhookAttemptTimeout = 10 * time.Second

// webhookRetryBackoff is the delay before the next attempt
// after failedAttempts failed attempts: 2^(n-1) seconds —
// 1s, 2s, 4s — matching the reference router (Math.pow(2, attempt) *
// 1000, webhookDispatcher.ts:125,134). A variable, not a
// const, so tests can shrink it to zero; production never
// reassigns it.
var webhookRetryBackoff = func(failedAttempts int) time.Duration {
	return time.Duration(1<<(failedAttempts-1)) * time.Second
}

// New builds a dispatcher. A nil logger disables delivery logging
// (deliveries still happen). A nil client is the production default:
// the SSRF-safe egress client (internal/egress) - every connection,
// including each retry, resolves the host once, refuses loopback /
// private / link-local / metadata / reserved and mixed answers, and
// dials only the validated IP; redirects are not followed (a 3xx is
// a terminal answer, as before); environment proxies are ignored;
// TLS verifies the hostname. Registration-time validation alone was
// not enough: the old plain client resolved DNS again at delivery,
// so a rebinding host could pass registration and then point at an
// internal address. Each attempt is bounded by its own 10s context
// (webhookAttemptTimeout). A non-nil client is used as given (tests).
func New(endpoints []Endpoint, client *http.Client, logger *log.Logger) *Dispatcher {
	if client == nil {
		client = egress.Default().Client(0, 0, nil)
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
			// A destination refused by the egress policy is
			// terminal: retrying would only re-resolve and be
			// refused again (or, worse, be retried until a
			// rebinding host flips to an allowed answer).
			if errors.Is(err, egress.ErrBlocked) {
				d.logf("webhook %s: delivery blocked by egress policy: %v", ep.ID, err)
				return
			}
			// Other transport errors are retryable (a refused
			// connection or a reset may clear on the next
			// attempt).
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
