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
// (10s). Retried: 5xx responses, transport errors, and 429 with
// Retry-After honored (AUD-REL-007 — a rate-limited endpoint is
// asking for slower traffic, not rejecting the payload; a
// Retry-After hint, delta-seconds or HTTP-date, replaces the
// exponential delay and is capped at webhookRetryAfterCap).
// Terminal: a 2xx is success and every other 4xx/3xx means the
// endpoint rejected the payload — retrying the same
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
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/egress"
	"github.com/raunakiitp/codvian-router/internal/webhooks/integrations"
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

// BudgetWarningEvent fires when an internal budget crosses its
// warning threshold (the server's budget machinery edge-triggers it
// via notifyBudgetWarning).
const BudgetWarningEvent = "budget.warning"

// Evaluation events, emitted by the eval scheduler when a scheduled
// evaluation completes, fails, or regresses against its baseline.
// They carry schedule_id + run_id and nothing else (no provider
// credential, no prompt).
const (
	EvalCompletedEvent  = "eval.completed"
	EvalRunFailedEvent  = "eval.run.failed"
	EvalRegressionEvent = "eval.regression"
)

// KnownEvents returns the canonical set of webhook event names a
// subscription may name. It is the SINGLE source of truth shared by
// the config-file validator (internal/config) and the registration
// API (internal/server), so the two sets can never drift: an event
// that one accepts is accepted by the other, and the same unknown
// name is rejected by both. The returned slice is a fresh copy, so a
// caller cannot mutate the set for everyone else.
//
// Every name here has a real emit site in the gateway; do not add a
// name without one (a subscribable-but-never-emitted event is a fake
// capability).
func KnownEvents() []string {
	return []string{
		QuotaExceededEvent,
		BudgetWarningEvent,
		ProviderDegradedEvent,
		ProviderRecoveredEvent,
		RequestCompletedEvent,
		RequestFailedEvent,
		ProviderExhaustedEvent,
		EvalCompletedEvent,
		EvalRunFailedEvent,
		EvalRegressionEvent,
		TestPingEvent,
	}
}

// Endpoint is one subscribed webhook endpoint. Kind selects the
// delivery payload shape: "" (or an unknown value, rejected at config
// validation) is the generic signed JSON envelope; "slack" / "discord"
// / "telegram" are chat integrations (see internal/webhooks/integrations)
// whose bodies are the platform format and are NOT HMAC-signed.
// Metadata carries optional per-endpoint secrets for chat kinds (e.g. a
// sealed telegram bot token, unsealed via SetTokenDecrypt).
type Endpoint struct {
	ID       string
	URL      string
	Events   []string
	Secret   string
	Kind     string
	Metadata map[string]string
}

// Dispatcher delivers events to the configured endpoints.
type Dispatcher struct {
	endpoints []Endpoint
	client    *http.Client
	log       *log.Logger
	// tokenDecrypt, when set, unseals a chat-kind token held in an
	// endpoint's Metadata (a sealed value produced by the config Vault).
	// nil = identity (a plaintext metadata token, or none).
	tokenDecrypt func(string) (string, error)
}

// SetTokenDecrypt installs the unseal function used for a chat-kind
// token stored in endpoint Metadata (the config Vault's Unseal is the
// production value). nil disables decryption (metadata tokens are then
// read as plaintext).
func (d *Dispatcher) SetTokenDecrypt(fn func(string) (string, error)) {
	if d != nil {
		d.tokenDecrypt = fn
	}
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

// webhookRetryAfterCap bounds any Retry-After delay a 429 endpoint
// asks for (AUD-REL-007): an endpoint answering "Retry-After: 86400"
// must not park the delivery goroutine for a day when the whole
// delivery budget is 4 attempts. A var, not a const, so tests can
// shrink it just like webhookRetryBackoff; production never reassigns it.
var webhookRetryAfterCap = 60 * time.Second

// webhookRetryAfter parses the Retry-After header of an HTTP 429
// response and returns the delay to wait before the next attempt,
// or 0 when the header is absent/invalid (the caller then falls
// back to the exponential backoff). RFC 7231 allows two forms:
// delta-seconds ("120") and an HTTP-date. A date in the past, a
// negative delta, or an unparseable value all mean "no usable
// hint". The result is capped at webhookRetryAfterCap.
func webhookRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return min(time.Duration(secs)*time.Second, webhookRetryAfterCap)
	}
	if t, err := http.ParseTime(h); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
		return min(d, webhookRetryAfterCap)
	}
	return 0
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
	ts := time.Now()
	body, err := json.Marshal(map[string]any{
		"event":     event,
		"timestamp": ts.UTC().Format(time.RFC3339),
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
		go d.safeDeliver(ep, event, body)
	}
}

// NotifyEndpoint delivers event+payload to the single endpoint with
// the given id, asynchronously, REGARDLESS of that endpoint's
// subscription set. It is the test-send path: an operator asking for
// a test delivery to an endpoint must get one even when the endpoint
// does not (yet) subscribe to the event, otherwise the only way to
// test connectivity would be to first subscribe — which is what the
// test exists to validate. Returns false when the dispatcher is nil
// or no endpoint with that id exists (nothing is delivered).
func (d *Dispatcher) NotifyEndpoint(id, event string, payload map[string]any) bool {
	if d == nil {
		return false
	}
	body, err := json.Marshal(map[string]any{
		"event":     event,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"data":      payload,
	})
	if err != nil {
		return false
	}
	for _, ep := range d.endpoints {
		if ep.ID == id {
			ep := ep
			go d.safeDeliver(ep, event, body)
			return true
		}
	}
	return false
}

