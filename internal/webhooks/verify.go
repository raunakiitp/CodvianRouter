package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WebhookReplayWindow is the maximum age (and tolerated clock skew) a
// generic webhook delivery's timestamp may have before VerifyDelivery
// rejects it as a replay. The sender emits the signed envelope
// timestamp both in the body and (mirrored) in the X-Codvian-Timestamp
// header; a receiver should verify within this window and reject
// anything older as a replay. Documented here so senders and
// subscribers agree on the boundary.
const WebhookReplayWindow = 5 * time.Minute

// TimestampHeader is the delivery header carrying the envelope
// timestamp (RFC3339), mirroring the signed body's "timestamp" field.
const TimestampHeader = "X-Codvian-Timestamp"

var (
	// ErrSignatureMismatch means the HMAC over the body did not match the
	// X-Codvian-Signature-256 header.
	ErrSignatureMismatch = errors.New("webhooks: signature does not match")
	// ErrStaleWebhookTimestamp means the delivery's (signed) timestamp is
	// outside WebhookReplayWindow — a replayed or badly-skewed delivery.
	ErrStaleWebhookTimestamp = errors.New("webhooks: delivery timestamp is outside the replay window")
)

// VerifyDelivery is the verifying half of the delivery contract: it
// authenticates a generic (signed) webhook delivery and rejects a
// replayed one. Order of checks:
//
//  1. the HMAC-SHA256 over the raw body must equal sigHeader
//     ("sha256=<hex>"), compared in constant time. The body already
//     contains the timestamp field, so the timestamp is authenticated by
//     the same signature — an attacker cannot move a captured body to a
//     fresh timestamp without breaking the MAC;
//  2. tsHeader (X-Codvian-Timestamp), when present, must equal the
//     signed body timestamp (a mismatch means a forged/mangled header);
//  3. the signed timestamp must be within window of now (both
//     directions, so tolerated clock skew is symmetric); a delivery
//     older (or newer) than window is rejected as stale/replayed.
//
// window <= 0 falls back to WebhookReplayWindow. A stale timestamp
// returns an error wrapping ErrStaleWebhookTimestamp. Chat-kind
// deliveries (Slack/Discord/Telegram) are not signed and are not
// verified here.
func VerifyDelivery(secret string, body []byte, sigHeader, tsHeader string, now time.Time, window time.Duration) error {
	if window <= 0 {
		window = WebhookReplayWindow
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sigHeader)) != 1 {
		return ErrSignatureMismatch
	}
	var env struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("webhooks: delivery body is not JSON: %w", err)
	}
	if tsHeader != "" && tsHeader != env.Timestamp {
		return fmt.Errorf("webhooks: %s %q does not match the signed body timestamp %q", TimestampHeader, tsHeader, env.Timestamp)
	}
	ts, err := time.Parse(time.RFC3339, env.Timestamp)
	if err != nil {
		return fmt.Errorf("webhooks: delivery timestamp %q is not RFC3339: %w", env.Timestamp, err)
	}
	age := now.Sub(ts)
	if age < 0 {
		age = -age
	}
	if age > window {
		return fmt.Errorf("%w: age %s exceeds %s", ErrStaleWebhookTimestamp, age.Round(time.Second), window)
	}
	return nil
}
