package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// buildSigned constructs a generic delivery body + its HMAC the way the
// dispatcher does, so the verifier can be exercised independently.
func buildSigned(t *testing.T, event, ts string, data map[string]any, secret string) (body []byte, sig string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"event": event, "timestamp": ts, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return body, "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// A4 item 6: a fresh delivery verifies.
func TestVerifyDeliveryAcceptsFresh(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Add(-30 * time.Second).Format(time.RFC3339)
	body, sig := buildSigned(t, QuotaExceededEvent, ts, map[string]any{"reason": "budget"}, "s3cr3t")
	if err := VerifyDelivery("s3cr3t", body, sig, ts, now, 0); err != nil {
		t.Fatalf("fresh delivery rejected: %v", err)
	}
}

// A4 item 6 (required replay test): a delivery whose signed timestamp is
// older than the replay window is rejected as a replay.
func TestVerifyDeliveryRejectsStaleTimestamp(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339) // > 5m window
	body, sig := buildSigned(t, QuotaExceededEvent, stale, map[string]any{"reason": "budget"}, "s3cr3t")
	err := VerifyDelivery("s3cr3t", body, sig, stale, now, 0)
	if err == nil {
		t.Fatal("stale (replayed) delivery was accepted, want rejection")
	}
	if !errors.Is(err, ErrStaleWebhookTimestamp) {
		t.Fatalf("error = %v, want it to wrap ErrStaleWebhookTimestamp", err)
	}
}

// A future timestamp beyond the window is also rejected (symmetric skew).
func TestVerifyDeliveryRejectsFutureTimestamp(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(10 * time.Minute).Format(time.RFC3339)
	body, sig := buildSigned(t, QuotaExceededEvent, future, nil, "s3cr3t")
	if err := VerifyDelivery("s3cr3t", body, sig, future, now, 0); !errors.Is(err, ErrStaleWebhookTimestamp) {
		t.Fatalf("future delivery err = %v, want ErrStaleWebhookTimestamp", err)
	}
}

// A tampered body breaks the MAC.
func TestVerifyDeliveryRejectsTamperedBody(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	body, sig := buildSigned(t, QuotaExceededEvent, ts, map[string]any{"reason": "budget"}, "s3cr3t")
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] = 'X' // corrupt one byte
	if err := VerifyDelivery("s3cr3t", tampered, sig, ts, now, 0); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("tampered body err = %v, want ErrSignatureMismatch", err)
	}
}

// A header timestamp that does not match the signed body is rejected.
func TestVerifyDeliveryRejectsHeaderMismatch(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	body, sig := buildSigned(t, QuotaExceededEvent, ts, nil, "s3cr3t")
	if err := VerifyDelivery("s3cr3t", body, sig, now.Add(time.Hour).Format(time.RFC3339), now, 0); err == nil {
		t.Fatal("mismatched timestamp header accepted, want rejection")
	}
}

// A custom window is honored.
func TestVerifyDeliveryCustomWindow(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Add(-2 * time.Minute).Format(time.RFC3339)
	body, sig := buildSigned(t, QuotaExceededEvent, ts, nil, "s3cr3t")
	if err := VerifyDelivery("s3cr3t", body, sig, ts, now, 3*time.Minute); err != nil {
		t.Fatalf("2m-old delivery with a 3m window rejected: %v", err)
	}
	if err := VerifyDelivery("s3cr3t", body, sig, ts, now, time.Minute); !errors.Is(err, ErrStaleWebhookTimestamp) {
		t.Fatalf("2m-old delivery with a 1m window err = %v, want stale", err)
	}
}

// A4 item 6 (sender half): the dispatcher sets X-Codvian-Timestamp equal
// to the signed body timestamp on a signed delivery.
func TestDeliverSetsTimestampHeader(t *testing.T) {
	var mu sync.Mutex
	var gotTS string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotTS = r.Header.Get(TimestampHeader)
		gotBody = b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New([]Endpoint{{ID: "ep", URL: srv.URL, Events: []string{QuotaExceededEvent}, Secret: "s3cr3t"}}, srv.Client(), nil)
	d.Notify(QuotaExceededEvent, map[string]any{"reason": "budget"})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ts, b := gotTS, append([]byte(nil), gotBody...)
		mu.Unlock()
		if ts != "" {
			var env struct {
				Timestamp string `json:"timestamp"`
			}
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatalf("delivered body not JSON: %v", err)
			}
			if ts != env.Timestamp {
				t.Errorf("X-Codvian-Timestamp = %q, want the body timestamp %q", ts, env.Timestamp)
			}
			// End-to-end: the same delivery verifies.
			mac := hmac.New(sha256.New, []byte("s3cr3t"))
			mac.Write(b)
			sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if err := VerifyDelivery("s3cr3t", b, sig, ts, time.Now(), 0); err != nil {
				t.Errorf("delivered webhook failed verification: %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no delivery arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