// safeDeliver runs one endpoint delivery in its own goroutine and
// recovers any panic (a panicking transport, client or signature
// path) so webhook delivery can never take the gateway process
// down. The delivery itself is best-effort and already drops
// failures; this only adds the last-resort guard the request path
// has and the background path lacked.
func (d *Dispatcher) safeDeliver(ep Endpoint, event string, body []byte) {
	defer func() {
		if r := recover(); r != nil {
			d.logf("webhook %s: delivery panicked: %v", ep.ID, r)
		}
	}()
	d.deliver(ep, event, body)
}

func (e Endpoint) subscribed(event string) bool {
	for _, ev := range e.Events {
		if ev == event {
			return true
		}
	}
	return false
}

// deliver sends one body to one endpoint. A chat-kind endpoint gets a
// platform-specific body built from the envelope payload and is NOT
// signed (Slack/Discord/Telegram do not verify an HMAC); every other
// endpoint sends the signed JSON envelope, exactly as before.
func (d *Dispatcher) deliver(ep Endpoint, event string, body []byte) {
	kind, isChat := integrations.ParseKind(ep.Kind)
	sig := ""
	targetURL := ep.URL
	if isChat {
		var env struct {
			Timestamp string         `json:"timestamp"`
			Data      map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			d.logf("webhook %s: decode envelope for %s: %v", ep.ID, kind, err)
			return
		}
		ts, perr := time.Parse(time.RFC3339, env.Timestamp)
		if perr != nil {
			ts = time.Now()
		}
		chatID := ""
		if kind == integrations.KindTelegram {
			if sealed := ep.Metadata["bot_token"]; sealed != "" {
				// The token is held in (sealed) metadata; the chat_id —
				// the destination — still lives in the endpoint URL.
				plain := sealed
				if d.tokenDecrypt != nil {
					var derr error
					if plain, derr = d.tokenDecrypt(sealed); derr != nil {
						d.logf("webhook %s: decrypt telegram bot token: %v", ep.ID, derr)
						return
					}
				}
				cid, cerr := integrations.ChatIDFromURL(ep.URL)
				if cerr != nil {
					d.logf("webhook %s: %v", ep.ID, cerr)
					return
				}
				built, berr := integrations.BuildTelegramURL(plain, cid)
				if berr != nil {
					d.logf("webhook %s: build telegram url: %v", ep.ID, berr)
					return
				}
				targetURL, chatID = built, cid
			} else {
				cid, perr := integrations.ChatIDFromURL(ep.URL)
				if perr != nil {
					d.logf("webhook %s: %v", ep.ID, perr)
					return
				}
				chatID = cid
			}
		}
		out, terr := integrations.Transform(kind, event, env.Data, chatID, ts)
		if terr != nil {
			d.logf("webhook %s: transform %s payload: %v", ep.ID, kind, terr)
			return
		}
		body = out
	} else if ep.Secret != "" {
		// The HMAC is over the body, which never changes across
		// attempts, so the signature is computed once and sent
		// on every attempt.
		mac := hmac.New(sha256.New, []byte(ep.Secret))
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	for attempt := 0; attempt <= webhookMaxRetries; attempt++ {
		// One fresh context per attempt: a slow attempt
		// consumes its own 10s, never the next attempt's.
		ctx, cancel := context.WithTimeout(context.Background(), webhookAttemptTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
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
			if errors.Is(err, egress.ErrBlocked) {
				d.logf("webhook %s: delivery blocked by egress policy: %v", ep.ID, err)
				return
			}
			if attempt == webhookMaxRetries {
				d.logf("webhook %s: delivery failed after %d attempts: %v", ep.ID, webhookMaxRetries+1, err)
				return
			}
			d.logf("webhook %s: delivery failed (attempt %d/%d): %v; retrying", ep.ID, attempt+1, webhookMaxRetries+1, err)
			time.Sleep(webhookRetryBackoff(attempt + 1))
			continue
		}
		if resp.StatusCode < 500 {
			if resp.StatusCode == http.StatusTooManyRequests {
				resp.Body.Close()
				if attempt == webhookMaxRetries {
					d.logf("webhook %s: endpoint returned %s after %d attempts", ep.ID, resp.Status, webhookMaxRetries+1)
					return
				}
				delay := webhookRetryBackoff(attempt + 1)
				if ra := webhookRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ra > 0 {
					delay = ra
				}
				d.logf("webhook %s: endpoint returned %s (attempt %d/%d); retrying after %s", ep.ID, resp.Status, attempt+1, webhookMaxRetries+1, delay)
				time.Sleep(delay)
				continue
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				d.logf("webhook %s: endpoint returned %s", ep.ID, resp.Status)
			}
			resp.Body.Close()
			return
		}
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
